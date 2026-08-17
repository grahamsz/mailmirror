// File overview: Admin API handlers for users, plugins, and remote image blocklist settings.

package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"rolltop/backend/auth"
	"rolltop/backend/logging"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

func (s *Server) apiAdminUsers(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := s.store.ListUsers(r.Context())
		if err != nil {
			s.serverError(w, err)
			return
		}
		out := make([]apiUser, 0, len(users))
		for _, user := range users {
			out = append(out, safeUser(user))
		}
		writeJSON(w, map[string]any{"users": out, "password_reset_from_address": s.adminPasswordResetFromAddress(r.Context(), cu.User.Email)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			Email    string `json:"email"`
			Name     string `json:"name"`
			Password string `json:"password"`
			IsAdmin  bool   `json:"is_admin"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if len(in.Password) < 12 {
			writeAPIError(w, http.StatusBadRequest, "Password must be at least 12 characters.")
			return
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			s.serverError(w, err)
			return
		}
		_, err = s.store.CreateUser(r.Context(), in.Email, in.Name, hash, in.IsAdmin)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "Could not create user.")
			return
		}
		_ = cu
		writeJSON(w, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiAdminUserPath(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAdmin(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "password" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if len(in.Password) < 12 {
			writeAPIError(w, http.StatusBadRequest, "Password must be at least 12 characters.")
			return
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if err := s.store.UpdateUserPasswordHash(r.Context(), id, hash); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, err)
			return
		}
		_ = cu
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if !s.verifyCSRF(w, r) {
			return
		}
		if id == cu.User.ID {
			writeAPIError(w, http.StatusBadRequest, "You cannot delete the account you are currently using.")
			return
		}
		if err := s.store.DeleteUser(r.Context(), id); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	http.NotFound(w, r)
}

func (s *Server) apiAdminPasswordResetSettings(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"from_address": s.adminPasswordResetFromAddress(r.Context(), cu.User.Email)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			FromAddress string `json:"from_address"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		from := strings.TrimSpace(in.FromAddress)
		if from != "" {
			addr, err := mail.ParseAddress(from)
			if err != nil || strings.TrimSpace(addr.Address) == "" {
				writeAPIError(w, http.StatusBadRequest, "Password Reset from address must be a valid email address.")
				return
			}
			from = strings.ToLower(strings.TrimSpace(addr.Address))
		}
		if err := s.store.SetSystemSetting(r.Context(), passwordResetFromAddressSetting, from); err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "from_address": s.adminPasswordResetFromAddress(r.Context(), cu.User.Email)})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminPasswordResetFromAddress(ctx context.Context, fallback string) string {
	value, err := s.store.GetSystemSetting(ctx, passwordResetFromAddressSetting)
	if err == nil && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fallback)
}

func (s *Server) apiAdminPlugins(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	settings, err := s.store.ListPluginSettings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"plugins": s.apiAdminPluginSettings(settings)})
}

func (s *Server) apiAdminPlugin(w http.ResponseWriter, r *http.Request, rest string) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	pluginID := strings.Trim(rest, "/")
	if pluginID == "" || strings.Contains(pluginID, "/") {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.store.SyncPluginDefinitions(r.Context(), plugins.DefinitionsFromManifests(s.pluginManifests)); err != nil {
		s.serverError(w, err)
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.store.SetPluginEnabled(r.Context(), pluginID, in.Enabled); err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, err)
		return
	}
	defer s.notifyPluginSettingChanged(users)
	logging.Debugf("plugin setting updated plugin_id=%s enabled=%t", pluginID, in.Enabled)
	if in.Enabled {
		if _, _, err := s.startBackendPlugin(r.Context(), pluginID); err != nil {
			log.Printf("backend plugin %s enabled but unavailable: %v", pluginID, err)
		}
	} else if err := s.stopBackendPlugin(pluginID); err != nil {
		s.serverError(w, err)
		return
	}
	settings, err := s.store.ListPluginSettings(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "plugins": s.apiAdminPluginSettings(settings)})
}

// notifyPluginSettingChanged invalidates plugin-decorated list payloads for
// every tenant and wakes active clients. It deliberately skips cache warming
// and new-mail web-push work because an enablement change is not a mail event.
func (s *Server) notifyPluginSettingChanged(users []store.User) {
	if s == nil {
		return
	}
	for _, user := range users {
		if user.ID <= 0 {
			continue
		}
		s.noteMailListChanged(user.ID)
		if s.events != nil {
			s.events.Notify(user.ID)
		}
	}
}

func (s *Server) apiAdminRemoteImageBlocklist(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAPIAdmin(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		hook, ok := remoteImageBlocklistHook()
		if !ok || !s.pluginEnabled(r.Context(), plugins.RemoteImageBlocklist) {
			writeJSON(w, map[string]any{"patterns": []string{}})
			return
		}
		rules, err := hook.ListRemoteImageRules(r.Context(), s.store.DB())
		if err != nil {
			s.serverError(w, err)
			return
		}
		patterns := make([]string, 0, len(rules))
		for _, rule := range rules {
			if rule.Enabled {
				patterns = append(patterns, rule.Pattern)
			}
		}
		writeJSON(w, map[string]any{"patterns": patterns})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in struct {
			Patterns []string `json:"patterns"`
			Text     string   `json:"text"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		patterns := in.Patterns
		if patterns == nil && strings.TrimSpace(in.Text) != "" {
			patterns = strings.Split(in.Text, "\n")
		}
		patterns, err := normalizeRemoteImageBlockPatterns(patterns)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		hook, ok := remoteImageBlocklistHook()
		if !ok || !s.pluginEnabled(r.Context(), plugins.RemoteImageBlocklist) {
			http.NotFound(w, r)
			return
		}
		if err := hook.ReplaceRemoteImageRules(r.Context(), s.store.DB(), patterns); err != nil {
			s.serverError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "patterns": patterns})
	default:
		methodNotAllowed(w)
	}
}

func normalizeRemoteImageBlockPatterns(patterns []string) ([]string, error) {
	const maxPatterns = 100
	const maxPatternLen = 500
	out := make([]string, 0, len(patterns))
	seen := map[string]bool{}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		if len(pattern) > maxPatternLen {
			return nil, fmt.Errorf("Blocklist regex is too long; keep each pattern under %d characters.", maxPatternLen)
		}
		if seen[pattern] {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("Invalid blocklist regex: %v", err)
		}
		seen[pattern] = true
		out = append(out, pattern)
		if len(out) > maxPatterns {
			return nil, fmt.Errorf("Keep the remote image blocklist under %d patterns.", maxPatterns)
		}
	}
	return out, nil
}
