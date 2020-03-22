// Copyright 2018 PingCAP, Inc.
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
	"strconv"

	"github.com/gorilla/mux"
	"github.com/pingcap/pd/v4/pkg/apiutil"
	"github.com/pingcap/pd/v4/server"
	"github.com/unrolled/render"
)

type adminHandler struct {
	svr *server.Server
	rd  *render.Render
}

func newAdminHandler(svr *server.Server, rd *render.Render) *adminHandler {
	return &adminHandler{
		svr: svr,
		rd:  rd,
	}
}

func (h *adminHandler) HandleDropCacheRegion(w http.ResponseWriter, r *http.Request) {
	rc := getCluster(r.Context())
	vars := mux.Vars(r)
	regionIDStr := vars["id"]
	regionID, err := strconv.ParseUint(regionIDStr, 10, 64)
	if err != nil {
		h.rd.JSON(w, http.StatusBadRequest, err.Error())
		return
	}
	rc.DropCacheRegion(regionID)
	h.rd.JSON(w, http.StatusOK, nil)
}

func (h *adminHandler) ResetTS(w http.ResponseWriter, r *http.Request) {
	handler := h.svr.GetHandler()
	var input map[string]interface{}
	if err := apiutil.ReadJSONRespondError(h.rd, w, r.Body, &input); err != nil {
		return
	}
	tsValue, ok := input["tso"].(string)
	if !ok || len(tsValue) == 0 {
		h.rd.JSON(w, http.StatusBadRequest, "invalid tso value")
		return
	}
	ts, err := strconv.ParseUint(tsValue, 10, 64)
	if err != nil {
		h.rd.JSON(w, http.StatusBadRequest, "invalid tso value")
		return
	}

	if err = handler.ResetTS(ts); err != nil {
		if err == server.ErrServerNotStarted {
			h.rd.JSON(w, http.StatusInternalServerError, err.Error())
		} else {
			h.rd.JSON(w, http.StatusForbidden, err.Error())
		}
	}
	h.rd.JSON(w, http.StatusOK, "success")
}
