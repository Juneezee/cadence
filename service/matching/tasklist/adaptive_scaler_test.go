// The MIT License (MIT)

// Copyright (c) 2017-2020 Uber Technologies Inc.

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

package tasklist

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"

	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/testlogger"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/stats"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/matching/config"
	"github.com/uber/cadence/service/matching/event"
)

type mockAdaptiveScalerDeps struct {
	id                 *Identifier
	mockManager        *MockManager
	mockQPSTracker     *stats.MockQPSTracker
	mockTimeSource     clock.MockedTimeSource
	mockMatchingClient *matching.MockClient
	dynamicClient      dynamicconfig.Client

	config *config.TaskListConfig
	logger log.Logger
	scope  metrics.Scope
}

func setupMocksForAdaptiveScaler(t *testing.T, taskListID *Identifier) (*adaptiveScalerImpl, *mockAdaptiveScalerDeps) {
	ctrl := gomock.NewController(t)
	logger := testlogger.New(t)
	scope := metrics.NoopScope(metrics.Matching)
	mockManager := NewMockManager(ctrl)
	mockQPSTracker := stats.NewMockQPSTracker(ctrl)
	mockTimeSource := clock.NewMockedTimeSourceAt(time.Now())
	mockMatchingClient := matching.NewMockClient(ctrl)
	dynamicClient := dynamicconfig.NewInMemoryClient()
	cfg := newTaskListConfig(taskListID, config.NewConfig(dynamicconfig.NewCollection(dynamicClient, logger), "test-host", func() []string { return nil }), "test-domain")

	deps := &mockAdaptiveScalerDeps{
		id:                 taskListID,
		mockManager:        mockManager,
		mockQPSTracker:     mockQPSTracker,
		mockTimeSource:     mockTimeSource,
		mockMatchingClient: mockMatchingClient,
		dynamicClient:      dynamicClient,
		config:             cfg,
	}

	scaler := NewAdaptiveScaler(taskListID, mockManager, cfg, mockTimeSource, logger, scope, mockMatchingClient, event.E{}).(*adaptiveScalerImpl)
	return scaler, deps
}

func TestAdaptiveScalerLifecycle(t *testing.T) {
	defer goleak.VerifyNone(t)
	taskListID, err := NewIdentifier("test-domain-id", "test-task-list", 0)
	require.NoError(t, err)
	scaler, _ := setupMocksForAdaptiveScaler(t, taskListID)

	// test idempotency
	assert.NotPanics(t, scaler.Start)
	assert.NotPanics(t, scaler.Start)
	assert.NotPanics(t, scaler.Stop)
	assert.NotPanics(t, scaler.Stop)
}

func TestAdaptiveScalerRun(t *testing.T) {
	testCases := []struct {
		name      string
		mockSetup func(*mockAdaptiveScalerDeps)
		cycles    int
	}{
		{
			name: "no op",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
			},
			cycles: 1,
		},
		{
			name: "overload start",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 300))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
			},
			cycles: 1,
		},
		{
			name: "overload sustained",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				// overload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 300))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)

				// overload passing sustained period
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 300))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(2),
					WritePartitions: partitions(2),
				}).Return(nil)
			},
			cycles: 2,
		},
		{
			name: "overload fluctuate",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				// overload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 300))

				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
				// load back to normal
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 100))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
				// overload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 300))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
				// load back to normal
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 100))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
			},
			cycles: 4,
		},
		{
			name: "underload start",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(10, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(10),
					ReadPartitions:  partitions(10),
				})
			},
			cycles: 1,
		},
		{
			name: "underload sustained",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(10, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(10),
					ReadPartitions:  partitions(10),
				})

				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(10, 0))
				// Partition 9 will be checked if it is drained, but it won't have received the update yet
				mockDescribeTaskList(deps, 9, withPartitionsAndQPS(10, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(10),
					ReadPartitions:  partitions(10),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					WritePartitions: partitions(1),
					ReadPartitions:  partitions(10),
				}).Return(nil)
			},
			cycles: 2,
		},
		{
			name: "underload sustained then drain",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(10, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(10),
					ReadPartitions:  partitions(10),
				})

				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(10, 0))
				// Partition 9 will be checked if it is drained, but it won't have received the update yet
				mockDescribeTaskList(deps, 9, withPartitionsAndQPS(10, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(10),
					ReadPartitions:  partitions(10),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					WritePartitions: partitions(1),
					ReadPartitions:  partitions(10),
				}).Return(nil)

				mockDescribeTaskList(deps, 0, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 9, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 8, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 7, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 6, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 5, withPartitionsAndBacklog(10, 1, 0))
				mockDescribeTaskList(deps, 4, withPartitionsAndBacklog(10, 1, 1))

				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(1),
					ReadPartitions:  partitions(10),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					WritePartitions: partitions(1),
					ReadPartitions:  partitions(5),
				}).Return(nil)
			},
			cycles: 3,
		},
		{
			name: "overload but no fluctuation",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				// overload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 210))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)

				// overload passing sustained period
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(1, 210))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(nil)
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(2),
					WritePartitions: partitions(2),
				}).Return(nil)

				// not overload with 1 partition, but avoid fluctuation, so don't scale down
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(2, 190))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					ReadPartitions:  partitions(2),
					WritePartitions: partitions(2),
				})
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(2, 190))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					ReadPartitions:  partitions(2),
					WritePartitions: partitions(2),
				})
			},
			cycles: 4,
		},
		{
			name: "isolation - aggregate metrics to scale up",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				deps.config.EnableTasklistIsolation = func() bool {
					return true
				}
				// overload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(2, 1))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(2, 400))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(2),
					ReadPartitions:  partitions(2),
				})

				// overload passing sustained period
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(2, 1))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(2, 400))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(2),
					ReadPartitions:  partitions(2),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(3),
					WritePartitions: partitions(3),
				}).Return(nil)
			},
			cycles: 2,
		},
		{
			name: "isolation - aggregate metrics to scale down",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				deps.config.EnableTasklistIsolation = func() bool {
					return true
				}
				// underload start
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(3, 200))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(3, 49))
				mockDescribeTaskList(deps, 2, withPartitionsAndQPS(3, 50))

				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(3),
					ReadPartitions:  partitions(3),
				})

				// underload passing sustained period
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(3, 200))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(3, 49))
				mockDescribeTaskList(deps, 2, withPartitionsAndQPS(3, 50))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(3),
					ReadPartitions:  partitions(3),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(3),
					WritePartitions: partitions(2),
				}).Return(nil)

				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(2, 200))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(2, 99))
				mockDescribeTaskList(deps, 2, withPartitionsAndBacklog(3, 2, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(2),
					ReadPartitions:  partitions(3),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(2),
					WritePartitions: partitions(2),
				}).Return(nil)
			},
			cycles: 3,
		},
		{
			name: "isolation - error calling DescribeTaskList results in no-op",
			mockSetup: func(deps *mockAdaptiveScalerDeps) {
				deps.config.EnableTasklistIsolation = func() bool {
					return true
				}
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(3, 0))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(3, 0))
				mockDescribeTaskListWithErr(deps, 2, context.DeadlineExceeded)

				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(3),
					ReadPartitions:  partitions(3),
				})

				// underload would normally pass sustain period, but the error resets it
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(3, 0))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(3, 0))
				mockDescribeTaskList(deps, 2, withPartitionsAndQPS(3, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(3),
					ReadPartitions:  partitions(3),
				})

				// Now we can scale down
				mockDescribeTaskList(deps, 0, withPartitionsAndQPS(3, 0))
				mockDescribeTaskList(deps, 1, withPartitionsAndQPS(3, 0))
				mockDescribeTaskList(deps, 2, withPartitionsAndQPS(3, 0))
				deps.mockManager.EXPECT().TaskListPartitionConfig().Return(&types.TaskListPartitionConfig{
					WritePartitions: partitions(3),
					ReadPartitions:  partitions(3),
				})
				deps.mockManager.EXPECT().UpdateTaskListPartitionConfig(gomock.Any(), &types.TaskListPartitionConfig{
					ReadPartitions:  partitions(3),
					WritePartitions: partitions(1),
				}).Return(nil)
			},
			cycles: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			taskListID, err := NewIdentifier("test-domain-id", "test-task-list", 0)
			require.NoError(t, err)
			scaler, deps := setupMocksForAdaptiveScaler(t, taskListID)
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingEnableAdaptiveScaler, true))
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingEnableGetNumberOfPartitionsFromCache, true))
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingPartitionUpscaleRPS, 200))
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingPartitionDownscaleFactor, 0.75))
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingPartitionUpscaleSustainedDuration, time.Second))
			require.NoError(t, deps.dynamicClient.UpdateValue(dynamicconfig.MatchingPartitionDownscaleSustainedDuration, time.Second))
			tc.mockSetup(deps)

			for i := 0; i < tc.cycles; i++ {
				scaler.run()
				deps.mockTimeSource.Advance(time.Second + time.Millisecond)
			}
		})
	}
}

func withPartitionsAndQPS(numPartitions int, qps float64) *types.DescribeTaskListResponse {
	return &types.DescribeTaskListResponse{
		Pollers:        nil,
		TaskListStatus: &types.TaskListStatus{NewTasksPerSecond: qps},
		PartitionConfig: &types.TaskListPartitionConfig{
			ReadPartitions:  partitions(numPartitions),
			WritePartitions: partitions(numPartitions),
		},
	}
}

func withPartitionsAndBacklog(numRead, numWrite int, backlog int64) *types.DescribeTaskListResponse {
	return &types.DescribeTaskListResponse{
		Pollers:        nil,
		TaskListStatus: &types.TaskListStatus{NewTasksPerSecond: 0, BacklogCountHint: backlog},
		PartitionConfig: &types.TaskListPartitionConfig{
			ReadPartitions:  partitions(numRead),
			WritePartitions: partitions(numWrite),
		},
	}
}

func mockDescribeTaskList(mocks *mockAdaptiveScalerDeps, partitionID int, resp *types.DescribeTaskListResponse) {
	if partitionID == 0 {
		mocks.mockManager.EXPECT().DescribeTaskList(true).Return(resp)
	} else {
		mocks.mockMatchingClient.EXPECT().DescribeTaskList(gomock.Any(), &types.MatchingDescribeTaskListRequest{
			DomainUUID: mocks.id.domainID,
			DescRequest: &types.DescribeTaskListRequest{
				TaskList: &types.TaskList{
					Name: mocks.id.GetPartition(partitionID),
					Kind: types.TaskListKindNormal.Ptr(),
				},
				TaskListType:          types.TaskListTypeDecision.Ptr(),
				IncludeTaskListStatus: true,
			},
		}).Return(resp, nil)
	}
}

func mockDescribeTaskListWithErr(mocks *mockAdaptiveScalerDeps, partitionID int, err error) {
	mocks.mockMatchingClient.EXPECT().DescribeTaskList(gomock.Any(), &types.MatchingDescribeTaskListRequest{
		DomainUUID: mocks.id.domainID,
		DescRequest: &types.DescribeTaskListRequest{
			TaskList: &types.TaskList{
				Name: mocks.id.GetPartition(partitionID),
				Kind: types.TaskListKindNormal.Ptr(),
			},
			TaskListType:          types.TaskListTypeDecision.Ptr(),
			IncludeTaskListStatus: true,
		},
	}).Return(nil, err)
}
