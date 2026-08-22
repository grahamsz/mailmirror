// File overview: Authenticated bulk read-state updates shared by the web and Android clients.

package web

import (
	"net/http"
)

const maxBulkReadMessageIDs = 1000

func (s *Server) apiBulkReadMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		IDs  []int64 `json:"ids"`
		Read bool    `json:"read"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "select at least one message")
		return
	}
	if len(in.IDs) > maxBulkReadMessageIDs {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too many messages selected")
		return
	}
	owned, err := s.store.OwnedMessageIDsForUser(r.Context(), cu.User.ID, in.IDs)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.store.MarkMessagesReadForUser(r.Context(), cu.User.ID, owned, in.Read, true); err != nil {
		s.serverError(w, err)
		return
	}
	s.notifyUserChanged(cu.User.ID)
	if s.syncer != nil && len(owned) > 0 {
		ids := append([]int64(nil), owned...)
		s.queueReadStatePush(cu.User.ID, ids)
	}
	writeJSON(w, map[string]any{"ok": true, "updated": len(owned)})
}
