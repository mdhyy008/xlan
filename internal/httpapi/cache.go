package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"xlan/core"
	"xlan/internal/cache"
)

// handleCacheClean 清理未完成传输的临时分片和已经结束的旧收件箱记录。
func (s *Server) handleCacheClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OlderThanSec int64 `json:"older_than_sec"`
		DryRun       bool  `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if req.OlderThanSec <= 0 {
		writeErr(w, http.StatusBadRequest, "BAD_AGE", "older_than_sec 必须大于 0")
		return
	}

	parts, freed, err := cache.CleanParts(s.RecvDir(), req.DryRun, func(id string) {
		s.clearRanges(id)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}
	records, err := cache.CleanRecords(s.Store, time.Now().Unix()-req.OlderThanSec, req.DryRun, func(it core.Item) {
		s.clearProgress(it.ID)
		s.clearRanges(it.ID)
		s.clear(it.ID)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STORE", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dry_run": req.DryRun,
		"parts": parts, "freed_bytes": freed, "records": records,
	})
}
