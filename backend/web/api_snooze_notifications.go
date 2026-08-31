// File overview: Authenticated, cursor-based due-reminder feed for native clients.

package web

import (
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

type apiSnoozeReminderNotification struct {
	ID        int64  `json:"id"`
	MessageID int64  `json:"message_id"`
	From      string `json:"from"`
	Subject   string `json:"subject"`
	DueAt     string `json:"due_at"`
}

type apiSnoozeReminderNotificationsResponse struct {
	UserID    int64                           `json:"user_id"`
	Count     int                             `json:"count"`
	Cursor    int64                           `json:"cursor"`
	Reminders []apiSnoozeReminderNotification `json:"reminders"`
}

func (s *Server) apiSnoozeReminderNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	rawAfter := strings.TrimSpace(r.URL.Query().Get("after"))
	if rawAfter == "" {
		cursor, err := s.store.LatestSnoozeReminderEventID(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, apiSnoozeReminderNotificationsResponse{
			UserID: cu.User.ID, Cursor: cursor, Reminders: []apiSnoozeReminderNotification{},
		})
		return
	}
	after, err := strconv.ParseInt(rawAfter, 10, 64)
	if err != nil || after < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid notification cursor")
		return
	}
	events, count, cursor, err := s.store.SnoozeReminderEventsAfter(r.Context(), cu.User.ID, after, newMailNotificationEnvelopeLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, apiSnoozeReminderNotificationsResponse{
		UserID: cu.User.ID, Count: count, Cursor: cursor, Reminders: apiSnoozeReminderNotifications(events),
	})
}

func apiSnoozeReminderNotifications(events []store.SnoozeReminderEvent) []apiSnoozeReminderNotification {
	out := make([]apiSnoozeReminderNotification, 0, len(events))
	for _, event := range events {
		out = append(out, apiSnoozeReminderNotification{
			ID: event.ID, MessageID: event.MessageID, From: event.FromAddr,
			Subject: event.Subject, DueAt: timeString(event.DueAt),
		})
	}
	return out
}
