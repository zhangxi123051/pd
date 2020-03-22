// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"context"

	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/v4/pkg/mock/mockcluster"
	"github.com/pingcap/pd/v4/pkg/mock/mockhbstream"
	"github.com/pingcap/pd/v4/pkg/mock/mockoption"
	"github.com/pingcap/pd/v4/pkg/testutil"
	"github.com/pingcap/pd/v4/server/core"
	"github.com/pingcap/pd/v4/server/kv"
	"github.com/pingcap/pd/v4/server/schedule"
	"github.com/pingcap/pd/v4/server/schedule/operator"
	"github.com/pingcap/pd/v4/server/schedule/opt"
	"github.com/pingcap/pd/v4/server/schedule/placement"
	"github.com/pingcap/pd/v4/server/statistics"
)

var _ = Suite(&testShuffleLeaderSuite{})

type testShuffleLeaderSuite struct{}

func (s *testShuffleLeaderSuite) TestShuffle(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	sl, err := schedule.CreateScheduler(ShuffleLeaderType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(ShuffleLeaderType, []string{"", ""}))
	c.Assert(err, IsNil)
	c.Assert(sl.Schedule(tc), IsNil)

	// Add stores 1,2,3,4
	tc.AddLeaderStore(1, 6)
	tc.AddLeaderStore(2, 7)
	tc.AddLeaderStore(3, 8)
	tc.AddLeaderStore(4, 9)
	// Add regions 1,2,3,4 with leaders in stores 1,2,3,4
	tc.AddLeaderRegion(1, 1, 2, 3, 4)
	tc.AddLeaderRegion(2, 2, 3, 4, 1)
	tc.AddLeaderRegion(3, 3, 4, 1, 2)
	tc.AddLeaderRegion(4, 4, 1, 2, 3)

	for i := 0; i < 4; i++ {
		op := sl.Schedule(tc)
		c.Assert(op, NotNil)
		c.Assert(op[0].Kind(), Equals, operator.OpLeader|operator.OpAdmin)
	}
}

var _ = Suite(&testBalanceAdjacentRegionSuite{})

type testBalanceAdjacentRegionSuite struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (s *testBalanceAdjacentRegionSuite) SetUpSuite(c *C) {
	s.ctx, s.cancel = context.WithCancel(context.Background())
}

func (s *testBalanceAdjacentRegionSuite) TearDownSuite(c *C) {
	s.cancel()
}

func (s *testBalanceAdjacentRegionSuite) TestBalance(c *C) {
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	sc, err := schedule.CreateScheduler(AdjacentRegionType, schedule.NewOperatorController(s.ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(AdjacentRegionType, []string{"32", "2"}))
	c.Assert(err, IsNil)

	c.Assert(sc.(*balanceAdjacentRegionScheduler).conf.LeaderLimit, Equals, uint64(32))
	c.Assert(sc.(*balanceAdjacentRegionScheduler).conf.PeerLimit, Equals, uint64(2))

	sc.(*balanceAdjacentRegionScheduler).conf.LeaderLimit = 0
	sc.(*balanceAdjacentRegionScheduler).conf.PeerLimit = 0
	c.Assert(sc.IsScheduleAllowed(tc), IsFalse)
	sc.(*balanceAdjacentRegionScheduler).conf.LeaderLimit = defaultAdjacentLeaderLimit
	c.Assert(sc.IsScheduleAllowed(tc), IsTrue)
	sc.(*balanceAdjacentRegionScheduler).conf.LeaderLimit = 0
	sc.(*balanceAdjacentRegionScheduler).conf.PeerLimit = defaultAdjacentPeerLimit
	c.Assert(sc.IsScheduleAllowed(tc), IsTrue)
	sc.(*balanceAdjacentRegionScheduler).conf.LeaderLimit = defaultAdjacentLeaderLimit
	c.Assert(sc.IsScheduleAllowed(tc), IsTrue)

	c.Assert(sc.Schedule(tc), IsNil)

	// Add stores 1,2,3,4
	tc.AddLeaderStore(1, 5)
	tc.AddLeaderStore(2, 0)
	tc.AddLeaderStore(3, 0)
	tc.AddLeaderStore(4, 0)
	// Add regions
	tc.AddLeaderRegionWithRange(1, "", "a", 1, 2, 3)
	tc.AddLeaderRegionWithRange(2, "a", "b", 1, 2, 3)
	tc.AddLeaderRegionWithRange(3, "b", "c", 1, 3, 4)
	tc.AddLeaderRegionWithRange(4, "c", "d", 1, 2, 3)
	tc.AddLeaderRegionWithRange(5, "e", "f", 1, 2, 3)
	tc.AddLeaderRegionWithRange(6, "f", "g", 1, 2, 3)
	tc.AddLeaderRegionWithRange(7, "z", "", 1, 2, 3)

	// check and do operator
	// transfer peer from store 1 to 4 for region 1 because the distribution of
	// the two regions is same, we will transfer the peer, which is leader now,
	// to a new store
	testutil.CheckTransferPeerWithLeaderTransfer(c, sc.Schedule(tc)[0], operator.OpAdjacent, 1, 4)
	// suppose we add peer in store 4, transfer leader to store 2, remove peer in store 1
	tc.AddLeaderRegionWithRange(1, "", "a", 2, 3, 4)

	// transfer leader from store 1 to store 2 for region 2 because we have a different peer location,
	// we can directly transfer leader to peer 2. we priority to transfer leader because less overhead
	testutil.CheckTransferLeader(c, sc.Schedule(tc)[0], operator.OpAdjacent, 1, 2)
	tc.AddLeaderRegionWithRange(2, "a", "b", 2, 1, 3)

	// transfer leader from store 1 to store 2 for region 3
	testutil.CheckTransferLeader(c, sc.Schedule(tc)[0], operator.OpAdjacent, 1, 4)
	tc.AddLeaderRegionWithRange(3, "b", "c", 4, 1, 3)

	// transfer peer from store 1 to store 4 for region 5
	// the region 5 just adjacent the region 6
	testutil.CheckTransferPeerWithLeaderTransfer(c, sc.Schedule(tc)[0], operator.OpAdjacent, 1, 4)
	tc.AddLeaderRegionWithRange(5, "e", "f", 2, 3, 4)

	c.Assert(sc.Schedule(tc), IsNil)
	c.Assert(sc.Schedule(tc), IsNil)
	testutil.CheckTransferLeader(c, sc.Schedule(tc)[0], operator.OpAdjacent, 2, 4)
	tc.AddLeaderRegionWithRange(1, "", "a", 4, 2, 3)
	for i := 0; i < 10; i++ {
		c.Assert(sc.Schedule(tc), IsNil)
	}
}

func (s *testBalanceAdjacentRegionSuite) TestNoNeedToBalance(c *C) {
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	sc, err := schedule.CreateScheduler(AdjacentRegionType, schedule.NewOperatorController(s.ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(AdjacentRegionType, nil))
	c.Assert(err, IsNil)
	c.Assert(sc.Schedule(tc), IsNil)

	// Add stores 1,2,3
	tc.AddLeaderStore(1, 2)
	tc.AddLeaderStore(2, 0)
	tc.AddLeaderStore(3, 0)

	tc.AddLeaderRegionWithRange(1, "", "a", 1, 2, 3)
	tc.AddLeaderRegionWithRange(2, "a", "b", 1, 2, 3)
	c.Assert(sc.Schedule(tc), IsNil)
}

type sequencer struct {
	maxID uint64
	curID uint64
}

func newSequencer(maxID uint64) *sequencer {
	return &sequencer{
		maxID: maxID,
		curID: 0,
	}
}

func (s *sequencer) next() uint64 {
	s.curID++
	if s.curID > s.maxID {
		s.curID = 1
	}
	return s.curID
}

var _ = Suite(&testScatterRegionSuite{})

type testScatterRegionSuite struct{}

func (s *testScatterRegionSuite) TestSixStores(c *C) {
	s.scatter(c, 6, 4)
}

func (s *testScatterRegionSuite) TestFiveStores(c *C) {
	s.scatter(c, 5, 5)
}

func (s *testScatterRegionSuite) checkOperator(op *operator.Operator, c *C) {
	for i := 0; i < op.Len(); i++ {
		if rp, ok := op.Step(i).(operator.RemovePeer); ok {
			for j := i + 1; j < op.Len(); j++ {
				if tr, ok := op.Step(j).(operator.TransferLeader); ok {
					c.Assert(rp.FromStore, Not(Equals), tr.FromStore)
					c.Assert(rp.FromStore, Not(Equals), tr.ToStore)
				}
			}
		}
	}
}

func (s *testScatterRegionSuite) scatter(c *C, numStores, numRegions uint64) {
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	// Add stores 1~6.
	for i := uint64(1); i <= numStores; i++ {
		tc.AddRegionStore(i, 0)
	}

	// Add regions 1~4.
	seq := newSequencer(3)
	// Region 1 has the same distribution with the Region 2, which is used to test selectPeerToReplace.
	tc.AddLeaderRegion(1, 1, 2, 3)
	for i := uint64(2); i <= numRegions; i++ {
		tc.AddLeaderRegion(i, seq.next(), seq.next(), seq.next())
	}

	scatterer := schedule.NewRegionScatterer(tc)

	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		if op, _ := scatterer.Scatter(region); op != nil {
			s.checkOperator(op, c)
			schedule.ApplyOperator(tc, op)
		}
	}

	countPeers := make(map[uint64]uint64)
	for i := uint64(1); i <= numRegions; i++ {
		region := tc.GetRegion(i)
		for _, peer := range region.GetPeers() {
			countPeers[peer.GetStoreId()]++
		}
	}

	// Each store should have the same number of peers.
	for _, count := range countPeers {
		c.Assert(count, Equals, numRegions*3/numStores)
	}
}

func (s *testScatterRegionSuite) TestStoreLimit(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)
	oc := schedule.NewOperatorController(ctx, tc, mockhbstream.NewHeartbeatStream())

	// Add stores 1~6.
	for i := uint64(1); i <= 5; i++ {
		tc.AddRegionStore(i, 0)
	}

	// Add regions 1~4.
	seq := newSequencer(3)
	// Region 1 has the same distribution with the Region 2, which is used to test selectPeerToReplace.
	tc.AddLeaderRegion(1, 1, 2, 3)
	for i := uint64(2); i <= 5; i++ {
		tc.AddLeaderRegion(i, seq.next(), seq.next(), seq.next())
	}

	scatterer := schedule.NewRegionScatterer(tc)

	for i := uint64(1); i <= 5; i++ {
		region := tc.GetRegion(i)
		if op, _ := scatterer.Scatter(region); op != nil {
			c.Assert(oc.AddWaitingOperator(op), Equals, 1)
		}
	}
}

var _ = Suite(&testRejectLeaderSuite{})

type testRejectLeaderSuite struct{}

func (s *testRejectLeaderSuite) TestRejectLeader(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := mockoption.NewScheduleOptions()
	opts.LabelProperties = map[string][]*metapb.StoreLabel{
		opt.RejectLeader: {{Key: "noleader", Value: "true"}},
	}
	tc := mockcluster.NewCluster(opts)

	// Add 3 stores 1,2,3.
	tc.AddLabelsStore(1, 1, map[string]string{"noleader": "true"})
	tc.UpdateLeaderCount(1, 1)
	tc.AddLeaderStore(2, 10)
	tc.AddLeaderStore(3, 0)
	// Add 2 regions with leader on 1 and 2.
	tc.AddLeaderRegion(1, 1, 2, 3)
	tc.AddLeaderRegion(2, 2, 1, 3)

	// The label scheduler transfers leader out of store1.
	oc := schedule.NewOperatorController(ctx, nil, nil)
	sl, err := schedule.CreateScheduler(LabelType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(LabelType, []string{"", ""}))
	c.Assert(err, IsNil)
	op := sl.Schedule(tc)
	testutil.CheckTransferLeader(c, op[0], operator.OpLeader, 1, 3)

	// If store3 is disconnected, transfer leader to store 2 instead.
	tc.SetStoreDisconnect(3)
	op = sl.Schedule(tc)
	testutil.CheckTransferLeader(c, op[0], operator.OpLeader, 1, 2)

	// As store3 is disconnected, store1 rejects leader. Balancer will not create
	// any operators.
	bs, err := schedule.CreateScheduler(BalanceLeaderType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(BalanceLeaderType, []string{"", ""}))
	c.Assert(err, IsNil)
	op = bs.Schedule(tc)
	c.Assert(op, IsNil)

	// Can't evict leader from store2, neither.
	el, err := schedule.CreateScheduler(EvictLeaderType, oc, core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(EvictLeaderType, []string{"2"}))
	c.Assert(err, IsNil)
	op = el.Schedule(tc)
	c.Assert(op, IsNil)

	// If the peer on store3 is pending, not transfer to store3 neither.
	tc.SetStoreUp(3)
	region := tc.Regions.GetRegion(1)
	for _, p := range region.GetPeers() {
		if p.GetStoreId() == 3 {
			region = region.Clone(core.WithPendingPeers(append(region.GetPendingPeers(), p)))
			break
		}
	}
	tc.Regions.AddRegion(region)
	op = sl.Schedule(tc)
	testutil.CheckTransferLeader(c, op[0], operator.OpLeader, 1, 2)
}

var _ = Suite(&testShuffleHotRegionSchedulerSuite{})

type testShuffleHotRegionSchedulerSuite struct{}

func (s *testShuffleHotRegionSchedulerSuite) TestBalance(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	newTestReplication(opt, 3, "zone", "host")
	tc := mockcluster.NewCluster(opt)
	hb, err := schedule.CreateScheduler(ShuffleHotRegionType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder("shuffle-hot-region", []string{"", ""}))
	c.Assert(err, IsNil)

	s.checkBalance(c, tc, opt, hb)
	opt.EnablePlacementRules = true
	s.checkBalance(c, tc, opt, hb)
}

func (s *testShuffleHotRegionSchedulerSuite) checkBalance(c *C, tc *mockcluster.Cluster, opt *mockoption.ScheduleOptions, hb schedule.Scheduler) {
	// Add stores 1, 2, 3, 4, 5, 6  with hot peer counts 3, 2, 2, 2, 0, 0.
	tc.AddLabelsStore(1, 3, map[string]string{"zone": "z1", "host": "h1"})
	tc.AddLabelsStore(2, 2, map[string]string{"zone": "z2", "host": "h2"})
	tc.AddLabelsStore(3, 2, map[string]string{"zone": "z3", "host": "h3"})
	tc.AddLabelsStore(4, 2, map[string]string{"zone": "z4", "host": "h4"})
	tc.AddLabelsStore(5, 0, map[string]string{"zone": "z5", "host": "h5"})
	tc.AddLabelsStore(6, 0, map[string]string{"zone": "z4", "host": "h6"})

	// Report store written bytes.
	tc.UpdateStorageWrittenBytes(1, 7.5*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(2, 4.5*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(3, 4.5*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(4, 6*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(5, 0)
	tc.UpdateStorageWrittenBytes(6, 0)

	// Region 1, 2 and 3 are hot regions.
	//| region_id | leader_store | follower_store | follower_store | written_bytes |
	//|-----------|--------------|----------------|----------------|---------------|
	//|     1     |       1      |        2       |       3        |      512KB    |
	//|     2     |       1      |        3       |       4        |      512KB    |
	//|     3     |       1      |        2       |       4        |      512KB    |
	tc.AddLeaderRegionWithWriteInfo(1, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 3})
	tc.AddLeaderRegionWithWriteInfo(2, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{3, 4})
	tc.AddLeaderRegionWithWriteInfo(3, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 4})
	opt.HotRegionCacheHitsThreshold = 0

	// try to get an operator
	var op []*operator.Operator
	for i := 0; i < 100; i++ {
		op = hb.Schedule(tc)
		if op != nil {
			break
		}
	}
	c.Assert(op, NotNil)
	c.Assert(op[0].Step(1).(operator.PromoteLearner).ToStore, Equals, op[0].Step(op[0].Len()-1).(operator.TransferLeader).ToStore)
	c.Assert(op[0].Step(1).(operator.PromoteLearner).ToStore, Not(Equals), 6)
}

var _ = Suite(&testHotRegionSchedulerSuite{})

type testHotRegionSchedulerSuite struct{}

func (s *testHotRegionSchedulerSuite) TestAbnormalReplica(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	opt.LeaderScheduleLimit = 0
	tc := mockcluster.NewCluster(opt)
	hb, err := schedule.CreateScheduler(HotReadRegionType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), nil)
	c.Assert(err, IsNil)

	tc.AddRegionStore(1, 3)
	tc.AddRegionStore(2, 2)
	tc.AddRegionStore(3, 2)

	// Report store read bytes.
	tc.UpdateStorageReadBytes(1, 7.5*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageReadBytes(2, 4.5*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageReadBytes(3, 4.5*MB*statistics.StoreHeartBeatReportInterval)

	tc.AddLeaderRegionWithReadInfo(1, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2})
	tc.AddLeaderRegionWithReadInfo(2, 2, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{1, 3})
	tc.AddLeaderRegionWithReadInfo(3, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 3})
	opt.HotRegionCacheHitsThreshold = 0
	c.Assert(tc.IsRegionHot(tc.GetRegion(1)), IsTrue)
	c.Assert(hb.Schedule(tc), IsNil)
}

var _ = Suite(&testEvictLeaderSuite{})

type testEvictLeaderSuite struct{}

func (s *testEvictLeaderSuite) TestEvictLeader(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	// Add stores 1, 2, 3
	tc.AddLeaderStore(1, 0)
	tc.AddLeaderStore(2, 0)
	tc.AddLeaderStore(3, 0)
	// Add regions 1, 2, 3 with leaders in stores 1, 2, 3
	tc.AddLeaderRegion(1, 1, 2)
	tc.AddLeaderRegion(2, 2, 1)
	tc.AddLeaderRegion(3, 3, 1)

	sl, err := schedule.CreateScheduler(EvictLeaderType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(EvictLeaderType, []string{"1"}))
	c.Assert(err, IsNil)
	c.Assert(sl.IsScheduleAllowed(tc), IsTrue)
	op := sl.Schedule(tc)
	testutil.CheckTransferLeader(c, op[0], operator.OpLeader, 1, 2)
}

var _ = Suite(&testShuffleRegionSuite{})

type testShuffleRegionSuite struct{}

func (s *testShuffleRegionSuite) TestShuffle(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	sl, err := schedule.CreateScheduler(ShuffleRegionType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(ShuffleRegionType, []string{"", ""}))
	c.Assert(err, IsNil)
	c.Assert(sl.IsScheduleAllowed(tc), IsTrue)
	c.Assert(sl.Schedule(tc), IsNil)

	// Add stores 1, 2, 3, 4
	tc.AddRegionStore(1, 6)
	tc.AddRegionStore(2, 7)
	tc.AddRegionStore(3, 8)
	tc.AddRegionStore(4, 9)
	// Add regions 1, 2, 3, 4 with leaders in stores 1,2,3,4
	tc.AddLeaderRegion(1, 1, 2, 3)
	tc.AddLeaderRegion(2, 2, 3, 4)
	tc.AddLeaderRegion(3, 3, 4, 1)
	tc.AddLeaderRegion(4, 4, 1, 2)

	for i := 0; i < 4; i++ {
		op := sl.Schedule(tc)
		c.Assert(op, NotNil)
		c.Assert(op[0].Kind(), Equals, operator.OpRegion|operator.OpAdmin)
	}
}

func (s *testShuffleRegionSuite) TestRole(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opt := mockoption.NewScheduleOptions()
	tc := mockcluster.NewCluster(opt)

	// update rule to 1leader+1follower+1learner
	opt.EnablePlacementRules = true
	tc.RuleManager.SetRule(&placement.Rule{
		GroupID: "pd",
		ID:      "default",
		Role:    placement.Voter,
		Count:   2,
	})
	tc.RuleManager.SetRule(&placement.Rule{
		GroupID: "pd",
		ID:      "learner",
		Role:    placement.Learner,
		Count:   1,
	})

	// Add stores 1, 2, 3, 4
	tc.AddRegionStore(1, 6)
	tc.AddRegionStore(2, 7)
	tc.AddRegionStore(3, 8)
	tc.AddRegionStore(4, 9)

	// Put a region with 1leader + 1follower + 1learner
	peers := []*metapb.Peer{
		{Id: 1, StoreId: 1},
		{Id: 2, StoreId: 2},
		{Id: 3, StoreId: 3, IsLearner: true},
	}
	region := core.NewRegionInfo(&metapb.Region{
		Id:          1,
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
		Peers:       peers,
	}, peers[0])
	tc.PutRegion(region)

	sl, err := schedule.CreateScheduler(ShuffleRegionType, schedule.NewOperatorController(ctx, nil, nil), core.NewStorage(kv.NewMemoryKV()), schedule.ConfigSliceDecoder(ShuffleRegionType, []string{"", ""}))
	c.Assert(err, IsNil)

	conf := sl.(*shuffleRegionScheduler).conf
	conf.Roles = []string{"follower"}
	ops := sl.Schedule(tc)
	c.Assert(ops, HasLen, 1)
	testutil.CheckTransferPeer(c, ops[0], operator.OpRegion, 2, 4) // transfer follower
	conf.Roles = []string{"learner"}
	ops = sl.Schedule(tc)
	c.Assert(ops, HasLen, 1)
	testutil.CheckTransferLearner(c, ops[0], operator.OpRegion, 3, 4) // transfer learner
}

var _ = Suite(&testSpecialUseSuite{})

type testSpecialUseSuite struct{}

func (s *testSpecialUseSuite) TestSpecialUseHotRegion(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oc := schedule.NewOperatorController(ctx, nil, nil)
	storage := core.NewStorage(kv.NewMemoryKV())
	cd := schedule.ConfigSliceDecoder(BalanceRegionType, []string{"", ""})
	bs, err := schedule.CreateScheduler(BalanceRegionType, oc, storage, cd)
	c.Assert(err, IsNil)
	hs, err := schedule.CreateScheduler(HotWriteRegionType, oc, storage, cd)
	c.Assert(err, IsNil)

	opt := mockoption.NewScheduleOptions()
	opt.HotRegionCacheHitsThreshold = 0
	tc := mockcluster.NewCluster(opt)
	tc.AddRegionStore(1, 10)
	tc.AddRegionStore(2, 4)
	tc.AddRegionStore(3, 2)
	tc.AddRegionStore(4, 0)
	tc.AddRegionStore(5, 0)
	tc.AddLeaderRegion(1, 1, 2, 3)
	tc.AddLeaderRegion(2, 1, 2, 3)
	tc.AddLeaderRegion(3, 1, 2, 3)
	tc.AddLeaderRegion(4, 1, 2, 3)
	tc.AddLeaderRegion(5, 1, 2, 3)

	// balance region without label
	ops := bs.Schedule(tc)
	c.Assert(ops, HasLen, 1)
	testutil.CheckTransferPeer(c, ops[0], operator.OpBalance, 1, 4)

	// cannot balance to store 4 and 5 with label
	tc.AddLabelsStore(4, 0, map[string]string{"specialUse": "hotRegion"})
	tc.AddLabelsStore(5, 0, map[string]string{"specialUse": "reserved"})
	ops = bs.Schedule(tc)
	c.Assert(ops, HasLen, 0)

	// can only move peer to 4
	tc.UpdateStorageWrittenBytes(1, 60*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(2, 6*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(3, 6*MB*statistics.StoreHeartBeatReportInterval)
	tc.UpdateStorageWrittenBytes(4, 0)
	tc.UpdateStorageWrittenBytes(5, 0)
	tc.AddLeaderRegionWithWriteInfo(1, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 3})
	tc.AddLeaderRegionWithWriteInfo(2, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 3})
	tc.AddLeaderRegionWithWriteInfo(3, 1, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{2, 3})
	tc.AddLeaderRegionWithWriteInfo(4, 2, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{1, 3})
	tc.AddLeaderRegionWithWriteInfo(5, 3, 512*KB*statistics.RegionHeartBeatReportInterval, 0, statistics.RegionHeartBeatReportInterval, []uint64{1, 2})
	ops = hs.Schedule(tc)
	c.Assert(ops, HasLen, 1)
	testutil.CheckTransferPeer(c, ops[0], operator.OpHotRegion, 1, 4)
}

func (s *testSpecialUseSuite) TestSpecialUseReserved(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oc := schedule.NewOperatorController(ctx, nil, nil)
	storage := core.NewStorage(kv.NewMemoryKV())
	cd := schedule.ConfigSliceDecoder(BalanceRegionType, []string{"", ""})
	bs, err := schedule.CreateScheduler(BalanceRegionType, oc, storage, cd)
	c.Assert(err, IsNil)

	opt := mockoption.NewScheduleOptions()
	opt.HotRegionCacheHitsThreshold = 0
	tc := mockcluster.NewCluster(opt)
	tc.AddRegionStore(1, 10)
	tc.AddRegionStore(2, 4)
	tc.AddRegionStore(3, 2)
	tc.AddRegionStore(4, 0)
	tc.AddLeaderRegion(1, 1, 2, 3)
	tc.AddLeaderRegion(2, 1, 2, 3)
	tc.AddLeaderRegion(3, 1, 2, 3)
	tc.AddLeaderRegion(4, 1, 2, 3)
	tc.AddLeaderRegion(5, 1, 2, 3)

	// balance region without label
	ops := bs.Schedule(tc)
	c.Assert(ops, HasLen, 1)
	testutil.CheckTransferPeer(c, ops[0], operator.OpBalance, 1, 4)

	// cannot balance to store 4 with label
	tc.AddLabelsStore(4, 0, map[string]string{"specialUse": "reserved"})
	ops = bs.Schedule(tc)
	c.Assert(ops, HasLen, 0)
}
