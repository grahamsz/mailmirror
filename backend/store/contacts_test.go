// File overview: Tests for contact, identity, and icon persistence.

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestContactsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "contacts@example.test", "Contacts", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-contacts@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Shared Name",
		Emails:      []ContactEmail{{Email: "shared@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, other.ID, Contact{
		DisplayName: "Other Shared",
		Emails:      []ContactEmail{{Email: "shared@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetContactByEmailForUser(ctx, user.ID, "shared@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != contact.ID || found.DisplayName != "Shared Name" {
		t.Fatalf("found = %+v", found)
	}
	items, err := db.AutocompleteContactsForUser(ctx, user.ID, "shared", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Shared Name" {
		t.Fatalf("autocomplete = %+v", items)
	}
}

func TestContactAutocompleteIncludesOnlyCurrentUsersRecentCorrespondents(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "recent@example.test", "Recent", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-recent@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	createNewMailEventMessage(t, ctx, db, user, 11, "Alice Sender <alice@example.test>", "Hello")
	createNewMailEventMessage(t, ctx, db, user, 12, "Alice Sender <alice@example.test>", "Again")
	createNewMailEventMessage(t, ctx, db, other, 21, "Alice Secret <alice-secret@example.test>", "Other tenant")

	items, err := db.AutocompleteContactsForUser(ctx, user.ID, "alice", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Email != "alice@example.test" || items[0].Label != "Recent" {
		t.Fatalf("recent autocomplete = %+v", items)
	}
}

func TestContactInteractionsAreExactAndScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "interaction-owner@example.test", "Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "interaction-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Alice",
		Emails:      []ContactEmail{{Email: "alice@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	received := createNewMailEventMessage(t, ctx, db, user, 11, "Alice <alice@example.test>", "Received note")
	sent := createNewMailEventMessage(t, ctx, db, user, 12, "Owner <interaction-owner@example.test>", "Sent note")
	createNewMailEventMessage(t, ctx, db, user, 13, "Not Alice <notalice@example.test>", "Substring should not match")
	createNewMailEventMessage(t, ctx, db, other, 21, "Alice <alice@example.test>", "Other tenant")
	now := time.Now().UTC()
	if _, err := db.mustDataDB(ctx, user.ID).ExecContext(ctx, `UPDATE messages
		SET from_addr = ?, to_addr = ?, date_unix = ? WHERE user_id = ? AND id = ?`,
		"Owner <interaction-owner@example.test>", "Alice <alice@example.test>", now.Unix(), user.ID, sent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.mustDataDB(ctx, user.ID).ExecContext(ctx, `UPDATE messages
		SET date_unix = ? WHERE user_id = ? AND id = ?`, now.Add(-time.Hour).Unix(), user.ID, received.ID); err != nil {
		t.Fatal(err)
	}

	items, err := db.ListContactInteractionsForUser(ctx, user.ID, contact.ID, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("interactions = %+v, want sent and received only", items)
	}
	if items[0].MessageID != sent.ID || items[0].Direction != "sent" {
		t.Fatalf("first interaction = %+v, want newest sent message", items[0])
	}
	if items[1].MessageID != received.ID || items[1].Direction != "received" {
		t.Fatalf("second interaction = %+v, want received message", items[1])
	}
	if _, err := db.ListContactInteractionsForUser(ctx, other.ID, contact.ID, 6); !IsNotFound(err) {
		t.Fatalf("other user contact lookup err = %v, want not found", err)
	}
}

func TestContactIconsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser(ctx, "icon@example.test", "Icon", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser(ctx, "other-icon@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := db.CreateContact(ctx, user.ID, Contact{
		DisplayName: "Icon Contact",
		Emails:      []ContactEmail{{Email: "icon@example.test", IsPrimary: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, BlobRecord{
		UserID: user.ID,
		Kind:   "contact_icon",
		Path:   "users/1/blobs/contacts/1/icons/icon.png",
		SHA256: "hash",
		Size:   4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetContactIcon(ctx, user.ID, contact.ID, blob.ID, "image/png", "icon.png", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetContactIconForUser(ctx, other.ID, contact.ID); !IsNotFound(err) {
		t.Fatalf("other user icon err = %v", err)
	}
	if _, err := db.GetContactIconByEmailForUser(ctx, other.ID, "icon@example.test"); !IsNotFound(err) {
		t.Fatalf("other user email icon err = %v", err)
	}
	icons, err := db.ListContactIconsByEmailsForUser(ctx, user.ID, []string{"Icon <icon@example.test>", "missing@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(icons) != 1 || icons["icon@example.test"].ContactID != contact.ID {
		t.Fatalf("owner batch icons = %+v", icons)
	}
	otherIcons, err := db.ListContactIconsByEmailsForUser(ctx, other.ID, []string{"icon@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(otherIcons) != 0 {
		t.Fatalf("other user batch icons = %+v", otherIcons)
	}
}
