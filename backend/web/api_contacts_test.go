package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"rolltop/backend/store"
)

func TestContactInteractionsEndpointUsesCurrentUser(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, err := db.CreateUser(ctx, "contact-profile-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "contact-profile-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, owner.ID, store.Contact{
		DisplayName: "Profile Contact",
		Emails:      []store.ContactEmail{{Email: "profile@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db}

	foreign := httptest.NewRecorder()
	server.apiContactInteractions(foreign, httptest.NewRequest(http.MethodGet, "/api/contacts/1/interactions", nil), currentUser{User: other}, contact.ID)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status = %d body=%s, want 404", foreign.Code, foreign.Body.String())
	}

	owned := httptest.NewRecorder()
	server.apiContactInteractions(owned, httptest.NewRequest(http.MethodGet, "/api/contacts/1/interactions", nil), currentUser{User: owner}, contact.ID)
	if owned.Code != http.StatusOK {
		t.Fatalf("owner status = %d body=%s", owned.Code, owned.Body.String())
	}
	var payload struct {
		Interactions []apiContactInteraction `json:"interactions"`
	}
	if err := json.NewDecoder(owned.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Interactions) != 0 {
		t.Fatalf("interactions = %+v, want empty", payload.Interactions)
	}
}
