// File overview: Crash-safe preservation of user-owned message state across UIDVALIDITY cache rebuilds.

package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

// snapshotMailboxGenerationStateTx copies non-derived state before message rows
// are deleted. The journal is keyed by content fingerprints rather than a UID
// alone because UIDs may be reused after UIDVALIDITY changes.
func snapshotMailboxGenerationStateTx(ctx context.Context, tx *sql.Tx, userID, accountID, mailboxID,
	targetUIDValidity, arrivalUIDFloor, now int64,
) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_generation_rebuilds
		(user_id, account_id, mailbox_id, target_uid_validity, arrival_uid_floor, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, account_id, mailbox_id) DO UPDATE SET
			arrival_uid_floor = CASE
				WHEN mailbox_generation_rebuilds.target_uid_validity = excluded.target_uid_validity
					AND mailbox_generation_rebuilds.arrival_uid_floor > 0
				THEN mailbox_generation_rebuilds.arrival_uid_floor
				ELSE excluded.arrival_uid_floor
			END,
			target_uid_validity = excluded.target_uid_validity,
			updated_at = excluded.updated_at`,
		userID, accountID, mailboxID, targetUIDValidity, arrivalUIDFloor, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_generation_rebuild_messages
		SET target_uid_validity = ?, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?`,
		targetUIDValidity, now, userID, accountID, mailboxID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_generation_rebuild_messages
		(user_id, account_id, mailbox_id, target_uid_validity, source_message_id, source_uid,
		 raw_sha256, canonical_sha256, message_id_hash, internal_date_unix, message_size,
		 is_read, read_sync_pending, is_starred, star_sync_pending,
		 has_snooze, snooze_id, snooze_thread_key, snooze_generation, snoozed_until,
		 snooze_reminded_at, snooze_created_at, snooze_updated_at,
		 has_new_mail_event, new_mail_event_id, new_mail_from_addr, new_mail_subject,
		 new_mail_created_at, created_at, updated_at)
		SELECT message.user_id, message.account_id, message.mailbox_id, ?, message.id, message.uid,
			blob.sha256, message.canonical_sha256, message.message_id_hash,
			message.internal_date_unix, message.size,
			message.is_read, message.read_sync_pending, message.is_starred, message.star_sync_pending,
			CASE WHEN snooze.id IS NULL THEN 0 ELSE 1 END, COALESCE(snooze.id, 0),
			COALESCE(snooze.thread_key, ''), COALESCE(snooze.generation, 0),
			COALESCE(snooze.snoozed_until, 0), COALESCE(snooze.reminded_at, 0),
			COALESCE(snooze.created_at, 0), COALESCE(snooze.updated_at, 0),
			CASE WHEN event.id IS NULL THEN 0 ELSE 1 END, COALESCE(event.id, 0),
			COALESCE(event.from_addr, ''), COALESCE(event.subject, ''), COALESCE(event.created_at, 0),
			?, ?
		FROM messages message
		JOIN blobs blob ON blob.user_id = message.user_id AND blob.id = message.blob_id
		LEFT JOIN message_snoozes snooze ON snooze.user_id = message.user_id AND snooze.message_id = message.id
		LEFT JOIN new_mail_events event ON event.user_id = message.user_id AND event.message_id = message.id
		LEFT JOIN pending_inbox_arrivals arrival ON arrival.user_id = message.user_id
			AND arrival.message_id = message.id AND arrival.classification = 'pending'
		WHERE message.user_id = ? AND message.account_id = ? AND message.mailbox_id = ?
		AND message.outbox_job_id = 0
		AND (message.read_sync_pending <> 0 OR message.star_sync_pending <> 0
			OR snooze.id IS NOT NULL OR event.id IS NOT NULL OR arrival.id IS NOT NULL
			OR EXISTS (SELECT 1 FROM snooze_reminder_events reminder
				WHERE reminder.user_id = message.user_id AND reminder.message_id = message.id)
			OR EXISTS (SELECT 1 FROM plugin_one_click_unsubscribe_sends send
				WHERE send.user_id = message.user_id AND send.message_id = message.id))
		ON CONFLICT(user_id, source_message_id) DO UPDATE SET
			target_uid_validity = excluded.target_uid_validity,
			source_uid = excluded.source_uid,
			raw_sha256 = excluded.raw_sha256,
			canonical_sha256 = excluded.canonical_sha256,
			message_id_hash = excluded.message_id_hash,
			internal_date_unix = excluded.internal_date_unix,
			message_size = excluded.message_size,
			is_read = excluded.is_read,
			read_sync_pending = excluded.read_sync_pending,
			is_starred = excluded.is_starred,
			star_sync_pending = excluded.star_sync_pending,
			has_snooze = excluded.has_snooze,
			snooze_id = excluded.snooze_id,
			snooze_thread_key = excluded.snooze_thread_key,
			snooze_generation = excluded.snooze_generation,
			snoozed_until = excluded.snoozed_until,
			snooze_reminded_at = excluded.snooze_reminded_at,
			snooze_created_at = excluded.snooze_created_at,
			snooze_updated_at = excluded.snooze_updated_at,
			has_new_mail_event = excluded.has_new_mail_event,
			new_mail_event_id = excluded.new_mail_event_id,
			new_mail_from_addr = excluded.new_mail_from_addr,
			new_mail_subject = excluded.new_mail_subject,
			new_mail_created_at = excluded.new_mail_created_at,
			updated_at = excluded.updated_at`,
		targetUIDValidity, now, now, userID, accountID, mailboxID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_generation_rebuild_inbox_arrivals
		(rebuild_message_id, user_id, original_arrival_id, sync_run_id, classification,
		 raw_sha256, canonical_sha256, message_id_hash, internal_date_unix, message_size,
		 matched_transfer_id, matched_expunged_id, created_at, available_at, finalized_at, updated_at)
		SELECT rebuild.id, arrival.user_id, arrival.id, arrival.sync_run_id, arrival.classification,
			arrival.raw_sha256, arrival.canonical_sha256, arrival.message_id_hash,
			arrival.internal_date_unix, arrival.message_size,
			arrival.matched_transfer_id, arrival.matched_expunged_id,
			arrival.created_at, arrival.available_at, arrival.finalized_at, arrival.updated_at
		FROM pending_inbox_arrivals arrival
		JOIN mailbox_generation_rebuild_messages rebuild
			ON rebuild.user_id = arrival.user_id AND rebuild.source_message_id = arrival.message_id
		WHERE rebuild.user_id = ? AND rebuild.account_id = ? AND rebuild.mailbox_id = ?
			AND rebuild.target_uid_validity = ? AND arrival.classification = 'pending'
		ON CONFLICT(rebuild_message_id) DO UPDATE SET
			original_arrival_id = excluded.original_arrival_id,
			sync_run_id = excluded.sync_run_id,
			classification = excluded.classification,
			raw_sha256 = excluded.raw_sha256,
			canonical_sha256 = excluded.canonical_sha256,
			message_id_hash = excluded.message_id_hash,
			internal_date_unix = excluded.internal_date_unix,
			message_size = excluded.message_size,
			matched_transfer_id = excluded.matched_transfer_id,
			matched_expunged_id = excluded.matched_expunged_id,
			created_at = excluded.created_at,
			available_at = excluded.available_at,
			finalized_at = excluded.finalized_at,
			updated_at = excluded.updated_at`, userID, accountID, mailboxID, targetUIDValidity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mailbox_generation_rebuild_snooze_events
		(rebuild_message_id, user_id, original_event_id, snooze_generation, from_addr, subject, due_at, created_at)
		SELECT rebuild.id, reminder.user_id, reminder.id, reminder.snooze_generation,
			reminder.from_addr, reminder.subject, reminder.due_at, reminder.created_at
		FROM snooze_reminder_events reminder
		JOIN mailbox_generation_rebuild_messages rebuild
			ON rebuild.user_id = reminder.user_id AND rebuild.source_message_id = reminder.message_id
		WHERE rebuild.user_id = ? AND rebuild.account_id = ? AND rebuild.mailbox_id = ?
			AND rebuild.target_uid_validity = ?`, userID, accountID, mailboxID, targetUIDValidity); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mailbox_generation_rebuild_unsubscribe_sends
		(rebuild_message_id, user_id, original_send_id, sender, unsubscribe_url, sent_at, created_at)
		SELECT rebuild.id, send.user_id, send.id, send.sender, send.unsubscribe_url, send.sent_at, send.created_at
		FROM plugin_one_click_unsubscribe_sends send
		JOIN mailbox_generation_rebuild_messages rebuild
			ON rebuild.user_id = send.user_id AND rebuild.source_message_id = send.message_id
		WHERE rebuild.user_id = ? AND rebuild.account_id = ? AND rebuild.mailbox_id = ?
			AND rebuild.target_uid_validity = ?`, userID, accountID, mailboxID, targetUIDValidity)
	return err
}

type mailboxGenerationRebuildState struct {
	id                    int64
	strength              int
	isRead                int
	readSyncPending       int
	isStarred             int
	starSyncPending       int
	hasSnooze             int
	snoozeID              int64
	snoozeThreadKey       string
	snoozeGeneration      int64
	snoozedUntil          int64
	snoozeRemindedAt      int64
	snoozeCreatedAt       int64
	snoozeUpdatedAt       int64
	hasNewMailEvent       int
	newMailEventID        int64
	newMailFromAddr       string
	newMailSubject        string
	newMailEventCreatedAt int64
}

type mailboxGenerationRebuildArrivalState struct {
	id                int64
	syncRunID         int64
	classification    ArrivalClassification
	rawSHA256         string
	canonicalSHA256   string
	messageIDHash     string
	internalDateUnix  int64
	messageSize       int64
	matchedTransferID int64
	matchedExpungedID int64
	createdAt         int64
	availableAt       int64
	finalizedAt       int64
	updatedAt         int64
}

// restoreMailboxGenerationStateTx reattaches state only when one journal row is
// the uniquely strongest match. Ambiguous duplicate messages fail closed and
// leave the journal untouched until the mailbox rebuild is finalized.
func restoreMailboxGenerationStateTx(ctx context.Context, tx *sql.Tx, messageID int64, m CreateMessage) error {
	if m.UIDValidity <= 0 || messageID <= 0 {
		return nil
	}
	var rawSHA string
	if err := tx.QueryRowContext(ctx, `SELECT sha256 FROM blobs WHERE user_id = ? AND id = ?`, m.UserID, m.BlobID).Scan(&rawSHA); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,
		CASE
			WHEN raw_sha256 <> '' AND raw_sha256 = ? COLLATE BINARY THEN 0
			WHEN canonical_sha256 <> '' AND canonical_sha256 = ? COLLATE BINARY THEN 1
			ELSE 2
		END AS strength,
		is_read, read_sync_pending, is_starred, star_sync_pending,
		has_snooze, snooze_id, snooze_thread_key, snooze_generation, snoozed_until,
		snooze_reminded_at, snooze_created_at, snooze_updated_at,
		has_new_mail_event, new_mail_event_id, new_mail_from_addr, new_mail_subject, new_mail_created_at
		FROM mailbox_generation_rebuild_messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?
		AND ((raw_sha256 <> '' AND raw_sha256 = ? COLLATE BINARY)
			OR (canonical_sha256 <> '' AND canonical_sha256 = ? COLLATE BINARY)
			OR (message_id_hash <> '' AND message_id_hash = ? COLLATE BINARY
				AND internal_date_unix = ? AND message_size = ?))
		ORDER BY strength, id LIMIT 2`,
		rawSHA, m.CanonicalSHA256,
		m.UserID, m.AccountID, m.MailboxID, m.UIDValidity,
		rawSHA, m.CanonicalSHA256, m.MessageIDHash,
		m.InternalDate.UTC().Unix(), m.Size)
	if err != nil {
		return err
	}
	defer rows.Close()
	var candidates []mailboxGenerationRebuildState
	for rows.Next() {
		var state mailboxGenerationRebuildState
		if err := rows.Scan(&state.id, &state.strength, &state.isRead, &state.readSyncPending,
			&state.isStarred, &state.starSyncPending, &state.hasSnooze, &state.snoozeID,
			&state.snoozeThreadKey, &state.snoozeGeneration, &state.snoozedUntil,
			&state.snoozeRemindedAt, &state.snoozeCreatedAt, &state.snoozeUpdatedAt,
			&state.hasNewMailEvent, &state.newMailEventID, &state.newMailFromAddr,
			&state.newMailSubject, &state.newMailEventCreatedAt); err != nil {
			return err
		}
		candidates = append(candidates, state)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(candidates) == 0 || (len(candidates) > 1 && candidates[0].strength == candidates[1].strength) {
		return nil
	}
	state := candidates[0]
	var arrivalState mailboxGenerationRebuildArrivalState
	hasPendingArrival := true
	err = tx.QueryRowContext(ctx, `SELECT original_arrival_id, COALESCE(sync_run_id, 0), classification,
		raw_sha256, canonical_sha256, message_id_hash, internal_date_unix, message_size,
		matched_transfer_id, matched_expunged_id, created_at, available_at, finalized_at, updated_at
		FROM mailbox_generation_rebuild_inbox_arrivals
		WHERE user_id = ? AND rebuild_message_id = ?`, m.UserID, state.id).Scan(
		&arrivalState.id, &arrivalState.syncRunID, &arrivalState.classification,
		&arrivalState.rawSHA256, &arrivalState.canonicalSHA256, &arrivalState.messageIDHash,
		&arrivalState.internalDateUnix, &arrivalState.messageSize,
		&arrivalState.matchedTransferID, &arrivalState.matchedExpungedID,
		&arrivalState.createdAt, &arrivalState.availableAt, &arrivalState.finalizedAt,
		&arrivalState.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		hasPendingArrival = false
	} else if err != nil {
		return err
	}
	if state.readSyncPending != 0 || state.starSyncPending != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET
			is_read = CASE WHEN ? <> 0 THEN ? ELSE is_read END,
			read_sync_pending = CASE WHEN ? <> 0 THEN 1 ELSE read_sync_pending END,
			is_starred = CASE WHEN ? <> 0 THEN ? ELSE is_starred END,
			star_sync_pending = CASE WHEN ? <> 0 THEN 1 ELSE star_sync_pending END
			WHERE user_id = ? AND id = ?`,
			state.readSyncPending, state.isRead, state.readSyncPending,
			state.starSyncPending, state.isStarred, state.starSyncPending,
			m.UserID, messageID); err != nil {
			return err
		}
	}
	if state.hasSnooze != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_snoozes
			(id, user_id, message_id, thread_key, generation, snoozed_until, reminded_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, state.snoozeID, m.UserID, messageID,
			state.snoozeThreadKey, state.snoozeGeneration, state.snoozedUntil,
			state.snoozeRemindedAt, state.snoozeCreatedAt, state.snoozeUpdatedAt); err != nil {
			return err
		}
	}
	if state.hasNewMailEvent != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO new_mail_events
			(id, user_id, message_id, from_addr, subject, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			state.newMailEventID, m.UserID, messageID, state.newMailFromAddr,
			state.newMailSubject, state.newMailEventCreatedAt); err != nil {
			return err
		}
	}
	if hasPendingArrival {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pending_inbox_arrivals
			(id, user_id, account_id, mailbox_id, message_id, sync_run_id, classification,
			 raw_sha256, canonical_sha256, message_id_hash, internal_date_unix, message_size,
			 matched_transfer_id, matched_expunged_id, created_at, available_at, finalized_at, updated_at)
			VALUES (?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			arrivalState.id, m.UserID, m.AccountID, m.MailboxID, messageID,
			arrivalState.syncRunID, arrivalState.classification, rawSHA,
			m.CanonicalSHA256, m.MessageIDHash,
			m.InternalDate.UTC().Unix(), m.Size,
			arrivalState.matchedTransferID, arrivalState.matchedExpungedID,
			arrivalState.createdAt, arrivalState.availableAt, arrivalState.finalizedAt,
			arrivalState.updatedAt); err != nil {
			return err
		}
	}
	reminders, err := tx.QueryContext(ctx, `SELECT original_event_id, snooze_generation,
		from_addr, subject, due_at, created_at
		FROM mailbox_generation_rebuild_snooze_events
		WHERE user_id = ? AND rebuild_message_id = ? ORDER BY original_event_id`, m.UserID, state.id)
	if err != nil {
		return err
	}
	type reminderState struct {
		id, generation, dueAt, createdAt int64
		from, subject                    string
	}
	var reminderStates []reminderState
	for reminders.Next() {
		var reminder reminderState
		if err := reminders.Scan(&reminder.id, &reminder.generation, &reminder.from,
			&reminder.subject, &reminder.dueAt, &reminder.createdAt); err != nil {
			_ = reminders.Close()
			return err
		}
		reminderStates = append(reminderStates, reminder)
	}
	if err := reminders.Close(); err != nil {
		return err
	}
	if err := reminders.Err(); err != nil {
		return err
	}
	for _, reminder := range reminderStates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO snooze_reminder_events
			(id, user_id, message_id, snooze_generation, from_addr, subject, due_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, reminder.id, m.UserID, messageID,
			reminder.generation, reminder.from, reminder.subject, reminder.dueAt, reminder.createdAt); err != nil {
			return err
		}
	}
	sends, err := tx.QueryContext(ctx, `SELECT original_send_id, sender, unsubscribe_url, sent_at, created_at
		FROM mailbox_generation_rebuild_unsubscribe_sends
		WHERE user_id = ? AND rebuild_message_id = ? ORDER BY original_send_id`, m.UserID, state.id)
	if err != nil {
		return err
	}
	type sendState struct {
		id, sentAt, createdAt int64
		sender, url           string
	}
	var sendStates []sendState
	for sends.Next() {
		var send sendState
		if err := sends.Scan(&send.id, &send.sender, &send.url, &send.sentAt, &send.createdAt); err != nil {
			_ = sends.Close()
			return err
		}
		sendStates = append(sendStates, send)
	}
	if err := sends.Close(); err != nil {
		return err
	}
	if err := sends.Err(); err != nil {
		return err
	}
	for _, send := range sendStates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_one_click_unsubscribe_sends
			(id, user_id, message_id, sender, unsubscribe_url, sent_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, send.id, m.UserID, messageID, send.sender,
			send.url, send.sentAt, send.createdAt); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM mailbox_generation_rebuild_messages
		WHERE user_id = ? AND id = ?`, m.UserID, state.id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected != 1 {
		return errors.New("mailbox generation rebuild state changed during restore")
	}
	return nil
}

// FinalizeMailboxGenerationRebuild discards states that did not match any
// message after a complete generation-bound mailbox fetch.
func (s *Store) FinalizeMailboxGenerationRebuild(ctx context.Context, userID, accountID, mailboxID int64, targetUIDValidity uint32) error {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || targetUIDValidity == 0 {
		return errors.New("invalid mailbox generation rebuild scope")
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_generation_rebuild_messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?`,
		userID, accountID, mailboxID, targetUIDValidity); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM mailbox_generation_rebuilds
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?`,
		userID, accountID, mailboxID, targetUIDValidity); err != nil {
		return err
	}
	return tx.Commit()
}

// MailboxGenerationRebuildPending reports whether a crash-resumable state
// journal still exists for this exact tenant mailbox generation.
func (s *Store) MailboxGenerationRebuildPending(ctx context.Context, userID, accountID, mailboxID int64, targetUIDValidity uint32) (bool, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || targetUIDValidity == 0 {
		return false, errors.New("invalid mailbox generation rebuild scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return false, err
	}
	var exists int
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM mailbox_generation_rebuilds
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?
	)`, userID, accountID, mailboxID, targetUIDValidity).Scan(&exists)
	return exists != 0, err
}

// MailboxGenerationRebuildArrivalUIDFloor returns the first UID that was not
// present when this exact mailbox generation rebuild began. Zero means the
// boundary has not yet been established.
func (s *Store) MailboxGenerationRebuildArrivalUIDFloor(ctx context.Context, userID, accountID, mailboxID int64, targetUIDValidity uint32) (uint32, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || targetUIDValidity == 0 {
		return 0, errors.New("invalid mailbox generation rebuild scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	var floor uint32
	err = db.QueryRowContext(ctx, `SELECT arrival_uid_floor FROM mailbox_generation_rebuilds
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?`,
		userID, accountID, mailboxID, targetUIDValidity).Scan(&floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return floor, err
}

// InitializeMailboxGenerationRebuildArrivalUIDFloor records a boundary for a
// legacy zero-floor marker exactly once and returns the durable value. A retry
// cannot move the boundary forward and silently reclassify intervening mail.
func (s *Store) InitializeMailboxGenerationRebuildArrivalUIDFloor(ctx context.Context, userID, accountID, mailboxID int64,
	targetUIDValidity, arrivalUIDFloor uint32,
) (uint32, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || targetUIDValidity == 0 || arrivalUIDFloor == 0 {
		return 0, errors.New("invalid mailbox generation rebuild arrival floor scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_generation_rebuilds
		SET arrival_uid_floor = ?, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND target_uid_validity = ? AND arrival_uid_floor = 0`,
		arrivalUIDFloor, nowUnix(), userID, accountID, mailboxID, targetUIDValidity); err != nil {
		return 0, err
	}
	var floor uint32
	if err := tx.QueryRowContext(ctx, `SELECT arrival_uid_floor FROM mailbox_generation_rebuilds
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND target_uid_validity = ?`,
		userID, accountID, mailboxID, targetUIDValidity).Scan(&floor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return floor, nil
}

// ListMailboxGenerationArrivalCandidates returns only rows already stored in
// the exact current mailbox generation at or beyond its durable arrival floor.
func (s *Store) ListMailboxGenerationArrivalCandidates(ctx context.Context, userID, accountID, mailboxID int64,
	targetUIDValidity, arrivalUIDFloor uint32,
) ([]MessageRecord, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || targetUIDValidity == 0 || arrivalUIDFloor == 0 {
		return nil, errors.New("invalid mailbox generation arrival candidate scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT message.id, message.user_id, message.account_id,
		message.mailbox_id, message.blob_id, message.message_id_header, message.in_reply_to,
		message.references_header, message.thread_key, message.subject, message.language_code,
		message.from_addr, message.to_addr, message.cc_addr, message.date_unix,
		message.internal_date_unix, message.uid, message.size, message.blob_path,
		message.body_text, message.body_html, message.is_read, message.read_sync_pending,
		message.is_starred, message.star_sync_pending, message.has_attachments,
		message.is_encrypted, message.is_signed, message.attachment_indexed_at,
		message.created_at, message.updated_at
		FROM messages message
		JOIN mailboxes mailbox ON mailbox.user_id = message.user_id
			AND mailbox.account_id = message.account_id AND mailbox.id = message.mailbox_id
		JOIN mailbox_generation_rebuilds rebuild ON rebuild.user_id = message.user_id
			AND rebuild.account_id = message.account_id AND rebuild.mailbox_id = message.mailbox_id
		WHERE message.user_id = ? AND message.account_id = ? AND message.mailbox_id = ?
			AND message.uid_validity = ? AND message.uid >= ?
			AND mailbox.uidvalidity = ? AND rebuild.target_uid_validity = ?
			AND rebuild.arrival_uid_floor = ?
		ORDER BY message.uid, message.id`, userID, accountID, mailboxID,
		targetUIDValidity, arrivalUIDFloor, targetUIDValidity, targetUIDValidity, arrivalUIDFloor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// MailboxGenerationRebuildExists reports whether this exact tenant mailbox has
// a crash-resumable marker, regardless of its target generation.
func (s *Store) MailboxGenerationRebuildExists(ctx context.Context, userID, accountID, mailboxID int64) (bool, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 {
		return false, errors.New("invalid mailbox generation rebuild scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return false, err
	}
	var exists int
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM mailbox_generation_rebuilds
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
	)`, userID, accountID, mailboxID).Scan(&exists)
	return exists != 0, err
}

// HasPendingMailboxGenerationRebuildsForUser reports whether any mailbox for
// one tenant is still rebuilding its local generation.
func (s *Store) HasPendingMailboxGenerationRebuildsForUser(ctx context.Context, userID int64) (bool, error) {
	if userID <= 0 {
		return false, errors.New("invalid mailbox generation rebuild user scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return false, err
	}
	var exists int
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM mailbox_generation_rebuilds WHERE user_id = ?
	)`, userID).Scan(&exists)
	return exists != 0, err
}

// PendingMailboxGenerationRebuild identifies one tenant mailbox that must
// resume its generation-bound refetch before held arrivals become eligible.
type PendingMailboxGenerationRebuild struct {
	UserID            int64
	AccountID         int64
	MailboxID         int64
	MailboxName       string
	TargetUIDValidity uint32
	inboxPriority     int
}

// ListPendingMailboxGenerationRebuilds supports crash recovery without
// broadening a resume to same-named mailboxes on another account or tenant.
func (s *Store) ListPendingMailboxGenerationRebuilds(ctx context.Context) ([]PendingMailboxGenerationRebuild, error) {
	if s.split {
		users, err := s.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		var out []PendingMailboxGenerationRebuild
		for _, user := range users {
			userStore, err := s.UserStore(ctx, user.ID)
			if err != nil {
				return nil, err
			}
			items, err := userStore.ListPendingMailboxGenerationRebuilds(ctx)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if item.UserID != user.ID {
					return nil, errors.New("mailbox generation rebuild crossed tenant boundary")
				}
			}
			out = append(out, items...)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].UserID != out[j].UserID {
				return out[i].UserID < out[j].UserID
			}
			if out[i].inboxPriority != out[j].inboxPriority {
				return out[i].inboxPriority < out[j].inboxPriority
			}
			if out[i].AccountID != out[j].AccountID {
				return out[i].AccountID < out[j].AccountID
			}
			return out[i].MailboxID < out[j].MailboxID
		})
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rebuild.user_id, rebuild.account_id,
		rebuild.mailbox_id, mailbox.name, rebuild.target_uid_validity,
		CASE WHEN lower(trim(mailbox.role)) = 'inbox' OR lower(trim(mailbox.name)) = 'inbox' THEN 0 ELSE 1 END
		FROM mailbox_generation_rebuilds rebuild
		JOIN mailboxes mailbox ON mailbox.user_id = rebuild.user_id
			AND mailbox.account_id = rebuild.account_id AND mailbox.id = rebuild.mailbox_id
		ORDER BY rebuild.user_id,
			CASE WHEN lower(trim(mailbox.role)) = 'inbox' OR lower(trim(mailbox.name)) = 'inbox' THEN 0 ELSE 1 END,
			rebuild.account_id, rebuild.mailbox_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingMailboxGenerationRebuild
	for rows.Next() {
		var item PendingMailboxGenerationRebuild
		if err := rows.Scan(&item.UserID, &item.AccountID, &item.MailboxID,
			&item.MailboxName, &item.TargetUIDValidity, &item.inboxPriority); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
