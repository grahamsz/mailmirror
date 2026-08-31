// File overview: Durable per-user SMTP outbox queue, leases, retries, and optimistic Sent rows.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	OutboxDeliveryPreparing = "preparing"
	OutboxDeliveryQueued    = "queued"
	OutboxDeliveryClaimed   = "claimed"
	OutboxDeliveryInFlight  = "smtp_in_flight"
	OutboxDeliveryRetryWait = "retry_wait"
	OutboxDeliveryAccepted  = "accepted"
	OutboxDeliveryFailed    = "failed"
	OutboxDeliveryUnknown   = "delivery_unknown"
	OutboxDeliveryCanceled  = "canceled"

	OutboxFilingNotStarted  = "not_started"
	OutboxFilingPending     = "pending"
	OutboxFilingAppending   = "appending"
	OutboxFilingReconciling = "reconciling"
	OutboxFilingRetryWait   = "retry_wait"
	OutboxFilingComplete    = "complete"
	OutboxFilingAttention   = "needs_attention"
)

const outboxSyntheticUIDFloor uint32 = 0xf0000000
const outboxSynchronousRestoreTimeout = 5 * time.Second

// OutboxEnqueue is the validated immutable state committed before compose may
// report that a message was queued.
type OutboxEnqueue struct {
	UserID          int64
	SubmissionKey   string
	SMTPAccountID   int64
	IMAPAccountID   int64
	SentMailboxID   int64
	EnvelopeFrom    string
	RecipientsJSON  string
	MessageIDHeader string
	Blob            BlobRecord
	RawSHA256       string
	RawSize         int64
	Message         CreateMessage
	Attachments     []Attachment
}

// EnqueueOutboxMessage atomically creates the queue row and its optimistic Sent
// message. The raw spool file and blob metadata must already be durable.
func (s *Store) EnqueueOutboxMessage(ctx context.Context, in OutboxEnqueue) (OutboxJob, MessageRecord, bool, error) {
	in.SubmissionKey = strings.TrimSpace(in.SubmissionKey)
	in.EnvelopeFrom = strings.TrimSpace(in.EnvelopeFrom)
	in.MessageIDHeader = strings.TrimSpace(in.MessageIDHeader)
	if in.UserID <= 0 || in.SubmissionKey == "" || in.SMTPAccountID <= 0 ||
		in.IMAPAccountID <= 0 || in.SentMailboxID <= 0 || in.Blob.ID <= 0 ||
		in.Blob.UserID != in.UserID || in.EnvelopeFrom == "" ||
		len(in.SubmissionKey) > 200 || strings.TrimSpace(in.RecipientsJSON) == "" ||
		strings.TrimSpace(in.RawSHA256) == "" || in.RawSize <= 0 {
		return OutboxJob{}, MessageRecord{}, false, errors.New("invalid outbox enqueue scope")
	}
	db, err := s.dataDB(ctx, in.UserID)
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	tx, releaseTx, err := beginDurableOutboxTx(ctx, db)
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	defer releaseTx()

	if existing, found, lookupErr := outboxJobBySubmissionTx(ctx, tx, in.UserID, in.SubmissionKey); lookupErr != nil {
		return OutboxJob{}, MessageRecord{}, false, lookupErr
	} else if found {
		if err := tx.Commit(); err != nil {
			return OutboxJob{}, MessageRecord{}, false, err
		}
		releaseTx()
		msg, err := s.GetMessageForUser(ctx, in.UserID, existing.OptimisticMessageID)
		return existing, msg, false, err
	}
	var smtpOwned, imapOwned, mailboxOwned, blobOwned bool
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM smtp_accounts WHERE user_id = ? AND id = ?),
		EXISTS(SELECT 1 FROM mail_accounts WHERE user_id = ? AND id = ?),
		EXISTS(SELECT 1 FROM mailboxes WHERE user_id = ? AND account_id = ? AND id = ?),
		EXISTS(SELECT 1 FROM blobs WHERE user_id = ? AND id = ? AND path = ?
			AND sha256 = ? AND size = ?)`,
		in.UserID, in.SMTPAccountID,
		in.UserID, in.IMAPAccountID,
		in.UserID, in.IMAPAccountID, in.SentMailboxID,
		in.UserID, in.Blob.ID, in.Blob.Path, in.RawSHA256, in.RawSize).
		Scan(&smtpOwned, &imapOwned, &mailboxOwned, &blobOwned); err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	if !smtpOwned || !imapOwned || !mailboxOwned || !blobOwned {
		return OutboxJob{}, MessageRecord{}, false, errors.New("outbox enqueue references data outside the user scope")
	}

	now := nowUnix()
	result, err := tx.ExecContext(ctx, `INSERT INTO outbox_jobs (
			user_id, submission_key, smtp_account_id, imap_account_id, sent_mailbox_id,
			envelope_from, recipients_json, message_id_header, blob_id, blob_path,
			raw_sha256, raw_size, delivery_state, filing_state, next_attempt_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, in.SubmissionKey, in.SMTPAccountID, in.IMAPAccountID, in.SentMailboxID,
		in.EnvelopeFrom, in.RecipientsJSON, in.MessageIDHeader, in.Blob.ID, in.Blob.Path,
		in.RawSHA256, in.RawSize, OutboxDeliveryQueued, OutboxFilingNotStarted, now, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: outbox_jobs.user_id, outbox_jobs.submission_key") {
			_ = tx.Rollback()
			releaseTx()
			existing, lookupErr := s.GetOutboxJobBySubmission(ctx, in.UserID, in.SubmissionKey)
			if lookupErr != nil {
				return OutboxJob{}, MessageRecord{}, false, lookupErr
			}
			msg, msgErr := s.GetMessageForUser(ctx, in.UserID, existing.OptimisticMessageID)
			return existing, msg, false, msgErr
		}
		return OutboxJob{}, MessageRecord{}, false, err
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	uid, err := reserveOutboxSyntheticUID(ctx, tx, in.UserID, in.IMAPAccountID, in.SentMailboxID, jobID)
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	m := in.Message
	m.UserID = in.UserID
	m.AccountID = in.IMAPAccountID
	m.MailboxID = in.SentMailboxID
	m.BlobID = in.Blob.ID
	m.MessageIDHeader = in.MessageIDHeader
	m.UID = uid
	m.UIDValidity = 0
	m.BlobPath = in.Blob.Path
	if strings.TrimSpace(m.ThreadKey) == "" {
		m.ThreadKey = ThreadKey(m.MessageIDHeader, m.InReplyTo, m.ReferencesHeader, m.Subject)
	}
	if strings.TrimSpace(m.MessageIDHash) == "" {
		m.MessageIDHash = HashedMessageID(m.MessageIDHeader)
	}
	dateUnix := m.Date.UTC().Unix()
	if m.Date.IsZero() {
		dateUnix = now
	}
	internalUnix := m.InternalDate.UTC().Unix()
	if m.InternalDate.IsZero() {
		internalUnix = dateUnix
	}
	messageResult, err := tx.ExecContext(ctx, `INSERT INTO messages (
			user_id, account_id, mailbox_id, blob_id, message_id_header,
			canonical_sha256, message_id_hash, in_reply_to, references_header,
			thread_key, thread_headers_checked_at, subject, language_code,
			from_addr, to_addr, cc_addr, date_unix, internal_date_unix,
			uid, uid_validity, size, blob_path, body_text, body_html,
			is_read, is_starred, has_attachments, is_encrypted, is_signed,
			import_completed_at, outbox_job_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.UserID, m.AccountID, m.MailboxID, m.BlobID, m.MessageIDHeader,
		m.CanonicalSHA256, m.MessageIDHash, m.InReplyTo, m.ReferencesHeader,
		m.ThreadKey, now, m.Subject, strings.ToLower(strings.TrimSpace(m.LanguageCode)),
		m.FromAddr, m.ToAddr, m.CCAddr, dateUnix, internalUnix,
		m.UID, m.Size, m.BlobPath, m.BodyText, m.BodyHTML,
		boolInt(true), boolInt(m.IsStarred), boolInt(m.HasAttachments),
		boolInt(m.IsEncrypted), boolInt(m.IsSigned), now, jobID, now, now)
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	messageID, err := messageResult.LastInsertId()
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO locations
		(user_id, message_id, mailbox_id, uid, created_at) VALUES (?, ?, ?, ?, ?)`,
		in.UserID, messageID, in.SentMailboxID, uid, now); err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	for _, attachment := range in.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments
			(user_id, message_id, blob_id, filename, content_type, content_id,
			 is_inline, size, blob_path, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?)`,
			in.UserID, messageID, in.Blob.ID, attachment.Filename,
			attachment.ContentType, attachment.ContentID, boolInt(attachment.IsInline),
			attachment.Size, now); err != nil {
			return OutboxJob{}, MessageRecord{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_jobs
		SET optimistic_message_id = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`, messageID, now, in.UserID, jobID); err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	releaseTx()
	job, err := s.GetOutboxJobForUser(ctx, in.UserID, jobID)
	if err != nil {
		return OutboxJob{}, MessageRecord{}, false, err
	}
	msg, err := s.GetMessageForUser(ctx, in.UserID, messageID)
	return job, msg, true, err
}

// beginDurableOutboxTx raises synchronous mode only on its reserved
// connection. A successful commit is on stable storage before compose returns
// HTTP 202; restoring NORMAL keeps the rest of Rolltop's write path unchanged.
func beginDurableOutboxTx(ctx context.Context, db *sql.DB) (*sql.Tx, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve durable outbox connection: %w", err)
	}
	restore := func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), outboxSynchronousRestoreTimeout)
		restoreErr := setSQLiteSynchronous(restoreCtx, conn, "NORMAL")
		cancel()
		if restoreErr != nil {
			_ = discardSQLConnection(conn)
			return
		}
		_ = conn.Close()
	}
	if err := setSQLiteSynchronous(ctx, conn, "FULL"); err != nil {
		restore()
		return nil, nil, fmt.Errorf("enable durable SQLite outbox write: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		restore()
		return nil, nil, fmt.Errorf("begin durable SQLite outbox write: %w", err)
	}
	var once sync.Once
	return tx, func() {
		once.Do(func() {
			_ = tx.Rollback()
			restore()
		})
	}, nil
}

func reserveOutboxSyntheticUID(ctx context.Context, tx *sql.Tx, userID, accountID, mailboxID, jobID int64) (uint32, error) {
	if jobID <= 0 || jobID >= int64(^uint32(0)-outboxSyntheticUIDFloor) {
		return 0, errors.New("outbox exhausted its local message identifier range")
	}
	candidate := ^uint32(0) - uint32(jobID)
	for candidate >= outboxSyntheticUIDFloor {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?
		)`, userID, accountID, mailboxID, candidate).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return candidate, nil
		}
		candidate--
	}
	return 0, errors.New("outbox could not reserve a local Sent identifier")
}

func (s *Store) GetOutboxJobForUser(ctx context.Context, userID, jobID int64) (OutboxJob, error) {
	if userID <= 0 || jobID <= 0 {
		return OutboxJob{}, ErrNotFound
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return OutboxJob{}, err
	}
	return scanOutboxJob(db.QueryRowContext(ctx, outboxJobSelect+` WHERE user_id = ? AND id = ?`, userID, jobID))
}

func (s *Store) GetOutboxJobBySubmission(ctx context.Context, userID int64, submissionKey string) (OutboxJob, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return OutboxJob{}, err
	}
	return scanOutboxJob(db.QueryRowContext(ctx, outboxJobSelect+` WHERE user_id = ? AND submission_key = ?`,
		userID, strings.TrimSpace(submissionKey)))
}

func (s *Store) ListOutboxJobsForUser(ctx context.Context, userID int64, limit int) ([]OutboxJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, outboxJobSelect+` WHERE user_id = ?
		ORDER BY CASE WHEN attention_at > acknowledged_at THEN 0
			WHEN completed_at = 0 THEN 1 ELSE 2 END, updated_at DESC, id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []OutboxJob
	for rows.Next() {
		job, err := scanOutboxJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) OutboxSummaryForUser(ctx context.Context, userID int64) (OutboxSummary, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return OutboxSummary{}, err
	}
	var summary OutboxSummary
	err = db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN delivery_state NOT IN ('failed','delivery_unknown','canceled')
			AND (delivery_state <> 'accepted' OR filing_state <> 'complete') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN attention_at > acknowledged_at THEN 1 ELSE 0 END), 0),
		COALESCE(MAX(id), 0)
		FROM outbox_jobs WHERE user_id = ?`, userID).
		Scan(&summary.Active, &summary.NeedsAttention, &summary.LatestID)
	return summary, err
}

func (s *Store) OutboxMessageStatesForUser(ctx context.Context, userID int64, messageIDs []int64) (map[int64]OutboxMessageState, error) {
	out := map[int64]OutboxMessageState{}
	ids := positiveUniqueInt64s(messageIDs)
	if len(ids) == 0 {
		return out, nil
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		args := make([]any, 0, end-start+1)
		args = append(args, userID)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, optimistic_message_id, delivery_state,
			filing_state, last_error, attention_at > acknowledged_at
			FROM outbox_jobs WHERE user_id = ? AND optimistic_message_id IN (`+
			sqlPlaceholders(end-start)+`)
			AND NOT (delivery_state = ? AND filing_state = ?)`,
			append(args, OutboxDeliveryAccepted, OutboxFilingComplete)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var state OutboxMessageState
			if err := rows.Scan(&state.OutboxID, &state.MessageID, &state.DeliveryState,
				&state.FilingState, &state.LastError, &state.Attention); err != nil {
				rows.Close()
				return nil, err
			}
			out[state.MessageID] = state
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func positiveUniqueInt64s(values []int64) []int64 {
	seen := make(map[int64]bool, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value > 0 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// RecoverInterruptedOutboxJobs classifies process-loss boundaries
// conservatively. An abandoned SMTP DATA operation is never retried.
func (s *Store) RecoverInterruptedOutboxJobs(ctx context.Context, userID int64) (int, error) {
	return s.recoverOutboxJobs(ctx, userID, 0)
}

// RecoverExpiredOutboxJobs repairs only abandoned leases, making it safe to
// call periodically while another Rolltop process may be delivering mail.
func (s *Store) RecoverExpiredOutboxJobs(ctx context.Context, userID int64, now time.Time) (int, error) {
	return s.recoverOutboxJobs(ctx, userID, now.UTC().Unix())
}

func (s *Store) recoverOutboxJobs(ctx context.Context, userID, expiredBefore int64) (int, error) {
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	now := nowUnix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	total := int64(0)
	leaseFilter := ""
	argsSuffix := []any{userID, OutboxDeliveryClaimed}
	if expiredBefore > 0 {
		leaseFilter = " AND lease_expires_at > 0 AND lease_expires_at <= ?"
		argsSuffix = append(argsSuffix, expiredBefore)
	}
	res, err := tx.ExecContext(ctx, `UPDATE outbox_jobs SET
		delivery_state = ?, lease_owner = '', lease_expires_at = 0,
		next_attempt_at = ?, updated_at = ?
		WHERE user_id = ? AND delivery_state = ?`+leaseFilter,
		append([]any{OutboxDeliveryQueued, now, now}, argsSuffix...)...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	total += n
	argsSuffix = []any{userID, OutboxDeliveryInFlight}
	if expiredBefore > 0 {
		argsSuffix = append(argsSuffix, expiredBefore)
	}
	res, err = tx.ExecContext(ctx, `UPDATE outbox_jobs SET
		delivery_state = ?, last_error_kind = 'process_interrupted',
		last_error = 'Rolltop restarted while SMTP delivery was in progress. Delivery may have succeeded.',
		attention_at = ?, lease_owner = '', lease_expires_at = 0, updated_at = ?
		WHERE user_id = ? AND delivery_state = ?`+leaseFilter,
		append([]any{OutboxDeliveryUnknown, now, now}, argsSuffix...)...)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n
	argsSuffix = []any{
		userID, OutboxDeliveryAccepted,
		OutboxFilingPending, OutboxFilingRetryWait, OutboxFilingReconciling,
	}
	if expiredBefore > 0 {
		argsSuffix = append(argsSuffix, expiredBefore)
	}
	res, err = tx.ExecContext(ctx, `UPDATE outbox_jobs SET
		next_attempt_at = ?, lease_owner = '', lease_expires_at = 0, updated_at = ?
		WHERE user_id = ? AND delivery_state = ?
		AND filing_state IN (?, ?, ?) AND lease_owner <> ''`+leaseFilter,
		append([]any{now, now}, argsSuffix...)...)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n
	argsSuffix = []any{userID, OutboxDeliveryAccepted, OutboxFilingAppending}
	if expiredBefore > 0 {
		argsSuffix = append(argsSuffix, expiredBefore)
	}
	res, err = tx.ExecContext(ctx, `UPDATE outbox_jobs SET
		filing_state = ?, next_attempt_at = ?, lease_owner = '', lease_expires_at = 0,
		updated_at = ? WHERE user_id = ? AND delivery_state = ?
		AND filing_state = ?`+leaseFilter,
		append([]any{OutboxFilingReconciling, now, now}, argsSuffix...)...)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	total += n
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(total), nil
}

// ClaimNextOutboxJob leases one due job. The compare-and-update keeps multiple
// goroutines or a future multi-process deployment from dispatching the same row.
func (s *Store) ClaimNextOutboxJob(ctx context.Context, userID int64, owner string, now time.Time, lease time.Duration) (OutboxJob, bool, error) {
	owner = strings.TrimSpace(owner)
	if userID <= 0 || owner == "" {
		return OutboxJob{}, false, errors.New("invalid outbox lease scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return OutboxJob{}, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxJob{}, false, err
	}
	defer tx.Rollback()
	nowUnixValue := now.UTC().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM outbox_jobs
		WHERE user_id = ? AND next_attempt_at <= ?
		AND (lease_owner = '' OR lease_expires_at <= ?)
		AND (
			delivery_state IN (?, ?)
			OR (delivery_state = ? AND filing_state IN (?, ?, ?))
		)
		ORDER BY next_attempt_at, id LIMIT 1`,
		userID, nowUnixValue, nowUnixValue,
		OutboxDeliveryQueued, OutboxDeliveryRetryWait,
		OutboxDeliveryAccepted, OutboxFilingPending, OutboxFilingRetryWait, OutboxFilingReconciling).
		Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxJob{}, false, nil
	}
	if err != nil {
		return OutboxJob{}, false, err
	}
	expires := now.Add(lease).UTC().Unix()
	res, err := tx.ExecContext(ctx, `UPDATE outbox_jobs SET
		lease_owner = ?, lease_expires_at = ?,
		delivery_state = CASE WHEN delivery_state IN (?, ?) THEN ? ELSE delivery_state END,
		updated_at = ?
		WHERE user_id = ? AND id = ? AND (lease_owner = '' OR lease_expires_at <= ?)`,
		owner, expires, OutboxDeliveryQueued, OutboxDeliveryRetryWait,
		OutboxDeliveryClaimed, nowUnixValue, userID, id, nowUnixValue)
	if err != nil {
		return OutboxJob{}, false, err
	}
	changed, err := res.RowsAffected()
	if err != nil || changed != 1 {
		return OutboxJob{}, false, err
	}
	job, err := scanOutboxJob(tx.QueryRowContext(ctx, outboxJobSelect+` WHERE user_id = ? AND id = ?`, userID, id))
	if err != nil {
		return OutboxJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) MarkOutboxSMTPInFlight(ctx context.Context, userID, jobID int64, owner string) error {
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `delivery_state = ?, attempt_count = attempt_count + 1,
		last_error_kind = '', last_error = '', updated_at = ?`, OutboxDeliveryInFlight, nowUnix())
}

// RenewOutboxLease keeps one slow network operation owned by the same worker.
// It never acquires an unowned job and therefore cannot revive terminal work.
func (s *Store) RenewOutboxLease(ctx context.Context, userID, jobID int64, owner string, expiresAt time.Time) error {
	if expiresAt.IsZero() {
		return errors.New("invalid outbox lease expiry")
	}
	return s.updateClaimedOutbox(ctx, userID, jobID, owner,
		`lease_expires_at = ?, updated_at = ?`, expiresAt.UTC().Unix(), nowUnix())
}

func (s *Store) MarkOutboxSMTPAccepted(ctx context.Context, userID, jobID int64, owner string) error {
	now := nowUnix()
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `delivery_state = ?, filing_state = ?,
		smtp_accepted_at = ?, next_attempt_at = ?, last_error_kind = '', last_error = '',
		lease_owner = '', lease_expires_at = 0, updated_at = ?`,
		OutboxDeliveryAccepted, OutboxFilingPending, now, now, now)
}

func (s *Store) RetryOutboxDelivery(ctx context.Context, userID, jobID int64, owner, kind, message string, next time.Time) error {
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `delivery_state = ?, next_attempt_at = ?,
		last_error_kind = ?, last_error = ?, lease_owner = '', lease_expires_at = 0, updated_at = ?`,
		OutboxDeliveryRetryWait, next.UTC().Unix(), cleanOutboxText(kind, 80),
		cleanOutboxText(message, 1000), nowUnix())
}

func (s *Store) FailOutboxDelivery(ctx context.Context, userID, jobID int64, owner, state, kind, message string) error {
	if state != OutboxDeliveryFailed && state != OutboxDeliveryUnknown {
		return errors.New("invalid terminal outbox delivery state")
	}
	now := nowUnix()
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `delivery_state = ?,
		last_error_kind = ?, last_error = ?, attention_at = ?,
		lease_owner = '', lease_expires_at = 0, updated_at = ?`,
		state, cleanOutboxText(kind, 80), cleanOutboxText(message, 1000), now, now)
}

func (s *Store) SetOutboxAppendBoundary(ctx context.Context, userID, jobID int64, owner string, uidValidity, uidNext uint32) error {
	if uidValidity == 0 || uidNext == 0 {
		return errors.New("invalid outbox append boundary")
	}
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `append_uid_validity = ?,
		append_uid_next = ?, filing_state = ?, updated_at = ?`,
		uidValidity, uidNext, OutboxFilingAppending, nowUnix())
}

func (s *Store) MarkOutboxFilingAppending(ctx context.Context, userID, jobID int64, owner string) error {
	return s.updateClaimedOutbox(ctx, userID, jobID, owner,
		`filing_state = ?, updated_at = ?`, OutboxFilingAppending, nowUnix())
}

func (s *Store) RetryOutboxFiling(ctx context.Context, userID, jobID int64, owner, state, kind, message string, next time.Time, attention bool) error {
	if state != OutboxFilingRetryWait && state != OutboxFilingReconciling && state != OutboxFilingAttention {
		return errors.New("invalid outbox filing retry state")
	}
	now := nowUnix()
	attentionAt := int64(0)
	if attention {
		attentionAt = now
	}
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `filing_state = ?,
		filing_attempt_count = filing_attempt_count + 1,
		next_attempt_at = ?, last_error_kind = ?, last_error = ?,
		attention_at = CASE WHEN ? > 0 THEN ? ELSE attention_at END,
		lease_owner = '', lease_expires_at = 0, updated_at = ?`,
		state, next.UTC().Unix(), cleanOutboxText(kind, 80), cleanOutboxText(message, 1000),
		attentionAt, attentionAt, now)
}

func (s *Store) CompleteOutboxFiling(ctx context.Context, userID, jobID int64, owner string, uid, uidValidity uint32) error {
	now := nowUnix()
	return s.updateClaimedOutbox(ctx, userID, jobID, owner, `filing_state = ?,
		appended_uid = ?, appended_uid_validity = ?, completed_at = ?,
		next_attempt_at = 0, last_error_kind = '', last_error = '',
		lease_owner = '', lease_expires_at = 0, updated_at = ?`,
		OutboxFilingComplete, uid, uidValidity, now, now)
}

func (s *Store) RecordOutboxAttempt(ctx context.Context, userID, jobID int64, attempt int, phase, outcome, kind, message string, started time.Time) error {
	if userID <= 0 || jobID <= 0 || attempt <= 0 {
		return errors.New("invalid outbox attempt")
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT OR REPLACE INTO outbox_attempts
		(user_id, outbox_job_id, attempt, phase, outcome, error_kind, error_text, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, jobID, attempt, cleanOutboxText(phase, 80), cleanOutboxText(outcome, 80),
		cleanOutboxText(kind, 80), cleanOutboxText(message, 1000),
		started.UTC().Unix(), nowUnix())
	return err
}

func (s *Store) AcknowledgeOutboxJob(ctx context.Context, userID, jobID int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE outbox_jobs
		SET acknowledged_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
		nowUnix(), nowUnix(), userID, jobID)
	return requireAffected(res, err)
}

func (s *Store) RequeueOutboxJob(ctx context.Context, userID, jobID int64, allowUnknown bool) error {
	db := s.mustDataDB(ctx, userID)
	now := nowUnix()
	states := []string{OutboxDeliveryFailed}
	if allowUnknown {
		states = append(states, OutboxDeliveryUnknown)
	}
	res, err := db.ExecContext(ctx, `UPDATE outbox_jobs SET delivery_state = ?,
		next_attempt_at = ?, last_error_kind = '', last_error = '',
		attention_at = 0, acknowledged_at = 0, lease_owner = '', lease_expires_at = 0,
		updated_at = ? WHERE user_id = ? AND id = ? AND delivery_state IN (`+
		sqlPlaceholders(len(states))+`)`,
		append([]any{OutboxDeliveryQueued, now, now, userID, jobID}, stringsToAny(states)...)...)
	return requireAffected(res, err)
}

func (s *Store) RequeueOutboxFiling(ctx context.Context, userID, jobID int64) error {
	now := nowUnix()
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE outbox_jobs SET
		filing_state = ?, filing_attempt_count = 0, next_attempt_at = ?, last_error_kind = '', last_error = '',
		attention_at = 0, acknowledged_at = 0, lease_owner = '', lease_expires_at = 0,
		updated_at = ? WHERE user_id = ? AND id = ? AND delivery_state = ?
			AND filing_state = ?`,
		OutboxFilingRetryWait, now, now, userID, jobID,
		OutboxDeliveryAccepted, OutboxFilingAttention)
	return requireAffected(res, err)
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func (s *Store) CancelOutboxJob(ctx context.Context, userID, jobID int64) (int64, BlobRecord, error) {
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, BlobRecord{}, err
	}
	defer tx.Rollback()
	var messageID int64
	var blob BlobRecord
	var blobCreated int64
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT o.optimistic_message_id, o.delivery_state,
		b.id, b.user_id, b.kind, b.path, b.sha256, b.size, b.created_at
		FROM outbox_jobs o JOIN blobs b ON b.user_id = o.user_id AND b.id = o.blob_id
		WHERE o.user_id = ? AND o.id = ?`, userID, jobID).
		Scan(&messageID, &state, &blob.ID, &blob.UserID, &blob.Kind, &blob.Path,
			&blob.SHA256, &blob.Size, &blobCreated); err != nil {
		return 0, BlobRecord{}, err
	}
	blob.CreatedAt = unixTime(blobCreated)
	if state != OutboxDeliveryQueued && state != OutboxDeliveryRetryWait &&
		state != OutboxDeliveryFailed {
		return 0, BlobRecord{}, errors.New("message can no longer be canceled")
	}
	if messageID > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages
			WHERE user_id = ? AND id = ? AND outbox_job_id = ?`, userID, messageID, jobID); err != nil {
			return 0, BlobRecord{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_jobs
		WHERE user_id = ? AND id = ?`, userID, jobID); err != nil {
		return 0, BlobRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, BlobRecord{}, err
	}
	return messageID, blob, nil
}

// PurgeOutboxJobsForIMAPAccount removes queue/history rows tied to an IMAP
// account before the explicit local-account purge deletes its mailboxes.
func (s *Store) PurgeOutboxJobsForIMAPAccount(ctx context.Context, userID, accountID int64) ([]BlobRecord, []int64, error) {
	return s.purgeOutboxJobs(ctx, userID, "imap_account_id", accountID)
}

// PurgeOutboxJobsForSMTPAccount cancels any still-local optimistic messages
// before an outgoing server is explicitly removed.
func (s *Store) PurgeOutboxJobsForSMTPAccount(ctx context.Context, userID, accountID int64) ([]BlobRecord, []int64, error) {
	return s.purgeOutboxJobs(ctx, userID, "smtp_account_id", accountID)
}

func (s *Store) purgeOutboxJobs(ctx context.Context, userID int64, column string, accountID int64) ([]BlobRecord, []int64, error) {
	if userID <= 0 || accountID <= 0 ||
		(column != "imap_account_id" && column != "smtp_account_id") {
		return nil, nil, errors.New("invalid outbox account purge scope")
	}
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var busy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_jobs
		WHERE user_id = ? AND `+column+` = ?
			AND (lease_owner <> '' OR delivery_state IN (?, ?)))`,
		userID, accountID, OutboxDeliveryClaimed, OutboxDeliveryInFlight).Scan(&busy); err != nil {
		return nil, nil, err
	}
	if busy {
		return nil, nil, ErrOutboxBusy
	}
	stateFilter := ""
	stateArgs := []any{userID, accountID}
	if column == "smtp_account_id" {
		stateFilter = ` AND delivery_state IN (?, ?, ?, ?)`
		stateArgs = append(stateArgs, OutboxDeliveryPreparing, OutboxDeliveryQueued,
			OutboxDeliveryRetryWait, OutboxDeliveryFailed)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT b.id, b.user_id, b.kind,
		b.path, b.sha256, b.size, b.created_at
		FROM outbox_jobs o JOIN blobs b ON b.user_id = o.user_id AND b.id = o.blob_id
		WHERE o.user_id = ? AND o.`+column+` = ?`+stateFilter, stateArgs...)
	if err != nil {
		return nil, nil, err
	}
	var blobs []BlobRecord
	for rows.Next() {
		var blob BlobRecord
		var created int64
		if err := rows.Scan(&blob.ID, &blob.UserID, &blob.Kind, &blob.Path,
			&blob.SHA256, &blob.Size, &created); err != nil {
			rows.Close()
			return nil, nil, err
		}
		blob.CreatedAt = unixTime(created)
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	messageRows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m
		JOIN outbox_jobs o ON o.user_id = m.user_id AND o.id = m.outbox_job_id
		WHERE o.user_id = ? AND o.`+column+` = ?`+stateFilter, stateArgs...)
	if err != nil {
		return nil, nil, err
	}
	var messageIDs []int64
	for messageRows.Next() {
		var id int64
		if err := messageRows.Scan(&id); err != nil {
			messageRows.Close()
			return nil, nil, err
		}
		messageIDs = append(messageIDs, id)
	}
	if err := messageRows.Err(); err != nil {
		messageRows.Close()
		return nil, nil, err
	}
	if err := messageRows.Close(); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ?
		AND outbox_job_id IN (SELECT o.id FROM outbox_jobs o WHERE o.user_id = ? AND o.`+
		column+` = ?`+stateFilter+`)`, append([]any{userID}, stateArgs...)...); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_jobs WHERE user_id = ? AND `+
		column+` = ?`+stateFilter, stateArgs...); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return blobs, messageIDs, nil
}

// PurgeCompletedOutboxHistory releases old queue-only references while leaving
// the promoted Sent message and its normal blob-retention lifecycle intact.
func (s *Store) PurgeCompletedOutboxHistory(ctx context.Context, userID int64, completedBefore time.Time) ([]BlobRecord, error) {
	if userID <= 0 || completedBefore.IsZero() {
		return nil, errors.New("invalid completed outbox purge scope")
	}
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cutoff := completedBefore.UTC().Unix()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT b.id, b.user_id, b.kind,
		b.path, b.sha256, b.size, b.created_at
		FROM outbox_jobs o JOIN blobs b ON b.user_id = o.user_id AND b.id = o.blob_id
		WHERE o.user_id = ? AND o.delivery_state = ? AND o.filing_state = ?
			AND o.completed_at > 0 AND o.completed_at <= ?`,
		userID, OutboxDeliveryAccepted, OutboxFilingComplete, cutoff)
	if err != nil {
		return nil, err
	}
	var blobs []BlobRecord
	for rows.Next() {
		var blob BlobRecord
		var created int64
		if err := rows.Scan(&blob.ID, &blob.UserID, &blob.Kind, &blob.Path,
			&blob.SHA256, &blob.Size, &created); err != nil {
			rows.Close()
			return nil, err
		}
		blob.CreatedAt = unixTime(created)
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_jobs
		WHERE user_id = ? AND delivery_state = ? AND filing_state = ?
			AND completed_at > 0 AND completed_at <= ?`,
		userID, OutboxDeliveryAccepted, OutboxFilingComplete, cutoff); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return blobs, nil
}

// PromoteOutboxMessage replaces the synthetic local UID with the confirmed
// server UID while retaining the message's local ID and URL.
func (s *Store) PromoteOutboxMessage(ctx context.Context, userID, jobID int64, remoteUID, remoteUIDValidity uint32) (*MessageRecord, error) {
	if userID <= 0 || jobID <= 0 || remoteUID == 0 || remoteUIDValidity == 0 {
		return nil, errors.New("invalid optimistic Sent promotion")
	}
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var messageID, accountID, mailboxID int64
	if err := tx.QueryRowContext(ctx, `SELECT optimistic_message_id, imap_account_id, sent_mailbox_id
		FROM outbox_jobs WHERE user_id = ? AND id = ?`, userID, jobID).
		Scan(&messageID, &accountID, &mailboxID); err != nil {
		return nil, err
	}
	var currentUID uint32
	var currentUIDValidity, currentOutboxJobID int64
	if err := tx.QueryRowContext(ctx, `SELECT uid, uid_validity, outbox_job_id
		FROM messages WHERE user_id = ? AND id = ?`, userID, messageID).
		Scan(&currentUID, &currentUIDValidity, &currentOutboxJobID); err != nil {
		return nil, err
	}
	if currentOutboxJobID == 0 && currentUID == remoteUID &&
		currentUIDValidity == int64(remoteUIDValidity) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if currentOutboxJobID != jobID {
		return nil, errors.New("optimistic Sent message is owned by another outbox job")
	}
	var displaced *MessageRecord
	var displacedID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ? AND id <> ?`,
		userID, accountID, mailboxID, remoteUID, messageID).Scan(&displacedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if displacedID > 0 {
		var msg MessageRecord
		if err := tx.QueryRowContext(ctx, `SELECT id, user_id, blob_id, blob_path
			FROM messages WHERE user_id = ? AND id = ?`, userID, displacedID).
			Scan(&msg.ID, &msg.UserID, &msg.BlobID, &msg.BlobPath); err != nil {
			return nil, err
		}
		displaced = &msg
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND id = ?`,
			userID, displacedID); err != nil {
			return nil, err
		}
	}
	now := nowUnix()
	res, err := tx.ExecContext(ctx, `UPDATE messages SET uid = ?, uid_validity = ?,
		outbox_job_id = 0, updated_at = ?
		WHERE user_id = ? AND id = ? AND outbox_job_id = ?`,
		remoteUID, remoteUIDValidity, now, userID, messageID, jobID)
	if err != nil {
		return nil, err
	}
	if changed, err := res.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE locations SET uid = ?
		WHERE user_id = ? AND message_id = ? AND mailbox_id = ?`,
		remoteUID, userID, messageID, mailboxID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET
		last_uid = CASE WHEN last_uid < ? THEN ? ELSE last_uid END, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND id = ? AND uidvalidity = ?`,
		remoteUID, remoteUID, now, userID, accountID, mailboxID, remoteUIDValidity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return displaced, nil
}

func (s *Store) updateClaimedOutbox(ctx context.Context, userID, jobID int64, owner, setClause string, args ...any) error {
	if userID <= 0 || jobID <= 0 || strings.TrimSpace(owner) == "" {
		return errors.New("invalid claimed outbox update")
	}
	args = append(args, userID, jobID, owner)
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE outbox_jobs SET `+setClause+
		` WHERE user_id = ? AND id = ? AND lease_owner = ?`, args...)
	return requireAffected(res, err)
}

func requireAffected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func cleanOutboxText(value string, limit int) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return -1
		}
		return r
	}, value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

const outboxJobSelect = `SELECT id, user_id, submission_key, smtp_account_id,
	imap_account_id, sent_mailbox_id, optimistic_message_id, envelope_from,
	recipients_json, message_id_header, blob_id, blob_path, raw_sha256, raw_size,
	delivery_state, filing_state, attempt_count, filing_attempt_count, next_attempt_at, lease_owner,
	lease_expires_at, append_uid_validity, append_uid_next, appended_uid,
	appended_uid_validity, last_error_kind, last_error, attention_at,
	acknowledged_at, smtp_accepted_at, completed_at, created_at, updated_at
	FROM outbox_jobs`

type outboxScanner interface {
	Scan(dest ...any) error
}

func scanOutboxJob(row outboxScanner) (OutboxJob, error) {
	var job OutboxJob
	var nextAttempt, leaseExpires, attention, acknowledged, accepted, completed, created, updated int64
	err := row.Scan(&job.ID, &job.UserID, &job.SubmissionKey, &job.SMTPAccountID,
		&job.IMAPAccountID, &job.SentMailboxID, &job.OptimisticMessageID,
		&job.EnvelopeFrom, &job.RecipientsJSON, &job.MessageIDHeader, &job.BlobID,
		&job.BlobPath, &job.RawSHA256, &job.RawSize, &job.DeliveryState,
		&job.FilingState, &job.AttemptCount, &job.FilingAttemptCount, &nextAttempt, &job.LeaseOwner,
		&leaseExpires, &job.AppendUIDValidity, &job.AppendUIDNext, &job.AppendedUID,
		&job.AppendedUIDValidity, &job.LastErrorKind, &job.LastError, &attention,
		&acknowledged, &accepted, &completed, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OutboxJob{}, ErrNotFound
		}
		return OutboxJob{}, err
	}
	job.NextAttemptAt = unixTime(nextAttempt)
	job.LeaseExpiresAt = unixTime(leaseExpires)
	job.AttentionAt = unixTime(attention)
	job.AcknowledgedAt = unixTime(acknowledged)
	job.SMTPAcceptedAt = unixTime(accepted)
	job.CompletedAt = unixTime(completed)
	job.CreatedAt = unixTime(created)
	job.UpdatedAt = unixTime(updated)
	return job, nil
}

func outboxJobBySubmissionTx(ctx context.Context, tx *sql.Tx, userID int64, key string) (OutboxJob, bool, error) {
	job, err := scanOutboxJob(tx.QueryRowContext(ctx, outboxJobSelect+
		` WHERE user_id = ? AND submission_key = ?`, userID, key))
	if errors.Is(err, ErrNotFound) {
		return OutboxJob{}, false, nil
	}
	return job, err == nil, err
}
