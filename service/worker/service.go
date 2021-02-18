// The MIT License (MIT)
//
// Copyright (c) 2017-2020 Uber Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/definition"
	"github.com/uber/cadence/common/domain"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/persistence"
	persistenceClient "github.com/uber/cadence/common/persistence/client"
	"github.com/uber/cadence/common/resource"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/common/service/dynamicconfig"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/worker/archiver"
	"github.com/uber/cadence/service/worker/batcher"
	"github.com/uber/cadence/service/worker/failovermanager"
	"github.com/uber/cadence/service/worker/indexer"
	"github.com/uber/cadence/service/worker/parentclosepolicy"
	"github.com/uber/cadence/service/worker/replicator"
	"github.com/uber/cadence/service/worker/scanner"
	"github.com/uber/cadence/service/worker/scanner/executions"
	"github.com/uber/cadence/service/worker/scanner/shardscanner"
	"github.com/uber/cadence/service/worker/scanner/timers"
)

type (
	// Service represents the cadence-worker service. This service hosts all background processing needed for cadence cluster:
	// 1. Replicator: Handles applying replication tasks generated by remote clusters.
	// 2. Indexer: Handles uploading of visibility records to elastic search.
	// 3. Archiver: Handles archival of workflow histories.
	Service struct {
		resource.Resource

		status int32
		stopC  chan struct{}
		params *service.BootstrapParams
		config *Config
	}

	// Config contains all the service config for worker
	Config struct {
		ArchiverConfig                    *archiver.Config
		IndexerCfg                        *indexer.Config
		ScannerCfg                        *scanner.Config
		BatcherCfg                        *batcher.Config
		failoverManagerCfg                *failovermanager.Config
		ThrottledLogRPS                   dynamicconfig.IntPropertyFn
		PersistenceGlobalMaxQPS           dynamicconfig.IntPropertyFn
		PersistenceMaxQPS                 dynamicconfig.IntPropertyFn
		EnableBatcher                     dynamicconfig.BoolPropertyFn
		EnableParentClosePolicyWorker     dynamicconfig.BoolPropertyFn
		EnableFailoverManager             dynamicconfig.BoolPropertyFn
		DomainReplicationMaxRetryDuration dynamicconfig.DurationPropertyFn
	}
)

// NewService builds a new cadence-worker service
func NewService(
	params *service.BootstrapParams,
) (resource.Resource, error) {

	serviceConfig := NewConfig(params)

	serviceResource, err := resource.New(
		params,
		common.WorkerServiceName,
		serviceConfig.PersistenceMaxQPS,
		serviceConfig.PersistenceGlobalMaxQPS,
		serviceConfig.ThrottledLogRPS,
		func(
			persistenceBean persistenceClient.Bean,
			logger log.Logger,
		) (persistence.VisibilityManager, error) {
			return persistenceBean.GetVisibilityManager(), nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &Service{
		Resource: serviceResource,
		status:   common.DaemonStatusInitialized,
		config:   serviceConfig,
		params:   params,
		stopC:    make(chan struct{}),
	}, nil
}

// NewConfig builds the new Config for cadence-worker service
func NewConfig(params *service.BootstrapParams) *Config {
	dc := dynamicconfig.NewCollection(
		params.DynamicConfig,
		params.Logger,
		dynamicconfig.ClusterNameFilter(params.ClusterMetadata.GetCurrentClusterName()),
	)
	config := &Config{
		ArchiverConfig: &archiver.Config{
			ArchiverConcurrency:           dc.GetIntProperty(dynamicconfig.WorkerArchiverConcurrency, 50),
			ArchivalsPerIteration:         dc.GetIntProperty(dynamicconfig.WorkerArchivalsPerIteration, 1000),
			TimeLimitPerArchivalIteration: dc.GetDurationProperty(dynamicconfig.WorkerTimeLimitPerArchivalIteration, archiver.MaxArchivalIterationTimeout()),
		},
		ScannerCfg: &scanner.Config{
			ScannerPersistenceMaxQPS: dc.GetIntProperty(dynamicconfig.ScannerPersistenceMaxQPS, 5),
			Persistence:              &params.PersistenceConfig,
			ClusterMetadata:          params.ClusterMetadata,
			TaskListScannerEnabled:   dc.GetBoolProperty(dynamicconfig.TaskListScannerEnabled, true),
			HistoryScannerEnabled:    dc.GetBoolProperty(dynamicconfig.HistoryScannerEnabled, false),
			ShardScanners: []*shardscanner.ScannerConfig{
				executions.ConcreteExecutionScannerConfig(dc),
				executions.CurrentExecutionScannerConfig(dc),
				timers.ScannerConfig(dc),
			},
			MaxWorkflowRetentionInDays: dc.GetIntProperty(dynamicconfig.MaxRetentionDays, domain.DefaultMaxWorkflowRetentionInDays),
		},
		BatcherCfg: &batcher.Config{
			AdminOperationToken: dc.GetStringProperty(dynamicconfig.AdminOperationToken, common.DefaultAdminOperationToken),
			ClusterMetadata:     params.ClusterMetadata,
		},
		failoverManagerCfg: &failovermanager.Config{
			AdminOperationToken: dc.GetStringProperty(dynamicconfig.AdminOperationToken, common.DefaultAdminOperationToken),
			ClusterMetadata:     params.ClusterMetadata,
		},
		EnableBatcher:                     dc.GetBoolProperty(dynamicconfig.EnableBatcher, false),
		EnableParentClosePolicyWorker:     dc.GetBoolProperty(dynamicconfig.EnableParentClosePolicyWorker, true),
		EnableFailoverManager:             dc.GetBoolProperty(dynamicconfig.EnableFailoverManager, true),
		ThrottledLogRPS:                   dc.GetIntProperty(dynamicconfig.WorkerThrottledLogRPS, 20),
		PersistenceGlobalMaxQPS:           dc.GetIntProperty(dynamicconfig.WorkerPersistenceGlobalMaxQPS, 0),
		PersistenceMaxQPS:                 dc.GetIntProperty(dynamicconfig.WorkerPersistenceMaxQPS, 500),
		DomainReplicationMaxRetryDuration: dc.GetDurationProperty(dynamicconfig.WorkerReplicationTaskMaxRetryDuration, 10*time.Minute),
	}
	advancedVisWritingMode := dc.GetStringProperty(
		dynamicconfig.AdvancedVisibilityWritingMode,
		common.GetDefaultAdvancedVisibilityWritingMode(params.PersistenceConfig.IsAdvancedVisibilityConfigExist()),
	)
	if advancedVisWritingMode() != common.AdvancedVisibilityWritingModeOff {
		config.IndexerCfg = &indexer.Config{
			IndexerConcurrency:       dc.GetIntProperty(dynamicconfig.WorkerIndexerConcurrency, 1000),
			ESProcessorNumOfWorkers:  dc.GetIntProperty(dynamicconfig.WorkerESProcessorNumOfWorkers, 1),
			ESProcessorBulkActions:   dc.GetIntProperty(dynamicconfig.WorkerESProcessorBulkActions, 1000),
			ESProcessorBulkSize:      dc.GetIntProperty(dynamicconfig.WorkerESProcessorBulkSize, 2<<24), // 16MB
			ESProcessorFlushInterval: dc.GetDurationProperty(dynamicconfig.WorkerESProcessorFlushInterval, 1*time.Second),
			ValidSearchAttributes:    dc.GetMapProperty(dynamicconfig.ValidSearchAttributes, definition.GetDefaultIndexedKeys()),
		}
	}
	return config
}

// Start is called to start the service
func (s *Service) Start() {
	if !atomic.CompareAndSwapInt32(&s.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}
	logger := s.GetLogger()
	logger.Info("worker starting", tag.ComponentWorker)

	s.Resource.Start()
	s.Resource.GetDomainReplicationQueue().Start()

	s.ensureDomainExists(common.SystemLocalDomainName)
	s.startScanner()
	if s.config.IndexerCfg != nil {
		s.startIndexer()
	}

	if s.GetClusterMetadata().IsGlobalDomainEnabled() {
		s.startReplicator()
	}
	if s.GetArchivalMetadata().GetHistoryConfig().ClusterConfiguredForArchival() {
		s.startArchiver()
	}
	if s.config.EnableBatcher() {
		s.ensureDomainExists(common.BatcherLocalDomainName)
		s.startBatcher()
	}
	if s.config.EnableParentClosePolicyWorker() {
		s.startParentClosePolicyProcessor()
	}
	if s.config.EnableFailoverManager() {
		s.startFailoverManager()
	}

	logger.Info("worker started", tag.ComponentWorker)
	<-s.stopC
}

// Stop is called to stop the service
func (s *Service) Stop() {
	if !atomic.CompareAndSwapInt32(&s.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	close(s.stopC)

	s.Resource.Stop()
	s.Resource.GetDomainReplicationQueue().Stop()

	s.params.Logger.Info("worker stopped", tag.ComponentWorker)
}

func (s *Service) startParentClosePolicyProcessor() {
	params := &parentclosepolicy.BootstrapParams{
		ServiceClient: s.params.PublicClient,
		MetricsClient: s.GetMetricsClient(),
		Logger:        s.GetLogger(),
		TallyScope:    s.params.MetricScope,
		ClientBean:    s.GetClientBean(),
	}
	processor := parentclosepolicy.New(params)
	if err := processor.Start(); err != nil {
		s.GetLogger().Fatal("error starting parentclosepolicy processor", tag.Error(err))
	}
}

func (s *Service) startBatcher() {
	params := &batcher.BootstrapParams{
		Config:        *s.config.BatcherCfg,
		ServiceClient: s.params.PublicClient,
		MetricsClient: s.GetMetricsClient(),
		Logger:        s.GetLogger(),
		TallyScope:    s.params.MetricScope,
		ClientBean:    s.GetClientBean(),
	}
	if err := batcher.New(params).Start(); err != nil {
		s.GetLogger().Fatal("error starting batcher", tag.Error(err))
	}
}

func (s *Service) startScanner() {
	params := &scanner.BootstrapParams{
		Config:     *s.config.ScannerCfg,
		TallyScope: s.params.MetricScope,
	}
	if err := scanner.New(s.Resource, params).Start(); err != nil {
		s.GetLogger().Fatal("error starting scanner", tag.Error(err))
	}
}

func (s *Service) startReplicator() {
	domainReplicationTaskExecutor := domain.NewReplicationTaskExecutor(
		s.Resource.GetMetadataManager(),
		s.Resource.GetTimeSource(),
		s.Resource.GetLogger(),
	)
	msgReplicator := replicator.NewReplicator(
		s.GetClusterMetadata(),
		s.GetClientBean(),
		s.GetLogger(),
		s.GetMetricsClient(),
		s.GetHostInfo(),
		s.GetWorkerServiceResolver(),
		s.GetDomainReplicationQueue(),
		domainReplicationTaskExecutor,
		s.config.DomainReplicationMaxRetryDuration(),
	)
	if err := msgReplicator.Start(); err != nil {
		msgReplicator.Stop()
		s.GetLogger().Fatal("fail to start replicator", tag.Error(err))
	}
}

func (s *Service) startIndexer() {
	visibilityIndexer := indexer.NewIndexer(
		s.config.IndexerCfg,
		s.GetMessagingClient(),
		s.params.ESClient,
		s.params.ESConfig,
		s.GetLogger(),
		s.GetMetricsClient(),
	)
	if err := visibilityIndexer.Start(); err != nil {
		visibilityIndexer.Stop()
		s.GetLogger().Fatal("fail to start indexer", tag.Error(err))
	}
}

func (s *Service) startArchiver() {
	bc := &archiver.BootstrapContainer{
		PublicClient:     s.GetSDKClient(),
		MetricsClient:    s.GetMetricsClient(),
		Logger:           s.GetLogger(),
		HistoryV2Manager: s.GetHistoryManager(),
		DomainCache:      s.GetDomainCache(),
		Config:           s.config.ArchiverConfig,
		ArchiverProvider: s.GetArchiverProvider(),
	}
	clientWorker := archiver.NewClientWorker(bc)
	if err := clientWorker.Start(); err != nil {
		clientWorker.Stop()
		s.GetLogger().Fatal("failed to start archiver", tag.Error(err))
	}
}

func (s *Service) startFailoverManager() {
	params := &failovermanager.BootstrapParams{
		Config:        *s.config.failoverManagerCfg,
		ServiceClient: s.params.PublicClient,
		MetricsClient: s.GetMetricsClient(),
		Logger:        s.GetLogger(),
		TallyScope:    s.params.MetricScope,
		ClientBean:    s.GetClientBean(),
	}
	if err := failovermanager.New(params).Start(); err != nil {
		s.Stop()
		s.GetLogger().Fatal("error starting failoverManager", tag.Error(err))
	}
}

func (s *Service) ensureDomainExists(domain string) {
	_, err := s.GetMetadataManager().GetDomain(context.Background(), &persistence.GetDomainRequest{Name: domain})
	switch err.(type) {
	case nil:
		// noop
	case *types.EntityNotExistsError:
		s.GetLogger().Info(fmt.Sprintf("domain %s does not exist, attempting to register domain", domain))
		s.registerSystemDomain(domain)
	default:
		s.GetLogger().Fatal("failed to verify if system domain exists", tag.Error(err))
	}
}

func (s *Service) registerSystemDomain(domain string) {

	currentClusterName := s.GetClusterMetadata().GetCurrentClusterName()
	_, err := s.GetMetadataManager().CreateDomain(context.Background(), &persistence.CreateDomainRequest{
		Info: &persistence.DomainInfo{
			ID:          getDomainID(domain),
			Name:        domain,
			Description: "Cadence internal system domain",
		},
		Config: &persistence.DomainConfig{
			Retention:  common.SystemDomainRetentionDays,
			EmitMetric: true,
		},
		ReplicationConfig: &persistence.DomainReplicationConfig{
			ActiveClusterName: currentClusterName,
			Clusters:          persistence.GetOrUseDefaultClusters(currentClusterName, nil),
		},
		IsGlobalDomain:  false,
		FailoverVersion: common.EmptyVersion,
	})
	if err != nil {
		if _, ok := err.(*types.DomainAlreadyExistsError); ok {
			return
		}
		s.GetLogger().Fatal("failed to register system domain", tag.Error(err))
	}
}

func getDomainID(domain string) string {
	var domainID string
	switch domain {
	case common.SystemLocalDomainName:
		domainID = common.SystemDomainID
	case common.BatcherLocalDomainName:
		domainID = common.BatcherDomainID
	}
	return domainID
}
