// File overview: Authenticated local snooze mutations and the explicit Snoozed list.

package web

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/store"
)

const maxSnoozeHorizon = 10 * 365 * 24 * time.Hour

type apiSnooze struct {
	ID           int64  `json:"id"`
	MessageID    int64  `json:"message_id"`
	SnoozedUntil string `json:"snoozed_until"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func apiSnoozeFromStore(snooze store.MessageSnooze) apiSnooze {
	return apiSnooze{
		ID:           snooze.ID,
		MessageID:    snooze.MessageID,
		SnoozedUntil: timeString(snooze.SnoozedUntil),
		CreatedAt:    timeString(snooze.CreatedAt),
		UpdatedAt:    timeString(snooze.UpdatedAt),
	}
}

func (s *Server) apiMessageSnooze(w http.ResponseWriter, r *http.Request, messageID int64) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		snooze, err := s.store.MessageSnoozeForUser(r.Context(), cu.User.ID, messageID)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, map[string]any{"snoozed": false})
			return
		}
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{
			"snoozed": snooze.SnoozedUntil.After(time.Now()),
			"snooze":  apiSnoozeFromStore(snooze),
		})
	case http.MethodPut, http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			Until     string `json:"until"`
			UntilUnix int64  `json:"until_unix"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		until, err := parseSnoozeUntil(in.Until, in.UntilUnix, time.Now().UTC())
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		snooze, err := s.store.SnoozeMessage(r.Context(), cu.User.ID, messageID, until)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.notifySnoozeStateChanged(cu.User.ID)
		writeJSON(w, map[string]any{"ok": true, "snoozed": true, "snooze": apiSnoozeFromStore(snooze)})
	case http.MethodDelete:
		if !s.verifyCSRF(w, r) {
			return
		}
		removed, err := s.store.UnsnoozeMessage(r.Context(), cu.User.ID, messageID)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if removed {
			s.notifySnoozeStateChanged(cu.User.ID)
		}
		writeJSON(w, map[string]any{"ok": true, "snoozed": false})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) apiSnoozes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	const pageSize = 50
	page := pageFromRequest(r)
	items, err := s.store.ListActiveSnoozedMessagesForUser(r.Context(), cu.User.ID, pageSize+1, (page-1)*pageSize, time.Now().UTC())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	hasNext := len(items) > pageSize
	if hasNext {
		items = items[:pageSize]
	}
	messages := make([]store.MessageRecord, 0, len(items))
	snoozes := make([]apiSnooze, 0, len(items))
	for _, item := range items {
		messages = append(messages, item.Message)
		snoozes = append(snoozes, apiSnoozeFromStore(item.Snooze))
	}
	conversations, err := s.conversationViews(r.Context(), cu.User.ID, messages, s.ownAddresses(r.Context(), cu.User))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, map[string]any{
		"conversations": s.apiConversationsWithAnnotations(r.Context(), cu.User.ID, conversations),
		"snoozes":       snoozes,
		"page":          page,
		"has_prev":      page > 1,
		"has_next":      hasNext,
	})
}

func parseSnoozeUntil(raw string, unix int64, now time.Time) (time.Time, error) {
	var until time.Time
	var err error
	if strings.TrimSpace(raw) != "" {
		until, err = time.Parse(time.RFC3339, strings.TrimSpace(raw))
		if err != nil {
			return time.Time{}, errors.New("until must be an RFC3339 timestamp")
		}
	} else if unix > 0 {
		until = time.Unix(unix, 0).UTC()
	} else {
		return time.Time{}, errors.New("until is required")
	}
	until = until.UTC().Truncate(time.Second)
	now = now.UTC()
	if !until.After(now) {
		return time.Time{}, errors.New("until must be in the future")
	}
	if until.After(now.Add(maxSnoozeHorizon)) {
		return time.Time{}, errors.New("until is too far in the future")
	}
	return until, nil
}
