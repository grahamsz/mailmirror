// File overview: Privacy-preserving message fingerprints and durable journals for Rolltop-initiated transfers.

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"
)

const (
	messageTransferTTL                   = 24 * time.Hour
	expungedFingerprintTTL               = 15 * time.Second
	maxUnconsumedMessageTransfersPerUser = 10000
)

// ArrivalFingerprint contains only one-way digests and non-content metadata.
// Raw or parsed message content must never be persisted in the arrival journal.
type ArrivalFingerprint struct {
	RawSHA256       string
	CanonicalSHA256 string
	MessageIDHash   string
	InternalDate    time.Time
	Size            int64
}

// MessageArrivalFingerprint builds the exact and canonical digests used for
// transfer correlation. Canonicalization changes line endings only.
func MessageArrivalFingerprint(raw []byte, messageID string, internalDate time.Time, size int64) ArrivalFingerprint {
	exact := sha256.Sum256(raw)
	if size <= 0 {
		size = int64(len(raw))
	}
	return ArrivalFingerprint{
		RawSHA256:       hex.EncodeToString(exact[:]),
		CanonicalSHA256: CanonicalMessageSHA256(raw),
		MessageIDHash:   HashedMessageID(messageID),
		InternalDate:    internalDate.UTC(),
		Size:            size,
	}
}

// CanonicalMessageSHA256 hashes RFC822 bytes after normalizing CRLF and lone
// CR line endings to LF. No MIME, header, whitespace, or content rewriting is performed.
func CanonicalMessageSHA256(raw []byte) string {
	h := sha256.New()
	writeCanonicalMessage(h, raw)
	return hex.EncodeToString(h.Sum(nil))
}

func writeCanonicalMessage(h hash.Hash, raw []byte) {
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\r' {
			continue
		}
		if start < i {
			_, _ = h.Write(raw[start:i])
		}
		_, _ = h.Write([]byte{'\n'})
		if i+1 < len(raw) && raw[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(raw) {
		_, _ = h.Write(raw[start:])
	}
}

// HashedMessageID normalizes an RFC822 Message-ID without retaining it.
func HashedMessageID(messageID string) string {
	normalized := strings.ToLower(strings.TrimSpace(messageID))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// MessageTransfer is one Rolltop-initiated IMAP move or copy operation.
type MessageTransfer struct {
	ID                             int64
	UserID                         int64
	SourceAccountID                int64
	DestinationAccountID           int64
	SourceMailboxID                int64
	DestinationMailboxID           int64
	SourceMessageID                int64
	SourceUID                      uint32
	SourceUIDValidity              int64
	DestinationUID                 uint32
	DestinationUIDValidity         int64
	Kind                           string
	State                          string
	Fingerprint                    ArrivalFingerprint
	ConsumedMessageID              int64
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	DispatchedAt                   time.Time
	DispatchOwner                  string
	DispatchAttempt                int64
	DispatchFinishedAt             time.Time
	DestinationSnapshotUIDValidity int64
	DestinationSnapshotUIDNext     uint32
	ExpiresAt                      time.Time
	WasCreated                     bool
}

// StageMessageTransfer snapshots the tenant-owned source before remote work.
// Moves remain within one account; copies may target another account owned by the user.
func (s *Store) StageMessageTransfer(ctx context.Context, userID, sourceMessageID, destinationMailboxID int64, kind, canonicalSHA string) (MessageTransfer, error) {
	if userID <= 0 || sourceMessageID <= 0 || destinationMailboxID <= 0 {
		return MessageTransfer{}, errors.New("invalid message transfer scope")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "move" && kind != "copy" {
		return MessageTransfer{}, errors.New("invalid message transfer kind")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageTransfer{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MessageTransfer{}, err
	}
	defer tx.Rollback()
	var sourceAccountID, sourceMailboxID, destinationAccountID, sourceMailboxUIDValidity, outboxJobID int64
	var sourceUID uint32
	var sourceUIDValidity int64
	var rawSHA, storedCanonical, messageID, storedMessageIDHash string
	var internalDate, size int64
	err = tx.QueryRowContext(ctx, `SELECT source.account_id, source.mailbox_id, source.uid,
		blob.sha256, source.canonical_sha256, source.message_id_header, source.message_id_hash,
		source.internal_date_unix, source.size, source.uid_validity, source.outbox_job_id, destination.account_id,
		source_mailbox.uidvalidity
		FROM messages source
		JOIN blobs blob ON blob.user_id = source.user_id AND blob.id = source.blob_id
		JOIN mailboxes destination ON destination.user_id = source.user_id AND destination.id = ?
		JOIN mailboxes source_mailbox ON source_mailbox.user_id = source.user_id AND source_mailbox.id = source.mailbox_id
		WHERE source.user_id = ? AND source.id = ?`, destinationMailboxID, userID, sourceMessageID).
		Scan(&sourceAccountID, &sourceMailboxID, &sourceUID, &rawSHA, &storedCanonical,
			&messageID, &storedMessageIDHash, &internalDate, &size, &sourceUIDValidity, &outboxJobID, &destinationAccountID,
			&sourceMailboxUIDValidity)
	if err != nil {
		return MessageTransfer{}, err
	}
	if outboxJobID > 0 {
		return MessageTransfer{}, errors.New("message is still being delivered; wait for its Sent copy to be confirmed")
	}
	if kind == "move" && sourceAccountID != destinationAccountID {
		return MessageTransfer{}, errors.New("message move accounts do not match")
	}
	if kind == "move" && (sourceUIDValidity <= 0 || sourceMailboxUIDValidity <= 0 || sourceUIDValidity != sourceMailboxUIDValidity) {
		return MessageTransfer{}, errors.New("message source mailbox generation changed; refresh before moving")
	}
	canonicalSHA = strings.TrimSpace(canonicalSHA)
	if storedCanonical != "" && canonicalSHA != "" && storedCanonical != canonicalSHA {
		return MessageTransfer{}, errors.New("message transfer fingerprint mismatch")
	}
	if canonicalSHA == "" {
		canonicalSHA = storedCanonical
	}
	messageIDHash := HashedMessageID(messageID)
	if storedMessageIDHash != "" {
		messageIDHash = storedMessageIDHash
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET canonical_sha256 = ?, message_id_hash = ?
		WHERE user_id = ? AND id = ?`, canonicalSHA, messageIDHash, userID, sourceMessageID); err != nil {
		return MessageTransfer{}, err
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_transfers
		WHERE user_id = ? AND state IN ('failed', 'consumed') AND expires_at <= ?`, userID, now); err != nil {
		return MessageTransfer{}, err
	}
	var existingID int64
	existingErr := tx.QueryRowContext(ctx, `SELECT id FROM message_transfers
		WHERE user_id = ? AND source_account_id = ? AND source_mailbox_id = ? AND source_uid = ?
			AND source_uid_validity = ? AND operation_kind = ?
			AND (? = 'move' OR (destination_account_id = ? AND destination_mailbox_id = ?))
			AND (state IN ('pending', 'succeeded') OR (state = 'consumed' AND expires_at > ?))
		ORDER BY id DESC LIMIT 1`, userID, sourceAccountID, sourceMailboxID, sourceUID,
		sourceUIDValidity, kind, kind, destinationAccountID, destinationMailboxID, now).Scan(&existingID)
	if existingErr == nil {
		if err := tx.Commit(); err != nil {
			return MessageTransfer{}, err
		}
		return s.getMessageTransfer(ctx, userID, existingID)
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return MessageTransfer{}, existingErr
	}
	var activeCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_transfers
		WHERE user_id = ? AND state IN ('pending', 'succeeded')`, userID).Scan(&activeCount); err != nil {
		return MessageTransfer{}, err
	}
	if activeCount >= maxUnconsumedMessageTransfersPerUser {
		return MessageTransfer{}, fmt.Errorf("too many unresolved message transfers; sync destination mailboxes before starting another")
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO message_transfers
		(user_id, source_account_id, destination_account_id, source_mailbox_id,
		 destination_mailbox_id, source_message_id, source_uid, source_uid_validity, operation_kind, state,
		 raw_sha256, canonical_sha256, message_id_hash, internal_date_unix, message_size,
		 created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, sourceAccountID, destinationAccountID, sourceMailboxID, destinationMailboxID,
		sourceMessageID, sourceUID, sourceUIDValidity, kind, rawSHA, canonicalSHA, messageIDHash, internalDate, size,
		now, now, now+int64(messageTransferTTL/time.Second))
	if err != nil {
		return MessageTransfer{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return MessageTransfer{}, err
	}
	if err := tx.Commit(); err != nil {
		return MessageTransfer{}, err
	}
	item, err := s.getMessageTransfer(ctx, userID, id)
	item.WasCreated = err == nil
	return item, err
}

// ClaimMessageTransferDispatch records the one caller allowed to issue the
// remote IMAP command. An existing unclaimed pending transfer is safe to
// resume; a claimed pending transfer has an unknown remote outcome and must be
// reconciled instead of dispatched again.
func (s *Store) ClaimMessageTransferDispatch(ctx context.Context, userID, transferID int64) (bool, error) {
	_, claimed, err := s.ClaimMessageTransferDispatchForOwner(ctx, userID, transferID, "legacy")
	return claimed, err
}

// MessageTransferDispatchClaim identifies one persisted remote-command attempt.
// Reconciliation uses the attempt number to avoid reopening a newer claim.
type MessageTransferDispatchClaim struct {
	Owner   string
	Attempt int64
}

// ClaimMessageTransferDispatchForOwner atomically claims one pending transfer
// for a process owner and returns its monotonically increasing attempt number.
func (s *Store) ClaimMessageTransferDispatchForOwner(ctx context.Context, userID, transferID int64, owner string) (MessageTransferDispatchClaim, bool, error) {
	if userID <= 0 || transferID <= 0 {
		return MessageTransferDispatchClaim{}, false, errors.New("invalid message transfer scope")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" || len(owner) > 128 {
		return MessageTransferDispatchClaim{}, false, errors.New("invalid message transfer dispatch owner")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	defer tx.Rollback()
	now := nowUnix()
	result, err := tx.ExecContext(ctx, `UPDATE message_transfers
		SET dispatched_at = ?, dispatch_owner = ?, dispatch_attempt = dispatch_attempt + 1,
			dispatch_finished_at = 0, updated_at = ?
		WHERE user_id = ? AND id = ? AND state = 'pending' AND dispatched_at = 0`,
		now, owner, now, userID, transferID)
	if err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return MessageTransferDispatchClaim{}, false, err
		}
		return MessageTransferDispatchClaim{}, false, nil
	}
	var attempt int64
	if err := tx.QueryRowContext(ctx, `SELECT dispatch_attempt FROM message_transfers
		WHERE user_id = ? AND id = ?`, userID, transferID).Scan(&attempt); err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MessageTransferDispatchClaim{}, false, err
	}
	return MessageTransferDispatchClaim{Owner: owner, Attempt: attempt}, true, nil
}

// FinishMessageTransferDispatch marks an unknown remote attempt as no longer
// active. It remains pending until operation-specific reconciliation proves
// whether the command was applied.
func (s *Store) FinishMessageTransferDispatch(ctx context.Context, userID, transferID int64, claim MessageTransferDispatchClaim) error {
	if userID <= 0 || transferID <= 0 || strings.TrimSpace(claim.Owner) == "" || claim.Attempt <= 0 {
		return errors.New("invalid message transfer dispatch claim")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	now := nowUnix()
	result, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET dispatch_finished_at = ?, updated_at = ?
		WHERE user_id = ? AND id = ? AND state = 'pending' AND dispatched_at > 0
			AND dispatch_owner = ? AND dispatch_attempt = ?`,
		now, now, userID, transferID, claim.Owner, claim.Attempt)
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

// ReopenMessageTransferDispatchAfterProof clears one inactive claim only when
// it is still the exact attempt that was reconciled. Callers must first obtain
// authoritative, operation-specific proof that the remote command was absent.
func (s *Store) ReopenMessageTransferDispatchAfterProof(ctx context.Context, userID, transferID int64, expected MessageTransferDispatchClaim, currentOwner string) (bool, error) {
	if userID <= 0 || transferID <= 0 || expected.Attempt < 0 || strings.TrimSpace(currentOwner) == "" {
		return false, errors.New("invalid message transfer dispatch reconciliation")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return false, err
	}
	now := nowUnix()
	result, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET dispatched_at = 0, dispatch_owner = '', dispatch_finished_at = 0, updated_at = ?
		WHERE user_id = ? AND id = ? AND state = 'pending' AND dispatched_at > 0
			AND dispatch_owner = ? AND dispatch_attempt = ?
			AND (dispatch_finished_at > 0 OR dispatch_owner <> ?)`,
		now, userID, transferID, expected.Owner, expected.Attempt, currentOwner)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil

}

// SetMessageTransferDestinationSnapshot stores the pre-dispatch destination
// generation and UIDNEXT boundary once. Concurrent callers retain the earliest
// durable boundary, which is the conservative window for later exact matching.
func (s *Store) SetMessageTransferDestinationSnapshot(ctx context.Context, userID, transferID int64, uidValidity int64, uidNext uint32) (MessageTransfer, error) {
	if userID <= 0 || transferID <= 0 || uidValidity <= 0 || uidNext == 0 {
		return MessageTransfer{}, errors.New("invalid message transfer destination snapshot")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageTransfer{}, err
	}
	now := nowUnix()
	if _, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET destination_snapshot_uid_validity = ?, destination_snapshot_uid_next = ?, updated_at = ?
		WHERE user_id = ? AND id = ? AND operation_kind = 'copy' AND state = 'pending'
			AND dispatched_at = 0 AND destination_snapshot_uid_validity = 0`,
		uidValidity, uidNext, now, userID, transferID); err != nil {
		return MessageTransfer{}, err
	}
	return s.getMessageTransfer(ctx, userID, transferID)
}

// MarkMessageTransferSucceeded makes a staged transfer eligible to classify
// its destination. COPYUID metadata is retained when the server supplies it.
func (s *Store) MarkMessageTransferSucceeded(ctx context.Context, userID, transferID int64, destinationUID uint32, destinationUIDValidity int64) error {
	if userID <= 0 || transferID <= 0 || destinationUIDValidity < 0 {
		return errors.New("invalid message transfer scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	now := nowUnix()
	result, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET state = CASE WHEN state = 'consumed' THEN 'consumed' ELSE 'succeeded' END,
			destination_uid = CASE WHEN ? > 0 THEN ? ELSE destination_uid END,
			destination_uid_validity = CASE WHEN ? > 0 THEN ? ELSE destination_uid_validity END,
			completed_at = CASE WHEN completed_at = 0 THEN ? ELSE completed_at END, updated_at = ?
		WHERE user_id = ? AND id = ? AND state IN ('pending', 'succeeded', 'consumed')`,
		destinationUID, destinationUID, destinationUIDValidity, destinationUIDValidity,
		now, now, userID, transferID)
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

// TerminalizeMessageTransferWithoutArrival retires a completed transfer whose
// destination cannot produce an Inbox-arrival classification. The consumed
// receipt remains briefly reusable so immediate retries stay idempotent.
func (s *Store) TerminalizeMessageTransferWithoutArrival(ctx context.Context, userID, transferID int64) error {
	if userID <= 0 || transferID <= 0 {
		return errors.New("invalid message transfer scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	now := nowUnix()
	result, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET state = 'consumed',
			consumed_at = CASE WHEN state = 'succeeded' THEN ? ELSE consumed_at END,
			completed_at = CASE WHEN completed_at = 0 THEN ? ELSE completed_at END,
			expires_at = CASE WHEN state = 'succeeded' THEN ? ELSE expires_at END,
			updated_at = ?
		WHERE user_id = ? AND id = ? AND state IN ('succeeded', 'consumed')
			AND EXISTS (SELECT 1 FROM mailboxes destination
				WHERE destination.user_id = message_transfers.user_id
					AND destination.id = message_transfers.destination_mailbox_id
					AND lower(trim(COALESCE(destination.role, ''))) <> 'inbox'
					AND lower(trim(destination.name)) <> 'inbox')`,
		now, now, now+int64(messageTransferTTL/time.Second), now, userID, transferID)
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

// MarkMessageTransferFailed prevents a failed remote operation from suppressing a later delivery.
func (s *Store) MarkMessageTransferFailed(ctx context.Context, userID, transferID int64) error {
	if userID <= 0 || transferID <= 0 {
		return errors.New("invalid message transfer scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	now := nowUnix()
	result, err := db.ExecContext(ctx, `UPDATE message_transfers
		SET state = 'failed', completed_at = CASE WHEN completed_at = 0 THEN ? ELSE completed_at END,
			updated_at = ?, expires_at = MIN(expires_at, ?)
		WHERE user_id = ? AND id = ? AND state IN ('pending', 'failed')`,
		now, now, now+int64(expungedFingerprintTTL/time.Second), userID, transferID)
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

func (s *Store) getMessageTransfer(ctx context.Context, userID, transferID int64) (MessageTransfer, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return MessageTransfer{}, err
	}
	return scanMessageTransfer(db.QueryRowContext(ctx, `SELECT id, user_id, source_account_id,
		destination_account_id, COALESCE(source_mailbox_id, 0), destination_mailbox_id,
		COALESCE(source_message_id, 0), source_uid, source_uid_validity, destination_uid, destination_uid_validity,
		operation_kind, state, raw_sha256, canonical_sha256, message_id_hash, internal_date_unix,
		message_size, COALESCE(consumed_message_id, 0), created_at, updated_at, dispatched_at,
		dispatch_owner, dispatch_attempt, dispatch_finished_at,
		destination_snapshot_uid_validity, destination_snapshot_uid_next, expires_at
		FROM message_transfers WHERE user_id = ? AND id = ?`, userID, transferID))
}

func scanMessageTransfer(row scanDest) (MessageTransfer, error) {
	var item MessageTransfer
	var internalDate, created, updated, dispatched, dispatchFinished, expires int64
	err := row.Scan(&item.ID, &item.UserID, &item.SourceAccountID, &item.DestinationAccountID,
		&item.SourceMailboxID, &item.DestinationMailboxID, &item.SourceMessageID,
		&item.SourceUID, &item.SourceUIDValidity, &item.DestinationUID, &item.DestinationUIDValidity, &item.Kind,
		&item.State, &item.Fingerprint.RawSHA256, &item.Fingerprint.CanonicalSHA256,
		&item.Fingerprint.MessageIDHash, &internalDate, &item.Fingerprint.Size,
		&item.ConsumedMessageID, &created, &updated, &dispatched, &item.DispatchOwner,
		&item.DispatchAttempt, &dispatchFinished, &item.DestinationSnapshotUIDValidity,
		&item.DestinationSnapshotUIDNext, &expires)
	item.Fingerprint.InternalDate = unixTime(internalDate)
	item.CreatedAt = unixTime(created)
	item.UpdatedAt = unixTime(updated)
	item.DispatchedAt = unixTime(dispatched)
	item.DispatchFinishedAt = unixTime(dispatchFinished)
	item.ExpiresAt = unixTime(expires)
	return item, err
}

// SetMessageArrivalFingerprint stores canonical values needed if reconciliation
// later deletes the source row in the same transaction that records its tombstone.
func (s *Store) SetMessageArrivalFingerprint(ctx context.Context, userID, messageID int64, fingerprint ArrivalFingerprint) error {
	if userID <= 0 || messageID <= 0 {
		return errors.New("invalid message fingerprint scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE messages SET canonical_sha256 = ?, message_id_hash = ?
		WHERE user_id = ? AND id = ?`, strings.TrimSpace(fingerprint.CanonicalSHA256),
		strings.TrimSpace(fingerprint.MessageIDHash), userID, messageID)
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

func validateHexFingerprint(value string) error {
	if value == "" {
		return nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid arrival fingerprint")
	}
	return nil
}
