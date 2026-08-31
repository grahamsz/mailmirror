package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

const legacyUpgradeExpiry = int64(4_102_444_800)

type legacyV21Fixture struct {
	UserID          int64
	Email           string
	AccountID       int64
	InboxMailboxID  int64
	SourceMailboxID int64
	BlobID          int64
	MessageID       int64
	NewMailEventID  int64
	SnoozeID        int64
	ReminderID      int64
	SubscriptionID  int64
	MoveMarkerID    int64
	RawSHA256       string
}

func newLegacyV21Fixture(userID int64, label string) legacyV21Fixture {
	base := userID * 1_000
	return legacyV21Fixture{
		UserID:          userID,
		Email:           label + "@upgrade.example.test",
		AccountID:       base + 1,
		InboxMailboxID:  base + 2,
		SourceMailboxID: base + 3,
		BlobID:          base + 4,
		MessageID:       base + 5,
		NewMailEventID:  base + 6,
		SnoozeID:        base + 7,
		ReminderID:      base + 8,
		SubscriptionID:  base + 9,
		MoveMarkerID:    base + 10,
		RawSHA256:       fmt.Sprintf("legacy-raw-sha-%d", userID),
	}
}

func TestUserSchemaUpgradeFromV21CombinedPreservesTenantData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rolltop.db")
	first := newLegacyV21Fixture(41, "first")
	second := newLegacyV21Fixture(92, "second")
	createLegacyV21UserDatabase(t, path, first, second)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	assertCurrentMigrationChecksums(t, ctx, db.DB(), currentUserMigrationSetsForUpgradeTest())
	assertCurrentMigrationChecksums(t, ctx, db.DB(), currentSystemMigrationSetsForUpgradeTest())
	assertLatestArrivalSchemaIsTenantScoped(t, ctx, db.DB())
	assertLegacyV21FixtureUpgraded(t, ctx, db.DB(), first)
	assertLegacyV21FixtureUpgraded(t, ctx, db.DB(), second)

	if _, err := db.GetMessageForUser(ctx, second.UserID, first.MessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second tenant reading first tenant message error = %v, want not found", err)
	}
	if _, err := db.GetMessageForUser(ctx, first.UserID, second.MessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first tenant reading second tenant message error = %v, want not found", err)
	}
}

func TestUserSchemaUpgradeFromV21SplitPrepareAndLazyOpen(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	systemPath := filepath.Join(dataDir, "rolltop.db")

	setup, err := OpenServer(systemPath, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	preparedUser, err := setup.CreateUser(ctx, "prepared@upgrade.example.test", "Prepared", "prepared-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	prepared := newLegacyV21Fixture(preparedUser.ID, "prepared")
	createLegacyV21UserDatabase(t, userDatabasePath(dataDir, prepared.UserID), prepared)

	server, err := OpenServer(systemPath, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.PrepareUserStores(ctx, nil); err != nil {
		t.Fatal(err)
	}
	preparedDB, err := server.UserDB(ctx, prepared.UserID)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentMigrationChecksums(t, ctx, preparedDB, currentUserMigrationSetsForUpgradeTest())
	assertLatestArrivalSchemaIsTenantScoped(t, ctx, preparedDB)
	assertLegacyV21FixtureUpgraded(t, ctx, preparedDB, prepared)

	lazyUser, err := server.CreateUser(ctx, "lazy@upgrade.example.test", "Lazy", "lazy-hash", false)
	if err != nil {
		t.Fatal(err)
	}
	lazy := newLegacyV21Fixture(lazyUser.ID, "lazy")
	createLegacyV21UserDatabase(t, userDatabasePath(dataDir, lazy.UserID), lazy)
	lazyStore, err := server.UserStore(ctx, lazy.UserID)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentMigrationChecksums(t, ctx, lazyStore.DB(), currentUserMigrationSetsForUpgradeTest())
	assertLatestArrivalSchemaIsTenantScoped(t, ctx, lazyStore.DB())
	assertLegacyV21FixtureUpgraded(t, ctx, lazyStore.DB(), lazy)

	assertCurrentMigrationChecksums(t, ctx, server.DB(), currentSystemMigrationSetsForUpgradeTest())
	var mailTable string
	if err := server.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'mail_accounts'`).Scan(&mailTable); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("system database mail_accounts lookup error = %v, want no table", err)
	}
	assertSplitDatabaseContainsOnlyUser(t, ctx, preparedDB, prepared.UserID, lazy.UserID)
	assertSplitDatabaseContainsOnlyUser(t, ctx, lazyStore.DB(), lazy.UserID, prepared.UserID)
	if _, err := server.GetMessageForUser(ctx, lazy.UserID, prepared.MessageID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lazy tenant reading prepared tenant message error = %v, want not found", err)
	}
}

func createLegacyV21UserDatabase(t *testing.T, path string, fixtures ...legacyV21Fixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	legacyStore := &Store{db: db, schema: schemaUser}
	ctx := context.Background()
	for _, set := range legacyUserMigrationSetsThroughV21() {
		if err := legacyStore.applyMigrationSet(ctx, set, nil); err != nil {
			_ = db.Close()
			t.Fatalf("apply legacy migration %s: %v", set.Version, err)
		}
	}
	assertCurrentMigrationChecksums(t, ctx, db, legacyUserMigrationSetsThroughV21())
	for _, fixture := range fixtures {
		seedLegacyV21Fixture(t, ctx, db, fixture)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedLegacyV21Fixture(t *testing.T, ctx context.Context, db *sql.DB, fixture legacyV21Fixture) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	fail := func(err error) {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(id, email, name, backup_email, password_hash, is_admin, date_locale, date_format, theme, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 'en-US', 'mdy', 'classic', 100, 101)`,
		fixture.UserID, fixture.Email, "Legacy "+fixture.Email, "backup-"+fixture.Email, "legacy-password-hash"); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mail_accounts
		(id, user_id, email, label, host, port, username, encrypted_password, use_tls, mailbox, sync_interval_minutes, created_at, updated_at)
		VALUES (?, ?, ?, 'Legacy account', 'imap.upgrade.example.test', 993, ?, ?, 1, '*', 11, 110, 111)`,
		fixture.AccountID, fixture.UserID, fixture.Email, fixture.Email, "encrypted-legacy-password-"+fixture.Email); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailboxes
		(id, user_id, account_id, name, role, icon, show_in_sidebar, show_in_all_mail, include_in_search,
		 uidvalidity, last_uid, remote_message_count, remote_unread_count, remote_uid_next, status_checked_at, created_at, updated_at)
		VALUES (?, ?, ?, 'INBOX', 'inbox', 'inbox', 1, 1, 1, 777, 52, 4, 2, 53, 120, 121, 122),
		       (?, ?, ?, 'Spam', 'junk', 'report', 1, 0, 1, 888, 61, 3, 1, 62, 123, 124, 125)`,
		fixture.InboxMailboxID, fixture.UserID, fixture.AccountID,
		fixture.SourceMailboxID, fixture.UserID, fixture.AccountID); err != nil {
		fail(err)
	}
	blobPath := fmt.Sprintf("users/%d/blobs/legacy-message.eml", fixture.UserID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (id, user_id, kind, path, sha256, size, created_at)
		VALUES (?, ?, 'message', ?, ?, 321, 130)`, fixture.BlobID, fixture.UserID, blobPath, fixture.RawSHA256); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages
		(id, user_id, account_id, mailbox_id, blob_id, message_id_header, thread_key, subject, from_addr, to_addr,
		 date_unix, internal_date_unix, uid, size, blob_path, body_text, is_read, is_starred, attachment_indexed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 140, 141, 61, 321, ?, 'Legacy body survives', 1, 1, 142, 142, 143)`,
		fixture.MessageID, fixture.UserID, fixture.AccountID, fixture.SourceMailboxID, fixture.BlobID,
		fmt.Sprintf("<legacy-%d@example.test>", fixture.UserID), fmt.Sprintf("legacy-thread-%d", fixture.UserID),
		"Legacy subject "+fixture.Email, "sender-"+fixture.Email, fixture.Email, blobPath); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO locations (user_id, message_id, mailbox_id, uid, created_at)
		VALUES (?, ?, ?, 61, 144)`, fixture.UserID, fixture.MessageID, fixture.SourceMailboxID); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO new_mail_events
		(id, user_id, message_id, from_addr, subject, created_at)
		VALUES (?, ?, ?, ?, ?, 150)`, fixture.NewMailEventID, fixture.UserID, fixture.MessageID,
		"sender-"+fixture.Email, "Legacy subject "+fixture.Email); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_snoozes
		(id, user_id, message_id, thread_key, generation, snoozed_until, reminded_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 3, 2000000000, 151, 152, 153)`, fixture.SnoozeID, fixture.UserID,
		fixture.MessageID, fmt.Sprintf("legacy-thread-%d", fixture.UserID)); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO snooze_reminder_events
		(id, user_id, message_id, snooze_generation, from_addr, subject, due_at, created_at)
		VALUES (?, ?, ?, 3, ?, ?, 2000000000, 154)`, fixture.ReminderID, fixture.UserID, fixture.MessageID,
		"sender-"+fixture.Email, "Legacy subject "+fixture.Email); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_push_subscriptions
		(id, user_id, endpoint, p256dh, auth, user_agent, last_new_mail_event_id, last_snooze_reminder_event_id,
		 created_at, updated_at, last_seen_at)
		VALUES (?, ?, ?, 'legacy-p256dh', 'legacy-auth', 'legacy-agent', ?, ?, 160, 161, 162)`,
		fixture.SubscriptionID, fixture.UserID, "https://push.example.test/"+fixture.Email,
		fixture.NewMailEventID, fixture.ReminderID); err != nil {
		fail(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_move_notifications
		(id, user_id, account_id, destination_mailbox_id, raw_sha256, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, 170, ?)`, fixture.MoveMarkerID, fixture.UserID, fixture.AccountID,
		fixture.InboxMailboxID, fixture.RawSHA256, legacyUpgradeExpiry); err != nil {
		fail(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertLegacyV21FixtureUpgraded(t *testing.T, ctx context.Context, db *sql.DB, fixture legacyV21Fixture) {
	t.Helper()
	var email, backupEmail string
	if err := db.QueryRowContext(ctx, `SELECT email, backup_email FROM users WHERE id = ?`, fixture.UserID).Scan(&email, &backupEmail); err != nil {
		t.Fatal(err)
	}
	if email != fixture.Email || backupEmail != "backup-"+fixture.Email {
		t.Fatalf("upgraded user = email %q backup %q", email, backupEmail)
	}
	var encryptedPassword string
	if err := db.QueryRowContext(ctx, `SELECT encrypted_password FROM mail_accounts WHERE user_id = ? AND id = ?`, fixture.UserID, fixture.AccountID).Scan(&encryptedPassword); err != nil {
		t.Fatal(err)
	}
	if encryptedPassword != "encrypted-legacy-password-"+fixture.Email {
		t.Fatal("upgraded account did not preserve encrypted credentials")
	}
	var inboxUIDValidity, sourceUIDValidity, sourceLastUID, sourceUIDNext int64
	if err := db.QueryRowContext(ctx, `SELECT inbox.uidvalidity, source.uidvalidity, source.last_uid, source.remote_uid_next
		FROM mailboxes AS inbox
		JOIN mailboxes AS source ON source.user_id = inbox.user_id AND source.id = ?
		WHERE inbox.user_id = ? AND inbox.id = ? AND inbox.account_id = ?`,
		fixture.SourceMailboxID, fixture.UserID, fixture.InboxMailboxID, fixture.AccountID).
		Scan(&inboxUIDValidity, &sourceUIDValidity, &sourceLastUID, &sourceUIDNext); err != nil {
		t.Fatal(err)
	}
	if inboxUIDValidity != 777 || sourceUIDValidity != 888 || sourceLastUID != 61 || sourceUIDNext != 62 {
		t.Fatalf("upgraded mailbox state = inbox generation %d source generation/last/next %d/%d/%d", inboxUIDValidity, sourceUIDValidity, sourceLastUID, sourceUIDNext)
	}
	var blobPath, blobSHA string
	var blobSize int64
	if err := db.QueryRowContext(ctx, `SELECT path, sha256, size FROM blobs WHERE user_id = ? AND id = ?`, fixture.UserID, fixture.BlobID).Scan(&blobPath, &blobSHA, &blobSize); err != nil {
		t.Fatal(err)
	}
	if blobSHA != fixture.RawSHA256 || blobSize != 321 || blobPath == "" {
		t.Fatalf("upgraded blob = path %q sha %q size %d", blobPath, blobSHA, blobSize)
	}
	var subject, body, canonicalSHA, messageIDHash string
	var uidValidity, importCompletedAt int64
	var isRead, isStarred int
	if err := db.QueryRowContext(ctx, `SELECT subject, body_text, is_read, is_starred, canonical_sha256, message_id_hash, uid_validity, import_completed_at
		FROM messages WHERE user_id = ? AND id = ?`, fixture.UserID, fixture.MessageID).
		Scan(&subject, &body, &isRead, &isStarred, &canonicalSHA, &messageIDHash, &uidValidity, &importCompletedAt); err != nil {
		t.Fatal(err)
	}
	if subject != "Legacy subject "+fixture.Email || body != "Legacy body survives" || isRead != 1 || isStarred != 1 {
		t.Fatalf("upgraded message = subject %q body %q read %d starred %d", subject, body, isRead, isStarred)
	}
	if canonicalSHA != "" || messageIDHash != "" || uidValidity != 0 {
		t.Fatalf("legacy message proof defaults = canonical %q message-id %q uidvalidity %d, want empty/empty/0", canonicalSHA, messageIDHash, uidValidity)
	}
	if importCompletedAt != 143 {
		t.Fatalf("legacy message import completion = %d, want prior updated_at 143", importCompletedAt)
	}
	var locationCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM locations
		WHERE user_id = ? AND message_id = ? AND mailbox_id = ? AND uid = 61`,
		fixture.UserID, fixture.MessageID, fixture.SourceMailboxID).Scan(&locationCount); err != nil {
		t.Fatal(err)
	}
	if locationCount != 1 {
		t.Fatalf("upgraded message location count = %d", locationCount)
	}
	var generation, remindedAt int64
	if err := db.QueryRowContext(ctx, `SELECT generation, reminded_at FROM message_snoozes
		WHERE user_id = ? AND id = ? AND message_id = ?`, fixture.UserID, fixture.SnoozeID, fixture.MessageID).
		Scan(&generation, &remindedAt); err != nil {
		t.Fatal(err)
	}
	if generation != 3 || remindedAt != 151 {
		t.Fatalf("upgraded snooze = generation %d reminded %d", generation, remindedAt)
	}
	var reminderGeneration int64
	if err := db.QueryRowContext(ctx, `SELECT snooze_generation FROM snooze_reminder_events
		WHERE user_id = ? AND id = ? AND message_id = ?`, fixture.UserID, fixture.ReminderID, fixture.MessageID).
		Scan(&reminderGeneration); err != nil {
		t.Fatal(err)
	}
	if reminderGeneration != 3 {
		t.Fatalf("upgraded reminder generation = %d", reminderGeneration)
	}
	var eventUserID, eventMessageID int64
	var eventFrom, eventSubject string
	if err := db.QueryRowContext(ctx, `SELECT user_id, message_id, from_addr, subject FROM new_mail_events WHERE id = ?`, fixture.NewMailEventID).
		Scan(&eventUserID, &eventMessageID, &eventFrom, &eventSubject); err != nil {
		t.Fatal(err)
	}
	if eventUserID != fixture.UserID || eventMessageID != fixture.MessageID || eventFrom != "sender-"+fixture.Email || eventSubject != "Legacy subject "+fixture.Email {
		t.Fatalf("upgraded new-mail event = user %d message %d from %q subject %q", eventUserID, eventMessageID, eventFrom, eventSubject)
	}
	var newMailCursor, snoozeCursor int64
	if err := db.QueryRowContext(ctx, `SELECT last_new_mail_event_id, last_snooze_reminder_event_id
		FROM web_push_subscriptions WHERE user_id = ? AND id = ?`, fixture.UserID, fixture.SubscriptionID).
		Scan(&newMailCursor, &snoozeCursor); err != nil {
		t.Fatal(err)
	}
	if newMailCursor != fixture.NewMailEventID || snoozeCursor != fixture.ReminderID {
		t.Fatalf("upgraded push cursors = new %d snooze %d", newMailCursor, snoozeCursor)
	}
	var markerUserID, markerAccountID, markerMailboxID, markerMessageID, markerExpiry int64
	var markerSHA string
	if err := db.QueryRowContext(ctx, `SELECT user_id, account_id, destination_mailbox_id, raw_sha256,
		COALESCE(consumed_message_id, 0), expires_at FROM pending_move_notifications WHERE id = ?`, fixture.MoveMarkerID).
		Scan(&markerUserID, &markerAccountID, &markerMailboxID, &markerSHA, &markerMessageID, &markerExpiry); err != nil {
		t.Fatal(err)
	}
	if markerUserID != fixture.UserID || markerAccountID != fixture.AccountID || markerMailboxID != fixture.InboxMailboxID || markerSHA != fixture.RawSHA256 || markerMessageID != 0 || markerExpiry != legacyUpgradeExpiry {
		t.Fatalf("upgraded legacy move marker = owner/account/mailbox %d/%d/%d raw %q consumed %d expiry %d", markerUserID, markerAccountID, markerMailboxID, markerSHA, markerMessageID, markerExpiry)
	}
	var transferUserID, sourceAccountID, destinationAccountID, destinationMailboxID, legacyMarkerID int64
	var operation, state, rawSHA, dispatchOwner string
	var dispatchAttempt, dispatchFinishedAt, snapshotValidity, snapshotUIDNext int64
	if err := db.QueryRowContext(ctx, `SELECT user_id, source_account_id, destination_account_id, destination_mailbox_id,
		operation_kind, state, raw_sha256, legacy_marker_id, dispatch_owner, dispatch_attempt, dispatch_finished_at,
		destination_snapshot_uid_validity, destination_snapshot_uid_next
		FROM message_transfers WHERE user_id = ? AND legacy_marker_id = ?`, fixture.UserID, fixture.MoveMarkerID).
		Scan(&transferUserID, &sourceAccountID, &destinationAccountID, &destinationMailboxID, &operation, &state,
			&rawSHA, &legacyMarkerID, &dispatchOwner, &dispatchAttempt, &dispatchFinishedAt, &snapshotValidity, &snapshotUIDNext); err != nil {
		t.Fatal(err)
	}
	if transferUserID != fixture.UserID || sourceAccountID != fixture.AccountID || destinationAccountID != fixture.AccountID || destinationMailboxID != fixture.InboxMailboxID || legacyMarkerID != fixture.MoveMarkerID {
		t.Fatalf("migrated transfer scope/source/destination = user %d source %d destination %d/%d marker %d", transferUserID, sourceAccountID, destinationAccountID, destinationMailboxID, legacyMarkerID)
	}
	if operation != "move" || state != "succeeded" || rawSHA != fixture.RawSHA256 {
		t.Fatalf("migrated transfer = operation %q state %q raw %q", operation, state, rawSHA)
	}
	if dispatchOwner != "" || dispatchAttempt != 0 || dispatchFinishedAt != 0 || snapshotValidity != 0 || snapshotUIDNext != 0 {
		t.Fatalf("migrated dispatch defaults = owner %q attempt %d finished %d snapshot %d/%d", dispatchOwner, dispatchAttempt, dispatchFinishedAt, snapshotValidity, snapshotUIDNext)
	}
}

func assertLatestArrivalSchemaIsTenantScoped(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	tables := map[string][]string{
		"message_transfers":                            {"user_id", "dispatch_owner", "dispatch_attempt", "dispatch_finished_at", "destination_snapshot_uid_validity", "destination_snapshot_uid_next"},
		"expunged_message_fingerprints":                {"user_id", "source_uid_validity", "canonical_sha256", "message_id_hash"},
		"pending_inbox_arrivals":                       {"user_id", "classification", "available_at", "matched_transfer_id", "matched_expunged_id"},
		"mailbox_generation_rebuilds":                  {"user_id", "target_uid_validity"},
		"mailbox_generation_rebuild_messages":          {"user_id", "target_uid_validity", "canonical_sha256", "message_id_hash"},
		"mailbox_generation_rebuild_snooze_events":     {"user_id", "original_event_id"},
		"mailbox_generation_rebuild_unsubscribe_sends": {"user_id", "original_send_id"},
		"mailbox_generation_blob_cleanup":              {"user_id", "blob_id", "blob_path"},
		"mailbox_generation_rebuild_inbox_arrivals":    {"user_id", "original_arrival_id", "classification"},
		"blob_cleanup_queue":                           {"user_id", "blob_id", "blob_path", "blob_sha256", "blob_size", "blob_created_at"},
		"messages":                                     {"user_id", "import_completed_at"},
	}
	for table, columns := range tables {
		assertTenantTableColumns(t, ctx, db, table, columns)
	}
}

func assertTenantTableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string, required []string) {
	t.Helper()
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	columns := make(map[string]int)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		columns[name] = notNull
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatalf("latest migration did not create table %s", table)
	}
	for _, column := range required {
		if _, ok := columns[column]; !ok {
			t.Fatalf("latest migration did not create %s.%s", table, column)
		}
	}
	if columns["user_id"] != 1 {
		t.Fatalf("%s.user_id is not NOT NULL", table)
	}
	fkRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_list(`+table+`)`)
	if err != nil {
		t.Fatal(err)
	}
	defer fkRows.Close()
	hasUserOwner := false
	for fkRows.Next() {
		var id, seq int
		var referencedTable, from, to, onUpdate, onDelete, match string
		if err := fkRows.Scan(&id, &seq, &referencedTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if referencedTable == "users" && from == "user_id" && to == "id" {
			hasUserOwner = true
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatal(err)
	}
	if !hasUserOwner {
		t.Fatalf("%s.user_id does not reference users(id)", table)
	}
}

func assertSplitDatabaseContainsOnlyUser(t *testing.T, ctx context.Context, db *sql.DB, ownerID, otherID int64) {
	t.Helper()
	var ownerCount, otherCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, ownerID).Scan(&ownerCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, otherID).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if ownerCount != 1 || otherCount != 0 {
		t.Fatalf("split user mirror counts = owner %d other %d", ownerCount, otherCount)
	}
	for _, table := range []string{"mail_accounts", "mailboxes", "blobs", "messages", "new_mail_events", "message_snoozes", "snooze_reminder_events", "web_push_subscriptions", "pending_move_notifications", "message_transfers", "outbox_jobs", "outbox_attempts"} {
		var foreignRows int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE user_id <> ?`, ownerID).Scan(&foreignRows); err != nil {
			t.Fatal(err)
		}
		if foreignRows != 0 {
			t.Fatalf("split database table %s contains %d non-owner rows", table, foreignRows)
		}
	}
}

func assertCurrentMigrationChecksums(t *testing.T, ctx context.Context, db *sql.DB, sets []migrationSet) {
	t.Helper()
	if len(sets) == 0 {
		t.Fatal("no migration sets supplied")
	}
	scope := sets[0].Scope
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE scope = ?`, scope).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(sets) {
		t.Fatalf("%s migration count = %d, want %d", scope, count, len(sets))
	}
	for _, set := range sets {
		var checksum string
		if err := db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE scope = ? AND version = ?`, set.Scope, set.Version).Scan(&checksum); err != nil {
			t.Fatalf("read migration %s/%s: %v", set.Scope, set.Version, err)
		}
		if want := migrationChecksum(set); checksum != want {
			t.Fatalf("migration %s/%s checksum = %q, want %q", set.Scope, set.Version, checksum, want)
		}
	}
}

func legacyUserMigrationSetsThroughV21() []migrationSet {
	return []migrationSet{
		userMigrationSet(),
		userBackupEmailMigrationSet(),
		userSearchPreferencesMigrationSet(),
		userSearchRankingMigrationSet(),
		userSenderStatsMigrationSet(),
		userSenderStatsTableMigrationSet(),
		userIdentityMailboxMigrationSet(),
		userIdentityIMAPMigrationSet(),
		userMessageListIndexMigrationSet(),
		userRemoteImageCacheMigrationSet(),
		userWebPushSubscriptionMigrationSet(),
		userSyncRunLatestMessageMigrationSet(),
		userNewMailEventMigrationSet(),
		userWebPushDeliveryCursorMigrationSet(),
		userSnoozeMigrationSet(),
		userSwipePreferencesMigrationSet(),
		userJunkMailboxRoleMigrationSet(),
		userPendingMoveNotificationMigrationSet(),
	}
}

func currentUserMigrationSetsForUpgradeTest() []migrationSet {
	sets := legacyUserMigrationSetsThroughV21()
	return append(sets,
		userInboxArrivalClassificationMigrationSet(),
		userMailboxGenerationArrivalJournalMigrationSet(),
		userTransferDispatchRecoveryMigrationSet(),
		userBlobCleanupQueueMigrationSet(),
		userMailboxGenerationArrivalFloorMigrationSet(),
		userMessageImportCompletionMigrationSet(),
		userSearchProgressIndexMigrationSet(),
		userOutboxMigrationSet(),
	)
}

func currentSystemMigrationSetsForUpgradeTest() []migrationSet {
	return []migrationSet{
		systemMigrationSet(),
		systemUserSearchPreferencesMigrationSet(),
		systemUserSearchRankingMigrationSet(),
		systemPasswordResetMigrationSet(),
	}
}

func userDatabasePath(dataDir string, userID int64) string {
	return filepath.Join(dataDir, "users", fmt.Sprintf("%d", userID), databaseFilename)
}
