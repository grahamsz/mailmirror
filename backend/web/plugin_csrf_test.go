package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

func TestDispatchProtectedAPIPathDefaultsToDenyingCSRFMutations(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.SetPluginEnabled(ctx, plugins.ClientSidePGP, true); err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "csrf@example.test", "CSRF", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	var guardedCalled, skippedCalled bool
	server := &Server{
		store:              db,
		masterKey:          []byte("0123456789abcdef0123456789abcdef"),
		protectedAPIRoutes: newProtectedAPIRouteRegistry(),
	}
	handle, err := server.protectedAPIRouteRegistry().register(plugins.ClientSidePGP, plugins.ProtectedAPIRoute{
		Path: "plugins/client_side_pgp/csrf-guarded",
		Handle: func(plugins.APIHost, string, http.ResponseWriter, *http.Request) {
			guardedCalled = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handle.Unregister)
	skippedHandle, err := server.protectedAPIRouteRegistry().register(plugins.ClientSidePGP, plugins.ProtectedAPIRoute{
		Path:          "plugins/client_side_pgp/csrf-skipped",
		SkipCSRFCheck: true,
		Handle: func(plugins.APIHost, string, http.ResponseWriter, *http.Request) {
			skippedCalled = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(skippedHandle.Unregister)

	post := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/"+path, nil)
		r = r.WithContext(context.WithValue(r.Context(), userContextKey, currentUser{User: user}))
		rec := httptest.NewRecorder()
		server.dispatchProtectedAPIPath(rec, r, path)
		return rec
	}

	rec := post("plugins/client_side_pgp/csrf-guarded")
	if guardedCalled {
		t.Fatal("mutation ran without a CSRF token")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("guarded POST status = %d, want 403", rec.Code)
	}

	post("plugins/client_side_pgp/csrf-skipped")
	if !skippedCalled {
		t.Fatal("SkipCSRFCheck route was blocked by the host gate")
	}

	// A request carrying the correct CSRF-derived token passes the gate.
	const cookieBase = "unit-test-session-base"
	token := server.csrfForBase(cookieBase)
	guardedPost := httptest.NewRequest(http.MethodPost, "/api/plugins/client_side_pgp/csrf-guarded", nil)
	guardedPost.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookieBase})
	guardedPost.Header.Set("X-CSRF-Token", token)
	guardedPost = guardedPost.WithContext(context.WithValue(guardedPost.Context(), userContextKey, currentUser{User: user}))
	rec = httptest.NewRecorder()
	server.dispatchProtectedAPIPath(rec, guardedPost, "plugins/client_side_pgp/csrf-guarded")
	if !guardedCalled {
		t.Fatalf("valid CSRF token was rejected with status %d", rec.Code)
	}
}
