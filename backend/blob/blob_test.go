// File overview: Tests for user-scoped blob path behavior.

package blob

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveRawMessageUsesUserDataDirectoryLayout(t *testing.T) {
	store := New(t.TempDir())
	saved, err := store.SaveRawMessage(42, 7, "INBOX", 99, []byte("From: a@example.test\r\n\r\nhello"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved.Path, "users/42/blobs/accounts/7/mailboxes/INBOX/") {
		t.Fatalf("blob path = %q", saved.Path)
	}
	if f, err := store.OpenUserBlob(42, saved.Path); err != nil {
		t.Fatalf("open saved blob: %v", err)
	} else {
		_ = f.Close()
	}
	if _, err := store.OpenUserBlob(43, saved.Path); err == nil {
		t.Fatal("other user opened blob")
	}
}

func TestSaveOutboxMessageIsDurableAndTenantScoped(t *testing.T) {
	store := New(t.TempDir())
	raw := []byte("From: sender@example.test\r\nTo: recipient@example.test\r\n\r\nqueued")
	saved, err := store.SaveOutboxMessage(42, "stable/submission key", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(saved.Path, "users/42/blobs/outbox/") || !strings.HasSuffix(saved.Path, ".eml") {
		t.Fatalf("outbox path=%q", saved.Path)
	}
	file, err := store.OpenUserBlob(42, saved.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(file)
	info, statErr := file.Stat()
	_ = file.Close()
	if readErr != nil || statErr != nil {
		t.Fatalf("read=%v stat=%v", readErr, statErr)
	}
	if string(got) != string(raw) {
		t.Fatalf("saved raw=%q, want %q", got, raw)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("outbox permissions=%#o, want owner-only", info.Mode().Perm())
	}
	if _, err := store.OpenUserBlob(43, saved.Path); err == nil {
		t.Fatal("other user opened queued message")
	}
	// Idempotent retries replace the same immutable submission path cleanly.
	again, err := store.SaveOutboxMessage(42, "stable/submission key", raw)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != saved.Path || again.SHA256 != saved.SHA256 {
		t.Fatalf("idempotent save=%+v, want path/hash from %+v", again, saved)
	}
	if _, err := os.Stat(filepath.Join(store.Root, saved.Path)); err != nil {
		t.Fatalf("durable outbox file missing: %v", err)
	}
}

func TestOpenUserBlobRejectsOldLayoutUserPath(t *testing.T) {
	oldPath := "blobs/users/9/accounts/1/mailboxes/INBOX/uid-1.eml"
	if userBlobPathAllowed(9, oldPath) {
		t.Fatalf("old layout path was allowed: %s", oldPath)
	}
	store := New(t.TempDir())
	if _, err := store.OpenUserBlob(9, oldPath); err == nil {
		t.Fatalf("opened old layout path: %s", oldPath)
	}
}
