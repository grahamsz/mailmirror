// File overview: Sync start, sync status, and sync event API handlers.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/buildinfo"
	"rolltop/backend/store"
)

func (s *Server) apiSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	var data viewData
	s.loadMailboxChrome(r.Context(), cu.User.ID, &data)
	writeJSONCached(w, r, map[string]any{
		"running":          data.SyncRunning,
		"latest":           apiSyncRunPtr(data.LatestSyncRun),
		"active_sync_runs": apiSyncRuns(data.ActiveSyncRuns),
	})
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "event streaming is not available")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsubscribe := s.events.Subscribe(cu.User.ID)
	defer unsubscribe()

	writeEvent := func() bool {
		payload, err := s.syncEventPayload(r.Context(), cu.User.ID)
		if err != nil {
			return false
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: chrome\ndata: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeEvent() {
		return
	}
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ch:
			if !ok || !writeEvent() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) syncEventPayload(ctx context.Context, userID int64) (map[string]any, error) {
	var data viewData
	s.loadMailboxChrome(ctx, userID, &data)
	swipePreferences, err := s.store.GetSwipePreferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	info := buildinfo.Current()
	return map[string]any{
		"mailboxes":             apiMailboxes(data.Mailboxes),
		"latest_sync_run":       apiSyncRunPtr(data.LatestSyncRun),
		"active_sync_runs":      apiSyncRuns(data.ActiveSyncRuns),
		"sync_running":          data.SyncRunning,
		"mail_generation":       s.mailListGeneration(userID),
		"swipe_preferences":     apiSwipePreferencesFromStore(swipePreferences),
		"server_started_at":     timeString(s.startedAt),
		"server_uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"build_version":         info.Version,
		"build_date":            info.BuildDate,
		"build_label":           info.Label,
		"build_commit":          info.Commit,
		"public_site_url":       info.PublicSiteURL,
		"storage_retained":      true,
	}, nil
}
func (s *Server) apiStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSONCached(w, r, s.cachedStorageStats(cu.User.ID))
}
func (s *Server) apiSyncRun(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	run, err := s.store.GetSyncRunForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !s.verifyCSRF(w, r) {
			return
		}
		if run.Status != "running" {
			writeAPIError(w, http.StatusConflict, "this sync run is already finished")
			return
		}
		active := s.syncRunner != nil && s.syncRunner.CancelSyncRun(cu.User.ID, id)
		if err := s.store.InterruptSyncRunForUser(r.Context(), cu.User.ID, id, "Cancelled by user."); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.events.Notify(cu.User.ID)
		writeJSON(w, map[string]any{"ok": true, "active_worker_cancelled": active})
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	live := syncRunLiveDetail{}
	if s.syncRunner != nil {
		details := s.syncRunner.SyncRunLiveDetails(cu.User.ID, id)
		live = syncRunLiveDetail{Active: details.Active, Cancellable: details.Cancellable || run.Status == "running", Phase: details.Phase, Detail: details.Detail, PhaseStartedAt: timeString(details.PhaseStartedAt)}
	}
	writeJSONCached(w, r, map[string]any{"sync_run": apiSyncRunFrom(run), "live": live})
}

type syncRunLiveDetail struct {
	Active         bool   `json:"active"`
	Cancellable    bool   `json:"cancellable"`
	Phase          string `json:"phase"`
	Detail         string `json:"detail"`
	PhaseStartedAt string `json:"phase_started_at"`
}
