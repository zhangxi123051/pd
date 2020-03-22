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

package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/pingcap/errcode"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/v4/pkg/apiutil"
	"github.com/pingcap/pd/v4/pkg/typeutil"
	"github.com/pingcap/pd/v4/server"
	"github.com/pingcap/pd/v4/server/config"
	"github.com/pingcap/pd/v4/server/core"
	"github.com/pingcap/pd/v4/server/schedule"
	"github.com/pkg/errors"
	"github.com/unrolled/render"
)

// MetaStore contains meta information about a store.
type MetaStore struct {
	*metapb.Store
	StateName string `json:"state_name"`
}

// StoreStatus contains status about a store.
type StoreStatus struct {
	Capacity           typeutil.ByteSize  `json:"capacity"`
	Available          typeutil.ByteSize  `json:"available"`
	UsedSize           typeutil.ByteSize  `json:"used_size"`
	LeaderCount        int                `json:"leader_count"`
	LeaderWeight       float64            `json:"leader_weight"`
	LeaderScore        float64            `json:"leader_score"`
	LeaderSize         int64              `json:"leader_size"`
	RegionCount        int                `json:"region_count"`
	RegionWeight       float64            `json:"region_weight"`
	RegionScore        float64            `json:"region_score"`
	RegionSize         int64              `json:"region_size"`
	SendingSnapCount   uint32             `json:"sending_snap_count,omitempty"`
	ReceivingSnapCount uint32             `json:"receiving_snap_count,omitempty"`
	ApplyingSnapCount  uint32             `json:"applying_snap_count,omitempty"`
	IsBusy             bool               `json:"is_busy,omitempty"`
	StartTS            *time.Time         `json:"start_ts,omitempty"`
	LastHeartbeatTS    *time.Time         `json:"last_heartbeat_ts,omitempty"`
	Uptime             *typeutil.Duration `json:"uptime,omitempty"`
}

// StoreInfo contains information about a store.
type StoreInfo struct {
	Store  *MetaStore   `json:"store"`
	Status *StoreStatus `json:"status"`
}

const (
	disconnectedName = "Disconnected"
	downStateName    = "Down"
)

func newStoreInfo(opt *config.ScheduleConfig, store *core.StoreInfo) *StoreInfo {
	s := &StoreInfo{
		Store: &MetaStore{
			Store:     store.GetMeta(),
			StateName: store.GetState().String(),
		},
		Status: &StoreStatus{
			Capacity:           typeutil.ByteSize(store.GetCapacity()),
			Available:          typeutil.ByteSize(store.GetAvailable()),
			UsedSize:           typeutil.ByteSize(store.GetUsedSize()),
			LeaderCount:        store.GetLeaderCount(),
			LeaderWeight:       store.GetLeaderWeight(),
			LeaderScore:        store.LeaderScore(core.StringToSchedulePolicy(opt.LeaderSchedulePolicy), 0),
			LeaderSize:         store.GetLeaderSize(),
			RegionCount:        store.GetRegionCount(),
			RegionWeight:       store.GetRegionWeight(),
			RegionScore:        store.RegionScore(opt.HighSpaceRatio, opt.LowSpaceRatio, 0),
			RegionSize:         store.GetRegionSize(),
			SendingSnapCount:   store.GetSendingSnapCount(),
			ReceivingSnapCount: store.GetReceivingSnapCount(),
			ApplyingSnapCount:  store.GetApplyingSnapCount(),
			IsBusy:             store.IsBusy(),
		},
	}

	if store.GetStoreStats() != nil {
		startTS := store.GetStartTime()
		s.Status.StartTS = &startTS
	}
	if lastHeartbeat := store.GetLastHeartbeatTS(); !lastHeartbeat.IsZero() {
		s.Status.LastHeartbeatTS = &lastHeartbeat
	}
	if upTime := store.GetUptime(); upTime > 0 {
		duration := typeutil.NewDuration(upTime)
		s.Status.Uptime = &duration
	}

	if store.GetState() == metapb.StoreState_Up {
		if store.DownTime() > opt.MaxStoreDownTime.Duration {
			s.Store.StateName = downStateName
		} else if store.IsDisconnected() {
			s.Store.StateName = disconnectedName
		}
	}
	return s
}

// StoresInfo records stores' info.
type StoresInfo struct {
	Count  int          `json:"count"`
	Stores []*StoreInfo `json:"stores"`
}

type storeHandler struct {
	*server.Handler
	rd *render.Render
}

func newStoreHandler(handler *server.Handler, rd *render.Render) *storeHandler {
	return &storeHandler{
		Handler: handler,
		rd:      rd,
	}
}

func (h *storeHandler) Get(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	store := rc.GetStore(storeID)
	if store == nil {
		h.rd.JSON(w, http.StatusInternalServerError, server.ErrStoreNotFound(storeID))
		return
	}

	storeInfo := newStoreInfo(h.GetScheduleConfig(), store)
	h.rd.JSON(w, http.StatusOK, storeInfo)
}

func (h *storeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	var err error
	_, force := r.URL.Query()["force"]
	if force {
		err = rc.BuryStore(storeID, force)
	} else {
		err = rc.RemoveStore(storeID)
	}

	if err != nil {
		apiutil.ErrorResp(h.rd, w, err)
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storeHandler) SetState(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	stateStr := r.URL.Query().Get("state")
	state, ok := metapb.StoreState_value[stateStr]
	if !ok {
		h.rd.JSON(w, http.StatusBadRequest, "invalid state")
		return
	}

	err := rc.SetStoreState(storeID, metapb.StoreState(state))
	if err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storeHandler) SetLabels(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	var input map[string]string
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &input); err != nil {
		return
	}

	labels := make([]*metapb.StoreLabel, 0, len(input))
	for k, v := range input {
		labels = append(labels, &metapb.StoreLabel{
			Key:   k,
			Value: v,
		})
	}

	if err := config.ValidateLabels(labels); err != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(err))
		return
	}

	_, force := r.URL.Query()["force"]
	if err := rc.UpdateStoreLabels(storeID, labels, force); err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storeHandler) SetWeight(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	var input map[string]interface{}
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &input); err != nil {
		return
	}

	leaderVal, ok := input["leader"]
	if !ok {
		h.rd.JSON(w, http.StatusBadRequest, "leader weight unset")
		return
	}
	regionVal, ok := input["region"]
	if !ok {
		h.rd.JSON(w, http.StatusBadRequest, "region weight unset")
		return
	}
	leader, ok := leaderVal.(float64)
	if !ok || leader < 0 {
		h.rd.JSON(w, http.StatusBadRequest, "badformat leader weight")
		return
	}
	region, ok := regionVal.(float64)
	if !ok || region < 0 {
		h.rd.JSON(w, http.StatusBadRequest, "badformat region weight")
		return
	}

	if err := rc.SetStoreWeight(storeID, leader, region); err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storeHandler) SetLimit(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	storeID, errParse := apiutil.ParseUint64VarsField(vars, "id")
	if errParse != nil {
		apiutil.ErrorResp(h.rd, w, errcode.NewInvalidInputErr(errParse))
		return
	}

	var input map[string]interface{}
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &input); err != nil {
		return
	}

	rateVal, ok := input["rate"]
	if !ok {
		h.rd.JSON(w, http.StatusBadRequest, "rate unset")
		return
	}
	rate, ok := rateVal.(float64)
	if !ok || rate < 0 {
		h.rd.JSON(w, http.StatusBadRequest, "badformat rate")
		return
	}

	if err := h.SetStoreLimit(storeID, rate/schedule.StoreBalanceBaseTime); err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

type storesHandler struct {
	*server.Handler
	rd *render.Render
}

func newStoresHandler(handler *server.Handler, rd *render.Render) *storesHandler {
	return &storesHandler{
		Handler: handler,
		rd:      rd,
	}
}

func (h *storesHandler) RemoveTombStone(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	err := rc.RemoveTombStoneRecords()
	if err != nil {
		apiutil.ErrorResp(h.rd, w, err)
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storesHandler) SetAllLimit(w http.ResponseWriter, r *http.Request) {
	var input map[string]interface{}
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &input); err != nil {
		return
	}

	rateVal, ok := input["rate"]
	if !ok {
		h.rd.JSON(w, http.StatusBadRequest, "rate unset")
		return
	}
	rate, ok := rateVal.(float64)
	if !ok || rate < 0 {
		h.rd.JSON(w, http.StatusBadRequest, "badformat rate")
		return
	}

	if err := h.SetAllStoresLimit(rate / schedule.StoreBalanceBaseTime); err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storesHandler) GetAllLimit(w http.ResponseWriter, r *http.Request) {
	limits, err := h.GetAllStoresLimit()
	if err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	type LimitResp struct {
		Rate float64 `json:"rate"`
		Mode string  `json:"mode"`
	}
	resp := make(map[uint64]*LimitResp)
	for s, l := range limits {
		resp[s] = &LimitResp{
			Rate: l.Rate() * schedule.StoreBalanceBaseTime,
			Mode: l.Mode().String(),
		}
	}

	h.rd.JSON(w, http.StatusOK, resp)
}

func (h *storesHandler) SetStoreLimitScene(w http.ResponseWriter, r *http.Request) {
	scene := h.Handler.GetStoreLimitScene()
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &scene); err != nil {
		return
	}
	h.Handler.SetStoreLimitScene(scene)
	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *storesHandler) GetStoreLimitScene(w http.ResponseWriter, r *http.Request) {
	scene := h.Handler.GetStoreLimitScene()
	h.rd.JSON(w, http.StatusOK, scene)
}

func (h *storesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	stores := rc.GetMetaStores()
	StoresInfo := &StoresInfo{
		Stores: make([]*StoreInfo, 0, len(stores)),
	}

	urlFilter, err := newStoreStateFilter(r.URL)
	if err != nil {
		h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	stores = urlFilter.filter(rc.GetMetaStores())
	for _, s := range stores {
		storeID := s.GetId()
		store := rc.GetStore(storeID)
		if store == nil {
			h.rd.JSON(w, http.StatusInternalServerError, server.ErrStoreNotFound(storeID))
			return
		}

		storeInfo := newStoreInfo(h.GetScheduleConfig(), store)
		StoresInfo.Stores = append(StoresInfo.Stores, storeInfo)
	}
	StoresInfo.Count = len(StoresInfo.Stores)

	h.rd.JSON(w, http.StatusOK, StoresInfo)
}

type storeStateFilter struct {
	accepts []metapb.StoreState
}

func newStoreStateFilter(u *url.URL) (*storeStateFilter, error) {
	var acceptStates []metapb.StoreState
	if v, ok := u.Query()["state"]; ok {
		for _, s := range v {
			state, err := strconv.Atoi(s)
			if err != nil {
				return nil, errors.WithStack(err)
			}

			storeState := metapb.StoreState(state)
			switch storeState {
			case metapb.StoreState_Up, metapb.StoreState_Offline, metapb.StoreState_Tombstone:
				acceptStates = append(acceptStates, storeState)
			default:
				return nil, errors.Errorf("unknown StoreState: %v", storeState)
			}
		}
	} else {
		// Accepts Up and Offline by default.
		acceptStates = []metapb.StoreState{metapb.StoreState_Up, metapb.StoreState_Offline}
	}

	return &storeStateFilter{
		accepts: acceptStates,
	}, nil
}

func (filter *storeStateFilter) filter(stores []*metapb.Store) []*metapb.Store {
	ret := make([]*metapb.Store, 0, len(stores))
	for _, s := range stores {
		state := s.GetState()
		for _, accept := range filter.accepts {
			if state == accept {
				ret = append(ret, s)
				break
			}
		}
	}
	return ret
}
