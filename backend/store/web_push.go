// File overview: User-scoped browser Web Push subscription persistence.

package store

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const maxWebPushEndpointLength = 4096

const (
	maxWebPushP256DHLength = 128
	maxWebPushAuthLength   = 64
)

var nonPublicWebPushPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

// ErrInvalidWebPushSubscription marks a subscription the browser sent wrong, as
// opposed to a storage failure. HTTP callers answer the first with 400 and the
// second with 500, so the two must stay distinguishable.
var ErrInvalidWebPushSubscription = errors.New("invalid web push subscription")

// SaveWebPushSubscription upserts one Push API endpoint for the signed-in user.
func (s *Store) SaveWebPushSubscription(ctx context.Context, userID int64, sub WebPushSubscription) (WebPushSubscription, error) {
	endpoint := strings.TrimSpace(sub.Endpoint)
	p256dh := strings.TrimSpace(sub.P256DH)
	auth := strings.TrimSpace(sub.Auth)
	if userID <= 0 || endpoint == "" || p256dh == "" || auth == "" {
		return WebPushSubscription{}, fmt.Errorf("%w: fields are incomplete", ErrInvalidWebPushSubscription)
	}
	if err := validateWebPushEndpoint(endpoint); err != nil {
		return WebPushSubscription{}, fmt.Errorf("%w: %w", ErrInvalidWebPushSubscription, err)
	}
	if err := validateWebPushKeyMaterial(p256dh, auth); err != nil {
		return WebPushSubscription{}, fmt.Errorf("%w: %w", ErrInvalidWebPushSubscription, err)
	}
	userAgent := trimLimit(strings.TrimSpace(sub.UserAgent), 500)
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO web_push_subscriptions
			(user_id, endpoint, p256dh, auth, user_agent, created_at, updated_at, last_seen_at,
			 last_new_mail_event_id, last_snooze_reminder_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT COALESCE(MAX(id), 0) FROM new_mail_events WHERE user_id = ?),
			(SELECT COALESCE(MAX(id), 0) FROM snooze_reminder_events WHERE user_id = ?))
		ON CONFLICT(user_id, endpoint) DO UPDATE SET
			p256dh = excluded.p256dh,
			auth = excluded.auth,
			user_agent = excluded.user_agent,
			updated_at = excluded.updated_at,
			last_seen_at = excluded.last_seen_at`,
		userID, endpoint, p256dh, auth, userAgent, ts, ts, ts, userID, userID)
	if err != nil {
		return WebPushSubscription{}, err
	}
	return s.getWebPushSubscription(ctx, userID, endpoint)
}

func (s *Store) getWebPushSubscription(ctx context.Context, userID int64, endpoint string) (WebPushSubscription, error) {
	return scanWebPushSubscription(s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, endpoint, p256dh, auth, user_agent,
		last_new_mail_event_id, last_snooze_reminder_event_id, created_at, updated_at, last_seen_at
		FROM web_push_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint))
}

// ListWebPushSubscriptions returns the user's registered browser push endpoints.
func (s *Store) ListWebPushSubscriptions(ctx context.Context, userID int64) ([]WebPushSubscription, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, endpoint, p256dh, auth, user_agent,
		last_new_mail_event_id, last_snooze_reminder_event_id, created_at, updated_at, last_seen_at
		FROM web_push_subscriptions WHERE user_id = ? ORDER BY updated_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebPushSubscription
	for rows.Next() {
		sub, err := scanWebPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// AdvanceWebPushSubscriptionNewMailCursor records a push accepted by one
// endpoint owned by the supplied user.
func (s *Store) AdvanceWebPushSubscriptionNewMailCursor(ctx context.Context, userID int64, delivered WebPushSubscription, eventID int64) (bool, error) {
	endpoint := strings.TrimSpace(delivered.Endpoint)
	if userID <= 0 || endpoint == "" || delivered.P256DH == "" || delivered.Auth == "" || eventID <= 0 {
		return false, errors.New("invalid web push delivery cursor")
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE web_push_subscriptions
		SET last_new_mail_event_id = ?
		WHERE user_id = ? AND endpoint = ? AND p256dh = ? AND auth = ?
			AND last_new_mail_event_id < ?`,
		eventID, userID, endpoint, delivered.P256DH, delivered.Auth, eventID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// AdvanceWebPushSubscriptionSnoozeReminderCursor records an accepted reminder
// push without changing the endpoint's independent new-mail cursor.
func (s *Store) AdvanceWebPushSubscriptionSnoozeReminderCursor(ctx context.Context, userID int64, delivered WebPushSubscription, eventID int64) (bool, error) {
	endpoint := strings.TrimSpace(delivered.Endpoint)
	if userID <= 0 || endpoint == "" || delivered.P256DH == "" || delivered.Auth == "" || eventID <= 0 {
		return false, errors.New("invalid web push snooze reminder cursor")
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE web_push_subscriptions
		SET last_snooze_reminder_event_id = ?
		WHERE user_id = ? AND endpoint = ? AND p256dh = ? AND auth = ?
			AND last_snooze_reminder_event_id < ?`,
		eventID, userID, endpoint, delivered.P256DH, delivered.Auth, eventID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// DeleteWebPushSubscription removes one endpoint owned by the signed-in user.
func (s *Store) DeleteWebPushSubscription(ctx context.Context, userID int64, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if userID <= 0 || endpoint == "" {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE user_id = ? AND endpoint = ?`, userID, endpoint)
	return err
}

// DeleteWebPushSubscriptionIfCurrent removes a rejected delivery target only
// when its encryption keys still match the attempted delivery.
func (s *Store) DeleteWebPushSubscriptionIfCurrent(ctx context.Context, userID int64, delivered WebPushSubscription) (bool, error) {
	endpoint := strings.TrimSpace(delivered.Endpoint)
	if userID <= 0 || endpoint == "" || delivered.P256DH == "" || delivered.Auth == "" {
		return false, errors.New("invalid web push subscription delete")
	}
	result, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM web_push_subscriptions
		WHERE user_id = ? AND endpoint = ? AND p256dh = ? AND auth = ?`,
		userID, endpoint, delivered.P256DH, delivered.Auth)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func scanWebPushSubscription(row scanDest) (WebPushSubscription, error) {
	var sub WebPushSubscription
	var created, updated, lastSeen int64
	err := row.Scan(&sub.ID, &sub.UserID, &sub.Endpoint, &sub.P256DH, &sub.Auth, &sub.UserAgent,
		&sub.LastNewMailEventID, &sub.LastSnoozeReminderEventID, &created, &updated, &lastSeen)
	sub.CreatedAt = unixTime(created)
	sub.UpdatedAt = unixTime(updated)
	sub.LastSeenAt = unixTime(lastSeen)
	return sub, err
}

func validateWebPushEndpoint(endpoint string) error {
	if len(endpoint) > maxWebPushEndpointLength {
		return errors.New("web push endpoint is too long")
	}
	if strings.Contains(endpoint, "#") {
		return errors.New("web push endpoint contains unsupported URL components")
	}
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.Opaque != "" {
		return errors.New("web push endpoint must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return errors.New("web push endpoint contains unsupported URL components")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home.arpa") {
		return errors.New("web push endpoint host is not public")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 || value > 65535 {
			return errors.New("web push endpoint port is invalid")
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok || !isPublicWebPushAddr(addr.Unmap()) {
			return errors.New("web push endpoint address is not public")
		}
		return nil
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("web push endpoint host %q is not public", host)
	}
	return nil
}

func isPublicWebPushAddr(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicWebPushPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func validateWebPushKeyMaterial(p256dh, auth string) error {
	if len(p256dh) > maxWebPushP256DHLength || len(auth) > maxWebPushAuthLength {
		return errors.New("web push subscription key material is too long")
	}
	publicKey, err := decodeWebPushSubscriptionValue(p256dh)
	if err != nil || len(publicKey) != 65 || publicKey[0] != 0x04 {
		return errors.New("web push subscription public key is invalid")
	}
	if x, y := elliptic.Unmarshal(elliptic.P256(), publicKey); x == nil || y == nil {
		return errors.New("web push subscription public key is invalid")
	}
	authSecret, err := decodeWebPushSubscriptionValue(auth)
	if err != nil || len(authSecret) != 16 {
		return errors.New("web push subscription auth secret is invalid")
	}
	return nil
}

func decodeWebPushSubscriptionValue(value string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	var lastErr error
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
