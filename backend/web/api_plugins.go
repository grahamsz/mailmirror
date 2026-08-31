// File overview: Plugin-specific API handlers and asset routes.

package web

import "net/http"

func (s *Server) apiPlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if _, ok := s.requireAPIAuth(w, r); !ok {
		return
	}
	settings, err := s.store.ListPluginSettings(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	enabled := make([]string, 0, len(settings))
	for _, setting := range settings {
		if setting.Enabled {
			enabled = append(enabled, setting.ID)
		}
	}
	writeJSON(w, map[string]any{"enabled": enabled})
}
