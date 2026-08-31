// File overview: Authenticated Web Push subscription API endpoints.

package web

import (
	"errors"
	"net/http"
	"strings"

	"rolltop/backend/store"
)

type webPushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) apiPushVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireAPIAuth(w, r); !ok {
		return
	}
	publicKey, err := s.webPushVAPIDPublicKey()
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "web push is not configured")
		return
	}
	writeJSON(w, map[string]string{"public_key": publicKey})
}

func (s *Server) apiPushSubscription(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in webPushSubscriptionRequest
		if !decodeJSON(w, r, &in) {
			return
		}
		sub, err := s.store.SaveWebPushSubscription(r.Context(), cu.User.ID, store.WebPushSubscription{
			Endpoint:  strings.TrimSpace(in.Endpoint),
			P256DH:    strings.TrimSpace(in.Keys.P256DH),
			Auth:      strings.TrimSpace(in.Keys.Auth),
			UserAgent: r.UserAgent(),
		})
		if err != nil {
			// A rejected endpoint is the caller's fault, but a store failure is
			// not: answering both with 400 hid outages behind a client error.
			if errors.Is(err, store.ErrInvalidWebPushSubscription) {
				writeAPIError(w, http.StatusBadRequest, "invalid web push subscription")
				return
			}
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "subscription_id": sub.ID})
	case http.MethodDelete:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if err := s.store.DeleteWebPushSubscription(r.Context(), cu.User.ID, in.Endpoint); err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
