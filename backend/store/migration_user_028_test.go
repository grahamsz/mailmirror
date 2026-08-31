package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestSearchProgressMigrationAddsPartialCoveringIndex(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE mailboxes (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		account_id INTEGER NOT NULL,
		name TEXT NOT NULL
	);
	CREATE TABLE messages (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		mailbox_id INTEGER NOT NULL,
		attachment_indexed_at INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO mailboxes(id, user_id, account_id, name) VALUES
		(10, 1, 100, 'INBOX'),
		(20, 2, 200, 'Archive')`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range userSearchProgressIndexMigrationSet().Statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var purged int
	if err := db.QueryRowContext(ctx, `SELECT search_index_purged FROM mailboxes WHERE id = 10`).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("backfilled search_index_purged=%d, want 0", purged)
	}
	var known int
	if err := db.QueryRowContext(ctx, `SELECT search_index_state_known FROM mailboxes WHERE id = 10`).Scan(&known); err != nil {
		t.Fatal(err)
	}
	if known != 0 {
		t.Fatalf("existing mailbox search_index_state_known=%d, want 0", known)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mailboxes(id, user_id, account_id, name) VALUES (30, 1, 100, 'New')`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT search_index_purged FROM mailboxes WHERE id = 30`).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged != 0 {
		t.Fatalf("default search_index_purged=%d, want 0", purged)
	}
	if err := db.QueryRowContext(ctx, `SELECT search_index_state_known FROM mailboxes WHERE id = 30`).Scan(&known); err != nil {
		t.Fatal(err)
	}
	if known != 1 {
		t.Fatalf("new mailbox search_index_state_known=%d, want 1", known)
	}
	var definition string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_messages_user_mailbox_search_committed'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(definition), " "))
	if !strings.Contains(normalized, "on messages(user_id, mailbox_id, id)") ||
		!strings.Contains(normalized, "where attachment_indexed_at > 0") {
		t.Fatalf("search progress index definition = %q", definition)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages(id, user_id, mailbox_id, attachment_indexed_at) VALUES
		(1, 1, 10, 100), (2, 1, 10, 0), (3, 2, 20, 100)`); err != nil {
		t.Fatal(err)
	}
	planRows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN
		SELECT mailbox_id, COUNT(*) FROM messages
		WHERE user_id = ? AND attachment_indexed_at > 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
		GROUP BY mailbox_id`, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer planRows.Close()
	var plan []string
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		if err := planRows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := planRows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, " "), "idx_messages_user_mailbox_search_committed") {
		t.Fatalf("search progress query plan = %v, want partial index", plan)
	}
}

func TestUser029IsLatestRegisteredUserMigration(t *testing.T) {
	sets := currentUserMigrationSetsForUpgradeTest()
	if len(sets) < 2 {
		t.Fatalf("registered user migrations=%d, want at least 2", len(sets))
	}
	latest := sets[len(sets)-1]
	predecessor := sets[len(sets)-2]
	if latest.Version != UserSchemaVersion029 {
		t.Fatalf("latest user migration=%q, want %q", latest.Version, UserSchemaVersion029)
	}
	if predecessor.Version != UserSchemaVersion028 {
		t.Fatalf("user-029 predecessor=%q, want %q", predecessor.Version, UserSchemaVersion028)
	}
}
