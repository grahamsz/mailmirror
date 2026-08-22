// File overview: SQLite persistence layer for tenant-scoped Rolltop users,
// sessions, and profile preferences. The system store is the source of truth
// for authentication and settings; split-mode user stores receive a mirrored
// user row through Store.userStore so joins inside the tenant database can stay
// local without exposing other users' data.

package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

const userSelectColumns = `id, email, name, backup_email, password_hash, is_admin, date_locale, date_format, theme, search_preset, search_recency_bias, search_fuzzy, search_sender_boost, search_sender_history, search_contact_boost, search_attachment_weight, search_compact_splitting, created_at, updated_at`

type scanDest interface {
	Scan(dest ...any) error
}

func cleanEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// CountUsers returns the number of local user accounts for setup gating.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUser inserts a system-level user row and seeds its per-user database in split mode.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string, isAdmin bool) (User, error) {
	email = cleanEmail(email)
	name = strings.TrimSpace(name)
	if email == "" || name == "" || passwordHash == "" {
		return User{}, errors.New("email, name, and password hash are required")
	}
	ts := nowUnix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (email, name, password_hash, is_admin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, email, name, passwordHash, boolInt(isAdmin), ts, ts)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return s.GetUserByID(ctx, id)
}

// ErrSetupAlreadyComplete reports that users already exist when a caller tried
// to create the initial admin account.
var ErrSetupAlreadyComplete = errors.New("setup is already complete")

// CreateInitialAdminIfNone creates the first admin account only when no users
// exist yet. The count check and insert share one immediate transaction, so two
// concurrent first-run requests cannot both pass the check and both become
// admins (the setup handler's earlier existence check alone was racy).
func (s *Store) CreateInitialAdminIfNone(ctx context.Context, email, name, passwordHash string) (User, error) {
	email = cleanEmail(email)
	name = strings.TrimSpace(name)
	if email == "" || name == "" || passwordHash == "" {
		return User{}, errors.New("email, name, and password hash are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return User{}, err
	}
	if n > 0 {
		return User{}, ErrSetupAlreadyComplete
	}
	ts := nowUnix()
	res, err := tx.ExecContext(ctx, `INSERT INTO users (email, name, password_hash, is_admin, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)`, email, name, passwordHash, ts, ts)
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUserByID(ctx, id)
}

// GetUserByID loads a user from the system database by ID.
func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userSelectColumns+` FROM users WHERE id = ?`, id))
}

// GetUserByEmail loads a user from the system database by normalized email.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userSelectColumns+` FROM users WHERE email = ?`, cleanEmail(email)))
}

// ListUsers returns all local users for admin pages and startup user-store preparation.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userSelectColumns+` FROM users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func scanUser(row scanDest) (User, error) {
	var u User
	var created, updated int64
	var isAdmin, searchSenderBoost, searchCompactSplitting int
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.BackupEmail, &u.PasswordHash, &isAdmin, &u.DateLocale, &u.DateFormat, &u.Theme, &u.SearchPreset, &u.SearchRecencyBias, &u.SearchFuzzy, &searchSenderBoost, &u.SearchSenderHistory, &u.SearchContactBoost, &u.SearchAttachmentWeight, &searchCompactSplitting, &created, &updated)
	if err != nil {
		return User{}, err
	}
	u.IsAdmin = isAdmin != 0
	u.SearchSenderBoost = searchSenderBoost != 0
	u.SearchCompactSplitting = searchCompactSplitting != 0
	u.CreatedAt = unixTime(created)
	u.UpdatedAt = unixTime(updated)
	normalizeUserPreferences(&u)
	u.BackupEmail = cleanEmail(u.BackupEmail)
	return u, nil
}

func normalizeUserPreferences(u *User) {
	u.DateFormat = normalizeUserDateFormat(u.DateFormat)
	u.Theme = normalizeUserTheme(u.Theme)
	u.SearchPreset = normalizeUserSearchPreset(u.SearchPreset)
	u.SearchRecencyBias = normalizeUserSearchRecencyBias(u.SearchRecencyBias)
	u.SearchFuzzy = normalizeUserSearchFuzzy(u.SearchFuzzy)
	u.SearchSenderHistory = normalizeUserSearchWeight(u.SearchSenderHistory, "normal")
	u.SearchContactBoost = normalizeUserSearchWeight(u.SearchContactBoost, "normal")
	u.SearchAttachmentWeight = normalizeUserSearchAttachmentWeight(u.SearchAttachmentWeight)
}

func (s *Store) UpdateUserBackupEmail(ctx context.Context, userID int64, backupEmail string) (User, error) {
	backupEmail = cleanEmail(backupEmail)
	_, err := s.db.ExecContext(ctx, `UPDATE users SET backup_email = ?, updated_at = ? WHERE id = ?`, backupEmail, nowUnix(), userID)
	if err != nil {
		return User{}, err
	}
	updated, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if err := s.mirrorCachedUser(ctx, updated); err != nil {
		return User{}, err
	}
	return updated, nil
}

func (s *Store) UpdateUserPasswordHash(ctx context.Context, userID int64, passwordHash string) error {
	if strings.TrimSpace(passwordHash) == "" {
		return errors.New("password hash is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, nowUnix(), userID)
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
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// UpdateUserDisplayPreferences preserves the existing search preferences while
// saving the legacy display/date profile fields used by older callers and tests.
func (s *Store) UpdateUserDisplayPreferences(ctx context.Context, userID int64, dateLocale, dateFormat, theme string) (User, error) {
	current, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	return s.UpdateUserPreferences(ctx, userID, dateLocale, dateFormat, theme, current.SearchPreset, current.SearchRecencyBias, current.SearchFuzzy, current.SearchSenderHistory, current.SearchContactBoost, current.SearchAttachmentWeight, current.SearchSenderBoost, current.SearchCompactSplitting)
}

// UpdateUserPreferences saves display/date settings plus query-time search
// tuning. These preferences are read once with the authenticated session user
// and passed through request memory into Bleve, avoiding an extra lookup on
// search routes.
func (s *Store) UpdateUserPreferences(ctx context.Context, userID int64, dateLocale, dateFormat, theme, searchPreset, searchRecencyBias, searchFuzzy, searchSenderHistory, searchContactBoost, searchAttachmentWeight string, searchSenderBoost, searchCompactSplitting bool) (User, error) {
	dateLocale = strings.TrimSpace(dateLocale)
	if len(dateLocale) > 64 {
		dateLocale = dateLocale[:64]
	}
	dateFormat = normalizeUserDateFormat(dateFormat)
	theme = normalizeUserTheme(theme)
	searchPreset = normalizeUserSearchPreset(searchPreset)
	searchRecencyBias = normalizeUserSearchRecencyBias(searchRecencyBias)
	searchFuzzy = normalizeUserSearchFuzzy(searchFuzzy)
	searchSenderHistory = normalizeUserSearchWeight(searchSenderHistory, "normal")
	searchContactBoost = normalizeUserSearchWeight(searchContactBoost, "normal")
	searchAttachmentWeight = normalizeUserSearchAttachmentWeight(searchAttachmentWeight)
	_, err := s.db.ExecContext(ctx, `UPDATE users SET date_locale = ?, date_format = ?, theme = ?, search_preset = ?, search_recency_bias = ?, search_fuzzy = ?, search_sender_boost = ?, search_sender_history = ?, search_contact_boost = ?, search_attachment_weight = ?, search_compact_splitting = ?, updated_at = ? WHERE id = ?`,
		dateLocale, dateFormat, theme, searchPreset, searchRecencyBias, searchFuzzy, boolInt(searchSenderBoost), searchSenderHistory, searchContactBoost, searchAttachmentWeight, boolInt(searchCompactSplitting), nowUnix(), userID)
	if err != nil {
		return User{}, err
	}
	updated, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if err := s.mirrorCachedUser(ctx, updated); err != nil {
		return User{}, err
	}
	return updated, nil
}

func (s *Store) mirrorCachedUser(ctx context.Context, user User) error {
	if !s.split || user.ID == 0 {
		return nil
	}
	s.mu.Lock()
	us := s.userStores[user.ID]
	s.mu.Unlock()
	if us == nil {
		return nil
	}
	return us.mirrorUser(ctx, user)
}

func normalizeUserDateFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "locale", "dmy", "ymd":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "mdy"
	}
}

func normalizeUserTheme(value string) string {
	theme := strings.ToLower(strings.TrimSpace(value))
	switch theme {
	case "classic":
		return "classic"
	case "classic_dark", "classic-dark":
		return "classic_dark"
	case "matrix", "modern":
		return "matrix"
	default:
		if safeUserThemeID(theme) {
			return theme
		}
		return "classic"
	}
}

func safeUserThemeID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for i, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok || (i == 0 && (r < 'a' || r > 'z')) {
			return false
		}
	}
	return true
}

func normalizeUserSearchPreset(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "strict", "forgiving":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "balanced"
	}
}

func normalizeUserSearchRecencyBias(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "light", "strong":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "normal"
	}
}

func normalizeUserSearchFuzzy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "forgiving":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "balanced"
	}
}

func normalizeUserSearchWeight(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "light", "normal", "strong":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return fallback
	}
}

func normalizeUserSearchAttachmentWeight(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "light", "strong":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "normal"
	}
}

// CreateSession stores a hashed session token with an expiry time.
func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash string, expiresAt time.Time) (Session, error) {
	ts := nowUnix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO sessions (user_id, token_hash, expires_at, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)`, userID, tokenHash, expiresAt.UTC().Unix(), ts, ts)
	if err != nil {
		return Session{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Session{}, err
	}
	return Session{ID: id, UserID: userID, TokenHash: tokenHash, ExpiresAt: expiresAt.UTC(), CreatedAt: unixTime(ts), LastSeenAt: unixTime(ts)}, nil
}

// GetSessionUser resolves a hashed session token to its session and user rows.
func (s *Store) GetSessionUser(ctx context.Context, tokenHash string) (Session, User, error) {
	var sess Session
	var u User
	var expires, created, lastSeen, userCreated, userUpdated int64
	var isAdmin, searchSenderBoost, searchCompactSplitting int
	err := s.db.QueryRowContext(ctx, `SELECT
			s.id, s.user_id, s.token_hash, s.expires_at, s.created_at, s.last_seen_at,
			u.id, u.email, u.name, u.backup_email, u.password_hash, u.is_admin, u.date_locale, u.date_format, u.theme, u.search_preset, u.search_recency_bias, u.search_fuzzy, u.search_sender_boost, u.search_sender_history, u.search_contact_boost, u.search_attachment_weight, u.search_compact_splitting, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, nowUnix()).
		Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &expires, &created, &lastSeen,
			&u.ID, &u.Email, &u.Name, &u.BackupEmail, &u.PasswordHash, &isAdmin, &u.DateLocale, &u.DateFormat, &u.Theme, &u.SearchPreset, &u.SearchRecencyBias, &u.SearchFuzzy, &searchSenderBoost, &u.SearchSenderHistory, &u.SearchContactBoost, &u.SearchAttachmentWeight, &searchCompactSplitting, &userCreated, &userUpdated)
	if err != nil {
		return Session{}, User{}, err
	}
	sess.ExpiresAt = unixTime(expires)
	sess.CreatedAt = unixTime(created)
	sess.LastSeenAt = unixTime(lastSeen)
	u.IsAdmin = isAdmin != 0
	u.SearchSenderBoost = searchSenderBoost != 0
	u.SearchCompactSplitting = searchCompactSplitting != 0
	u.CreatedAt = unixTime(userCreated)
	u.UpdatedAt = unixTime(userUpdated)
	u.BackupEmail = cleanEmail(u.BackupEmail)
	normalizeUserPreferences(&u)
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, nowUnix(), sess.ID)
	return sess, u, nil
}

// DeleteSession removes one session token hash during logout.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteExpiredSessions removes expired sessions during startup or maintenance.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, nowUnix())
	return err
}
