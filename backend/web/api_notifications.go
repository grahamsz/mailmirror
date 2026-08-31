// File overview: Authenticated, cursor-based new-mail feed for native notifications.

package web

import (
	"net/http"
	"strconv"

	"rolltop/backend/store"
)

const newMailNotificationEnvelopeLimit = 5

type apiNewMailNotification struct {
	EventID   int64  `json:"event_id"`
	MessageID int64  `json:"message_id"`
	FromAddr  string `json:"from_addr"`
	Subject   string `json:"subject"`
}

type apiNewMailNotificationsResponse struct {
	UserID   int64                    `json:"user_id"`
	Cursor   int64                    `json:"cursor"`
	Count    int                      `json:"count"`
	Messages []apiNewMailNotification `json:"messages"`
}

func (s *Server) apiNewMailNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}

	values, hasAfter := r.URL.Query()["after"]
	if !hasAfter {
		cursor, err := s.store.LatestNewMailEventID(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, apiNewMailNotificationsResponse{
			UserID:   cu.User.ID,
			Cursor:   cursor,
			Messages: []apiNewMailNotification{},
		})
		return
	}
	if len(values) != 1 {
		writeAPIError(w, http.StatusBadRequest, "invalid new-mail cursor")
		return
	}
	after, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || after < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid new-mail cursor")
		return
	}
	events, count, cursor, err := s.store.NewMailEventsAfter(r.Context(), cu.User.ID, after, newMailNotificationEnvelopeLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, apiNewMailNotificationsResponse{
		UserID:   cu.User.ID,
		Cursor:   cursor,
		Count:    count,
		Messages: apiNewMailNotifications(events),
	})
}

func apiNewMailNotifications(events []store.NewMailEvent) []apiNewMailNotification {
	out := make([]apiNewMailNotification, 0, len(events))
	for _, event := range events {
		out = append(out, apiNewMailNotification{
			EventID:   event.ID,
			MessageID: event.MessageID,
			FromAddr:  event.FromAddr,
			Subject:   event.Subject,
		})
	}
	return out
}
