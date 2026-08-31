// File overview: Message creation, lookup, update, and conversation-thread persistence helpers.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateMessage is the insert payload for a mirrored message and its mailbox location.
type CreateMessage struct {
	UserID           int64
	AccountID        int64
	MailboxID        int64
	BlobID           int64
	MessageIDHeader  string
	CanonicalSHA256  string
	MessageIDHash    string
	InReplyTo        string
	ReferencesHeader string
	ThreadKey        string
	Subject          string
	LanguageCode     string
	FromAddr         string
	ToAddr           string
	CCAddr           string
	Date             time.Time
	InternalDate     time.Time
	UID              uint32
	UIDValidity      int64
	Size             int64
	BlobPath         string
	BodyText         string
	BodyHTML         string
	IsRead           bool
	IsStarred        bool
	HasAttachments   bool
	IsEncrypted      bool
	IsSigned         bool
	ImportPending    bool
}

// CreateMessage inserts or updates a mirrored message row and its mailbox location.
func (s *Store) CreateMessage(ctx context.Context, m CreateMessage) (MessageRecord, error) {
	db, err := s.dataDB(ctx, m.UserID)
	if err != nil {
		return MessageRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MessageRecord{}, err
	}
	defer tx.Rollback()
	if m.UIDValidity > 0 {
		var currentUIDValidity int64
		err := tx.QueryRowContext(ctx, `SELECT uidvalidity FROM mailboxes
			WHERE user_id = ? AND account_id = ? AND id = ?`, m.UserID, m.AccountID, m.MailboxID).
			Scan(&currentUIDValidity)
		if errors.Is(err, sql.ErrNoRows) {
			return MessageRecord{}, ErrNotFound
		}
		if err != nil {
			return MessageRecord{}, err
		}
		if currentUIDValidity <= 0 || currentUIDValidity != m.UIDValidity {
			return MessageRecord{}, fmt.Errorf("mailbox generation changed before storing UID %d", m.UID)
		}
	}
	ts := nowUnix()
	importCompletedAt := ts
	if m.ImportPending {
		importCompletedAt = 0
	}
	if strings.TrimSpace(m.ThreadKey) == "" {
		m.ThreadKey = ThreadKey(m.MessageIDHeader, m.InReplyTo, m.ReferencesHeader, m.Subject)
	}
	if strings.TrimSpace(m.MessageIDHash) == "" {
		m.MessageIDHash = HashedMessageID(m.MessageIDHeader)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages
			(user_id, account_id, mailbox_id, blob_id, message_id_header, canonical_sha256, message_id_hash, in_reply_to, references_header, thread_key, thread_headers_checked_at, subject, language_code, from_addr, to_addr, cc_addr, date_unix, internal_date_unix, uid, uid_validity, size, blob_path, body_text, body_html, is_read, is_starred, has_attachments, is_encrypted, is_signed, import_completed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.UserID, m.AccountID, m.MailboxID, m.BlobID, m.MessageIDHeader, m.CanonicalSHA256, m.MessageIDHash, m.InReplyTo, m.ReferencesHeader, m.ThreadKey, ts, m.Subject, strings.ToLower(strings.TrimSpace(m.LanguageCode)), m.FromAddr, m.ToAddr, m.CCAddr,
		m.Date.UTC().Unix(), m.InternalDate.UTC().Unix(), m.UID, m.UIDValidity, m.Size, m.BlobPath, m.BodyText, m.BodyHTML, boolInt(m.IsRead), boolInt(m.IsStarred), boolInt(m.HasAttachments), boolInt(m.IsEncrypted), boolInt(m.IsSigned), importCompletedAt, ts, ts)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: messages.user_id, messages.account_id, messages.mailbox_id, messages.uid") {
			var existingID int64
			if m.UIDValidity > 0 {
				var storedUIDValidity int64
				if queryErr := tx.QueryRowContext(ctx, `SELECT id, uid_validity FROM messages
					WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?`,
					m.UserID, m.AccountID, m.MailboxID, m.UID).Scan(&existingID, &storedUIDValidity); queryErr != nil {
					return MessageRecord{}, queryErr
				}
				if storedUIDValidity != m.UIDValidity {
					return MessageRecord{}, fmt.Errorf("mailbox generation changed: stored UID %d belongs to another generation", m.UID)
				}
			} else if queryErr := tx.QueryRowContext(ctx, `SELECT id FROM messages
				WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?`,
				m.UserID, m.AccountID, m.MailboxID, m.UID).Scan(&existingID); queryErr != nil {
				return MessageRecord{}, queryErr
			}
			if _, updateErr := tx.ExecContext(ctx, `UPDATE messages SET
				canonical_sha256 = CASE WHEN ? <> '' THEN ? ELSE canonical_sha256 END,
				message_id_hash = CASE WHEN ? <> '' THEN ? ELSE message_id_hash END,
				uid_validity = CASE WHEN ? > 0 THEN ? ELSE uid_validity END,
				import_completed_at = CASE WHEN import_completed_at = 0 AND ? > 0 THEN ? ELSE import_completed_at END,
				updated_at = ?
				WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?`,
				m.CanonicalSHA256, m.CanonicalSHA256, m.MessageIDHash, m.MessageIDHash,
				m.UIDValidity, m.UIDValidity, importCompletedAt, importCompletedAt, ts, m.UserID, m.AccountID, m.MailboxID, m.UID); updateErr != nil {
				return MessageRecord{}, updateErr
			}
			if restoreErr := restoreMailboxGenerationStateTx(ctx, tx, existingID, m); restoreErr != nil {
				return MessageRecord{}, restoreErr
			}
			if err := tx.Commit(); err != nil {
				return MessageRecord{}, err
			}
			if err := s.upsertPluginMessageLanguage(ctx, m.UserID, existingID, m.LanguageCode); err != nil {
				return MessageRecord{}, err
			}
			return s.GetMessageByUID(ctx, m.UserID, m.AccountID, m.MailboxID, m.UID)
		}
		return MessageRecord{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MessageRecord{}, err
	}
	if err := restoreMailboxGenerationStateTx(ctx, tx, id, m); err != nil {
		return MessageRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageRecord{}, err
	}
	if err := s.upsertPluginMessageLanguage(ctx, m.UserID, id, m.LanguageCode); err != nil {
		return MessageRecord{}, err
	}
	return s.GetMessageForUser(ctx, m.UserID, id)
}

// MarkMessageImportCompleted makes one fully persisted and indexed message
// eligible for generation-bound checkpoint advancement.
func (s *Store) MarkMessageImportCompleted(ctx context.Context, userID, messageID int64) error {
	return s.MarkMessagesImportCompleted(ctx, userID, []int64{messageID})
}

// MarkMessagesImportCompleted marks a batch of user-owned messages complete.
// Missing IDs are terminal: a post-store hook may have already moved or
// deleted the source row. Cross-tenant IDs remain untouched by the user scope.
func (s *Store) MarkMessagesImportCompleted(ctx context.Context, userID int64, messageIDs []int64) error {
	if userID <= 0 {
		return errors.New("invalid message import completion user scope")
	}
	ids := make([]int64, 0, len(messageIDs))
	seen := make(map[int64]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			return errors.New("invalid message import completion ID")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	completedAt := nowUnix()
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, completedAt, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		_, err := tx.ExecContext(ctx, `UPDATE messages
			SET import_completed_at = CASE WHEN import_completed_at = 0 THEN ? ELSE import_completed_at END
			WHERE user_id = ? AND id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMessageByUID loads one message by account/mailbox UID inside a user scope.
func (s *Store) GetMessageByUID(ctx context.Context, userID, accountID, mailboxID int64, uid uint32) (MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageRecord{}, err
	}
	var m MessageRecord
	var dateUnix, internalUnix, indexedAt, created, updated int64
	err = db.QueryRowContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?`, userID, accountID, mailboxID, uid).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.MailboxID, &m.BlobID, &m.MessageIDHeader, &m.InReplyTo, &m.ReferencesHeader, &m.ThreadKey, &m.Subject, &m.LanguageCode, &m.FromAddr, &m.ToAddr, &m.CCAddr,
			&dateUnix, &internalUnix, &m.UID, &m.Size, &m.BlobPath, &m.BodyText, &m.BodyHTML, &m.IsRead, &m.ReadSyncPending, &m.IsStarred, &m.StarSyncPending, &m.HasAttachments, &m.IsEncrypted, &m.IsSigned, &indexedAt, &created, &updated)
	m.Date = unixTime(dateUnix)
	m.InternalDate = unixTime(internalUnix)
	m.AttachmentIndexedAt = unixTime(indexedAt)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// CreateLocation records that a message appears in a mailbox at a specific UID.
func (s *Store) CreateLocation(ctx context.Context, userID, messageID, mailboxID int64, uid uint32) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO locations (user_id, message_id, mailbox_id, uid, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, messageID, mailboxID, uid, nowUnix())
	return err
}

// GetMessageForUser loads one message by local ID inside a user scope.
func (s *Store) GetMessageForUser(ctx context.Context, userID, id int64) (MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageRecord{}, err
	}
	var m MessageRecord
	var dateUnix, internalUnix, indexedAt, created, updated int64
	err = db.QueryRowContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.MailboxID, &m.BlobID, &m.MessageIDHeader, &m.InReplyTo, &m.ReferencesHeader, &m.ThreadKey, &m.Subject, &m.LanguageCode, &m.FromAddr, &m.ToAddr, &m.CCAddr,
			&dateUnix, &internalUnix, &m.UID, &m.Size, &m.BlobPath, &m.BodyText, &m.BodyHTML, &m.IsRead, &m.ReadSyncPending, &m.IsStarred, &m.StarSyncPending, &m.HasAttachments, &m.IsEncrypted, &m.IsSigned, &indexedAt, &created, &updated)
	m.Date = unixTime(dateUnix)
	m.InternalDate = unixTime(internalUnix)
	m.AttachmentIndexedAt = unixTime(indexedAt)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// GetMessageUIDValidityForUser returns the IMAP mailbox generation captured
// when a message row was stored. Mutation paths compare it with the current
// mailbox generation before using the row's UID remotely.
func (s *Store) GetMessageUIDValidityForUser(ctx context.Context, userID, id int64) (int64, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var uidValidity int64
	err = db.QueryRowContext(ctx, `SELECT uid_validity FROM messages
		WHERE user_id = ? AND id = ?`, userID, id).Scan(&uidValidity)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return uidValidity, err
}

// GetMessageEnvelopeForUser loads one message without body_text/body_html. Hot
// message-open paths use this so large cached display bodies are timed under body
// hydration instead of the initial ID lookup.
func (s *Store) GetMessageEnvelopeForUser(ctx context.Context, userID, id int64) (MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageRecord{}, err
	}
	var m MessageRecord
	var dateUnix, internalUnix, indexedAt, created, updated int64
	err = db.QueryRowContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.MailboxID, &m.BlobID, &m.MessageIDHeader, &m.InReplyTo, &m.ReferencesHeader, &m.ThreadKey, &m.Subject, &m.LanguageCode, &m.FromAddr, &m.ToAddr, &m.CCAddr,
			&dateUnix, &internalUnix, &m.UID, &m.Size, &m.BlobPath, &m.IsRead, &m.ReadSyncPending, &m.IsStarred, &m.StarSyncPending, &m.HasAttachments, &m.IsEncrypted, &m.IsSigned, &indexedAt, &created, &updated)
	m.Date = unixTime(dateUnix)
	m.InternalDate = unixTime(internalUnix)
	m.AttachmentIndexedAt = unixTime(indexedAt)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// GetMessageByBlobIDForUser finds the message that owns a blob record for one user.
func (s *Store) GetMessageByBlobIDForUser(ctx context.Context, userID, blobID int64) (MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageRecord{}, err
	}
	var m MessageRecord
	var dateUnix, internalUnix, indexedAt, created, updated int64
	err = db.QueryRowContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND blob_id = ?`, userID, blobID).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.MailboxID, &m.BlobID, &m.MessageIDHeader, &m.InReplyTo, &m.ReferencesHeader, &m.ThreadKey, &m.Subject, &m.LanguageCode, &m.FromAddr, &m.ToAddr, &m.CCAddr,
			&dateUnix, &internalUnix, &m.UID, &m.Size, &m.BlobPath, &m.BodyText, &m.BodyHTML, &m.IsRead, &m.ReadSyncPending, &m.IsStarred, &m.StarSyncPending, &m.HasAttachments, &m.IsEncrypted, &m.IsSigned, &indexedAt, &created, &updated)
	m.Date = unixTime(dateUnix)
	m.InternalDate = unixTime(internalUnix)
	m.AttachmentIndexedAt = unixTime(indexedAt)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// DeleteMessageForUser removes one local message row and dependent data for a user.
func (s *Store) DeleteMessageForUser(ctx context.Context, userID, id int64) error {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeMailboxMessages removes all local message references for one mailbox and resets its UID checkpoint.
func (s *Store) PurgeMailboxMessages(ctx context.Context, userID, accountID, mailboxID int64) ([]MessageRecord, error) {
	if err := s.ResetMailboxLastUID(ctx, userID, mailboxID); err != nil {
		return nil, err
	}
	var out []MessageRecord
	for {
		batch, err := s.PurgeMailboxMessageBatch(ctx, userID, accountID, mailboxID, 250)
		if err != nil {
			return out, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
	}
}

// PurgeMailboxMessageBatch removes a small batch of local message rows for one mailbox.
func (s *Store) PurgeMailboxMessageBatch(ctx context.Context, userID, accountID, mailboxID int64, limit int) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 250
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND outbox_job_id = 0 ORDER BY id LIMIT ?`, userID, accountID, mailboxID, limit)
	if err != nil {
		return nil, err
	}
	messages, err := scanMessages(rows)
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil || len(messages) == 0 {
		return messages, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	for _, msg := range messages {
		if _, err := stmt.ExecContext(ctx, userID, msg.ID); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return messages, nil
}

// DeleteMessagesMissingUIDs removes local messages no longer present in a remote mailbox UID set.
// It intentionally does not create expunge evidence because the UID list is not
// bound to a verified mailbox generation.
func (s *Store) DeleteMessagesMissingUIDs(ctx context.Context, userID, accountID, mailboxID int64, remoteUIDs []uint32) ([]MessageRecord, error) {
	return s.deleteMessagesMissingUIDs(ctx, userID, accountID, mailboxID, remoteUIDs, 0, 0, nil, false)
}

// DeleteMessagesMissingUIDsAndRecordExpunges records short-lived source
// fingerprints and deletes stale rows in one transaction, but only while the
// mailbox still has the UIDVALIDITY attached to the remote UID snapshot. Only
// local UIDs below the snapshot's UIDNEXT are eligible, so rows created after
// the remote search cannot be mistaken for expunges. A generation mismatch or
// missing snapshot bound is a safe no-op. Callers may supply canonical digests
// loaded from retained raw blobs; exact and metadata matches are still recorded
// when a canonical digest is unavailable.
func (s *Store) DeleteMessagesMissingUIDsAndRecordExpunges(ctx context.Context, userID, accountID, mailboxID int64, remoteUIDs []uint32, remoteUIDValidity, remoteUIDNext uint32, canonicalByMessageID map[int64]string) ([]MessageRecord, error) {
	if remoteUIDValidity == 0 || remoteUIDNext == 0 {
		return nil, nil
	}
	return s.deleteMessagesMissingUIDs(ctx, userID, accountID, mailboxID, remoteUIDs, int64(remoteUIDValidity), remoteUIDNext, canonicalByMessageID, true)
}

func (s *Store) deleteMessagesMissingUIDs(ctx context.Context, userID, accountID, mailboxID int64, remoteUIDs []uint32, remoteUIDValidity int64, remoteUIDNext uint32, canonicalByMessageID map[int64]string, recordExpunges bool) ([]MessageRecord, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	remote := make(map[uint32]bool, len(remoteUIDs))
	for _, uid := range remoteUIDs {
		remote[uid] = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if recordExpunges {
		var currentUIDValidity int64
		err := tx.QueryRowContext(ctx, `SELECT uidvalidity FROM mailboxes
			WHERE user_id = ? AND account_id = ? AND id = ?`, userID, accountID, mailboxID).Scan(&currentUIDValidity)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if currentUIDValidity <= 0 || currentUIDValidity != remoteUIDValidity {
			return nil, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND outbox_job_id = 0`, userID, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	local, err := scanMessages(rows)
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var stale []MessageRecord
	for _, msg := range local {
		if recordExpunges && msg.UID >= remoteUIDNext {
			continue
		}
		if !remote[msg.UID] {
			stale = append(stale, msg)
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`)
	if err != nil {
		return nil, err
	}
	for _, msg := range stale {
		if recordExpunges {
			if err := recordExpungedMessageFingerprintTx(ctx, tx, userID, msg.ID, canonicalByMessageID[msg.ID], nowUnix()); err != nil {
				_ = stmt.Close()
				return nil, err
			}
		}
		if _, err := stmt.ExecContext(ctx, userID, msg.ID); err != nil {
			_ = stmt.Close()
			return nil, err
		}
	}
	if err := stmt.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stale, nil
}
