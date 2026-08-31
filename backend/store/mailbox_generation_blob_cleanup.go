// File overview: Durable, tenant-scoped cleanup of message blobs orphaned by mailbox generation resets.

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MailboxGenerationBlobCleanup identifies one message blob that became stale
// when a mailbox's UIDVALIDITY changed. The row deliberately outlives blob
// metadata so cleanup can resume after either side of the filesystem/SQLite
// transition.
type MailboxGenerationBlobCleanup struct {
	ID                int64
	UserID            int64
	AccountID         int64
	MailboxID         int64
	TargetUIDValidity int64
	BlobID            int64
	BlobPath          string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func queueMailboxGenerationBlobCleanupTx(ctx context.Context, tx *sql.Tx, userID, accountID, mailboxID, targetUIDValidity, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_generation_blob_cleanup
		(user_id, account_id, mailbox_id, target_uid_validity, blob_id, blob_path, created_at, updated_at)
		SELECT DISTINCT message.user_id, message.account_id, message.mailbox_id, ?, blob.id, blob.path, ?, ?
		FROM messages message
		JOIN blobs blob ON blob.user_id = message.user_id AND blob.id = message.blob_id
		WHERE message.user_id = ? AND message.account_id = ? AND message.mailbox_id = ?
			AND message.outbox_job_id = 0
		ON CONFLICT(user_id, blob_id) DO UPDATE SET
			account_id = excluded.account_id,
			mailbox_id = excluded.mailbox_id,
			target_uid_validity = excluded.target_uid_validity,
			blob_path = excluded.blob_path,
			updated_at = excluded.updated_at`,
		targetUIDValidity, now, now, userID, accountID, mailboxID)
	return err
}

// ListMailboxGenerationBlobCleanup returns queued cleanup entries for one
// tenant mailbox. Entries from older target generations are included so a
// second UIDVALIDITY change cannot strand unfinished cleanup.
func (s *Store) ListMailboxGenerationBlobCleanup(ctx context.Context, userID, accountID, mailboxID int64, limit int) ([]MailboxGenerationBlobCleanup, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 {
		return nil, errors.New("invalid mailbox generation blob cleanup scope")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id,
		target_uid_validity, blob_id, blob_path, created_at, updated_at
		FROM mailbox_generation_blob_cleanup
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
		ORDER BY id LIMIT ?`, userID, accountID, mailboxID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailboxGenerationBlobCleanup
	for rows.Next() {
		var item MailboxGenerationBlobCleanup
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &item.UserID, &item.AccountID, &item.MailboxID,
			&item.TargetUIDValidity, &item.BlobID, &item.BlobPath, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		item.CreatedAt = unixTime(createdAt)
		item.UpdatedAt = unixTime(updatedAt)
		out = append(out, item)
	}
	return out, rows.Err()
}

// CompleteMailboxGenerationBlobCleanup removes one queued blob only after
// proving that no current tenant row references it. deletePath must not access
// this Store: the callback runs while a SQLite writer lock prevents a message
// from reattaching the blob between the reference check and metadata deletion.
// A callback or commit failure leaves the journal entry available for retry.
func (s *Store) CompleteMailboxGenerationBlobCleanup(ctx context.Context, userID, cleanupID int64, deletePath func(string) error) error {
	if userID <= 0 || cleanupID <= 0 {
		return errors.New("invalid mailbox generation blob cleanup entry")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE mailbox_generation_blob_cleanup
		SET updated_at = ? WHERE user_id = ? AND id = ?`, nowUnix(), userID, cleanupID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return ErrNotFound
	}

	var blobID int64
	var queuedPath string
	if err := tx.QueryRowContext(ctx, `SELECT blob_id, blob_path
		FROM mailbox_generation_blob_cleanup WHERE user_id = ? AND id = ?`, userID, cleanupID).
		Scan(&blobID, &queuedPath); err != nil {
		return err
	}

	var currentPath string
	err = tx.QueryRowContext(ctx, `SELECT path FROM blobs WHERE user_id = ? AND id = ?`, userID, blobID).Scan(&currentPath)
	switch {
	case err == nil && currentPath != queuedPath:
		return finishMailboxGenerationBlobCleanupTx(ctx, tx, userID, cleanupID)
	case err == nil:
		var referenced int
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM messages WHERE user_id = ? AND blob_id = ?)
			OR EXISTS (SELECT 1 FROM attachments WHERE user_id = ? AND blob_id = ?)
			OR EXISTS (SELECT 1 FROM contact_icons WHERE user_id = ? AND blob_id = ?)
			OR EXISTS (SELECT 1 FROM remote_image_cache WHERE user_id = ? AND blob_id = ?)`,
			userID, blobID, userID, blobID, userID, blobID, userID, blobID).Scan(&referenced); err != nil {
			return err
		}
		if referenced != 0 {
			return finishMailboxGenerationBlobCleanupTx(ctx, tx, userID, cleanupID)
		}
		if deletePath != nil && queuedPath != "" {
			if err := deletePath(queuedPath); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE user_id = ? AND id = ? AND path = ?`, userID, blobID, queuedPath)
		if err != nil {
			return err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected != 1 {
			return errors.New("mailbox generation blob metadata changed during cleanup")
		}
	case errors.Is(err, sql.ErrNoRows):
		var pathOwnerID int64
		pathErr := tx.QueryRowContext(ctx, `SELECT id FROM blobs WHERE user_id = ? AND path = ?`, userID, queuedPath).Scan(&pathOwnerID)
		if pathErr == nil {
			return finishMailboxGenerationBlobCleanupTx(ctx, tx, userID, cleanupID)
		}
		if !errors.Is(pathErr, sql.ErrNoRows) {
			return pathErr
		}
		if deletePath != nil && queuedPath != "" {
			if err := deletePath(queuedPath); err != nil {
				return err
			}
		}
	default:
		return err
	}

	return finishMailboxGenerationBlobCleanupTx(ctx, tx, userID, cleanupID)
}

func finishMailboxGenerationBlobCleanupTx(ctx context.Context, tx *sql.Tx, userID, cleanupID int64) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM mailbox_generation_blob_cleanup WHERE user_id = ? AND id = ?`, userID, cleanupID)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return errors.New("mailbox generation blob cleanup entry changed during completion")
	}
	return tx.Commit()
}
