// File overview: Atomic local mailbox reset when IMAP UIDVALIDITY changes or stored rows are unproven.

package store

import (
	"context"
	"database/sql"
	"errors"
)

// ResetMailboxForRemoteGeneration discards local rows and the incremental UID
// checkpoint when they cannot be proven to belong to remoteUIDValidity. A reset
// requires the first UID that was not present at the reset boundary; zero is
// accepted only when the call is a same-generation no-op. The boundary is
// committed atomically with the rebuild marker and local-row reset.
func (s *Store) ResetMailboxForRemoteGeneration(ctx context.Context, userID, accountID, mailboxID int64,
	remoteUIDValidity, arrivalUIDFloor uint32,
) ([]MessageRecord, bool, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || remoteUIDValidity == 0 {
		return nil, false, errors.New("invalid mailbox generation reset scope")
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var cachedUIDValidity int64
	var lastUID uint32
	err = tx.QueryRowContext(ctx, `SELECT uidvalidity, last_uid FROM mailboxes
		WHERE user_id = ? AND account_id = ? AND id = ?`, userID, accountID, mailboxID).
		Scan(&cachedUIDValidity, &lastUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}

	var messageCount, unprovenCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE
		WHEN uid_validity <= 0 OR uid_validity <> ? THEN 1 ELSE 0 END), 0)
		FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND outbox_job_id = 0`,
		remoteUIDValidity, userID, accountID, mailboxID).Scan(&messageCount, &unprovenCount); err != nil {
		return nil, false, err
	}
	remoteGeneration := int64(remoteUIDValidity)
	reset := unprovenCount > 0 || (cachedUIDValidity != remoteGeneration && (cachedUIDValidity > 0 || lastUID > 0 || messageCount > 0))
	if !reset {
		if cachedUIDValidity != remoteGeneration {
			if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET uidvalidity = ?, updated_at = ?
				WHERE user_id = ? AND account_id = ? AND id = ?`, remoteUIDValidity, nowUnix(), userID, accountID, mailboxID); err != nil {
				return nil, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	if arrivalUIDFloor == 0 {
		return nil, false, ErrMailboxGenerationArrivalUIDFloorRequired
	}

	// Reset callers only need tenant-scoped document identifiers for derived
	// cleanup and auditing. Loading every cached body here can make the reset
	// transaction hold SQLite's writer lock for minutes on a large mailbox.
	rows, err := tx.QueryContext(ctx, `SELECT id, user_id FROM messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND outbox_job_id = 0
		ORDER BY id`, userID, accountID, mailboxID)
	if err != nil {
		return nil, false, err
	}
	stale := make([]MessageRecord, 0, messageCount)
	for rows.Next() {
		var message MessageRecord
		if err := rows.Scan(&message.ID, &message.UserID); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		stale = append(stale, message)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	now := nowUnix()
	if err := snapshotMailboxGenerationStateTx(ctx, tx, userID, accountID, mailboxID,
		remoteGeneration, int64(arrivalUIDFloor), now); err != nil {
		return nil, false, err
	}
	if err := queueMailboxGenerationBlobCleanupTx(ctx, tx, userID, accountID, mailboxID, remoteGeneration, now); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND outbox_job_id = 0`, userID, accountID, mailboxID); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET uidvalidity = ?, last_uid = 0, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND id = ?`, remoteUIDValidity, now, userID, accountID, mailboxID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return stale, true, nil
}
