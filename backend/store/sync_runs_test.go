package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTouchSyncRunRefreshesUpdatedAtOnlyForRunningRuns(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	user, err := db.CreateUser(ctx, "touch@example.test", "Touch", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, MailAccount{
		UserID: user.ID, Email: "touch@example.test", Host: "imap.example.test", Port: 993,
		Username: "touch", EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateSyncRun(ctx, user.ID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.GetSyncRunForUser(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchSyncRun(ctx, user.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetSyncRunForUser(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt.Before(before.UpdatedAt) {
		t.Fatalf("updated_at went backwards: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if after.Status != "running" {
		t.Fatalf("status changed to %q", after.Status)
	}

	if err := db.InterruptSyncRunForUser(ctx, user.ID, run.ID, "test"); err != nil {
		t.Fatal(err)
	}
	interruptedAt, err := db.GetSyncRunForUser(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchSyncRun(ctx, user.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	final, err := db.GetSyncRunForUser(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.UpdatedAt.Equal(interruptedAt.UpdatedAt) || final.FinishedAt.Unix() != interruptedAt.FinishedAt.Unix() {
		t.Fatalf("touch modified a terminal run: %v vs %v", final, interruptedAt)
	}
}

func TestCreateInitialAdminIfNoneIsAtomicAndSingleShot(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	admin, err := db.CreateInitialAdminIfNone(ctx, "Founder@Example.test", "Founder", "argon2hash")
	if err != nil {
		t.Fatal(err)
	}
	if !admin.IsAdmin || admin.Email != "founder@example.test" {
		t.Fatalf("initial admin = %+v", admin)
	}

	if _, err := db.CreateInitialAdminIfNone(ctx, "second@example.test", "Second", "argon2hash"); err != ErrSetupAlreadyComplete {
		t.Fatalf("second initial admin error = %v, want ErrSetupAlreadyComplete", err)
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %d, want only the first admin", len(users))
	}
}
