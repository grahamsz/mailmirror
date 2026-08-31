// File overview: Message language and attachment indexing metadata persistence.

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

const searchRecoverySynchronousRestoreTimeout = 5 * time.Second

type setSQLiteSynchronousFunc func(context.Context, *sql.Conn, string) error
type sqliteDurabilityBarrierFunc func(context.Context, *sql.Conn) error

// MarkSearchVisibleMessagesPendingIndex schedules a complete, tenant-scoped
// search rebuild without changing message content, IMAP state, or blob rows.
func (s *Store) MarkSearchVisibleMessagesPendingIndex(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	return markSearchVisibleMessagesPendingIndexWithSynchronous(ctx, db, userID, setSQLiteSynchronous, fullSQLiteWALCheckpoint)
}

// markSearchVisibleMessagesPendingIndexWithSynchronous commits the recovery
// prerequisite with synchronous=FULL on one dedicated SQLite connection. In
// WAL mode that commit is durable before the caller fsyncs the Bleve quarantine
// rename and removes the recovery marker.
func markSearchVisibleMessagesPendingIndexWithSynchronous(ctx context.Context, db *sql.DB, userID int64, setSynchronous setSQLiteSynchronousFunc, durabilityBarrier sqliteDurabilityBarrierFunc) (marked int64, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("reserve durable search recovery connection: %w", err)
	}
	fullEnabled := false
	discarded := false
	defer func() {
		if fullEnabled {
			restoreCtx, cancel := context.WithTimeout(context.Background(), searchRecoverySynchronousRestoreTimeout)
			restoreErr := setSynchronous(restoreCtx, conn, "NORMAL")
			cancel()
			if restoreErr != nil {
				discardErr := discardSQLConnection(conn)
				discarded = discardErr == nil
				err = errors.Join(err, fmt.Errorf("restore SQLite synchronous mode after durable search recovery write: %w", restoreErr), discardErr)
			}
		}
		if !discarded {
			err = errors.Join(err, conn.Close())
		}
	}()

	// Restore NORMAL even if a driver reports an error after partially applying
	// the PRAGMA; setting NORMAL on an unchanged connection is harmless.
	fullEnabled = true
	if err := setSynchronous(ctx, conn, "FULL"); err != nil {
		return 0, fmt.Errorf("enable durable SQLite search recovery write: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin durable search recovery write: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages
			SET attachment_indexed_at = 0
			WHERE user_id = ?
				AND mailbox_id IN (
					SELECT id FROM mailboxes WHERE user_id = ? AND include_in_search = 1
				)`, userID, userID)
	if err != nil {
		return 0, err
	}
	marked, err = result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit durable search recovery write: %w", err)
	}
	// SQLite can optimize an UPDATE whose target values are already zero into a
	// read-only transaction. A FULL checkpoint is therefore the durability
	// barrier: it syncs any pre-existing WAL frame that made those zero values
	// visible before Bleve is quarantined and the marker is cleared.
	if err := durabilityBarrier(ctx, conn); err != nil {
		return marked, fmt.Errorf("checkpoint durable search recovery write: %w", err)
	}
	return marked, nil
}

func setSQLiteSynchronous(ctx context.Context, conn *sql.Conn, mode string) error {
	if mode != "FULL" && mode != "NORMAL" {
		return fmt.Errorf("unsupported SQLite synchronous mode %q", mode)
	}
	_, err := conn.ExecContext(ctx, "PRAGMA synchronous = "+mode)
	return err
}

func fullSQLiteWALCheckpoint(ctx context.Context, conn *sql.Conn) error {
	var busy, logFrames, checkpointedFrames int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return err
	}
	if logFrames < 0 || checkpointedFrames < 0 {
		return fmt.Errorf("WAL checkpoint unavailable: log_frames=%d checkpointed_frames=%d", logFrames, checkpointedFrames)
	}
	if busy != 0 || checkpointedFrames != logFrames {
		return fmt.Errorf("WAL checkpoint incomplete: busy=%d log_frames=%d checkpointed_frames=%d", busy, logFrames, checkpointedFrames)
	}
	return nil
}

func discardSQLConnection(conn *sql.Conn) error {
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	return err
}

// ListMessagesNeedingAttachmentIndex returns messages whose raw bodies still need attachment text extraction.
func (s *Store) ListMessagesNeedingAttachmentIndex(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	messages, _, err := s.ListMessagesNeedingAttachmentIndexAfter(ctx, userID, 0, limit)
	return messages, err
}

// ListMessagesNeedingAttachmentIndexAfter returns a circular, tenant-scoped
// page after messageID. wrapped reports that the page crossed back to lower IDs.
// The cursor keeps one failed raw message from pinning every later message while
// still bounding each attachment-index turn.
func (s *Store) ListMessagesNeedingAttachmentIndexAfter(ctx context.Context, userID, messageID int64, limit int) ([]MessageRecord, bool, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if messageID < 0 {
		messageID = 0
	}
	messages, err := s.listMessagesNeedingAttachmentIndexRange(ctx, userID, messageID, limit, false)
	if err != nil {
		return nil, false, err
	}
	if messageID == 0 || len(messages) == limit {
		return messages, false, nil
	}
	wrapped, err := s.listMessagesNeedingAttachmentIndexRange(ctx, userID, messageID, limit-len(messages), true)
	if err != nil {
		return nil, false, err
	}
	return append(messages, wrapped...), len(wrapped) > 0, nil
}

func (s *Store) listMessagesNeedingAttachmentIndexRange(ctx context.Context, userID, messageID int64, limit int, throughCursor bool) ([]MessageRecord, error) {
	query := `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND attachment_indexed_at = 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
			AND id > ? ORDER BY id LIMIT ?`
	if throughCursor {
		query = `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND attachment_indexed_at = 0
			AND mailbox_id NOT IN (
				SELECT id FROM mailboxes WHERE user_id = ? AND search_index_purged = 1
			)
			AND id <= ? ORDER BY id LIMIT ?`
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, query, userID, userID, messageID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesWithReadSyncPending returns locally changed read-state rows waiting for IMAP sync.
func (s *Store) ListMessagesWithReadSyncPending(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND read_sync_pending = 1
			AND outbox_job_id = 0 ORDER BY updated_at LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// ListMessagesWithStarSyncPending returns locally changed star-state rows waiting for IMAP sync.
func (s *Store) ListMessagesWithStarSyncPending(ctx context.Context, userID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND star_sync_pending = 1
			AND outbox_job_id = 0 ORDER BY updated_at LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MarkMessageAttachmentIndexed records that attachment text extraction ran for a message.
func (s *Store) MarkMessageAttachmentIndexed(ctx context.Context, userID, messageID int64, hasAttachments bool) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET has_attachments = ?, attachment_indexed_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, boolInt(hasAttachments), nowUnix(), nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MessageAttachmentIndexUpdate is one durable post-Bleve marker update.
type MessageAttachmentIndexUpdate struct {
	MessageID      int64
	HasAttachments bool
}

// MarkMessagesAttachmentIndexed records one already-committed Bleve batch in a
// single SQLite transaction. Rebuilds must not turn a 250-document Bleve batch
// into 250 separate SQLite commits.
func (s *Store) MarkMessagesAttachmentIndexed(ctx context.Context, userID int64, updates []MessageAttachmentIndexUpdate) error {
	if userID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `UPDATE messages SET has_attachments = ?, attachment_indexed_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`)
	if err != nil {
		return err
	}
	defer statement.Close()
	now := nowUnix()
	for _, update := range updates {
		if update.MessageID <= 0 {
			return ErrNotFound
		}
		result, err := statement.ExecContext(ctx, boolInt(update.HasAttachments), now, now, userID, update.MessageID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// SkipPendingHistoricalAttachmentIndexing completes deferred historical
// attachment enrichment without doing a whole-mailbox background rebuild.
// New messages are indexed during their normal IMAP import; historical
// attachment extraction is deliberately an explicit full-text rebuild task.
func (s *Store) SkipPendingHistoricalAttachmentIndexing(ctx context.Context, userID int64) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("user id must be positive")
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages
		SET attachment_indexed_at = ?, updated_at = ?
		WHERE user_id = ? AND attachment_indexed_at = 0`, nowUnix(), nowUnix(), userID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkMessageAttachmentIndexPending keeps a fallback search document eligible
// for later raw-body and attachment enrichment.
func (s *Store) MarkMessageAttachmentIndexPending(ctx context.Context, userID, messageID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET attachment_indexed_at = 0, updated_at = ?
		WHERE user_id = ? AND id = ?`, nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkMailboxSearchIndexPurged records a completed explicit Bleve purge without
// scheduling ordinary attachment recovery. The next folder sync or explicit
// rebuild clears this state before repairing the mailbox.
func (s *Store) MarkMailboxSearchIndexPurged(ctx context.Context, userID, mailboxID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 1, search_index_state_known = 1
		WHERE user_id = ? AND id = ?`, userID, mailboxID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// PreparePurgedMailboxSearchIndexRepair atomically re-enables exact repair for
// an explicitly purged mailbox. Existing per-message commit markers are kept:
// repair diffs SQLite against Bleve directly, so surviving documents from a
// partial purge must not be needlessly re-indexed. The returned bool reports
// whether purge state was consumed.
func (s *Store) PreparePurgedMailboxSearchIndexRepair(ctx context.Context, userID, mailboxID int64) (bool, error) {
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var purged bool
	if err := tx.QueryRowContext(ctx, `SELECT search_index_purged FROM mailboxes
		WHERE user_id = ? AND id = ?`, userID, mailboxID).Scan(&purged); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if !purged {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 0, search_index_state_known = 0
		WHERE user_id = ? AND id = ?`, userID, mailboxID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkMailboxSearchIndexActive records that exact mailbox repair completed.
// It is the only transition from migration-time unknown state to verified
// active state, so settings never guesses current Bleve coverage from markers.
func (s *Store) MarkMailboxSearchIndexActive(ctx context.Context, userID, mailboxID int64) error {
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET search_index_purged = 0, search_index_state_known = 1
		WHERE user_id = ? AND id = ?`, userID, mailboxID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateMessageLanguage stores plugin-detected language metadata for search filtering.
func (s *Store) UpdateMessageLanguage(ctx context.Context, userID, messageID int64, languageCode string) error {
	languageCode = strings.ToLower(strings.TrimSpace(languageCode))
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET language_code = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, strings.ToLower(strings.TrimSpace(languageCode)), nowUnix(), userID, messageID)
	if err != nil {
		return err
	}
	return s.upsertPluginMessageLanguage(ctx, userID, messageID, languageCode)
}

func (s *Store) upsertPluginMessageLanguage(ctx context.Context, userID, messageID int64, languageCode string) error {
	languageCode = strings.ToLower(strings.TrimSpace(languageCode))
	if userID == 0 || messageID == 0 || languageCode == "" {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO plugin_language_messages
			(user_id, message_id, language_code, detected_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, message_id) DO UPDATE SET
			language_code = excluded.language_code,
			detected_at = excluded.detected_at`,
		userID, messageID, languageCode, nowUnix())
	return err
}

// UpdateMessageSecurityState stores plugin-detected encrypted/signed metadata discovered while parsing raw messages.
func (s *Store) UpdateMessageSecurityState(ctx context.Context, userID, messageID int64, encrypted, signed bool) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE messages SET is_encrypted = ?, is_signed = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, boolInt(encrypted), boolInt(signed), nowUnix(), userID, messageID)
	return err
}
