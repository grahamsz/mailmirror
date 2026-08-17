// File overview: Top-level API dispatcher. It routes authenticated API requests to account, mail, message, contact, plugin, sync, and admin handlers.

package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	switch {
	case path == "bootstrap":
		s.apiBootstrap(w, r)
	case path == "setup":
		s.apiSetup(w, r)
	case path == "login":
		s.apiLogin(w, r)
	case path == "password-reset/request":
		s.apiPasswordResetRequest(w, r)
	case path == "password-reset/complete":
		s.apiPasswordResetComplete(w, r)
	case path == "logout":
		s.apiLogout(w, r)
	case path == "profile":
		s.apiProfile(w, r)
	case path == "profile/swipes":
		s.apiSwipePreferences(w, r)
	case path == "mail":
		s.apiMail(w, r)
	case path == "snoozes":
		s.apiSnoozes(w, r)
	case path == "search":
		s.apiSearch(w, r)
	case path == "compose":
		s.apiCompose(w, r)
	case path == "compose/draft":
		s.apiComposeDraft(w, r)
	case path == "sync/status":
		s.apiSyncStatus(w, r)
	case path == "events":
		s.apiEvents(w, r)
	case path == "storage":
		s.apiStorage(w, r)
	case path == "push/vapid-public-key":
		s.apiPushVAPIDPublicKey(w, r)
	case path == "push/subscription":
		s.apiPushSubscription(w, r)
	case path == "notifications/new-mail":
		s.apiNewMailNotifications(w, r)
	case path == "notifications/reminders":
		s.apiSnoozeReminderNotifications(w, r)
	case path == "plugins":
		s.apiPlugins(w, r)
	case strings.HasPrefix(path, "plugins/"):
		s.apiBackendPlugin(w, r, strings.TrimPrefix(path, "plugins/"))
	case path == "contacts":
		s.apiContacts(w, r)
	case strings.HasPrefix(path, "contacts/"):
		s.apiContactPath(w, r, strings.TrimPrefix(path, "contacts/"))
	case path == "brand-icons":
		s.apiBrandIcons(w, r)
	case path == "account":
		s.apiAccount(w, r)
	case path == "account/imap":
		s.apiIMAPAccount(w, r)
	case strings.HasPrefix(path, "account/imap/"):
		s.apiIMAPAccountPath(w, r, strings.TrimPrefix(path, "account/imap/"))
	case path == "account/smtp":
		s.apiSMTPAccount(w, r)
	case strings.HasPrefix(path, "account/smtp/"):
		s.apiSMTPAccountPath(w, r, strings.TrimPrefix(path, "account/smtp/"))
	case path == "account/identities":
		s.apiMailIdentity(w, r)
	case path == "account/sync":
		s.apiAccountSync(w, r)
	case path == "account/folders/progress":
		s.apiAccountFolderProgress(w, r)
	case strings.HasPrefix(path, "account/folders/"):
		s.apiAccountFolder(w, r, strings.TrimPrefix(path, "account/folders/"))
	case path == "admin/users":
		s.apiAdminUsers(w, r)
	case strings.HasPrefix(path, "admin/users/"):
		s.apiAdminUserPath(w, r, strings.TrimPrefix(path, "admin/users/"))
	case path == "admin/password-reset":
		s.apiAdminPasswordResetSettings(w, r)
	case path == "admin/plugins":
		s.apiAdminPlugins(w, r)
	case strings.HasPrefix(path, "admin/plugins/"):
		s.apiAdminPlugin(w, r, strings.TrimPrefix(path, "admin/plugins/"))
	case path == "admin/remote-image-blocklist":
		s.apiAdminRemoteImageBlocklist(w, r)
	case path == "messages/bulk-move":
		s.apiBulkMoveMessages(w, r)
	case path == "messages/bulk-copy":
		s.apiBulkCopyMessages(w, r)
	case path == "messages/bulk-read":
		s.apiBulkReadMessages(w, r)
	case strings.HasPrefix(path, "messages/"):
		s.apiMessagePath(w, r, strings.TrimPrefix(path, "messages/"))
	case strings.HasPrefix(path, "sync-runs/"):
		s.apiSyncRun(w, r, strings.TrimPrefix(path, "sync-runs/"))
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) requireAPIAuth(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	cu, ok := current(r)
	if !ok {
		if sessionLookupFailed(r) {
			// The session cookie could not be checked against the store;
			// answering 401 here would read as a forced logout in the client.
			writeAPIError(w, http.StatusServiceUnavailable, "session lookup temporarily unavailable, retry shortly")
			return currentUser{}, false
		}
		writeAPIError(w, http.StatusUnauthorized, "login required")
		return currentUser{}, false
	}
	// Keep an already-warm compose identity cache available while this signed-in
	// user is active. This only touches memory; it never adds a database lookup
	// to ordinary mail, search, or event requests.
	s.touchComposeIdentityCache(cu.User.ID)
	return cu, true
}

func (s *Server) requireAPIAdmin(w http.ResponseWriter, r *http.Request) (currentUser, bool) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return currentUser{}, false
	}
	if !cu.User.IsAdmin {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return currentUser{}, false
	}
	return cu, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dest); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONCached(w http.ResponseWriter, r *http.Request, value any) {
	_, _ = writeJSONCachedWithETag(w, r, value)
}

func writeJSONCachedWithETag(w http.ResponseWriter, r *http.Request, value any) (string, bool) {
	body, etag, err := cachedJSONBody(value)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "failed to encode response")
		return "", false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	if r.Method == http.MethodGet && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return etag, true
	}
	_, _ = w.Write(body)
	return etag, true
}

func etagMatches(header string, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}
