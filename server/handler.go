// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errcode"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/pingcap/pd/v4/server/cluster"
	"github.com/pingcap/pd/v4/server/config"
	"github.com/pingcap/pd/v4/server/core"
	"github.com/pingcap/pd/v4/server/schedule"
	"github.com/pingcap/pd/v4/server/schedule/operator"
	"github.com/pingcap/pd/v4/server/schedule/opt"
	"github.com/pingcap/pd/v4/server/schedulers"
	"github.com/pingcap/pd/v4/server/statistics"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	// SchedulerConfigHandlerPath is the api router path of the schedule config handler.
	SchedulerConfigHandlerPath = "/api/v1/scheduler-config"

	// ErrServerNotStarted is error info for server not started.
	ErrServerNotStarted = errors.New("The server has not been started")
	// ErrOperatorNotFound is error info for operator not found.
	ErrOperatorNotFound = errors.New("operator not found")
	// ErrAddOperator is error info for already have an operator when adding operator.
	ErrAddOperator = errors.New("failed to add operator, maybe already have one")
	// ErrRegionNotAdjacent is error info for region not adjacent.
	ErrRegionNotAdjacent = errors.New("two regions are not adjacent")
	// ErrRegionNotFound is error info for region not found.
	ErrRegionNotFound = func(regionID uint64) error {
		return errors.Errorf("region %v not found", regionID)
	}
	// ErrRegionAbnormalPeer is error info for region has abonormal peer.
	ErrRegionAbnormalPeer = func(regionID uint64) error {
		return errors.Errorf("region %v has abnormal peer", regionID)
	}
	// ErrStoreNotFound is error info for store not found.
	ErrStoreNotFound = func(storeID uint64) error {
		return errors.Errorf("store %v not found", storeID)
	}
	// ErrPluginNotFound is error info for plugin not found.
	ErrPluginNotFound = func(pluginPath string) error {
		return errors.Errorf("plugin is not found: %s", pluginPath)
	}
)

// Handler is a helper to export methods to handle API/RPC requests.
type Handler struct {
	s               *Server
	opt             *config.ScheduleOption
	pluginChMap     map[string]chan string
	pluginChMapLock sync.RWMutex
}

func newHandler(s *Server) *Handler {
	return &Handler{s: s, opt: s.scheduleOpt, pluginChMap: make(map[string]chan string), pluginChMapLock: sync.RWMutex{}}
}

// GetRaftCluster returns RaftCluster.
func (h *Handler) GetRaftCluster() (*cluster.RaftCluster, error) {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil, errors.WithStack(cluster.ErrNotBootstrapped)
	}
	return rc, nil
}

// GetOperatorController returns OperatorController.
func (h *Handler) GetOperatorController() (*schedule.OperatorController, error) {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil, errors.WithStack(cluster.ErrNotBootstrapped)
	}
	return rc.GetOperatorController(), nil
}

// IsSchedulerPaused returns whether scheduler is paused.
func (h *Handler) IsSchedulerPaused(name string) (bool, error) {
	c, err := h.GetRaftCluster()
	if err != nil {
		return true, err
	}
	sc, ok := c.GetSchedulers()[name]
	if !ok {
		return true, errors.Errorf("scheduler %v not found", name)
	}
	return sc.IsPaused(), nil
}

// GetScheduleConfig returns ScheduleConfig.
func (h *Handler) GetScheduleConfig() *config.ScheduleConfig {
	return h.s.GetScheduleConfig()
}

// GetSchedulers returns all names of schedulers.
func (h *Handler) GetSchedulers() ([]string, error) {
	c, err := h.GetRaftCluster()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.GetSchedulers()))
	for name := range c.GetSchedulers() {
		names = append(names, name)
	}
	return names, nil
}

// GetStores returns all stores in the cluster.
func (h *Handler) GetStores() ([]*core.StoreInfo, error) {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil, errors.WithStack(cluster.ErrNotBootstrapped)
	}
	storeMetas := rc.GetMetaStores()
	stores := make([]*core.StoreInfo, 0, len(storeMetas))
	for _, s := range storeMetas {
		storeID := s.GetId()
		store := rc.GetStore(storeID)
		if store == nil {
			return nil, ErrStoreNotFound(storeID)
		}
		stores = append(stores, store)
	}
	return stores, nil
}

// GetHotWriteRegions gets all hot write regions stats.
func (h *Handler) GetHotWriteRegions() *statistics.StoreHotPeersInfos {
	c, err := h.GetRaftCluster()
	if err != nil {
		return nil
	}
	return c.GetHotWriteRegions()
}

// GetHotReadRegions gets all hot read regions stats.
func (h *Handler) GetHotReadRegions() *statistics.StoreHotPeersInfos {
	c, err := h.GetRaftCluster()
	if err != nil {
		return nil
	}
	return c.GetHotReadRegions()
}

// GetHotBytesWriteStores gets all hot write stores stats.
func (h *Handler) GetHotBytesWriteStores() map[uint64]float64 {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil
	}
	return rc.GetStoresBytesWriteStat()
}

// GetHotBytesReadStores gets all hot write stores stats.
func (h *Handler) GetHotBytesReadStores() map[uint64]float64 {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil
	}
	return rc.GetStoresBytesReadStat()
}

// GetHotKeysWriteStores gets all hot write stores stats.
func (h *Handler) GetHotKeysWriteStores() map[uint64]float64 {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil
	}
	return rc.GetStoresKeysWriteStat()
}

// GetHotKeysReadStores gets all hot write stores stats.
func (h *Handler) GetHotKeysReadStores() map[uint64]float64 {
	rc := h.s.GetRaftCluster()
	if rc == nil {
		return nil
	}
	return rc.GetStoresKeysReadStat()
}

// AddScheduler adds a scheduler.
func (h *Handler) AddScheduler(name string, args ...string) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	s, err := schedule.CreateScheduler(name, c.GetOperatorController(), h.s.storage, schedule.ConfigSliceDecoder(name, args))
	if err != nil {
		return err
	}
	log.Info("create scheduler", zap.String("scheduler-name", s.GetName()))
	if err = c.AddScheduler(s, args...); err != nil {
		log.Error("can not add scheduler", zap.String("scheduler-name", s.GetName()), zap.Error(err))
	} else if err = h.opt.Persist(c.GetStorage()); err != nil {
		log.Error("can not persist scheduler config", zap.Error(err))
	}
	return err
}

// RemoveScheduler removes a scheduler by name.
func (h *Handler) RemoveScheduler(name string) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}
	if err = c.RemoveScheduler(name); err != nil {
		log.Error("can not remove scheduler", zap.String("scheduler-name", name), zap.Error(err))
	}
	return err
}

// PauseOrResumeScheduler pasues a scheduler for delay seconds or resume a paused scheduler.
// t == 0 : resume scheduler.
// t > 0 : scheduler delays t seconds.
func (h *Handler) PauseOrResumeScheduler(name string, t int64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}
	if err = c.PauseOrResumeScheduler(name, t); err != nil {
		if t == 0 {
			log.Error("can not resume scheduler", zap.String("scheduler-name", name), zap.Error(err))
		} else {
			log.Error("can not pause scheduler", zap.String("scheduler-name", name), zap.Error(err))
		}
	}
	return err
}

// AddBalanceLeaderScheduler adds a balance-leader-scheduler.
func (h *Handler) AddBalanceLeaderScheduler() error {
	return h.AddScheduler(schedulers.BalanceLeaderType)
}

// AddBalanceRegionScheduler adds a balance-region-scheduler.
func (h *Handler) AddBalanceRegionScheduler() error {
	return h.AddScheduler(schedulers.BalanceRegionType)
}

// AddBalanceHotRegionScheduler adds a balance-hot-region-scheduler.
func (h *Handler) AddBalanceHotRegionScheduler() error {
	return h.AddScheduler(schedulers.HotRegionType)
}

// AddLabelScheduler adds a label-scheduler.
func (h *Handler) AddLabelScheduler() error {
	return h.AddScheduler(schedulers.LabelType)
}

// AddScatterRangeScheduler adds a balance-range-leader-scheduler
func (h *Handler) AddScatterRangeScheduler(args ...string) error {
	return h.AddScheduler(schedulers.ScatterRangeType, args...)
}

// AddAdjacentRegionScheduler adds a balance-adjacent-region-scheduler.
func (h *Handler) AddAdjacentRegionScheduler(args ...string) error {
	return h.AddScheduler(schedulers.AdjacentRegionType, args...)
}

// AddGrantLeaderScheduler adds a grant-leader-scheduler.
func (h *Handler) AddGrantLeaderScheduler(storeID uint64) error {
	return h.AddScheduler(schedulers.GrantLeaderType, strconv.FormatUint(storeID, 10))
}

// AddEvictLeaderScheduler adds an evict-leader-scheduler.
func (h *Handler) AddEvictLeaderScheduler(storeID uint64) error {
	return h.AddScheduler(schedulers.EvictLeaderType, strconv.FormatUint(storeID, 10))
}

// AddShuffleLeaderScheduler adds a shuffle-leader-scheduler.
func (h *Handler) AddShuffleLeaderScheduler() error {
	return h.AddScheduler(schedulers.ShuffleLeaderType)
}

// AddShuffleRegionScheduler adds a shuffle-region-scheduler.
func (h *Handler) AddShuffleRegionScheduler() error {
	return h.AddScheduler(schedulers.ShuffleRegionType)
}

// AddShuffleHotRegionScheduler adds a shuffle-hot-region-scheduler.
func (h *Handler) AddShuffleHotRegionScheduler(limit uint64) error {
	return h.AddScheduler(schedulers.ShuffleHotRegionType, strconv.FormatUint(limit, 10))
}

// AddRandomMergeScheduler adds a random-merge-scheduler.
func (h *Handler) AddRandomMergeScheduler() error {
	return h.AddScheduler(schedulers.RandomMergeType)
}

// GetOperator returns the region operator.
func (h *Handler) GetOperator(regionID uint64) (*operator.Operator, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}

	op := c.GetOperator(regionID)
	if op == nil {
		return nil, ErrOperatorNotFound
	}

	return op, nil
}

// GetOperatorStatus returns the status of the region operator.
func (h *Handler) GetOperatorStatus(regionID uint64) (*schedule.OperatorWithStatus, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}

	op := c.GetOperatorStatus(regionID)
	if op == nil {
		return nil, ErrOperatorNotFound
	}

	return op, nil
}

// RemoveOperator removes the region operator.
func (h *Handler) RemoveOperator(regionID uint64) error {
	c, err := h.GetOperatorController()
	if err != nil {
		return err
	}

	op := c.GetOperator(regionID)
	if op == nil {
		return ErrOperatorNotFound
	}

	_ = c.RemoveOperator(op)
	return nil
}

// GetOperators returns the running operators.
func (h *Handler) GetOperators() ([]*operator.Operator, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}
	return c.GetOperators(), nil
}

// GetWaitingOperators returns the waiting operators.
func (h *Handler) GetWaitingOperators() ([]*operator.Operator, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}
	return c.GetWaitingOperators(), nil
}

// GetAdminOperators returns the running admin operators.
func (h *Handler) GetAdminOperators() ([]*operator.Operator, error) {
	return h.GetOperatorsOfKind(operator.OpAdmin)
}

// GetLeaderOperators returns the running leader operators.
func (h *Handler) GetLeaderOperators() ([]*operator.Operator, error) {
	return h.GetOperatorsOfKind(operator.OpLeader)
}

// GetRegionOperators returns the running region operators.
func (h *Handler) GetRegionOperators() ([]*operator.Operator, error) {
	return h.GetOperatorsOfKind(operator.OpRegion)
}

// GetOperatorsOfKind returns the running operators of the kind.
func (h *Handler) GetOperatorsOfKind(mask operator.OpKind) ([]*operator.Operator, error) {
	ops, err := h.GetOperators()
	if err != nil {
		return nil, err
	}
	var results []*operator.Operator
	for _, op := range ops {
		if op.Kind()&mask != 0 {
			results = append(results, op)
		}
	}
	return results, nil
}

// GetHistory returns finished operators' history since start.
func (h *Handler) GetHistory(start time.Time) ([]operator.OpHistory, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}
	return c.GetHistory(start), nil
}

// SetAllStoresLimit is used to set limit of all stores.
func (h *Handler) SetAllStoresLimit(rate float64) error {
	c, err := h.GetOperatorController()
	if err != nil {
		return err
	}
	c.SetAllStoresLimit(rate, schedule.StoreLimitManual)
	return nil
}

// GetAllStoresLimit is used to get limit of all stores.
func (h *Handler) GetAllStoresLimit() (map[uint64]*schedule.StoreLimit, error) {
	c, err := h.GetOperatorController()
	if err != nil {
		return nil, err
	}
	return c.GetAllStoresLimit(), nil
}

// SetStoreLimit is used to set the limit of a store.
func (h *Handler) SetStoreLimit(storeID uint64, rate float64) error {
	c, err := h.GetOperatorController()
	if err != nil {
		return err
	}
	c.SetStoreLimit(storeID, rate, schedule.StoreLimitManual)
	return nil
}

// AddTransferLeaderOperator adds an operator to transfer leader to the store.
func (h *Handler) AddTransferLeaderOperator(regionID uint64, storeID uint64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	newLeader := region.GetStoreVoter(storeID)
	if newLeader == nil {
		return errors.Errorf("region has no voter in store %v", storeID)
	}

	op, err := operator.CreateTransferLeaderOperator("admin-transfer-leader", c, region, region.GetLeader().GetStoreId(), newLeader.GetStoreId(), operator.OpAdmin)
	if err != nil {
		log.Debug("fail to create transfer leader operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddTransferRegionOperator adds an operator to transfer region to the stores.
func (h *Handler) AddTransferRegionOperator(regionID uint64, storeIDs map[uint64]struct{}) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	if c.IsPlacementRulesEnabled() {
		// Cannot determine role when placement rules enabled. Not supported now.
		return errors.New("transfer region is not supported when placement rules enabled")
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	if len(storeIDs) > c.GetMaxReplicas() {
		return errors.Errorf("the number of stores is %v, beyond the max replicas", len(storeIDs))
	}

	var store *core.StoreInfo
	for id := range storeIDs {
		store = c.GetStore(id)
		if store == nil {
			return core.NewStoreNotFoundErr(id)
		}
		if store.IsTombstone() {
			return errcode.Op("operator.add").AddTo(core.StoreTombstonedErr{StoreID: id})
		}
	}

	peers := make(map[uint64]*metapb.Peer)
	for id := range storeIDs {
		peers[id] = &metapb.Peer{StoreId: id}
	}

	op, err := operator.CreateMoveRegionOperator("admin-move-region", c, region, operator.OpAdmin, peers)
	if err != nil {
		log.Debug("fail to create move region operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddTransferPeerOperator adds an operator to transfer peer.
func (h *Handler) AddTransferPeerOperator(regionID uint64, fromStoreID, toStoreID uint64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	oldPeer := region.GetStorePeer(fromStoreID)
	if oldPeer == nil {
		return errors.Errorf("region has no peer in store %v", fromStoreID)
	}

	toStore := c.GetStore(toStoreID)
	if toStore == nil {
		return core.NewStoreNotFoundErr(toStoreID)
	}
	if toStore.IsTombstone() {
		return errcode.Op("operator.add").AddTo(core.StoreTombstonedErr{StoreID: toStoreID})
	}

	newPeer := &metapb.Peer{StoreId: toStoreID, IsLearner: oldPeer.GetIsLearner()}
	op, err := operator.CreateMovePeerOperator("admin-move-peer", c, region, operator.OpAdmin, fromStoreID, newPeer)
	if err != nil {
		log.Debug("fail to create move peer operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// checkAdminAddPeerOperator checks adminAddPeer operator with given region ID and store ID.
func (h *Handler) checkAdminAddPeerOperator(regionID uint64, toStoreID uint64) (*cluster.RaftCluster, *core.RegionInfo, error) {
	c, err := h.GetRaftCluster()
	if err != nil {
		return nil, nil, err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return nil, nil, ErrRegionNotFound(regionID)
	}

	if region.GetStorePeer(toStoreID) != nil {
		return nil, nil, errors.Errorf("region already has peer in store %v", toStoreID)
	}

	toStore := c.GetStore(toStoreID)
	if toStore == nil {
		return nil, nil, core.NewStoreNotFoundErr(toStoreID)
	}
	if toStore.IsTombstone() {
		return nil, nil, errcode.Op("operator.add").AddTo(core.StoreTombstonedErr{StoreID: toStoreID})
	}

	return c, region, nil
}

// AddAddPeerOperator adds an operator to add peer.
func (h *Handler) AddAddPeerOperator(regionID uint64, toStoreID uint64) error {
	c, region, err := h.checkAdminAddPeerOperator(regionID, toStoreID)
	if err != nil {
		return err
	}

	newPeer := &metapb.Peer{StoreId: toStoreID}
	op, err := operator.CreateAddPeerOperator("admin-add-peer", c, region, newPeer, operator.OpAdmin)
	if err != nil {
		log.Debug("fail to create add peer operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddAddLearnerOperator adds an operator to add learner.
func (h *Handler) AddAddLearnerOperator(regionID uint64, toStoreID uint64) error {
	c, region, err := h.checkAdminAddPeerOperator(regionID, toStoreID)
	if err != nil {
		return err
	}

	newPeer := &metapb.Peer{
		StoreId:   toStoreID,
		IsLearner: true,
	}

	op, err := operator.CreateAddPeerOperator("admin-add-learner", c, region, newPeer, operator.OpAdmin)
	if err != nil {
		log.Debug("fail to create add learner operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddRemovePeerOperator adds an operator to remove peer.
func (h *Handler) AddRemovePeerOperator(regionID uint64, fromStoreID uint64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	if region.GetStorePeer(fromStoreID) == nil {
		return errors.Errorf("region has no peer in store %v", fromStoreID)
	}

	op, err := operator.CreateRemovePeerOperator("admin-remove-peer", c, operator.OpAdmin, region, fromStoreID)
	if err != nil {
		log.Debug("fail to create move peer operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddMergeRegionOperator adds an operator to merge region.
func (h *Handler) AddMergeRegionOperator(regionID uint64, targetID uint64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	target := c.GetRegion(targetID)
	if target == nil {
		return ErrRegionNotFound(targetID)
	}

	if !opt.IsRegionHealthy(c, region) || !opt.IsRegionReplicated(c, region) {
		return ErrRegionAbnormalPeer(regionID)
	}

	if !opt.IsRegionHealthy(c, target) || !opt.IsRegionReplicated(c, target) {
		return ErrRegionAbnormalPeer(targetID)
	}

	// for the case first region (start key is nil) with the last region (end key is nil) but not adjacent
	if (!bytes.Equal(region.GetStartKey(), target.GetEndKey()) || len(region.GetStartKey()) == 0) &&
		(!bytes.Equal(region.GetEndKey(), target.GetStartKey()) || len(region.GetEndKey()) == 0) {
		return ErrRegionNotAdjacent
	}

	ops, err := operator.CreateMergeRegionOperator("admin-merge-region", c, region, target, operator.OpAdmin)
	if err != nil {
		log.Debug("fail to create merge region operator", zap.Error(err))
		return err
	}
	if ok := c.GetOperatorController().AddOperator(ops...); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddSplitRegionOperator adds an operator to split a region.
func (h *Handler) AddSplitRegionOperator(regionID uint64, policyStr string, keys []string) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	policy, ok := pdpb.CheckPolicy_value[strings.ToUpper(policyStr)]
	if !ok {
		return errors.Errorf("check policy %s is not supported", policyStr)
	}

	var splitKeys [][]byte
	if pdpb.CheckPolicy(policy) == pdpb.CheckPolicy_USEKEY {
		for i := range keys {
			k, err := hex.DecodeString(keys[i])
			if err != nil {
				return errors.Errorf("split key %s is not in hex format", keys[i])
			}
			splitKeys = append(splitKeys, k)
		}
	}

	op := operator.CreateSplitRegionOperator("admin-split-region", region, operator.OpAdmin, pdpb.CheckPolicy(policy), splitKeys)
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// AddScatterRegionOperator adds an operator to scatter a region.
func (h *Handler) AddScatterRegionOperator(regionID uint64) error {
	c, err := h.GetRaftCluster()
	if err != nil {
		return err
	}

	region := c.GetRegion(regionID)
	if region == nil {
		return ErrRegionNotFound(regionID)
	}

	if c.IsRegionHot(region) {
		return errors.Errorf("region %d is a hot region", regionID)
	}

	op, err := c.GetRegionScatter().Scatter(region)
	if err != nil {
		return err
	}

	if op == nil {
		return nil
	}
	if ok := c.GetOperatorController().AddOperator(op); !ok {
		return errors.WithStack(ErrAddOperator)
	}
	return nil
}

// GetDownPeerRegions gets the region with down peer.
func (h *Handler) GetDownPeerRegions() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.DownPeer), nil
}

// GetExtraPeerRegions gets the region exceeds the specified number of peers.
func (h *Handler) GetExtraPeerRegions() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.ExtraPeer), nil
}

// GetMissPeerRegions gets the region less than the specified number of peers.
func (h *Handler) GetMissPeerRegions() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.MissPeer), nil
}

// GetPendingPeerRegions gets the region with pending peer.
func (h *Handler) GetPendingPeerRegions() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.PendingPeer), nil
}

// GetSchedulerConfigHandler gets the handler of schedulers.
func (h *Handler) GetSchedulerConfigHandler() http.Handler {
	c, err := h.GetRaftCluster()
	if err != nil {
		return nil
	}
	mux := http.NewServeMux()
	for name, handler := range c.GetSchedulers() {
		prefix := path.Join(pdRootPath, SchedulerConfigHandlerPath, name)
		urlPath := prefix + "/"
		mux.Handle(urlPath, http.StripPrefix(prefix, handler))
	}
	return mux
}

// GetOfflinePeer gets the region with offline peer.
func (h *Handler) GetOfflinePeer() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.OfflinePeer), nil
}

// GetEmptyRegion gets the region with empty size.
func (h *Handler) GetEmptyRegion() ([]*core.RegionInfo, error) {
	c := h.s.GetRaftCluster()
	if c == nil {
		return nil, cluster.ErrNotBootstrapped
	}
	return c.GetRegionStatsByType(statistics.EmptyRegion), nil
}

// ResetTS resets the ts with specified tso.
func (h *Handler) ResetTS(ts uint64) error {
	tsoServer := h.s.tso
	if tsoServer == nil {
		return ErrServerNotStarted
	}
	return tsoServer.ResetUserTimestamp(ts)
}

// SetStoreLimitScene sets the limit values for differents scenes
func (h *Handler) SetStoreLimitScene(scene *schedule.StoreLimitScene) {
	cluster := h.s.GetRaftCluster()
	cluster.GetStoreLimiter().ReplaceStoreLimitScene(scene)
}

// GetStoreLimitScene returns the limit valus for different scenes
func (h *Handler) GetStoreLimitScene() *schedule.StoreLimitScene {
	cluster := h.s.GetRaftCluster()
	return cluster.GetStoreLimiter().StoreLimitScene()
}

// PluginLoad loads the plugin referenced by the pluginPath
func (h *Handler) PluginLoad(pluginPath string) error {
	h.pluginChMapLock.Lock()
	defer h.pluginChMapLock.Unlock()
	cluster, err := h.GetRaftCluster()
	if err != nil {
		return err
	}
	c := cluster.GetCoordinator()
	ch := make(chan string)
	h.pluginChMap[pluginPath] = ch
	c.LoadPlugin(pluginPath, ch)
	return nil
}

// PluginUnload unloads the plugin referenced by the pluginPath
func (h *Handler) PluginUnload(pluginPath string) error {
	h.pluginChMapLock.Lock()
	defer h.pluginChMapLock.Unlock()
	if ch, ok := h.pluginChMap[pluginPath]; ok {
		ch <- cluster.PluginUnload
		return nil
	}
	return ErrPluginNotFound(pluginPath)
}

// GetAddr returns the server urls for clients.
func (h *Handler) GetAddr() string {
	return h.s.GetAddr()
}
