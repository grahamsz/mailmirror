// File overview: Mail account and mailbox persistence, defaults, hierarchy, and sync-mode helpers.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const DefaultMailboxPattern = "*"

func prepareMailAccount(a MailAccount) (MailAccount, error) {
	if a.UserID == 0 || strings.TrimSpace(a.Email) == "" || strings.TrimSpace(a.Host) == "" || strings.TrimSpace(a.Username) == "" || a.Port == 0 || a.EncryptedPassword == "" {
		return MailAccount{}, errors.New("mail account fields are incomplete")
	}
	if strings.TrimSpace(a.Mailbox) == "" {
		a.Mailbox = DefaultMailboxPattern
	}
	if strings.TrimSpace(a.SMTPHost) == "" {
		a.SMTPHost = a.Host
	}
	if a.SMTPPort == 0 {
		a.SMTPPort = 587
	}
	if strings.TrimSpace(a.SMTPUsername) == "" {
		a.SMTPUsername = a.Username
	}
	if a.EncryptedSMTPPassword == "" {
		a.EncryptedSMTPPassword = a.EncryptedPassword
	}
	if a.SyncIntervalMinutes <= 0 {
		a.SyncIntervalMinutes = 15
	}
	a.Email = strings.TrimSpace(a.Email)
	a.Label = trimLimit(strings.TrimSpace(a.Label), 240)
	if a.Label == "" {
		a.Label = firstNonEmpty(a.Email, a.Username, a.Host)
	}
	a.Host = strings.TrimSpace(a.Host)
	a.Username = strings.TrimSpace(a.Username)
	a.SMTPHost = strings.TrimSpace(a.SMTPHost)
	a.SMTPUsername = strings.TrimSpace(a.SMTPUsername)
	a.Mailbox = strings.TrimSpace(a.Mailbox)
	return a, nil
}

// CreateMailAccount inserts a new IMAP account for a user and applies account defaults.
func (s *Store) CreateMailAccount(ctx context.Context, a MailAccount) (MailAccount, error) {
	a, err := prepareMailAccount(a)
	if err != nil {
		return MailAccount{}, err
	}
	ts := nowUnix()
	res, err := s.mustDataDB(ctx, a.UserID).ExecContext(ctx, `INSERT INTO mail_accounts
			(user_id, email, label, host, port, username, encrypted_password, use_tls, smtp_host, smtp_port, smtp_username, encrypted_smtp_password, smtp_use_tls, mailbox, sync_interval_minutes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.UserID, a.Email, a.Label, a.Host, a.Port, a.Username, a.EncryptedPassword,
		boolInt(a.UseTLS), a.SMTPHost, a.SMTPPort, a.SMTPUsername, a.EncryptedSMTPPassword,
		boolInt(a.SMTPUseTLS), a.Mailbox, a.SyncIntervalMinutes, ts, ts)
	if err != nil {
		return MailAccount{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return MailAccount{}, err
	}
	return s.GetMailAccountForUser(ctx, a.UserID, id)
}

// UpsertMailAccount creates a new IMAP account when ID is zero, or updates the
// explicitly selected account when ID is present. Callers must choose the target
// account instead of relying on a hidden "first account" update.
func (s *Store) UpsertMailAccount(ctx context.Context, a MailAccount) (MailAccount, error) {
	if a.ID == 0 {
		return s.CreateMailAccount(ctx, a)
	}
	return s.updateMailAccount(ctx, a)
}

func (s *Store) updateMailAccount(ctx context.Context, a MailAccount) (MailAccount, error) {
	a, err := prepareMailAccount(a)
	if err != nil {
		return MailAccount{}, err
	}
	res, err := s.mustDataDB(ctx, a.UserID).ExecContext(ctx, `UPDATE mail_accounts SET
			email = ?, label = ?, host = ?, port = ?, username = ?, encrypted_password = ?, use_tls = ?, smtp_host = ?, smtp_port = ?, smtp_username = ?, encrypted_smtp_password = ?, smtp_use_tls = ?, mailbox = ?, sync_interval_minutes = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		a.Email, a.Label, a.Host, a.Port, a.Username, a.EncryptedPassword, boolInt(a.UseTLS), a.SMTPHost, a.SMTPPort, a.SMTPUsername, a.EncryptedSMTPPassword,
		boolInt(a.SMTPUseTLS), a.Mailbox, a.SyncIntervalMinutes, nowUnix(), a.UserID, a.ID)
	if err != nil {
		return MailAccount{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return MailAccount{}, err
	}
	if n == 0 {
		return MailAccount{}, ErrNotFound
	}
	return s.GetMailAccountForUser(ctx, a.UserID, a.ID)
}

// GetMailAccount returns the first configured IMAP account for user-level defaults.
func (s *Store) GetMailAccount(ctx context.Context, userID int64) (MailAccount, error) {
	return scanMailAccount(s.mustDataDB(ctx, userID).QueryRowContext(ctx, mailAccountSelectSQL()+` WHERE user_id = ? ORDER BY id LIMIT 1`, userID))
}

// GetMailAccountForUser loads one IMAP account scoped to the signed-in user.
func (s *Store) GetMailAccountForUser(ctx context.Context, userID, accountID int64) (MailAccount, error) {
	if accountID <= 0 {
		return MailAccount{}, ErrNotFound
	}
	return scanMailAccount(s.mustDataDB(ctx, userID).QueryRowContext(ctx, mailAccountSelectSQL()+` WHERE user_id = ? AND id = ?`, userID, accountID))
}

// ListMailAccountsForUser returns every IMAP account configured by one user.
func (s *Store) ListMailAccountsForUser(ctx context.Context, userID int64) ([]MailAccount, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, mailAccountSelectSQL()+` WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMailAccounts(rows)
}

// ListAccounts returns all IMAP accounts across users for startup/background scheduling only.
func (s *Store) ListAccounts(ctx context.Context) ([]MailAccount, error) {
	if s.split {
		users, err := s.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		out := []MailAccount{}
		for _, user := range users {
			accounts, err := s.ListMailAccountsForUser(ctx, user.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, accounts...)
		}
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, mailAccountSelectSQL()+` ORDER BY user_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMailAccounts(rows)
}

func mailAccountSelectSQL() string {
	return `SELECT id, user_id, email, label, host, port, username, encrypted_password, use_tls, smtp_host, smtp_port, smtp_username, encrypted_smtp_password, smtp_use_tls, mailbox, sync_interval_minutes, created_at, updated_at FROM mail_accounts`
}

func scanMailAccounts(rows *sql.Rows) ([]MailAccount, error) {
	var accounts []MailAccount
	for rows.Next() {
		a, err := scanMailAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func scanMailAccount(row rowScanner) (MailAccount, error) {
	var a MailAccount
	var created, updated int64
	err := row.Scan(&a.ID, &a.UserID, &a.Email, &a.Label, &a.Host, &a.Port, &a.Username, &a.EncryptedPassword, &a.UseTLS, &a.SMTPHost, &a.SMTPPort, &a.SMTPUsername, &a.EncryptedSMTPPassword, &a.SMTPUseTLS, &a.Mailbox, &a.SyncIntervalMinutes, &created, &updated)
	a.CreatedAt = unixTime(created)
	a.UpdatedAt = unixTime(updated)
	a.applySMTPDefaults()
	return a, err
}

func (a *MailAccount) applySMTPDefaults() {
	if strings.TrimSpace(a.SMTPHost) == "" {
		a.SMTPHost = a.Host
	}
	if a.SMTPPort == 0 {
		a.SMTPPort = 587
	}
	if strings.TrimSpace(a.SMTPUsername) == "" {
		a.SMTPUsername = a.Username
	}
	if a.EncryptedSMTPPassword == "" {
		a.EncryptedSMTPPassword = a.EncryptedPassword
	}
}

// GetOrCreateMailbox returns the local mailbox row for an account/name pair, creating it when discovery finds a new folder.
func (s *Store) GetOrCreateMailbox(ctx context.Context, userID, accountID int64, name string) (Mailbox, error) {
	return s.GetOrCreateMailboxWithRole(ctx, userID, accountID, name, "")
}

// GetOrCreateMailboxWithRole records trusted special-use metadata found during
// IMAP discovery. It fills only an unassigned role and never overwrites another
// assigned role. Duplicate special roles are also left untouched so one unusual
// server response cannot make folder settings ambiguous.
func (s *Store) GetOrCreateMailboxWithRole(ctx context.Context, userID, accountID int64, name, discoveredRole string) (Mailbox, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "INBOX"
	}
	ts := nowUnix()
	syncMode := defaultMailboxSyncMode(name)
	role := normalizeMailboxRole(discoveredRole)
	if role == "" {
		role = defaultMailboxRole(name)
	}
	db := s.mustDataDB(ctx, userID)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Mailbox{}, err
	}
	insertRole := role
	if role != "" {
		var roleAlreadyAssigned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM mailboxes WHERE user_id = ? AND account_id = ? AND role = ? AND name <> ?
		)`, userID, accountID, role, name).Scan(&roleAlreadyAssigned); err != nil {
			_ = tx.Rollback()
			return Mailbox{}, err
		}
		if roleAlreadyAssigned {
			insertRole = ""
		}
	}
	icon := defaultMailboxIcon(name, insertRole)
	showInAllMail := defaultMailboxShowInAllMail(insertRole)
	_, err = tx.ExecContext(ctx, `INSERT INTO mailboxes (user_id, account_id, name, sync_mode, role, icon, show_in_sidebar, show_in_all_mail, include_in_search, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, 1, ?, ?)
		ON CONFLICT(user_id, account_id, name) DO NOTHING`, userID, accountID, name, syncMode, insertRole, icon, boolInt(showInAllMail), ts, ts)
	if err != nil {
		_ = tx.Rollback()
		return Mailbox{}, err
	}
	if role != "" {
		roleIcon := defaultMailboxIcon(name, role)
		_, err = tx.ExecContext(ctx, `UPDATE mailboxes
			SET role = ?,
				icon = CASE WHEN icon = 'folder' THEN ? ELSE icon END,
				show_in_all_mail = CASE WHEN ? IN ('drafts', 'trash', 'junk') THEN 0 ELSE show_in_all_mail END,
				updated_at = ?
			WHERE user_id = ? AND account_id = ? AND name = ? AND role = ''
			AND NOT EXISTS (
				SELECT 1 FROM mailboxes assigned
				WHERE assigned.user_id = ? AND assigned.account_id = ? AND assigned.role = ? AND assigned.name <> ?
			)`, role, roleIcon, role, ts, userID, accountID, name, userID, accountID, role, name)
		if err != nil {
			_ = tx.Rollback()
			return Mailbox{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Mailbox{}, err
	}
	return s.GetMailbox(ctx, userID, accountID, name)
}

// NextUIDForMailbox returns the next UID that should be fetched after the mailbox's last stored UID.
func (s *Store) NextUIDForMailbox(ctx context.Context, userID, mailboxID int64) (uint32, error) {
	var next uint32
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT COALESCE(MAX(uid), 0) + 1
		FROM messages WHERE user_id = ? AND mailbox_id = ? AND outbox_job_id = 0`,
		userID, mailboxID).Scan(&next)
	if next == 0 {
		next = 1
	}
	return next, err
}

// GetMailbox loads a mailbox by account and remote name inside one user scope.
func (s *Store) GetMailbox(ctx context.Context, userID, accountID int64, name string) (Mailbox, error) {
	var m Mailbox
	var created, updated, statusChecked int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, account_id, name, sync_mode, role, icon, show_in_sidebar, show_in_all_mail, include_in_search, uidvalidity, last_uid,
			remote_message_count, remote_unread_count, remote_uid_next, status_checked_at, created_at, updated_at
		FROM mailboxes WHERE user_id = ? AND account_id = ? AND name = ?`, userID, accountID, name).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.Name, &m.SyncMode, &m.Role, &m.Icon, &m.ShowInSidebar, &m.ShowInAllMail, &m.IncludeInSearch, &m.UIDValidity, &m.LastUID,
			&m.RemoteMessageCount, &m.RemoteUnreadCount, &m.RemoteUIDNext, &statusChecked, &created, &updated)
	m.SyncMode = normalizeSyncMode(m.SyncMode)
	m.Role = normalizeMailboxRole(m.Role)
	m.Icon = normalizeMailboxIcon(m.Icon, m.Name, m.Role)
	m.StatusCheckedAt = unixTime(statusChecked)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// GetMailboxForUser loads a mailbox by local ID inside one user scope.
func (s *Store) GetMailboxForUser(ctx context.Context, userID, mailboxID int64) (Mailbox, error) {
	var m Mailbox
	var created, updated, statusChecked int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, account_id, name, sync_mode, role, icon, show_in_sidebar, show_in_all_mail, include_in_search, uidvalidity, last_uid,
			remote_message_count, remote_unread_count, remote_uid_next, status_checked_at, created_at, updated_at
		FROM mailboxes WHERE user_id = ? AND id = ?`, userID, mailboxID).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.Name, &m.SyncMode, &m.Role, &m.Icon, &m.ShowInSidebar, &m.ShowInAllMail, &m.IncludeInSearch, &m.UIDValidity, &m.LastUID,
			&m.RemoteMessageCount, &m.RemoteUnreadCount, &m.RemoteUIDNext, &statusChecked, &created, &updated)
	m.SyncMode = normalizeSyncMode(m.SyncMode)
	m.Role = normalizeMailboxRole(m.Role)
	m.Icon = normalizeMailboxIcon(m.Icon, m.Name, m.Role)
	m.StatusCheckedAt = unixTime(statusChecked)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// GetMailboxByRoleForAccount loads the single special-purpose folder, such as
// Sent or Trash, assigned to one IMAP account. UpdateMailboxSettings prevents
// duplicate special roles within the same account, so callers can treat this as
// a direct lookup rather than a list.
func (s *Store) GetMailboxByRoleForAccount(ctx context.Context, userID, accountID int64, role string) (Mailbox, error) {
	role = normalizeMailboxRole(role)
	if role == "" {
		return Mailbox{}, ErrNotFound
	}
	var m Mailbox
	var created, updated, statusChecked int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, account_id, name, sync_mode, role, icon, show_in_sidebar, show_in_all_mail, include_in_search, uidvalidity, last_uid,
			remote_message_count, remote_unread_count, remote_uid_next, status_checked_at, created_at, updated_at
		FROM mailboxes WHERE user_id = ? AND account_id = ? AND role = ?`, userID, accountID, role).
		Scan(&m.ID, &m.UserID, &m.AccountID, &m.Name, &m.SyncMode, &m.Role, &m.Icon, &m.ShowInSidebar, &m.ShowInAllMail, &m.IncludeInSearch, &m.UIDValidity, &m.LastUID,
			&m.RemoteMessageCount, &m.RemoteUnreadCount, &m.RemoteUIDNext, &statusChecked, &created, &updated)
	m.SyncMode = normalizeSyncMode(m.SyncMode)
	m.Role = normalizeMailboxRole(m.Role)
	m.Icon = normalizeMailboxIcon(m.Icon, m.Name, m.Role)
	m.StatusCheckedAt = unixTime(statusChecked)
	m.CreatedAt = unixTime(created)
	m.UpdatedAt = unixTime(updated)
	return m, err
}

// ListMailboxesForUser returns folder summaries with local, remote, and indexing counters for chrome/settings.
func (s *Store) ListMailboxesForUser(ctx context.Context, userID int64) ([]MailboxSummary, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT mb.id, mb.user_id, mb.account_id, mb.name, mb.sync_mode, mb.role, mb.icon,
			mb.show_in_sidebar, mb.show_in_all_mail, mb.include_in_search, mb.search_index_purged, mb.search_index_state_known, mb.uidvalidity, mb.last_uid, mb.created_at, mb.updated_at,
			mb.remote_message_count, mb.remote_unread_count, mb.remote_uid_next, mb.status_checked_at,
				ma.email, ma.label,
				count(m.id),
				COALESCE(sum(CASE WHEN m.is_read = 0 THEN 1 ELSE 0 END), 0),
				COALESCE(sum(CASE WHEN m.blob_path != '' AND b.kind IN ('message', 'message-cache') AND b.size > 0 THEN 1 ELSE 0 END), 0),
				COALESCE(sum(CASE WHEN sn.id IS NOT NULL THEN 1 ELSE 0 END), 0),
				COALESCE(sum(CASE WHEN sn.id IS NOT NULL AND m.is_read = 0 THEN 1 ELSE 0 END), 0)
			FROM mailboxes mb
			JOIN mail_accounts ma ON ma.id = mb.account_id AND ma.user_id = mb.user_id
			LEFT JOIN messages m ON m.user_id = mb.user_id AND m.mailbox_id = mb.id
			LEFT JOIN blobs b ON b.id = m.blob_id AND b.user_id = m.user_id
			LEFT JOIN message_snoozes sn ON sn.user_id = m.user_id
				AND sn.thread_key = COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)
				AND sn.snoozed_until > ?
			WHERE mb.user_id = ?
			GROUP BY mb.id
			ORDER BY CASE WHEN mb.role = 'inbox' OR lower(mb.name) = 'inbox' THEN 0 ELSE 1 END, ma.email, lower(mb.name)`, nowUnix(), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailboxSummary
	for rows.Next() {
		var ms MailboxSummary
		var created, updated, statusChecked int64
		var localMessages, localUnread, cachedMessages, hiddenMessages, hiddenUnread int
		if err := rows.Scan(&ms.ID, &ms.UserID, &ms.AccountID, &ms.Name, &ms.SyncMode, &ms.Role, &ms.Icon,
			&ms.ShowInSidebar, &ms.ShowInAllMail, &ms.IncludeInSearch, &ms.SearchIndexPurged, &ms.SearchIndexKnown, &ms.UIDValidity, &ms.LastUID, &created, &updated,
			&ms.RemoteMessageCount, &ms.RemoteUnreadCount, &ms.RemoteUIDNext, &statusChecked, &ms.AccountEmail, &ms.AccountLabel,
			&localMessages, &localUnread, &cachedMessages, &hiddenMessages, &hiddenUnread); err != nil {
			return nil, err
		}
		ms.SyncMode = normalizeSyncMode(ms.SyncMode)
		ms.Role = normalizeMailboxRole(ms.Role)
		ms.Icon = normalizeMailboxIcon(ms.Icon, ms.Name, ms.Role)
		ms.StatusCheckedAt = unixTime(statusChecked)
		ms.CreatedAt = unixTime(created)
		ms.UpdatedAt = unixTime(updated)
		ms.LocalMessageCount = localMessages
		ms.CachedMessageCount = cachedMessages
		visibleLocalMessages := max(0, localMessages-hiddenMessages)
		ms.MessageCount = visibleLocalMessages
		ms.UnreadCount = max(0, localUnread-hiddenUnread)
		if statusChecked > 0 {
			ms.MessageCount = max(0, ms.RemoteMessageCount-hiddenMessages)
			ms.UnreadCount = max(0, ms.RemoteUnreadCount-hiddenUnread)
		}
		ms.SyncPercent = mailboxSyncPercent(ms.LastUID, ms.RemoteUIDNext, ms.MessageCount)
		ms.LocalSyncPercent = localMailboxSyncPercent(visibleLocalMessages, ms.MessageCount)
		out = append(out, ms)
	}
	return out, rows.Err()
}

// LastUIDs returns the per-mailbox UID checkpoints used by incremental sync planning.
func (s *Store) LastUIDs(ctx context.Context, userID, accountID int64) (map[string]uint32, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT name, last_uid FROM mailboxes WHERE user_id = ? AND account_id = ?`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]uint32)
	for rows.Next() {
		var name string
		var uid uint32
		if err := rows.Scan(&name, &uid); err != nil {
			return nil, err
		}
		out[name] = uid
	}
	return out, rows.Err()
}

// UpdateMailboxSyncMode updates only the folder sync mode for quick UI toggles.
func (s *Store) UpdateMailboxSyncMode(ctx context.Context, userID, mailboxID int64, mode string) error {
	mode = normalizeSyncMode(mode)
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes SET sync_mode = ?, updated_at = ? WHERE user_id = ? AND id = ?`, mode, nowUnix(), userID, mailboxID)
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

// UpdateMailboxSettings updates sync mode, role, icon, and visibility flags for one folder.
func (s *Store) UpdateMailboxSettings(ctx context.Context, userID, mailboxID int64, settings MailboxSettings) error {
	settings.SyncMode = normalizeSyncMode(settings.SyncMode)
	settings.Role = normalizeMailboxRole(settings.Role)
	settings.Icon = normalizeMailboxIcon(settings.Icon, "", settings.Role)
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var accountID int64
	var mailboxName string
	var previousIncludeInSearch bool
	if err := tx.QueryRowContext(ctx, `SELECT account_id, name, include_in_search FROM mailboxes WHERE user_id = ? AND id = ?`, userID, mailboxID).Scan(&accountID, &mailboxName, &previousIncludeInSearch); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if settings.SyncMode == "inherit" && len(mailboxParentNames(mailboxName)) == 0 {
		_ = tx.Rollback()
		return fmt.Errorf("%w: top-level folders cannot inherit sync mode", ErrInvalidMailboxSettings)
	}
	if settings.Role == "inbox" || settings.Role == "sent" || settings.Role == "drafts" || settings.Role == "trash" || settings.Role == "junk" || settings.Role == "all" {
		var existingName string
		err := tx.QueryRowContext(ctx, `SELECT name FROM mailboxes
			WHERE user_id = ? AND account_id = ? AND role = ? AND id <> ?
			LIMIT 1`, userID, accountID, settings.Role, mailboxID).Scan(&existingName)
		if err == nil {
			_ = tx.Rollback()
			return fmt.Errorf("%w: %s is already assigned to %s", ErrDuplicateMailboxRole, settings.Role, existingName)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE mailboxes
		SET sync_mode = ?, role = ?, icon = ?, show_in_sidebar = ?, show_in_all_mail = ?, include_in_search = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		settings.SyncMode, settings.Role, settings.Icon, boolInt(settings.ShowInSidebar), boolInt(settings.ShowInAllMail), boolInt(settings.IncludeInSearch), nowUnix(), userID, mailboxID)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if !previousIncludeInSearch && settings.IncludeInSearch {
		if _, err := tx.ExecContext(ctx, `UPDATE messages
			SET attachment_indexed_at = 0
			WHERE user_id = ? AND mailbox_id = ? AND attachment_indexed_at > 0`, userID, mailboxID); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes
			SET search_index_purged = 0, search_index_state_known = 0
			WHERE user_id = ? AND id = ?`, userID, mailboxID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// EffectiveMailboxSyncMode resolves inherit/auto/manual/never for a mailbox under its account defaults.
func (s *Store) EffectiveMailboxSyncMode(ctx context.Context, userID, accountID int64, mailbox Mailbox) (string, error) {
	mode := normalizeSyncMode(mailbox.SyncMode)
	if mode != "inherit" {
		return mode, nil
	}
	for _, parent := range mailboxParentNames(mailbox.Name) {
		mb, err := s.GetMailbox(ctx, userID, accountID, parent)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		parentMode := normalizeSyncMode(mb.SyncMode)
		if parentMode != "inherit" {
			return parentMode, nil
		}
	}
	return "auto", nil
}

// ListMessagesForMailboxIndex pages through messages used to rebuild or remove one mailbox's search documents.
func (s *Store) ListMessagesForMailboxIndex(ctx context.Context, userID, mailboxID int64, limit int, afterID int64) ([]MessageRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, account_id, mailbox_id, blob_id, message_id_header, in_reply_to, references_header, thread_key, subject, language_code, from_addr, to_addr, cc_addr,
			date_unix, internal_date_unix, uid, size, blob_path, body_text, body_html, is_read, read_sync_pending, is_starred, star_sync_pending, has_attachments, is_encrypted, is_signed, attachment_indexed_at, created_at, updated_at
		FROM messages WHERE user_id = ? AND mailbox_id = ? AND id > ? ORDER BY id LIMIT ?`, userID, mailboxID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func normalizeSyncMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "inherit":
		return "inherit"
	case "manual":
		return "manual"
	case "never":
		return "never"
	default:
		return "auto"
	}
}

func normalizeMailboxRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "inbox":
		return "inbox"
	case "sent":
		return "sent"
	case "draft", "drafts":
		return "drafts"
	case "trash":
		return "trash"
	case "junk", "spam":
		return "junk"
	case "all":
		return "all"
	default:
		return ""
	}
}

func normalizeMailboxIcon(icon string, name string, role string) string {
	icon = strings.ToLower(strings.TrimSpace(icon))
	switch icon {
	case "inbox", "delete", "folder", "folder_open", "archive", "send", "draft", "sell", "shopping_bag", "label", "star", "report", "block", "mail":
		return icon
	}
	return defaultMailboxIcon(name, role)
}

func defaultMailboxSyncMode(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), "INBOX") {
		return "auto"
	}
	return "manual"
}

func defaultMailboxRole(name string) string {
	clean := strings.ToLower(strings.TrimSpace(name))
	switch clean {
	case "inbox":
		return "inbox"
	case "sent", "sent mail", "sent items", "[gmail]/sent mail":
		return "sent"
	case "draft", "drafts", "[gmail]/drafts":
		return "drafts"
	case "trash", "deleted", "deleted items", "[gmail]/trash":
		return "trash"
	case "junk", "spam", "junk e-mail", "junk email", "[gmail]/spam":
		return "junk"
	default:
		return ""
	}
}

func defaultMailboxIcon(name string, role string) string {
	switch normalizeMailboxRole(role) {
	case "inbox":
		return "inbox"
	case "sent":
		return "send"
	case "drafts":
		return "draft"
	case "trash":
		return "delete"
	case "junk":
		return "report"
	case "all":
		return "archive"
	}
	clean := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(clean, "archive"):
		return "archive"
	case strings.Contains(clean, "sent"):
		return "send"
	case strings.Contains(clean, "draft"):
		return "draft"
	case strings.Contains(clean, "spam"), strings.Contains(clean, "junk"):
		return "report"
	default:
		return "folder"
	}
}

func defaultMailboxShowInAllMail(role string) bool {
	switch normalizeMailboxRole(role) {
	case "drafts", "trash", "junk":
		return false
	default:
		return true
	}
}

func localMailboxSyncPercent(localMessages, totalMessages int) int {
	if totalMessages <= 0 {
		if localMessages > 0 {
			return 100
		}
		return 0
	}
	if localMessages <= 0 {
		return 0
	}
	if localMessages >= totalMessages {
		return 100
	}
	return (localMessages * 100) / totalMessages
}

func mailboxSyncPercent(lastUID uint32, remoteUIDNext uint32, messageCount int) int {
	if remoteUIDNext > 1 {
		total := remoteUIDNext - 1
		if lastUID >= total {
			return 100
		}
		return int((uint64(lastUID) * 100) / uint64(total))
	}
	if messageCount > 0 {
		return 100
	}
	return 0
}

func mailboxParentNames(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	var parents []string
	for i := len(name) - 1; i > 0; i-- {
		switch name[i] {
		case '.', '/', '\\':
			parent := strings.TrimSpace(name[:i])
			if parent != "" {
				parents = append(parents, parent)
			}
		}
	}
	return parents
}

// UpdateMailboxLastUID advances the incremental sync checkpoint after a message UID is handled.
func (s *Store) UpdateMailboxLastUID(ctx context.Context, userID, mailboxID int64, uid uint32) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes SET last_uid = CASE WHEN last_uid < ? THEN ? ELSE last_uid END, updated_at = ?
		WHERE id = ? AND user_id = ?`, uid, uid, nowUnix(), mailboxID, userID)
	return err
}

// UpdateMailboxLastUIDForGeneration advances the incremental checkpoint only
// while the mailbox still belongs to the generation that produced uid. A
// concurrent UIDVALIDITY reset must never carry an old-generation UID into the
// new checkpoint.
func (s *Store) UpdateMailboxLastUIDForGeneration(ctx context.Context, userID, accountID, mailboxID int64, uid, expectedUIDValidity uint32) error {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || expectedUIDValidity == 0 {
		return errors.New("invalid generation-bound mailbox checkpoint scope")
	}
	db := s.mustDataDB(ctx, userID)
	res, err := db.ExecContext(ctx, `UPDATE mailboxes
		SET last_uid = CASE WHEN last_uid < ? THEN ? ELSE last_uid END, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND id = ? AND uidvalidity = ?`,
		uid, uid, nowUnix(), userID, accountID, mailboxID, expectedUIDValidity)
	if err != nil {
		return err
	}
	matched, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if matched == 0 {
		return s.mailboxGenerationScopeError(ctx, db, userID, accountID, mailboxID, expectedUIDValidity)
	}
	return nil
}

// ResetMailboxLastUID clears the incremental checkpoint so the next sync refetches the folder from UID 1.
func (s *Store) ResetMailboxLastUID(ctx context.Context, userID, mailboxID int64) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes SET last_uid = 0, updated_at = ? WHERE id = ? AND user_id = ?`, nowUnix(), mailboxID, userID)
	return err
}

// UpdateMailboxRemoteStatus stores IMAP STATUS counters separately from local mirrored counts.
func (s *Store) UpdateMailboxRemoteStatus(ctx context.Context, userID, mailboxID int64, messageCount, unreadCount int, uidNext uint32, uidValidity uint32) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE mailboxes
		SET remote_message_count = ?, remote_unread_count = ?, remote_uid_next = ?, uidvalidity = ?, status_checked_at = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`,
		messageCount, unreadCount, uidNext, uidValidity, nowUnix(), nowUnix(), mailboxID, userID)
	return err
}

// InitializeMailboxRemoteStatus records the first proven mailbox generation
// and its counters. It cannot replace an established UIDVALIDITY, including
// one installed by a concurrent reset.
func (s *Store) InitializeMailboxRemoteStatus(ctx context.Context, userID, accountID, mailboxID int64, messageCount, unreadCount int, uidNext, uidValidity uint32) error {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || uidValidity == 0 {
		return errors.New("invalid mailbox status initialization scope")
	}
	db := s.mustDataDB(ctx, userID)
	res, err := db.ExecContext(ctx, `UPDATE mailboxes
		SET remote_message_count = ?, remote_unread_count = ?, remote_uid_next = ?,
			uidvalidity = ?, status_checked_at = ?, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND id = ? AND uidvalidity = 0`,
		messageCount, unreadCount, uidNext, uidValidity, nowUnix(), nowUnix(),
		userID, accountID, mailboxID)
	if err != nil {
		return err
	}
	matched, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if matched == 0 {
		return s.mailboxGenerationScopeError(ctx, db, userID, accountID, mailboxID, uidValidity)
	}
	return nil
}

// UpdateMailboxRemoteStatusForGeneration records counters without allowing a
// stale STATUS result to replace a UIDVALIDITY installed by a concurrent reset.
func (s *Store) UpdateMailboxRemoteStatusForGeneration(ctx context.Context, userID, accountID, mailboxID int64, messageCount, unreadCount int, uidNext, expectedUIDValidity uint32) error {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || expectedUIDValidity == 0 {
		return errors.New("invalid generation-bound mailbox status scope")
	}
	db := s.mustDataDB(ctx, userID)
	res, err := db.ExecContext(ctx, `UPDATE mailboxes
		SET remote_message_count = ?, remote_unread_count = ?, remote_uid_next = ?, status_checked_at = ?, updated_at = ?
		WHERE user_id = ? AND account_id = ? AND id = ? AND uidvalidity = ?`,
		messageCount, unreadCount, uidNext, nowUnix(), nowUnix(), userID, accountID, mailboxID, expectedUIDValidity)
	if err != nil {
		return err
	}
	matched, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if matched == 0 {
		return s.mailboxGenerationScopeError(ctx, db, userID, accountID, mailboxID, expectedUIDValidity)
	}
	return nil
}

// MessageExistsByUID checks whether a UID has already been mirrored for an account mailbox.
func (s *Store) MessageExistsByUID(ctx context.Context, userID, accountID, mailboxID int64, uid uint32) (bool, error) {
	var id int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id FROM messages WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ?`,
		userID, accountID, mailboxID, uid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// MessageExistsByUIDForGeneration binds both the mailbox and message lookup to
// one UIDVALIDITY. This prevents a reused UID from being mistaken for a row
// observed before a concurrent mailbox reset.
func (s *Store) MessageExistsByUIDForGeneration(ctx context.Context, userID, accountID, mailboxID int64, uid, expectedUIDValidity uint32) (bool, error) {
	if userID <= 0 || accountID <= 0 || mailboxID <= 0 || expectedUIDValidity == 0 {
		return false, errors.New("invalid generation-bound message lookup scope")
	}
	db := s.mustDataDB(ctx, userID)
	var currentUIDValidity int64
	var exists bool
	err := db.QueryRowContext(ctx, `SELECT uidvalidity, EXISTS(
		SELECT 1 FROM messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ? AND uid = ? AND uid_validity = ?
			AND import_completed_at > 0
	) FROM mailboxes WHERE user_id = ? AND account_id = ? AND id = ?`,
		userID, accountID, mailboxID, uid, expectedUIDValidity,
		userID, accountID, mailboxID).Scan(&currentUIDValidity, &exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if currentUIDValidity != int64(expectedUIDValidity) {
		return false, fmt.Errorf("mailbox UIDVALIDITY is %d, expected %d: %w",
			currentUIDValidity, expectedUIDValidity, ErrMailboxGenerationChanged)
	}
	return exists, nil
}

func (s *Store) mailboxGenerationScopeError(ctx context.Context, db *sql.DB, userID, accountID, mailboxID int64, expectedUIDValidity uint32) error {
	var currentUIDValidity int64
	err := db.QueryRowContext(ctx, `SELECT uidvalidity FROM mailboxes
		WHERE user_id = ? AND account_id = ? AND id = ?`, userID, accountID, mailboxID).Scan(&currentUIDValidity)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("mailbox UIDVALIDITY is %d, expected %d: %w",
		currentUIDValidity, expectedUIDValidity, ErrMailboxGenerationChanged)
}

// MessageUIDsForMailbox returns the local UID set for one user-owned account mailbox.
func (s *Store) MessageUIDsForMailbox(ctx context.Context, userID, accountID, mailboxID int64) ([]uint32, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT uid FROM messages
		WHERE user_id = ? AND account_id = ? AND mailbox_id = ?
			AND import_completed_at > 0 AND outbox_job_id = 0 ORDER BY uid`,
		userID, accountID, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	uids := []uint32{}
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		uids = append(uids, uid)
	}
	return uids, rows.Err()
}
