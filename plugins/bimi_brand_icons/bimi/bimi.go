// File overview: BIMI domain normalization, lookup records, and asset URL helpers.

package bimi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

const (
	positiveTTL = 30 * 24 * time.Hour
	negativeTTL = 7 * 24 * time.Hour
	maxSVGBytes = 256 * 1024
)

// Result is the cacheable outcome of a BIMI DNS/logo lookup.
type Result struct {
	Domain    string
	LogoURL   string
	SVG       string
	Status    string
	Error     string
	FetchedAt time.Time
	ExpiresAt time.Time
}

// Icon is the stored BIMI icon cache row for one user/domain pair.
type Icon struct {
	ID        int64
	UserID    int64
	Domain    string
	LogoURL   string
	SVG       string
	Status    string
	Error     string
	FetchedAt time.Time
	ExpiresAt time.Time
	UpdatedAt time.Time
}

// IconMeta is the cached BIMI row without the SVG payload.
type IconMeta struct {
	ID        int64
	UserID    int64
	Domain    string
	LogoURL   string
	Status    string
	Error     string
	HasSVG    bool
	FetchedAt time.Time
	ExpiresAt time.Time
	UpdatedAt time.Time
}

// GetIcon loads one cached BIMI icon row for a user/domain pair.
func GetIcon(ctx context.Context, db *sql.DB, userID int64, domain string) (Icon, error) {
	var icon Icon
	var fetchedAt, expiresAt, updatedAt int64
	err := db.QueryRowContext(ctx, `SELECT id, user_id, domain, logo_url, svg, status, error, fetched_at, expires_at, updated_at
		FROM plugin_bimi_brand_icons WHERE user_id = ? AND domain = ?`, userID, NormalizeDomain(domain)).
		Scan(&icon.ID, &icon.UserID, &icon.Domain, &icon.LogoURL, &icon.SVG, &icon.Status, &icon.Error, &fetchedAt, &expiresAt, &updatedAt)
	if err != nil {
		return Icon{}, err
	}
	icon.FetchedAt = unixTime(fetchedAt)
	icon.ExpiresAt = unixTime(expiresAt)
	icon.UpdatedAt = unixTime(updatedAt)
	return icon, nil
}

// GetIconMeta loads cached BIMI metadata without reading the SVG text payload.
func GetIconMeta(ctx context.Context, db *sql.DB, userID int64, domain string) (IconMeta, error) {
	var icon IconMeta
	var fetchedAt, expiresAt, updatedAt int64
	var hasSVG int
	err := db.QueryRowContext(ctx, `SELECT id, user_id, domain, logo_url, status, error,
			CASE WHEN length(svg) > 0 THEN 1 ELSE 0 END,
			fetched_at, expires_at, updated_at
		FROM plugin_bimi_brand_icons WHERE user_id = ? AND domain = ?`, userID, NormalizeDomain(domain)).
		Scan(&icon.ID, &icon.UserID, &icon.Domain, &icon.LogoURL, &icon.Status, &icon.Error, &hasSVG, &fetchedAt, &expiresAt, &updatedAt)
	if err != nil {
		return IconMeta{}, err
	}
	icon.HasSVG = hasSVG != 0
	icon.FetchedAt = unixTime(fetchedAt)
	icon.ExpiresAt = unixTime(expiresAt)
	icon.UpdatedAt = unixTime(updatedAt)
	return icon, nil
}

// UpsertIcon records a BIMI lookup result, including negative or error states with retry timing.
func UpsertIcon(ctx context.Context, db *sql.DB, icon Icon) error {
	domain := NormalizeDomain(icon.Domain)
	if icon.UserID == 0 || domain == "" {
		return nil
	}
	if icon.FetchedAt.IsZero() {
		icon.FetchedAt = time.Now().UTC()
	}
	if icon.UpdatedAt.IsZero() {
		icon.UpdatedAt = icon.FetchedAt
	}
	_, err := db.ExecContext(ctx, `INSERT INTO plugin_bimi_brand_icons
			(user_id, domain, logo_url, svg, status, error, fetched_at, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, domain) DO UPDATE SET
			logo_url = excluded.logo_url,
			svg = excluded.svg,
			status = excluded.status,
			error = excluded.error,
			fetched_at = excluded.fetched_at,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`,
		icon.UserID, domain, icon.LogoURL, icon.SVG, icon.Status, icon.Error,
		icon.FetchedAt.UTC().Unix(), icon.ExpiresAt.UTC().Unix(), icon.UpdatedAt.UTC().Unix())
	return err
}

func unixTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// Resolver performs BIMI TXT lookup, logo URL validation, SVG fetch, and retry timing.
type Resolver struct {
	DNS        *net.Resolver
	HTTPClient *http.Client
	Now        func() time.Time
}

// DomainFromAddress extracts the normalized sender domain used as the BIMI cache key.
func DomainFromAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addrs, err := mail.ParseAddressList(value); err == nil && len(addrs) > 0 {
		return NormalizeDomain(domainFromEmail(addrs[0].Address))
	}
	if idx := strings.LastIndex(value, "@"); idx >= 0 {
		return NormalizeDomain(domainFromEmail(value[idx+1:]))
	}
	return ""
}

// NormalizeDomain canonicalizes domains before DNS lookup or cache access.
func NormalizeDomain(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, "<>.,;:()[]{}\"'")
	if value == "" || len(value) > 253 {
		return ""
	}
	for _, part := range strings.Split(value, ".") {
		if part == "" || len(part) > 63 {
			return ""
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return ""
		}
		if strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return ""
		}
	}
	return value
}

// Fetch resolves and validates BIMI metadata for one domain and returns a cacheable result.
func (r Resolver) Fetch(ctx context.Context, domain string) Result {
	now := r.now()
	result := Result{
		Domain:    NormalizeDomain(domain),
		Status:    "missing",
		FetchedAt: now,
		ExpiresAt: now.Add(negativeTTL),
	}
	if result.Domain == "" {
		result.Status = "invalid"
		result.Error = "invalid domain"
		return result
	}
	logoURL, err := r.lookupLogoURL(ctx, result.Domain)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.LogoURL = logoURL
	svg, err := r.fetchSVG(ctx, logoURL)
	if err != nil {
		result.Status = "invalid"
		result.Error = err.Error()
		return result
	}
	result.SVG = svg
	result.Status = "ok"
	result.Error = ""
	result.ExpiresAt = now.Add(positiveTTL)
	return result
}

func (r Resolver) lookupLogoURL(ctx context.Context, domain string) (string, error) {
	resolver := r.DNS
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	records, err := resolver.LookupTXT(ctx, "default._bimi."+domain)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		fields := parseBIMITXT(record)
		if !strings.EqualFold(fields["v"], "BIMI1") {
			continue
		}
		logoURL := strings.TrimSpace(fields["l"])
		if logoURL == "" {
			return "", errors.New("BIMI record has no logo URL")
		}
		if err := validateLogoURLWithResolver(ctx, resolver, logoURL); err != nil {
			return "", err
		}
		return logoURL, nil
	}
	return "", errors.New("BIMI record not found")
}

func (r Resolver) fetchSVG(ctx context.Context, rawURL string) (string, error) {
	resolver := r.DNS
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	// The URL string is validated pre-dial by validateLogoURL (and per redirect
	// below), but the transport re-resolves DNS at dial time. Validating the
	// resolved IP again inside DialContext closes the rebinding window where a
	// second lookup returns a private address.
	clientCopy := *client
	if clientCopy.Transport == nil {
		clientCopy.Transport = http.DefaultTransport
	}
	baseTransport, ok := clientCopy.Transport.(*http.Transport)
	if ok {
		transport := baseTransport.Clone()
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return safeDialContext(ctx, resolver, network, address)
		}
		clientCopy.Transport = transport
	}
	originalCheck := clientCopy.CheckRedirect
	clientCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originalCheck != nil {
			if err := originalCheck(req, via); err != nil {
				return err
			}
		}
		if len(via) >= 5 {
			return errors.New("too many BIMI logo redirects")
		}
		return validateLogoURLWithResolver(req.Context(), resolver, req.URL.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "image/svg+xml, image/*;q=0.5")
	req.Header.Set("User-Agent", "rolltop/1.0 BIMI")
	res, err := clientCopy.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("fetch BIMI logo: HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxSVGBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxSVGBytes {
		return "", errors.New("BIMI logo is too large")
	}
	svg := strings.TrimSpace(string(body))
	if err := validateSVG(svg); err != nil {
		return "", err
	}
	return svg, nil
}

func (r Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func parseBIMITXT(record string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(record, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return out
}

func validateLogoURL(ctx context.Context, resolver *net.Resolver, rawURL string) error {
	return validateLogoURLWithResolver(ctx, resolver, rawURL)
}

func validateLogoURLWithResolver(ctx context.Context, resolver *net.Resolver, rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return errors.New("BIMI logo URL must use HTTPS")
	}
	if u.User != nil {
		return errors.New("BIMI logo URL must not include credentials")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return errors.New("BIMI logo URL has no host")
	}
	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
		return errors.New("BIMI logo URL host is not allowed")
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("BIMI logo URL host has no addresses")
	}
	for _, ip := range ips {
		if !publicIP(ip.IP) {
			return errors.New("BIMI logo URL resolves to a private address")
		}
	}
	return nil
}

// safeDialContext resolves the target once and only dials addresses that pass
// publicIP validation at dial time. This is the same defense the remote-image
// fetcher uses: a hostile DNS answer that flips to a private address between
// URL validation and connection never reaches the network.
func safeDialContext(ctx context.Context, resolver *net.Resolver, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var firstErr error
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	for _, candidate := range ips {
		if !publicIP(candidate.IP) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, errors.New("BIMI logo host resolves only to private addresses")
}

func validateSVG(svg string) error {
	lower := strings.ToLower(svg)
	if !strings.Contains(lower, "<svg") {
		return errors.New("BIMI logo is not SVG")
	}
	blocked := []string{"<script", "<foreignobject", "<?xml-stylesheet", " onload=", " onerror=", " javascript:"}
	for _, token := range blocked {
		if strings.Contains(lower, token) {
			return errors.New("BIMI logo SVG contains unsupported active content")
		}
	}
	return nil
}

func publicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else {
		ip = ip.To16()
	}
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			// CGNAT 100.64.0.0/10.
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			// Benchmarking 198.18.0.0/15.
			return false
		case v4[0] == 255:
			// Limited broadcast.
			return false
		case v4[0] == 0:
			// "This network" range beyond the unspecified address.
			return false
		}
	}
	return true
}

func domainFromEmail(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[at+1:]
	}
	return value
}
