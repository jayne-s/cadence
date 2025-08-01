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

package activecluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"

	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/config"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/types"
)

const (
	numShards = 10
)

func TestStartStop(t *testing.T) {
	tests := []struct {
		name                    string
		externalEntityProviders func(ctrl *gomock.Controller) []ExternalEntityProvider
		wantError               string
	}{
		{
			name: "no external entity provider is provided",
			externalEntityProviders: func(ctrl *gomock.Controller) []ExternalEntityProvider {
				return nil
			},
		},
		{
			name: "external entity providers provided",
			externalEntityProviders: func(ctrl *gomock.Controller) []ExternalEntityProvider {
				p1 := NewMockExternalEntityProvider(ctrl)
				p1.EXPECT().ChangeEvents().Return(make(chan ChangeType)).AnyTimes()
				p1.EXPECT().SupportedType().Return("type1").AnyTimes()

				p2 := NewMockExternalEntityProvider(ctrl)
				p2.EXPECT().ChangeEvents().Return(make(chan ChangeType)).AnyTimes()
				p2.EXPECT().SupportedType().Return("type2").AnyTimes()

				return []ExternalEntityProvider{p1, p2}
			},
		},
		{
			name: "duplicate external entity providers provided",
			externalEntityProviders: func(ctrl *gomock.Controller) []ExternalEntityProvider {
				p1 := NewMockExternalEntityProvider(ctrl)
				p1.EXPECT().ChangeEvents().Return(make(chan ChangeType)).AnyTimes()
				p1.EXPECT().SupportedType().Return("type1").AnyTimes()

				p2 := NewMockExternalEntityProvider(ctrl)
				p2.EXPECT().ChangeEvents().Return(make(chan ChangeType)).AnyTimes()
				p2.EXPECT().SupportedType().Return("type1").AnyTimes()

				return []ExternalEntityProvider{p1, p1}
			},
			wantError: "already registered",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer goleak.VerifyNone(t)
			ctrl := gomock.NewController(t)
			domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
				return getDomainCacheEntry(nil), nil
			}

			metricsCl := metrics.NewNoopMetricsClient()
			logger := log.NewNoop()
			clusterMetadata := cluster.NewMetadata(
				config.ClusterGroupMetadata{},
				func(d string) bool { return false },
				metricsCl,
				logger,
			)
			timeSrc := clock.NewMockedTimeSource()
			mgr, err := NewManager(domainIDToDomainFn, clusterMetadata, metricsCl, logger, tc.externalEntityProviders(ctrl), nil, numShards, WithTimeSource(timeSrc))
			if tc.wantError != "" {
				assert.ErrorContains(t, err, tc.wantError)
				return
			}
			assert.NoError(t, err)
			mgr.Start()
			mgr.Stop()
		})
	}
}

func TestNotifyChangeCallbacks(t *testing.T) {
	defer goleak.VerifyNone(t)
	domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
		return getDomainCacheEntry(nil), nil
	}

	metricsCl := metrics.NewNoopMetricsClient()
	logger := log.NewNoop()
	clusterMetadata := cluster.NewMetadata(
		config.ClusterGroupMetadata{},
		func(d string) bool { return false },
		metricsCl,
		logger,
	)
	timeSrc := clock.NewMockedTimeSource()
	ctrl := gomock.NewController(t)
	externalEntityProvider := NewMockExternalEntityProvider(ctrl)

	entityChangeEventsCh := make(chan ChangeType)
	externalEntityProvider.EXPECT().ChangeEvents().Return(entityChangeEventsCh).AnyTimes()
	externalEntityProvider.EXPECT().SupportedType().Return("test-type").AnyTimes()

	mgr, err := NewManager(domainIDToDomainFn, clusterMetadata, metricsCl, logger, []ExternalEntityProvider{externalEntityProvider}, nil, numShards, WithTimeSource(timeSrc))
	assert.NoError(t, err)
	mgr.Start()
	defer mgr.Stop()

	// register change callbacks
	var changeCallbackCount int32
	mgr.RegisterChangeCallback(1, func(changeType ChangeType) {
		atomic.AddInt32(&changeCallbackCount, 1)
	})
	defer mgr.UnregisterChangeCallback(1)
	mgr.RegisterChangeCallback(2, func(changeType ChangeType) {
		atomic.AddInt32(&changeCallbackCount, 1)
	})
	defer mgr.UnregisterChangeCallback(2)

	// advance the time so ticker ticks
	timeSrc.Advance(notifyChangeCallbacksInterval + 10*time.Millisecond)
	// let other goroutine to execute
	time.Sleep(20 * time.Millisecond)

	// no external entity change event occurred so change callbacks should not be notified
	assert.Equal(t, atomic.LoadInt32(&changeCallbackCount), int32(0))

	// trigger a few external entity change events
	for i := 0; i < 3; i++ {
		select {
		case entityChangeEventsCh <- ChangeTypeEntityMap:
		default:
		}
	}
	// let other goroutine to execute
	time.Sleep(20 * time.Millisecond)

	// advance the time so ticker ticks
	timeSrc.Advance(notifyChangeCallbacksInterval + 10*time.Millisecond)
	// let other goroutine to execute
	time.Sleep(20 * time.Millisecond)

	// assert that change callbacks are notified
	assert.Equal(t, atomic.LoadInt32(&changeCallbackCount), int32(2), "change callbacks should be notified for 2 times for 2 shards registered")
}

func TestClusterNameForFailoverVersion(t *testing.T) {
	tests := []struct {
		name                 string
		activeClusterCfg     *types.ActiveClusters
		clusterGroupMetadata config.ClusterGroupMetadata
		failoverVersion      int64
		expectedResult       string
		expectedError        string
	}{
		{
			name:             "not active-active domain, returns result from cluster metadata",
			activeClusterCfg: nil,
			clusterGroupMetadata: config.ClusterGroupMetadata{
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 0,
			expectedResult:  "cluster1",
		},
		{
			name:             "not active-active domain, invalid failover version",
			activeClusterCfg: nil,
			clusterGroupMetadata: config.ClusterGroupMetadata{
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 1,
			expectedError:   "failed to resolve failover version to a cluster: could not resolve failover version: 1",
		},
		{
			name: "active-active domain, failover version maps to a cluster in metadata",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   0,
					},
					"us-east": {
						ActiveClusterName: "cluster2",
						FailoverVersion:   2,
					},
				},
			},
			clusterGroupMetadata: config.ClusterGroupMetadata{
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 0,
			expectedResult:  "cluster1",
		},
		{
			name: "active-active domain, failover version maps to a region in metadata",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   0,
					},
					"us-east": {
						ActiveClusterName: "cluster2",
						FailoverVersion:   2,
					},
				},
			},
			clusterGroupMetadata: config.ClusterGroupMetadata{
				Regions: map[string]config.RegionInformation{
					"us-west": {
						InitialFailoverVersion: 1,
					},
					"us-east": {
						InitialFailoverVersion: 3,
					},
				},
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 3,
			expectedResult:  "cluster2",
		},
		{
			name: "active-active domain, failover version doesn't map to a cluster or region",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   0,
					},
					"us-east": {
						ActiveClusterName: "cluster2",
						FailoverVersion:   2,
					},
				},
			},
			clusterGroupMetadata: config.ClusterGroupMetadata{
				Regions: map[string]config.RegionInformation{
					"us-west": {
						InitialFailoverVersion: 1,
					},
					"us-east": {
						InitialFailoverVersion: 3,
					},
				},
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 5,
			expectedError:   "failed to resolve failover version to a region: could not resolve failover version to region: 5",
		},
		{
			name: "active-active domain, failover version maps to a region in metadata but it's missing in domain's active cluster config",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					// us-west is missing in the domain's active cluster config
					"us-east": {
						ActiveClusterName: "cluster2",
						FailoverVersion:   2,
					},
				},
			},
			clusterGroupMetadata: config.ClusterGroupMetadata{
				Regions: map[string]config.RegionInformation{
					"us-west": {
						InitialFailoverVersion: 1,
					},
					"us-east": {
						InitialFailoverVersion: 3,
					},
				},
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					"cluster2": {
						InitialFailoverVersion: 2,
					},
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 1,
			expectedError:   "could not find region us-west in the domain test-domain-id's active cluster config",
		},
		{
			name: "active-active domain, failover version maps to a region and domain's active cluster config has a cluster for the region but cluster metadata doesn't have the cluster",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   0,
					},
					"us-east": {
						ActiveClusterName: "cluster2",
						FailoverVersion:   2,
					},
				},
			},
			clusterGroupMetadata: config.ClusterGroupMetadata{
				Regions: map[string]config.RegionInformation{
					"us-west": {
						InitialFailoverVersion: 1,
					},
					"us-east": {
						InitialFailoverVersion: 3,
					},
				},
				ClusterGroup: map[string]config.ClusterInformation{
					"cluster1": {
						InitialFailoverVersion: 0,
					},
					// cluster2 is missing
				},
				FailoverVersionIncrement: 100,
			},
			failoverVersion: 1,
			expectedError:   "could not find cluster cluster0 for region us-west",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
				return getDomainCacheEntry(tc.activeClusterCfg), nil
			}

			metricsCl := metrics.NewNoopMetricsClient()
			logger := log.NewNoop()
			clusterMetadata := cluster.NewMetadata(
				tc.clusterGroupMetadata,
				func(d string) bool { return false },
				metricsCl,
				logger,
			)
			timeSrc := clock.NewMockedTimeSource()
			mgr, err := NewManager(domainIDToDomainFn, clusterMetadata, metricsCl, logger, nil, nil, numShards, WithTimeSource(timeSrc))
			assert.NoError(t, err)
			result, err := mgr.ClusterNameForFailoverVersion(tc.failoverVersion, "test-domain-id")
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
			} else {
				assert.NoError(t, err)
			}
			if result != tc.expectedResult {
				t.Fatalf("expected cluster name %v, got %v", tc.expectedResult, result)
			}
		})
	}
}

func TestFailoverVersionOfNewWorkflow(t *testing.T) {
	metricsCl := metrics.NewNoopMetricsClient()
	logger := log.NewNoop()
	clusterMetadata := cluster.NewMetadata(
		config.ClusterGroupMetadata{
			ClusterGroup: map[string]config.ClusterInformation{
				"cluster0": {
					InitialFailoverVersion: 1,
					Region:                 "us-west",
				},
				"cluster1": {
					InitialFailoverVersion: 3,
					Region:                 "us-east",
				},
			},
			Regions: map[string]config.RegionInformation{
				"us-west": {
					InitialFailoverVersion: 0,
				},
				"us-east": {
					InitialFailoverVersion: 2,
				},
			},
			FailoverVersionIncrement: 100,
			CurrentClusterName:       "cluster0",
		},
		func(d string) bool { return false },
		metricsCl,
		logger,
	)

	tests := []struct {
		name                    string
		req                     *types.HistoryStartWorkflowExecutionRequest
		externalEntityProviders func(ctrl *gomock.Controller) []ExternalEntityProvider
		activeClusterCfg        *types.ActiveClusters
		expectedFailoverVersion int64
		expectedError           string
	}{
		{
			name:          "start request nil",
			req:           nil,
			expectedError: "request is nil",
		},
		{
			name: "not active-active domain, returns failover version of the domain",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
			},
			activeClusterCfg:        nil, // not active-active domain
			expectedFailoverVersion: 1,
		},
		{
			name: "active-active domain, start request nil",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID:   "test-domain-id",
				StartRequest: nil,
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
				},
			},
			expectedError: "start request is nil",
		},
		{
			name: "active-active domain, policy has external entity but corresponding provider is missing",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
				StartRequest: &types.StartWorkflowExecutionRequest{
					ActiveClusterSelectionPolicy: &types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyExternalEntity.Ptr(),
						ExternalEntityType:             "city",
						ExternalEntityKey:              "seattle",
					},
				},
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
				},
			},
			expectedError: "external entity provider for type \"city\" not found",
		},
		{
			name: "active-active domain, policy has external entity. successfully get failover version from external entity",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
				StartRequest: &types.StartWorkflowExecutionRequest{
					ActiveClusterSelectionPolicy: &types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyExternalEntity.Ptr(),
						ExternalEntityType:             "city",
						ExternalEntityKey:              "seattle",
					},
				},
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
				},
			},
			externalEntityProviders: func(ctrl *gomock.Controller) []ExternalEntityProvider {
				externalEntityProvider := NewMockExternalEntityProvider(ctrl)
				externalEntityProvider.EXPECT().SupportedType().Return("city").AnyTimes()
				externalEntityProvider.EXPECT().GetExternalEntity(gomock.Any(), "seattle").Return(&ExternalEntity{
					FailoverVersion: 7,
				}, nil)
				return []ExternalEntityProvider{externalEntityProvider}
			},
			expectedFailoverVersion: 7,
		},
		{
			name: "active-active domain, policy is nil. returns failover version of the active cluster in current region",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
				StartRequest: &types.StartWorkflowExecutionRequest{
					ActiveClusterSelectionPolicy: nil,
				},
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   20,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   22,
					},
				},
			},
			expectedFailoverVersion: 20, // failover version of cluster0 in RegionToClusterMap
		},
		{
			name: "active-active domain, policy is region sticky but region is missing in domain's active cluster config",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
				StartRequest: &types.StartWorkflowExecutionRequest{
					ActiveClusterSelectionPolicy: &types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyRegionSticky.Ptr(),
						StickyRegion:                   "us-west",
					},
				},
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					// missing "us-west" here
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   22,
					},
				},
			},
			expectedError: "could not find region us-west in the domain test-domain-id's active cluster config",
		},
		{
			name: "active-active domain, policy is region sticky. returns failover version of the active cluster in sticky region",
			req: &types.HistoryStartWorkflowExecutionRequest{
				DomainUUID: "test-domain-id",
				StartRequest: &types.StartWorkflowExecutionRequest{
					ActiveClusterSelectionPolicy: &types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyRegionSticky.Ptr(),
						StickyRegion:                   "us-west",
					},
				},
			},
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   20,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   22,
					},
				},
			},
			expectedFailoverVersion: 20, // failover version of cluster0 in RegionToClusterMap
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
				return getDomainCacheEntry(tc.activeClusterCfg), nil
			}

			timeSrc := clock.NewMockedTimeSource()
			ctrl := gomock.NewController(t)
			var providers []ExternalEntityProvider
			if tc.externalEntityProviders != nil {
				providers = tc.externalEntityProviders(ctrl)
			}
			mgr, err := NewManager(
				domainIDToDomainFn,
				clusterMetadata,
				metricsCl,
				logger,
				providers,
				nil,
				numShards,
				WithTimeSource(timeSrc),
			)
			assert.NoError(t, err)

			result, err := mgr.FailoverVersionOfNewWorkflow(context.Background(), tc.req)
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
			} else {
				assert.NoError(t, err)
			}
			if result != tc.expectedFailoverVersion {
				t.Fatalf("expected failover version %v, got %v", tc.expectedFailoverVersion, result)
			}
		})
	}
}

func TestLookupWorkflow(t *testing.T) {
	metricsCl := metrics.NewNoopMetricsClient()
	logger := log.NewNoop()
	clusterMetadata := cluster.NewMetadata(
		config.ClusterGroupMetadata{
			ClusterGroup: map[string]config.ClusterInformation{
				"cluster0": {
					InitialFailoverVersion: 1,
					Region:                 "us-west",
				},
				"cluster1": {
					InitialFailoverVersion: 3,
					Region:                 "us-east",
				},
			},
			Regions: map[string]config.RegionInformation{
				"us-west": {
					InitialFailoverVersion: 0,
				},
				"us-east": {
					InitialFailoverVersion: 2,
				},
			},
			FailoverVersionIncrement: 100,
			CurrentClusterName:       "cluster0",
		},
		func(d string) bool { return false },
		metricsCl,
		logger,
	)

	tests := []struct {
		name                        string
		externalEntityProviders     func(ctrl *gomock.Controller) []ExternalEntityProvider
		getClusterSelectionPolicyFn func(ctx context.Context, domainID, wfID, rID string) (*types.ActiveClusterSelectionPolicy, error)
		mockFn                      func(em *persistence.MockExecutionManager)
		activeClusterCfg            *types.ActiveClusters
		expectedResult              *LookupResult
		expectedError               string
	}{
		{
			name:             "domain is not active-active",
			activeClusterCfg: nil,
			expectedResult: &LookupResult{
				ClusterName:     "cluster0",
				FailoverVersion: 1,
			},
		},
		{
			name: "domain is active-active, failed to fetch workflow activeness metadata",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   3,
					},
				},
			},
			mockFn: func(em *persistence.MockExecutionManager) {
				em.EXPECT().GetActiveClusterSelectionPolicy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, errors.New("failed to fetch workflow activeness metadata"))
			},
			expectedError: "failed to fetch workflow activeness metadata",
		},
		{
			name: "domain is active-active, activeness metadata not-found which means region sticky",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   3,
					},
				},
			},
			mockFn: func(em *persistence.MockExecutionManager) {
				em.EXPECT().GetActiveClusterSelectionPolicy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, &types.EntityNotExistsError{})
			},
			expectedResult: &LookupResult{
				ClusterName:     "cluster0",
				FailoverVersion: 1,
			},
		},
		{
			name: "domain is active-active, activeness metadata shows region sticky",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   3,
					},
				},
			},
			mockFn: func(em *persistence.MockExecutionManager) {
				em.EXPECT().GetActiveClusterSelectionPolicy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyRegionSticky.Ptr(),
						StickyRegion:                   "us-east",
					}, nil)
			},
			expectedResult: &LookupResult{
				Region:          "us-east",
				ClusterName:     "cluster1",
				FailoverVersion: 3,
			},
		},
		{
			name: "domain is active-active, activeness metadata shows external entity",
			activeClusterCfg: &types.ActiveClusters{
				ActiveClustersByRegion: map[string]types.ActiveClusterInfo{
					"us-west": {
						ActiveClusterName: "cluster0",
						FailoverVersion:   1,
					},
					"us-east": {
						ActiveClusterName: "cluster1",
						FailoverVersion:   3,
					},
				},
			},
			mockFn: func(em *persistence.MockExecutionManager) {
				em.EXPECT().GetActiveClusterSelectionPolicy(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return(&types.ActiveClusterSelectionPolicy{
						ActiveClusterSelectionStrategy: types.ActiveClusterSelectionStrategyExternalEntity.Ptr(),
						ExternalEntityType:             "city",
						ExternalEntityKey:              "boston",
					}, nil)
			},
			externalEntityProviders: func(ctrl *gomock.Controller) []ExternalEntityProvider {
				externalEntityProvider := NewMockExternalEntityProvider(ctrl)
				externalEntityProvider.EXPECT().SupportedType().Return("city").AnyTimes()
				externalEntityProvider.EXPECT().GetExternalEntity(gomock.Any(), "boston").Return(&ExternalEntity{
					Region:          "us-east",
					FailoverVersion: 102,
				}, nil)
				return []ExternalEntityProvider{externalEntityProvider}
			},
			expectedResult: &LookupResult{
				Region:          "us-east",
				ClusterName:     "cluster1",
				FailoverVersion: 102,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			domainIDToDomainFn := func(id string) (*cache.DomainCacheEntry, error) {
				return getDomainCacheEntry(tc.activeClusterCfg), nil
			}

			timeSrc := clock.NewMockedTimeSource()
			ctrl := gomock.NewController(t)
			var providers []ExternalEntityProvider
			if tc.externalEntityProviders != nil {
				providers = tc.externalEntityProviders(ctrl)
			}

			wfID := "test-workflow-id"
			shardID := 6 // corresponds to wfID given numShards
			em := persistence.NewMockExecutionManager(ctrl)
			if tc.mockFn != nil {
				tc.mockFn(em)
			}
			emProvider := NewMockExecutionManagerProvider(ctrl)
			emProvider.EXPECT().GetExecutionManager(shardID).Return(em, nil).AnyTimes()

			mgr, err := NewManager(
				domainIDToDomainFn,
				clusterMetadata,
				metricsCl,
				logger,
				providers,
				emProvider,
				numShards,
				WithTimeSource(timeSrc),
			)
			assert.NoError(t, err)

			result, err := mgr.LookupWorkflow(context.Background(), "test-domain-id", wfID, "test-run-id")
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectedResult != nil {
				if result == nil {
					t.Fatalf("expected result not nil, got nil")
				}
				assert.Equal(t, tc.expectedResult, result)
			}
		})
	}
}

func getDomainCacheEntry(cfg *types.ActiveClusters) *cache.DomainCacheEntry {
	// only thing we care in domain cache entry is the active clusters config
	return cache.NewDomainCacheEntryForTest(
		nil,
		nil,
		true,
		&persistence.DomainReplicationConfig{
			ActiveClusters:    cfg,
			ActiveClusterName: "cluster0",
		},
		1,
		nil,
		1,
		1,
		1,
	)
}
