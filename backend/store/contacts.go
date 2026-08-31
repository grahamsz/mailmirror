// File overview: Contact book, me-contact, contact email, identity, and icon persistence.

package store

import (
	"context"
	"database/sql"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
)

type contactAutocompleteCandidate struct {
	item      ContactAutocomplete
	saved     bool
	frequency int
	recency   int
}

// ContactInteraction is one recent, user-owned message exchanged with a saved
// contact. It intentionally contains only the compact fields needed by the
// contact profile rather than hydrating message bodies.
type ContactInteraction struct {
	MessageID      int64
	Subject        string
	FromAddr       string
	Date           time.Time
	Direction      string
	HasAttachments bool
}

// NormalizeContactEmail canonicalizes email addresses for contact matching and Me identity lookup.
func NormalizeContactEmail(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(value); err == nil {
		value = addr.Address
	}
	return strings.ToLower(strings.Trim(value, "<> \t\r\n"))
}

// CreateContact inserts a user-owned contact plus its child email/phone/address/URL rows.
func (s *Store) CreateContact(ctx context.Context, userID int64, c Contact) (Contact, error) {
	c = normalizeContactForSave(userID, c)
	ts := nowUnix()
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO contacts
			(user_id, name_prefix, given_name, additional_name, family_name, name_suffix, display_name, nickname, organization, department, job_title, birthday, notes, categories, is_me, is_primary, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, c.NamePrefix, c.GivenName, c.AdditionalName, c.FamilyName, c.NameSuffix, c.DisplayName, c.Nickname, c.Organization, c.Department, c.JobTitle, c.Birthday, c.Notes, c.Categories, boolInt(c.IsMe), boolInt(c.IsPrimary), ts, ts)
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if err := replaceContactChildren(ctx, tx, userID, id, c, ts); err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if c.IsMe && c.IsPrimary {
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET is_primary = 0, updated_at = ? WHERE user_id = ? AND id <> ? AND is_me = 1`, ts, userID, id); err != nil {
			_ = tx.Rollback()
			return Contact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	if err := s.ensurePrimaryMeContact(ctx, userID); err != nil {
		return Contact{}, err
	}
	return s.GetContactForUser(ctx, userID, id)
}

// UpdateContact replaces a user-owned contact and synchronizes child detail rows.
func (s *Store) UpdateContact(ctx context.Context, userID, id int64, c Contact) (Contact, error) {
	c = normalizeContactForSave(userID, c)
	ts := nowUnix()
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE contacts SET
			name_prefix = ?, given_name = ?, additional_name = ?, family_name = ?, name_suffix = ?, display_name = ?, nickname = ?, organization = ?, department = ?, job_title = ?, birthday = ?, notes = ?, categories = ?, is_me = ?, is_primary = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		c.NamePrefix, c.GivenName, c.AdditionalName, c.FamilyName, c.NameSuffix, c.DisplayName, c.Nickname, c.Organization, c.Department, c.JobTitle, c.Birthday, c.Notes, c.Categories, boolInt(c.IsMe), boolInt(c.IsPrimary), ts, userID, id)
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if n == 0 {
		_ = tx.Rollback()
		return Contact{}, ErrNotFound
	}
	if err := replaceContactChildren(ctx, tx, userID, id, c, ts); err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if c.IsMe && c.IsPrimary {
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET is_primary = 0, updated_at = ? WHERE user_id = ? AND id <> ? AND is_me = 1`, ts, userID, id); err != nil {
			_ = tx.Rollback()
			return Contact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	if err := s.ensurePrimaryMeContact(ctx, userID); err != nil {
		return Contact{}, err
	}
	return s.GetContactForUser(ctx, userID, id)
}

// DeleteContactForUser removes one contact and dependent detail rows inside the user database.
func (s *Store) DeleteContactForUser(ctx context.Context, userID, id int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM contacts WHERE user_id = ? AND id = ?`, userID, id)
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
	return s.ensurePrimaryMeContact(ctx, userID)
}

// GetContactForUser loads a contact and its detail rows for one user.
func (s *Store) GetContactForUser(ctx context.Context, userID, id int64) (Contact, error) {
	row := s.mustDataDB(ctx, userID).QueryRowContext(ctx, contactSelectSQL()+` WHERE user_id = ? AND id = ?`, userID, id)
	c, err := scanContact(row)
	if err != nil {
		return Contact{}, err
	}
	if err := s.loadContactDetails(ctx, userID, &c); err != nil {
		return Contact{}, err
	}
	return c, nil
}

// GetContactByEmailForUser finds a contact by normalized email inside one user's address book.
func (s *Store) GetContactByEmailForUser(ctx context.Context, userID int64, email string) (Contact, error) {
	normalized := NormalizeContactEmail(email)
	if normalized == "" {
		return Contact{}, ErrNotFound
	}
	row := s.mustDataDB(ctx, userID).QueryRowContext(ctx, contactSelectSQL()+`
		WHERE user_id = ? AND id = (
			SELECT contact_id FROM contact_emails WHERE user_id = ? AND normalized_email = ? LIMIT 1
		)`, userID, userID, normalized)
	c, err := scanContact(row)
	if err != nil {
		return Contact{}, err
	}
	if err := s.loadContactDetails(ctx, userID, &c); err != nil {
		return Contact{}, err
	}
	return c, nil
}

// EnsureMeContactForEmail creates or updates the Me contact used for compose identities and reply targeting.
func (s *Store) EnsureMeContactForEmail(ctx context.Context, userID int64, email, displayName string) (Contact, error) {
	email = strings.TrimSpace(email)
	if NormalizeContactEmail(email) == "" {
		return Contact{}, ErrNotFound
	}
	displayName = trimLimit(displayName, 240)
	if displayName == "" {
		displayName = email
	}
	hasMe, err := s.hasMeContact(ctx, userID)
	if err != nil {
		return Contact{}, err
	}
	contact, err := s.GetContactByEmailForUser(ctx, userID, email)
	if err == nil {
		contact.IsMe = true
		if !hasMe {
			contact.IsPrimary = true
		}
		if strings.TrimSpace(contact.DisplayName) == "" || strings.EqualFold(strings.TrimSpace(contact.DisplayName), strings.TrimSpace(email)) {
			contact.DisplayName = displayName
		}
		updated, err := s.UpdateContact(ctx, userID, contact.ID, contact)
		if err != nil {
			return Contact{}, err
		}
		if err := s.SyncMailIdentitiesForMeContacts(ctx, userID); err != nil {
			return Contact{}, err
		}
		return updated, nil
	}
	if !IsNotFound(err) {
		return Contact{}, err
	}
	created, err := s.CreateContact(ctx, userID, Contact{
		DisplayName: displayName,
		IsMe:        true,
		IsPrimary:   !hasMe,
		Emails: []ContactEmail{{
			Label:     "email",
			Email:     email,
			IsPrimary: true,
		}},
	})
	if err != nil {
		return Contact{}, err
	}
	if err := s.SyncMailIdentitiesForMeContacts(ctx, userID); err != nil {
		return Contact{}, err
	}
	return created, nil
}

func (s *Store) hasMeContact(ctx context.Context, userID int64) (bool, error) {
	var n int
	if err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE user_id = ? AND is_me = 1`, userID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListContactsForUser returns contacts matching the optional address-book query.
func (s *Store) ListContactsForUser(ctx context.Context, userID int64, query string, limit int) ([]Contact, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	query = strings.TrimSpace(query)
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.mustDataDB(ctx, userID).QueryContext(ctx, contactSelectSQL()+`
			WHERE user_id = ?
			ORDER BY lower(CASE WHEN display_name <> '' THEN display_name ELSE family_name || ' ' || given_name END), id
			LIMIT ?`, userID, limit)
	} else {
		like := "%" + strings.ToLower(query) + "%"
		rows, err = s.mustDataDB(ctx, userID).QueryContext(ctx, contactSelectSQL()+`
			WHERE user_id = ? AND (
				lower(display_name) LIKE ? OR lower(given_name) LIKE ? OR lower(family_name) LIKE ? OR lower(organization) LIKE ?
				OR EXISTS (SELECT 1 FROM contact_emails WHERE contact_emails.user_id = contacts.user_id AND contact_emails.contact_id = contacts.id AND lower(email) LIKE ?)
			)
			ORDER BY lower(CASE WHEN display_name <> '' THEN display_name ELSE family_name || ' ' || given_name END), id
			LIMIT ?`, userID, like, like, like, like, like, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts, err := scanContacts(rows)
	if err != nil {
		return nil, err
	}
	for i := range contacts {
		if err := s.loadContactDetails(ctx, userID, &contacts[i]); err != nil {
			return nil, err
		}
	}
	return contacts, nil
}

// ListContactInteractionsForUser returns the newest distinct conversations
// involving any email saved on the contact. The contact lookup and message
// query are both scoped to userID; browser callers never choose that value.
func (s *Store) ListContactInteractionsForUser(ctx context.Context, userID, contactID int64, limit int) ([]ContactInteraction, error) {
	if limit <= 0 || limit > 20 {
		limit = 6
	}
	contact, err := s.GetContactForUser(ctx, userID, contactID)
	if err != nil {
		return nil, err
	}
	targets := map[string]struct{}{}
	for _, item := range contact.Emails {
		if email := NormalizeContactEmail(item.Email); email != "" {
			targets[email] = struct{}{}
		}
	}
	if len(targets) == 0 {
		return []ContactInteraction{}, nil
	}

	// Walk the existing user/date index newest-first. The SQL substring test
	// narrows candidates cheaply; parsed addresses below provide exact matching.
	predicates := make([]string, 0, len(targets)*3)
	args := make([]any, 0, 2+len(targets)*3)
	args = append(args, userID)
	for email := range targets {
		for _, column := range []string{"from_addr", "to_addr", "cc_addr"} {
			predicates = append(predicates, "instr(lower("+column+"), ?) > 0")
			args = append(args, email)
		}
	}
	candidateLimit := limit * 12
	if candidateLimit < 48 {
		candidateLimit = 48
	}
	args = append(args, candidateLimit)
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT
			id, message_id_header, thread_key, subject, from_addr, to_addr, cc_addr,
			date_unix, has_attachments
		FROM messages
		WHERE user_id = ? AND (`+strings.Join(predicates, " OR ")+`)
		ORDER BY date_unix DESC, id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seenConversations := map[string]struct{}{}
	out := make([]ContactInteraction, 0, limit)
	for rows.Next() {
		var (
			messageID, dateUnix int64
			messageIDHeader     string
			threadKey           string
			subject             string
			fromAddr            string
			toAddr              string
			ccAddr              string
			hasAttachments      bool
		)
		if err := rows.Scan(&messageID, &messageIDHeader, &threadKey, &subject, &fromAddr, &toAddr, &ccAddr, &dateUnix, &hasAttachments); err != nil {
			return nil, err
		}
		fromContact := contactAddressFieldMatches(fromAddr, targets)
		toContact := contactAddressFieldMatches(toAddr, targets) || contactAddressFieldMatches(ccAddr, targets)
		if !fromContact && !toContact {
			continue
		}
		conversationKey := strings.TrimSpace(threadKey)
		if conversationKey == "" {
			conversationKey = strings.TrimSpace(messageIDHeader)
		}
		if conversationKey == "" {
			conversationKey = "id:" + strconv.FormatInt(messageID, 10)
		}
		if _, seen := seenConversations[conversationKey]; seen {
			continue
		}
		seenConversations[conversationKey] = struct{}{}
		direction := "sent"
		if fromContact {
			direction = "received"
		}
		out = append(out, ContactInteraction{
			MessageID:      messageID,
			Subject:        subject,
			FromAddr:       fromAddr,
			Date:           unixTime(dateUnix),
			Direction:      direction,
			HasAttachments: hasAttachments,
		})
		if len(out) == limit {
			break
		}
	}
	return out, rows.Err()
}

func contactAddressFieldMatches(value string, targets map[string]struct{}) bool {
	for _, address := range autocompleteAddresses(value) {
		if _, ok := targets[NormalizeContactEmail(address.Address)]; ok {
			return true
		}
	}
	return false
}

// AutocompleteContactsForUser merges saved contacts with recent user-owned
// correspondents. Recent addresses remain suggestions only; they are not imported.
func (s *Store) AutocompleteContactsForUser(ctx context.Context, userID int64, query string, limit int) ([]ContactAutocomplete, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	query = strings.ToLower(strings.TrimSpace(query))
	like := "%" + query + "%"
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT
			c.id, c.display_name, c.given_name, c.family_name, e.email, e.label,
			CASE WHEN ci.id IS NULL THEN '' ELSE '/contacts/' || c.id || '/icon' END AS icon_url
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		LEFT JOIN contact_icons ci ON ci.user_id = c.user_id AND ci.contact_id = c.id
		WHERE e.user_id = ? AND (? = '' OR lower(e.email) LIKE ? OR lower(c.display_name) LIKE ? OR lower(c.given_name) LIKE ? OR lower(c.family_name) LIKE ? OR lower(c.organization) LIKE ?)
		ORDER BY e.is_primary DESC, lower(CASE WHEN c.display_name <> '' THEN c.display_name ELSE c.given_name || ' ' || c.family_name END), lower(e.email)
			LIMIT ?`, userID, query, like, like, like, like, like, limit*2)
	if err != nil {
		return nil, err
	}
	candidates := map[string]*contactAutocompleteCandidate{}
	for rows.Next() {
		var item ContactAutocomplete
		var display, given, family string
		if err := rows.Scan(&item.ContactID, &display, &given, &family, &item.Email, &item.Label, &item.IconURL); err != nil {
			rows.Close()
			return nil, err
		}
		item.Name = contactName(display, given, family)
		key := NormalizeContactEmail(item.Email)
		if key != "" {
			candidates[key] = &contactAutocompleteCandidate{item: item, saved: true}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	own, err := s.autocompleteOwnAddresses(ctx, userID)
	if err != nil {
		return nil, err
	}
	recentRows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT from_addr, to_addr, cc_addr
		FROM messages WHERE user_id = ? ORDER BY date_unix DESC, id DESC LIMIT 250`, userID)
	if err != nil {
		return nil, err
	}
	recentIndex := 250
	for recentRows.Next() {
		var from, to, cc string
		if err := recentRows.Scan(&from, &to, &cc); err != nil {
			recentRows.Close()
			return nil, err
		}
		for _, address := range autocompleteAddresses(from, to, cc) {
			key := NormalizeContactEmail(address.Address)
			if key == "" || own[key] || !autocompleteAddressMatches(address, query) {
				continue
			}
			entry := candidates[key]
			if entry == nil {
				entry = &contactAutocompleteCandidate{item: ContactAutocomplete{
					Name:  strings.TrimSpace(address.Name),
					Email: address.Address,
					Label: "Recent",
				}}
				candidates[key] = entry
			}
			entry.frequency++
			if recentIndex > entry.recency {
				entry.recency = recentIndex
			}
		}
		recentIndex--
	}
	if err := recentRows.Err(); err != nil {
		recentRows.Close()
		return nil, err
	}
	recentRows.Close()

	ranked := make([]*contactAutocompleteCandidate, 0, len(candidates))
	for _, item := range candidates {
		ranked = append(ranked, item)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := autocompleteCandidateScore(ranked[i], query)
		right := autocompleteCandidateScore(ranked[j], query)
		if left != right {
			return left > right
		}
		return strings.ToLower(ranked[i].item.Email) < strings.ToLower(ranked[j].item.Email)
	})
	out := make([]ContactAutocomplete, 0, min(limit, len(ranked)))
	for _, item := range ranked {
		out = append(out, item.item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) autocompleteOwnAddresses(ctx context.Context, userID int64) (map[string]bool, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT email FROM mail_accounts WHERE user_id = ?
		UNION SELECT e.email FROM contact_emails e JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 1`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		if normalized := NormalizeContactEmail(address); normalized != "" {
			out[normalized] = true
		}
	}
	return out, rows.Err()
}

func autocompleteAddresses(values ...string) []*mail.Address {
	var out []*mail.Address
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 8192 {
			value = value[:8192]
		}
		addresses, err := mail.ParseAddressList(value)
		if err != nil {
			addresses = nil
			for _, part := range strings.Split(value, ";") {
				if address, parseErr := mail.ParseAddress(strings.TrimSpace(part)); parseErr == nil {
					addresses = append(addresses, address)
				}
			}
		}
		for _, address := range addresses {
			key := NormalizeContactEmail(address.Address)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, address)
		}
	}
	return out
}

func autocompleteAddressMatches(address *mail.Address, query string) bool {
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(address.Address), query) ||
		strings.Contains(strings.ToLower(address.Name), query)
}

func autocompleteCandidateScore(item *contactAutocompleteCandidate, query string) int {
	score := item.recency + min(item.frequency, 20)*20
	if item.saved {
		score += 500
	}
	if query == "" {
		return score
	}
	email := strings.ToLower(item.item.Email)
	name := strings.ToLower(item.item.Name)
	switch {
	case email == query:
		score += 2000
	case strings.HasPrefix(email, query):
		score += 1200
	case strings.HasPrefix(name, query):
		score += 1000
	default:
		score += 400
	}
	return score
}

// ListContactEmailsForSearchBoostForUser returns non-Me contact email addresses
// used as optional from: ranking nudges during best-match search. The caller
// applies the actual boost weight, so this helper stays cacheable and tenant-scoped.
func (s *Store) ListContactEmailsForSearchBoostForUser(ctx context.Context, userID int64, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT DISTINCT e.email
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 0 AND e.normalized_email != ''
		ORDER BY lower(e.email)
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		identity := SenderIdentity(email)
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		out = append(out, identity)
	}
	return out, rows.Err()
}

// ListMeContactsForUser returns contacts marked as the signed-in user's own identities.
func (s *Store) ListMeContactsForUser(ctx context.Context, userID int64) ([]Contact, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, contactSelectSQL()+`
		WHERE user_id = ? AND is_me = 1
		ORDER BY is_primary DESC, lower(CASE WHEN display_name <> '' THEN display_name ELSE given_name || ' ' || family_name END), id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts, err := scanContacts(rows)
	if err != nil {
		return nil, err
	}
	for i := range contacts {
		if err := s.loadContactDetails(ctx, userID, &contacts[i]); err != nil {
			return nil, err
		}
	}
	return contacts, nil
}

// ListMeContactEmailsForUser returns normalized Me email addresses for reply and sender detection.
func (s *Store) ListMeContactEmailsForUser(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT e.email
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 1
		ORDER BY c.is_primary DESC, e.is_primary DESC, e.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		if strings.TrimSpace(email) != "" {
			out = append(out, email)
		}
	}
	return out, rows.Err()
}

// SetContactIcon records a contact icon blob for one user-owned contact.
func (s *Store) SetContactIcon(ctx context.Context, userID, contactID, blobID int64, contentType, filename string, size int64) (ContactIcon, error) {
	if _, err := s.GetContactForUser(ctx, userID, contactID); err != nil {
		return ContactIcon{}, err
	}
	blob, err := s.GetBlobForUser(ctx, userID, blobID)
	if err != nil {
		return ContactIcon{}, err
	}
	ts := nowUnix()
	_, err = s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO contact_icons (user_id, contact_id, blob_id, content_type, filename, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, contact_id) DO UPDATE SET blob_id = excluded.blob_id, content_type = excluded.content_type, filename = excluded.filename, size = excluded.size, updated_at = excluded.updated_at`,
		userID, contactID, blob.ID, strings.TrimSpace(contentType), strings.TrimSpace(filename), size, ts, ts)
	if err != nil {
		return ContactIcon{}, err
	}
	return s.GetContactIconForUser(ctx, userID, contactID)
}

// GetContactIconForUser loads icon metadata for one user-owned contact.
func (s *Store) GetContactIconForUser(ctx context.Context, userID, contactID int64) (ContactIcon, error) {
	var icon ContactIcon
	var created, updated int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT ci.id, ci.user_id, ci.contact_id, ci.blob_id, ci.content_type, ci.filename, ci.size, b.path, ci.created_at, ci.updated_at
		FROM contact_icons ci
		JOIN blobs b ON b.user_id = ci.user_id AND b.id = ci.blob_id
		WHERE ci.user_id = ? AND ci.contact_id = ?`, userID, contactID).
		Scan(&icon.ID, &icon.UserID, &icon.ContactID, &icon.BlobID, &icon.ContentType, &icon.Filename, &icon.Size, &icon.BlobPath, &created, &updated)
	icon.CreatedAt = unixTime(created)
	icon.UpdatedAt = unixTime(updated)
	return icon, err
}

// GetContactIconByEmailForUser loads icon metadata by matching a contact email.
func (s *Store) GetContactIconByEmailForUser(ctx context.Context, userID int64, email string) (ContactIcon, error) {
	normalized := NormalizeContactEmail(email)
	if normalized == "" {
		return ContactIcon{}, ErrNotFound
	}
	var contactID int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT e.contact_id
		FROM contact_emails e
		JOIN contact_icons ci ON ci.user_id = e.user_id AND ci.contact_id = e.contact_id
		WHERE e.user_id = ? AND e.normalized_email = ?
		LIMIT 1`, userID, normalized).Scan(&contactID)
	if err != nil {
		return ContactIcon{}, err
	}
	return s.GetContactIconForUser(ctx, userID, contactID)
}

// ListContactIconsByEmailsForUser loads contact icon metadata for a batch of normalized emails.
func (s *Store) ListContactIconsByEmailsForUser(ctx context.Context, userID int64, emails []string) (map[string]ContactIcon, error) {
	normalized := make([]string, 0, len(emails))
	seen := map[string]bool{}
	for _, email := range emails {
		key := NormalizeContactEmail(email)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		normalized = append(normalized, key)
	}
	out := map[string]ContactIcon{}
	if userID <= 0 || len(normalized) == 0 {
		return out, nil
	}
	var hasIcon int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT 1 FROM contact_icons WHERE user_id = ? LIMIT 1`, userID).Scan(&hasIcon)
	if IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(normalized); start += 500 {
		end := start + 500
		if end > len(normalized) {
			end = len(normalized)
		}
		chunk := normalized[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, email := range chunk {
			args = append(args, email)
		}
		rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT e.normalized_email, ci.id, ci.user_id, ci.contact_id, ci.blob_id, ci.content_type, ci.filename, ci.size, b.path, ci.created_at, ci.updated_at
			FROM contact_emails e
			JOIN contact_icons ci ON ci.user_id = e.user_id AND ci.contact_id = e.contact_id
			JOIN blobs b ON b.user_id = ci.user_id AND b.id = ci.blob_id
			WHERE e.user_id = ? AND e.normalized_email IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			var icon ContactIcon
			var created, updated int64
			if err := rows.Scan(&key, &icon.ID, &icon.UserID, &icon.ContactID, &icon.BlobID, &icon.ContentType, &icon.Filename, &icon.Size, &icon.BlobPath, &created, &updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			icon.CreatedAt = unixTime(created)
			icon.UpdatedAt = unixTime(updated)
			if _, exists := out[key]; !exists {
				out[key] = icon
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteContactIconForUser removes icon metadata from a user-owned contact.
func (s *Store) DeleteContactIconForUser(ctx context.Context, userID, contactID int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM contact_icons WHERE user_id = ? AND contact_id = ?`, userID, contactID)
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

func contactSelectSQL() string {
	return `SELECT id, user_id, name_prefix, given_name, additional_name, family_name, name_suffix, display_name, nickname, organization, department, job_title, birthday, notes, categories, is_me, is_primary, created_at, updated_at FROM contacts`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanContact(row rowScanner) (Contact, error) {
	var c Contact
	var created, updated int64
	var isMe, isPrimary int
	err := row.Scan(&c.ID, &c.UserID, &c.NamePrefix, &c.GivenName, &c.AdditionalName, &c.FamilyName, &c.NameSuffix, &c.DisplayName, &c.Nickname, &c.Organization, &c.Department, &c.JobTitle, &c.Birthday, &c.Notes, &c.Categories, &isMe, &isPrimary, &created, &updated)
	c.IsMe = isMe != 0
	c.IsPrimary = isPrimary != 0
	c.CreatedAt = unixTime(created)
	c.UpdatedAt = unixTime(updated)
	return c, err
}

func scanContacts(rows *sql.Rows) ([]Contact, error) {
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) loadContactDetails(ctx context.Context, userID int64, c *Contact) error {
	emails, err := s.listContactEmails(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	phones, err := s.listContactPhones(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	addresses, err := s.listContactAddresses(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	urls, err := s.listContactURLs(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	c.Emails = emails
	c.Phones = phones
	c.Addresses = addresses
	c.URLs = urls
	if icon, err := s.GetContactIconForUser(ctx, userID, c.ID); err == nil {
		c.Icon = &icon
	} else if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

func replaceContactChildren(ctx context.Context, tx *sql.Tx, userID, contactID int64, c Contact, ts int64) error {
	for _, table := range []string{"contact_emails", "contact_phones", "contact_addresses", "contact_urls"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id = ? AND contact_id = ?`, userID, contactID); err != nil {
			return err
		}
	}
	for i, email := range c.Emails {
		email.Email = strings.TrimSpace(email.Email)
		email.NormalizedEmail = NormalizeContactEmail(email.Email)
		if email.Email == "" || email.NormalizedEmail == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_emails (user_id, contact_id, label, email, normalized_email, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(email.Label), email.Email, email.NormalizedEmail, boolInt(email.IsPrimary || i == 0), ts, ts); err != nil {
			return err
		}
	}
	for i, phone := range c.Phones {
		phone.Number = strings.TrimSpace(phone.Number)
		if phone.Number == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_phones (user_id, contact_id, label, number, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(phone.Label), phone.Number, boolInt(phone.IsPrimary || i == 0), ts, ts); err != nil {
			return err
		}
	}
	for i, addr := range c.Addresses {
		if strings.TrimSpace(addr.Street+addr.Locality+addr.Region+addr.PostalCode+addr.Country) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_addresses (user_id, contact_id, label, street, locality, region, postal_code, country, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(addr.Label), strings.TrimSpace(addr.Street), strings.TrimSpace(addr.Locality), strings.TrimSpace(addr.Region), strings.TrimSpace(addr.PostalCode), strings.TrimSpace(addr.Country), boolInt(addr.IsPrimary || i == 0), ts, ts); err != nil {
			return err
		}
	}
	for i, u := range c.URLs {
		u.URL = strings.TrimSpace(u.URL)
		if u.URL == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_urls (user_id, contact_id, label, url, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(u.Label), u.URL, boolInt(u.IsPrimary || i == 0), ts, ts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) listContactEmails(ctx context.Context, userID, contactID int64) ([]ContactEmail, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, email, normalized_email, is_primary, created_at, updated_at
		FROM contact_emails WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactEmail
	for rows.Next() {
		var item ContactEmail
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Email, &item.NormalizedEmail, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactPhones(ctx context.Context, userID, contactID int64) ([]ContactPhone, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, number, is_primary, created_at, updated_at
		FROM contact_phones WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactPhone
	for rows.Next() {
		var item ContactPhone
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Number, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactAddresses(ctx context.Context, userID, contactID int64) ([]ContactAddress, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, street, locality, region, postal_code, country, is_primary, created_at, updated_at
		FROM contact_addresses WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactAddress
	for rows.Next() {
		var item ContactAddress
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Street, &item.Locality, &item.Region, &item.PostalCode, &item.Country, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactURLs(ctx context.Context, userID, contactID int64) ([]ContactURL, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, url, is_primary, created_at, updated_at
		FROM contact_urls WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactURL
	for rows.Next() {
		var item ContactURL
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.URL, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func normalizeContactForSave(userID int64, c Contact) Contact {
	c.UserID = userID
	c.NamePrefix = trimLimit(c.NamePrefix, 80)
	c.GivenName = trimLimit(c.GivenName, 160)
	c.AdditionalName = trimLimit(c.AdditionalName, 160)
	c.FamilyName = trimLimit(c.FamilyName, 160)
	c.NameSuffix = trimLimit(c.NameSuffix, 80)
	c.DisplayName = trimLimit(c.DisplayName, 240)
	c.Nickname = trimLimit(c.Nickname, 160)
	c.Organization = trimLimit(c.Organization, 240)
	c.Department = trimLimit(c.Department, 160)
	c.JobTitle = trimLimit(c.JobTitle, 160)
	c.Birthday = trimLimit(c.Birthday, 32)
	c.Notes = trimLimit(c.Notes, 12000)
	c.Categories = trimLimit(c.Categories, 1000)
	if c.DisplayName == "" {
		c.DisplayName = contactName("", c.GivenName, c.FamilyName)
	}
	if c.DisplayName == "" && len(c.Emails) > 0 {
		c.DisplayName = strings.TrimSpace(c.Emails[0].Email)
	}
	if c.DisplayName == "" {
		c.DisplayName = strings.TrimSpace(c.Organization)
	}
	return c
}

func contactName(display, given, family string) string {
	display = strings.TrimSpace(display)
	if display != "" {
		return display
	}
	return strings.TrimSpace(strings.Join(strings.Fields(strings.TrimSpace(given)+" "+strings.TrimSpace(family)), " "))
}

func trimLimit(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

func (s *Store) ensurePrimaryMeContact(ctx context.Context, userID int64) error {
	var n int
	if err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE user_id = ? AND is_me = 1 AND is_primary = 1`, userID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE contacts SET is_primary = 1, updated_at = ? WHERE id = (
		SELECT id FROM contacts WHERE user_id = ? AND is_me = 1 ORDER BY id LIMIT 1
	)`, nowUnix(), userID)
	return err
}
