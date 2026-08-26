package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store"
)

func TestHandleAppServesNeutralShellWithoutStartupData(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "shell-owner@example.test", "Shell Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	distDir := filepath.Join(dir, frontendDistDir)
	if err := os.MkdirAll(distDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distDir, "index.html"), []byte(`<!doctype html><html><head><meta name="rolltop-startup" /></head><body><div id="root"></div><script type="module" src="/assets/index.js"></script></body></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	server := &Server{store: db, startedAt: time.Now()}
	req := httptest.NewRequest(http.MethodGet, "/mail?shell=1", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: owner}))
	rec := httptest.NewRecorder()
	server.handleApp(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The inert startup <meta> marker stays in the document; the injected
	// session-bearing <script id="rolltop-startup"> must not.
	if strings.Contains(body, `<script id="rolltop-startup"`) || strings.Contains(body, owner.Email) || strings.Contains(body, owner.Name) {
		t.Fatalf("neutral shell leaked session data: %s", body)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Fatalf("neutral shell vary = %q, want empty so Cache.put accepts it", got)
	}

	personalized := httptest.NewRecorder()
	authedReq := httptest.NewRequest(http.MethodGet, "/mail", nil)
	authedReq = authedReq.WithContext(context.WithValue(authedReq.Context(), userContextKey, currentUser{User: owner}))
	server.handleApp(personalized, authedReq)
	if !strings.Contains(personalized.Body.String(), `<script id="rolltop-startup"`) {
		t.Fatalf("personalized document lost startup payload: %s", personalized.Body.String())
	}
	if got := personalized.Header().Get("Vary"); got != "*" {
		t.Fatalf("personalized vary = %q, want *", got)
	}
}
