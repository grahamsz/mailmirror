// File overview: SQLite migration runner shared by system and user stores.
// The concrete 001 schemas live in migration_system_001.go and
// migration_user_001.go. This file owns the flow around those schemas: choose
// which schema family applies to the opened SQLite handle, report progress to
// startup callers, record applied schema versions in schema_migrations, and
// reject checksum drift so a migration cannot silently change after use.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"rolltop/backend/plugins"
)

const (
	SystemSchemaVersion    = "system-001"
	SystemSchemaVersion002 = "system-002"
	SystemSchemaVersion003 = "system-003"
	SystemSchemaVersion004 = "system-004"
	UserSchemaVersion      = "user-001"
	UserSchemaVersion002   = "user-002"
	UserSchemaVersion003   = "user-003"
	UserSchemaVersion004   = "user-004"
	UserSchemaVersion005   = "user-005"
	UserSchemaVersion006   = "user-006"
	UserSchemaVersion007   = "user-007"
	UserSchemaVersion008   = "user-008"
	UserSchemaVersion009   = "user-009"
	UserSchemaVersion010   = "user-010"
	UserSchemaVersion011   = "user-011"
	UserSchemaVersion012   = "user-012"
	UserSchemaVersion013   = "user-013"
	UserSchemaVersion014   = "user-014"
	UserSchemaVersion015   = "user-015"
	UserSchemaVersion016   = "user-016"
	UserSchemaVersion017   = "user-017"
	UserSchemaVersion018   = "user-018"
	UserSchemaVersion019   = "user-019"
	UserSchemaVersion020   = "user-020"
	UserSchemaVersion021   = "user-021"
	UserSchemaVersion022   = "user-022"
	UserSchemaVersion023   = "user-023"
	UserSchemaVersion024   = "user-024"
	UserSchemaVersion025   = "user-025"
	UserSchemaVersion026   = "user-026"
	UserSchemaVersion027   = "user-027"
	UserSchemaVersion028   = "user-028"
	UserSchemaVersion029   = "user-029"
)

// MigrationProgress is emitted while Store.OpenServerWithProgress and
// PrepareUserStores apply schema work. cmd/rolltop turns these fields into
// the startup page and /api/startup response.
type MigrationProgress struct {
	Scope     string `json:"scope"`
	Migration string `json:"migration"`
	Step      string `json:"step"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
}

// MigrationReporter receives best-effort migration progress. It must not log
// secrets or message contents because startup status is exposed over HTTP.
type MigrationReporter func(MigrationProgress)

type schemaKind int

const (
	schemaCombined schemaKind = iota
	schemaSystem
	schemaUser
)

// migrationSet is deliberately coarse-grained for this pre-deployment app:
// one checksum-protected migration owns the system DB schema, and one owns the
// per-user DB schema. Future migrations should append new versions here rather
// than reviving ad hoc open-time schema fixes in Store.Open.
type migrationSet struct {
	Scope      string
	Version    string
	Label      string
	Statements []string
	After      []migrationStep
}

type migrationStep struct {
	Label string
	Run   func(context.Context, *Store) error
}

// migrate chooses the schema family for this SQLite handle. Split server
// stores run only system migrations; user stores run only user migrations; unit
// tests using Open run both against a single database for convenience.
func (s *Store) migrate(ctx context.Context, kind schemaKind, progress MigrationReporter) error {
	sets := make([]migrationSet, 0, 2)
	systemSets := []migrationSet{
		systemMigrationSet(),
		systemUserSearchPreferencesMigrationSet(),
		systemUserSearchRankingMigrationSet(),
		systemPasswordResetMigrationSet(),
	}
	userSets := []migrationSet{
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
		userInboxArrivalClassificationMigrationSet(),
		userMailboxGenerationArrivalJournalMigrationSet(),
		userTransferDispatchRecoveryMigrationSet(),
		userBlobCleanupQueueMigrationSet(),
		userMailboxGenerationArrivalFloorMigrationSet(),
		userMessageImportCompletionMigrationSet(),
		userSearchProgressIndexMigrationSet(),
		userOutboxMigrationSet(),
	}
	switch kind {
	case schemaSystem:
		sets = append(sets, systemSets...)
	case schemaUser:
		sets = append(sets, userSets...)
	default:
		sets = append(sets, systemSets...)
		sets = append(sets, userSets...)
	}
	for _, set := range sets {
		if err := s.applyMigrationSet(ctx, set, progress); err != nil {
			return err
		}
	}
	switch kind {
	case schemaSystem:
		return s.applyPluginMigrationsForScope(ctx, plugins.ScopeSystem)
	case schemaUser:
		return s.applyPluginMigrationsForScope(ctx, plugins.ScopeUser)
	default:
		if err := s.applyPluginMigrationsForScope(ctx, plugins.ScopeSystem); err != nil {
			return err
		}
		return s.applyPluginMigrationsForScope(ctx, plugins.ScopeUser)
	}
}

// applyMigrationSet runs DDL inside a transaction, records the checksum, then
// performs idempotent seed/backfill steps outside the transaction so startup
// progress can show both structural work and data preparation.
func (s *Store) applyMigrationSet(ctx context.Context, set migrationSet, progress MigrationReporter) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	if err := s.ensureSchemaMigrationTable(ctx); err != nil {
		return err
	}
	checksum := migrationChecksum(set)
	applied, err := s.migrationApplied(ctx, set.Scope, set.Version, checksum)
	if err != nil {
		return err
	}
	total := len(set.Statements) + len(set.After)
	if total == 0 {
		total = 1
	}
	if applied {
		reportMigration(progress, MigrationProgress{Scope: set.Scope, Migration: set.Label, Step: "already applied", Done: total, Total: total})
	} else {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for i, stmt := range set.Statements {
			step := migrationStatementLabel(stmt)
			reportMigration(progress, MigrationProgress{Scope: set.Scope, Migration: set.Label, Step: step, Done: i, Total: total})
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply %s migration %s: %w", set.Scope, step, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (scope, version, applied_at, checksum) VALUES (?, ?, ?, ?)`, set.Scope, set.Version, nowUnix(), checksum); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	baseDone := len(set.Statements)
	for i, step := range set.After {
		reportMigration(progress, MigrationProgress{Scope: set.Scope, Migration: set.Label, Step: step.Label, Done: baseDone + i, Total: total})
		if err := step.Run(ctx, s); err != nil {
			return fmt.Errorf("run %s post migration step %s: %w", set.Scope, step.Label, err)
		}
	}
	reportMigration(progress, MigrationProgress{Scope: set.Scope, Migration: set.Label, Step: "complete", Done: total, Total: total})
	return nil
}

func (s *Store) ensureSchemaMigrationTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		scope TEXT NOT NULL,
		version TEXT NOT NULL,
		applied_at INTEGER NOT NULL,
		checksum TEXT NOT NULL,
		PRIMARY KEY(scope, version)
	)`)
	return err
}

func (s *Store) migrationApplied(ctx context.Context, scope, version, checksum string) (bool, error) {
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE scope = ? AND version = ?`, scope, version).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existing != checksum {
		return false, fmt.Errorf("%s schema migration %s checksum mismatch", scope, version)
	}
	return true, nil
}

func migrationChecksum(set migrationSet) string {
	h := sha256.New()
	_, _ = h.Write([]byte(set.Scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(set.Version))
	for _, stmt := range set.Statements {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(stmt)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func migrationStatementLabel(stmt string) string {
	fields := strings.Fields(strings.ReplaceAll(stmt, "\n", " "))
	if len(fields) == 0 {
		return "statement"
	}
	if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") {
		if strings.EqualFold(fields[1], "TABLE") {
			return "create table " + createObjectName(fields, 2)
		}
		if strings.EqualFold(fields[1], "INDEX") || (len(fields) >= 4 && strings.EqualFold(fields[1], "UNIQUE") && strings.EqualFold(fields[2], "INDEX")) {
			start := 2
			if strings.EqualFold(fields[1], "UNIQUE") {
				start = 3
			}
			return "create index " + createObjectName(fields, start)
		}
	}
	if len(fields) > 5 {
		return strings.Join(fields[:5], " ")
	}
	return strings.Join(fields, " ")
}

func createObjectName(fields []string, start int) string {
	for i := start; i < len(fields); i++ {
		word := strings.Trim(fields[i], "`(),")
		lower := strings.ToLower(word)
		if word == "" || lower == "if" || lower == "not" || lower == "exists" {
			continue
		}
		return word
	}
	return "object"
}

func reportMigration(progress MigrationReporter, p MigrationProgress) {
	if progress != nil {
		progress(p)
	}
}
