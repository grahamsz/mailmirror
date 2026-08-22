// File overview: Sync-run lifecycle, progress tracking, interruption handling, and user account discovery.

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CreateSyncRun starts a sync progress row for one user/account.
func (s *Store) CreateSyncRun(ctx context.Context, userID, accountID int64) (SyncRun, error) {
	started := nowUnix()
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO sync_runs (user_id, account_id, status, started_at, updated_at) VALUES (?, ?, 'running', ?, ?)`, userID, accountID, started, started)
	if err != nil {
		return SyncRun{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SyncRun{}, err
	}
	return s.GetSyncRunForUser(ctx, userID, id)
}

// InterruptStaleSyncRuns marks a run that has made no durable progress for the
// supplied age as interrupted. A live runner updates sync_runs throughout each
// bounded turn; rows older than this are abandoned UI state, not active work.
func (s *Store) InterruptStaleSyncRuns(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, fmt.Errorf("stale sync run age must be positive")
	}
	if s.split {
		users, err := s.ListUsers(ctx)
		if err != nil {
			return 0, err
		}
		var total int64
		for _, user := range users {
			userStore, err := s.UserStore(ctx, user.ID)
			if err != nil {
				return total, err
			}
			n, err := userStore.InterruptStaleSyncRuns(ctx, maxAge)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	now := nowUnix()
	cutoff := time.Now().Add(-maxAge).Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE sync_runs
		SET status = 'interrupted', finished_at = ?, updated_at = ?,
			error = CASE WHEN error = '' THEN 'Sync stopped making progress and was marked interrupted.' ELSE error END
		WHERE status = 'running' AND updated_at <= ?`, now, now, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MarkRunningSyncRunsInterrupted marks stale running jobs interrupted during startup recovery.
func (s *Store) MarkRunningSyncRunsInterrupted(ctx context.Context) (int64, error) {
	if s.split {
		users, err := s.ListUsers(ctx)
		if err != nil {
			return 0, err
		}
		var total int64
		for _, user := range users {
			us, err := s.UserStore(ctx, user.ID)
			if err != nil {
				return total, err
			}
			n, err := us.MarkRunningSyncRunsInterrupted(ctx)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE sync_runs
		SET status = 'interrupted', finished_at = ?, updated_at = ?, error = CASE WHEN error = '' THEN 'Server restarted before this sync finished.' ELSE error END
		WHERE status = 'running'`, now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SyncProgress is the mutable progress snapshot copied into sync_runs during long operations.
type SyncProgress struct {
	MessagesSeen       int
	MessagesStored     int
	MessagesSkipped    int
	NewMessages        int
	LatestNewFrom      string
	LatestNewSubject   string
	LatestNewMessageID int64
	MessagesTotal      int
	MailboxesDone      int
	MailboxesTotal     int
	CurrentMailbox     string
	CurrentUID         uint32
}

// UpdateSyncRunProgress stores the latest mailbox/message progress snapshot for a sync run.
func (s *Store) UpdateSyncRunProgress(ctx context.Context, userID, id int64, p SyncProgress) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE sync_runs
		SET updated_at = ?, messages_seen = ?, messages_stored = ?, messages_skipped = ?,
			latest_new_from = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_from END,
			latest_new_subject = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_subject END,
			latest_new_message_id = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_message_id END,
			new_messages = MAX(new_messages, ?), messages_total = ?, mailboxes_done = ?, mailboxes_total = ?, current_mailbox = ?, current_uid = ?
		WHERE user_id = ? AND id = ?`,
		nowUnix(), p.MessagesSeen, p.MessagesStored, p.MessagesSkipped,
		p.NewMessages, p.LatestNewFrom, p.NewMessages, p.LatestNewSubject,
		p.NewMessages, p.LatestNewMessageID, p.NewMessages, p.MessagesTotal,
		p.MailboxesDone, p.MailboxesTotal, p.CurrentMailbox, p.CurrentUID, userID, id)
	return err
}

// TouchSyncRun refreshes only updated_at on a running run so long single-message
// operations (IMAP append + reconcile) are not mislabeled stale by the reaper
// while they are still actively making progress between full progress writes.
func (s *Store) TouchSyncRun(ctx context.Context, userID, id int64) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx,
		`UPDATE sync_runs SET updated_at = ? WHERE user_id = ? AND id = ? AND status = 'running'`,
		nowUnix(), userID, id)
	return err
}

// FinishSyncRun finalizes a sync run with status, progress, and optional error text.
func (s *Store) FinishSyncRun(ctx context.Context, userID, id int64, status string, p SyncProgress, errText string) error {
	if len(errText) > 1000 {
		errText = errText[:1000]
	}
	now := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE sync_runs
		SET status = ?, finished_at = ?, updated_at = ?, messages_seen = ?, messages_stored = ?, messages_skipped = ?,
			latest_new_from = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_from END,
			latest_new_subject = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_subject END,
			latest_new_message_id = CASE WHEN ? >= new_messages THEN ? ELSE latest_new_message_id END,
			new_messages = MAX(new_messages, ?), messages_total = ?,
			mailboxes_done = ?, mailboxes_total = ?, current_mailbox = ?, current_uid = ?, error = ?
		WHERE user_id = ? AND id = ? AND NOT (status = 'interrupted' AND finished_at != 0)`,
		status, now, now, p.MessagesSeen, p.MessagesStored, p.MessagesSkipped,
		p.NewMessages, p.LatestNewFrom, p.NewMessages, p.LatestNewSubject,
		p.NewMessages, p.LatestNewMessageID, p.NewMessages, p.MessagesTotal, p.MailboxesDone, p.MailboxesTotal,
		p.CurrentMailbox, p.CurrentUID, errText, userID, id)
	return err
}

// InterruptSyncRunForUser makes a no-longer-desired active run visible as
// interrupted immediately. The runner's deferred finish cannot overwrite this
// terminal state, so cancellation is safe even when IMAP needs a moment to
// notice its context cancellation.
func (s *Store) InterruptSyncRunForUser(ctx context.Context, userID, id int64, reason string) error {
	if userID <= 0 || id <= 0 {
		return fmt.Errorf("sync run and user id must be positive")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "Cancelled by user."
	}
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	now := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE sync_runs
		SET status = 'interrupted', finished_at = ?, updated_at = ?, error = ?
		WHERE user_id = ? AND id = ? AND status = 'running'`, now, now, reason, userID, id)
	return err
}

// GetSyncRunForUser loads one sync run scoped to the signed-in user.
func (s *Store) GetSyncRunForUser(ctx context.Context, userID, id int64) (SyncRun, error) {
	var r SyncRun
	var started, finished, updated int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, account_id, status, started_at, finished_at, updated_at,
			messages_seen, messages_stored, messages_skipped, new_messages, latest_new_from, latest_new_subject, latest_new_message_id, messages_total, mailboxes_done, mailboxes_total, current_mailbox, current_uid, error
		FROM sync_runs WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&r.ID, &r.UserID, &r.AccountID, &r.Status, &started, &finished, &updated,
			&r.MessagesSeen, &r.MessagesStored, &r.MessagesSkipped, &r.NewMessages, &r.LatestNewFrom, &r.LatestNewSubject, &r.LatestNewMessageID, &r.MessagesTotal, &r.MailboxesDone, &r.MailboxesTotal, &r.CurrentMailbox, &r.CurrentUID, &r.Error)
	r.StartedAt = unixTime(started)
	r.FinishedAt = unixTime(finished)
	r.UpdatedAt = unixTime(updated)
	return r, err
}

// ListSyncRunsForUser returns recent sync runs for status widgets and settings history.
func (s *Store) ListSyncRunsForUser(ctx context.Context, userID int64, limit int) ([]SyncRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	scanLimit := limit * 20
	if scanLimit < 100 {
		scanLimit = 100
	}
	if scanLimit > 1000 {
		scanLimit = 1000
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, status, started_at, finished_at, updated_at,
			messages_seen, messages_stored, messages_skipped, new_messages, latest_new_from, latest_new_subject, latest_new_message_id, messages_total, mailboxes_done, mailboxes_total, current_mailbox, current_uid, error
		FROM sync_runs WHERE user_id = ? ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, updated_at DESC, id DESC LIMIT ?`, userID, scanLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRun
	seenNoopFolders := map[string]bool{}
	for rows.Next() {
		var r SyncRun
		var started, finished, updated int64
		if err := rows.Scan(&r.ID, &r.UserID, &r.AccountID, &r.Status, &started, &finished, &updated,
			&r.MessagesSeen, &r.MessagesStored, &r.MessagesSkipped, &r.NewMessages, &r.LatestNewFrom, &r.LatestNewSubject, &r.LatestNewMessageID, &r.MessagesTotal, &r.MailboxesDone, &r.MailboxesTotal, &r.CurrentMailbox, &r.CurrentUID, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = unixTime(started)
		r.FinishedAt = unixTime(finished)
		r.UpdatedAt = unixTime(updated)
		if !keepSyncRunInRecentList(r, seenNoopFolders) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// ListLatestSyncRunsByMailboxForUser returns one latest run for every
// account/mailbox pair with recorded mailbox progress. Unlike the bounded
// recent-activity feed, this query considers the user's complete run history so
// an infrequently synced folder does not appear to have never run.
func (s *Store) ListLatestSyncRunsByMailboxForUser(ctx context.Context, userID int64) ([]SyncRun, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `WITH ranked AS (
		SELECT id, user_id, account_id, status, started_at, finished_at, updated_at,
			messages_seen, messages_stored, messages_skipped, new_messages, latest_new_from, latest_new_subject, latest_new_message_id,
			messages_total, mailboxes_done, mailboxes_total, current_mailbox, current_uid, error,
			ROW_NUMBER() OVER (
				PARTITION BY account_id, lower(trim(current_mailbox))
				ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, updated_at DESC, id DESC
			) AS mailbox_rank
		FROM sync_runs
		WHERE user_id = ? AND trim(current_mailbox) <> ''
	)
	SELECT id, user_id, account_id, status, started_at, finished_at, updated_at,
		messages_seen, messages_stored, messages_skipped, new_messages, latest_new_from, latest_new_subject, latest_new_message_id,
		messages_total, mailboxes_done, mailboxes_total, current_mailbox, current_uid, error
	FROM ranked WHERE mailbox_rank = 1
	ORDER BY account_id, lower(trim(current_mailbox))`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncRun
	for rows.Next() {
		var run SyncRun
		var started, finished, updated int64
		if err := rows.Scan(&run.ID, &run.UserID, &run.AccountID, &run.Status, &started, &finished, &updated,
			&run.MessagesSeen, &run.MessagesStored, &run.MessagesSkipped, &run.NewMessages, &run.LatestNewFrom, &run.LatestNewSubject, &run.LatestNewMessageID,
			&run.MessagesTotal, &run.MailboxesDone, &run.MailboxesTotal, &run.CurrentMailbox, &run.CurrentUID, &run.Error); err != nil {
			return nil, err
		}
		run.StartedAt = unixTime(started)
		run.FinishedAt = unixTime(finished)
		run.UpdatedAt = unixTime(updated)
		out = append(out, run)
	}
	return out, rows.Err()
}

// keepSyncRunInRecentList keeps failure/interruption visibility and useful work history,
// while collapsing successful no-op runs to the newest row per account/folder.
func keepSyncRunInRecentList(run SyncRun, seenNoopFolders map[string]bool) bool {
	status := strings.TrimSpace(strings.ToLower(run.Status))
	if status != "" && status != "ok" {
		return true
	}
	if run.MessagesStored > 0 || run.NewMessages > 0 {
		return true
	}
	key := syncRunNoopFolderKey(run)
	if seenNoopFolders[key] {
		return false
	}
	seenNoopFolders[key] = true
	return true
}

// syncRunNoopFolderKey groups zero-message runs by the mailbox label users see in sync history.
func syncRunNoopFolderKey(run SyncRun) string {
	mailbox := strings.ToLower(strings.TrimSpace(run.CurrentMailbox))
	if mailbox == "" {
		mailbox = "__account__"
	}
	return fmt.Sprintf("%d:%s", run.AccountID, mailbox)
}

// ListUserIDsWithAccounts returns user IDs that have IMAP accounts for background scheduling.
func (s *Store) ListUserIDsWithAccounts(ctx context.Context) ([]int64, error) {
	if s.split {
		users, err := s.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		var ids []int64
		for _, user := range users {
			var count int
			if err := s.mustDataDB(ctx, user.ID).QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_accounts WHERE user_id = ?`, user.ID).Scan(&count); err != nil {
				return nil, err
			}
			if count > 0 {
				ids = append(ids, user.ID)
			}
		}
		return ids, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT user_id FROM mail_accounts ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
