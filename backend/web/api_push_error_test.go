// File overview: Tests that the Web Push subscription endpoint separates a
// subscription the browser sent wrong from a storage failure, so an outage is
// never reported to the client as a client mistake.

package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rolltop/backend/store"
	"rolltop/internal/testlog"
)

func newPushSubscriptionRequest(t *testing.T, server *Server, user store.User, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/push/subscription", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, currentUser{User: user}))
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "push-test-base"})
	req.Header.Set("X-CSRF-Token", server.csrfForBase("push-test-base"))
	return req
}

func newPushTestServer(t *testing.T) (*Server, store.User) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(context.Background(), "push@example.test", "Push", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: db, masterKey: make([]byte, 32)}, user
}

func TestPushSubscribeRejectsInvalidSubscriptionWithoutLogging(t *testing.T) {
	logs := testlog.Capture(t)
	server, user := newPushTestServer(t)
	rec := httptest.NewRecorder()

	server.apiPushSubscription(rec, newPushSubscriptionRequest(t, server, user,
		`{"endpoint":"http://localhost/push","keys":{"p256dh":"nope","auth":"nope"}}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if logs.Len() != 0 {
		t.Fatalf("a rejected subscription was logged as a server failure: %q", logs.String())
	}
}

// A store failure used to be reported as 400 with no log line, which hid an
// outage behind a client error and left the operator nothing to go on.
func TestPushSubscribeReportsStoreFailureAsServerError(t *testing.T) {
	logs := testlog.Capture(t)
	server, user := newPushTestServer(t)
	// Closing the store turns the upsert into an infrastructure failure while
	// the subscription itself stays valid.
	if err := server.store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	server.apiPushSubscription(rec, newPushSubscriptionRequest(t, server, user, validPushSubscriptionBody(t)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if logs.Len() == 0 {
		t.Fatal("store failure during subscribe was not logged")
	}
}

func validPushSubscriptionBody(t *testing.T) string {
	t.Helper()
	// A real P-256 point and a 16-byte auth secret: the store checks the key
	// material lies on the curve before it touches the database.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := elliptic.Marshal(elliptic.P256(), key.X, key.Y)
	body, err := json.Marshal(map[string]any{
		"endpoint": "https://push.example.test/subscription/abc",
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(publicKey),
			"auth":   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
