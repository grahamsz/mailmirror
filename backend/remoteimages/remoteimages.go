// File overview: Remote email image extraction and user-scoped warm cache.

package remoteimages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"rolltop/backend/blob"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	MaxImagesPerMessage = 50
	MaxImageBytes       = 10 * 1024 * 1024
	FetchTimeout        = 2 * time.Second
	SuccessTTL          = 30 * 24 * time.Hour
	ErrorTTL            = 15 * time.Minute
	BlockedTTL          = 24 * time.Hour
)

type Candidate struct {
	URL    string
	Source string
}

type Cache struct {
	Store *store.Store
	Blobs *blob.Store
	Allow func(context.Context, plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error)
}

var (
	imgTagRE        = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	imageAttrRE     = regexp.MustCompile(`(?is)\b(src|srcset)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	cssURLRE        = regexp.MustCompile(`(?is)url\(\s*("([^"]*)"|'([^']*)'|([^'")\s]+))\s*\)`)
	remoteWarmSlots = make(chan struct{}, 4)
)

// Extract finds remote image URLs in img src/srcset and CSS url(...) references.
func Extract(bodyHTML string) []Candidate {
	if strings.TrimSpace(bodyHTML) == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []Candidate{}
	add := func(raw, source string) {
		for _, candidate := range normalizeCandidates(raw) {
			if candidate == "" || seen[candidate] {
				continue
			}
			seen[candidate] = true
			out = append(out, Candidate{URL: candidate, Source: source})
			if len(out) >= MaxImagesPerMessage {
				return
			}
		}
	}
	for _, tag := range imgTagRE.FindAllString(bodyHTML, -1) {
		for _, match := range imageAttrRE.FindAllStringSubmatch(tag, -1) {
			value := firstNonEmpty(match[3], match[4], match[5])
			if strings.EqualFold(match[1], "srcset") {
				for _, part := range strings.Split(value, ",") {
					fields := strings.Fields(strings.TrimSpace(part))
					if len(fields) > 0 {
						add(fields[0], "srcset")
					}
				}
			} else {
				add(value, "img-src")
			}
			if len(out) >= MaxImagesPerMessage {
				return out
			}
		}
	}
	for _, match := range cssURLRE.FindAllStringSubmatch(bodyHTML, -1) {
		add(firstNonEmpty(match[2], match[3], match[4]), "css-url")
		if len(out) >= MaxImagesPerMessage {
			return out
		}
	}
	return out
}

func Hash(rawURL string) string {
	sum := sha256.Sum256([]byte(normalizeURL(rawURL)))
	return hex.EncodeToString(sum[:])
}

func (c Cache) WarmMessageAsync(req plugins.RemoteImageFetchRequest, bodyHTML string) {
	candidates := Extract(bodyHTML)
	if len(candidates) == 0 || c.Store == nil || c.Blobs == nil {
		return
	}
	select {
	case remoteWarmSlots <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-remoteWarmSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(candidates))*FetchTimeout+FetchTimeout)
		defer cancel()
		_ = c.WarmMessage(ctx, req, candidates)
	}()
}

func (c Cache) WarmMessage(ctx context.Context, req plugins.RemoteImageFetchRequest, candidates []Candidate) error {
	if c.Store == nil || c.Blobs == nil {
		return nil
	}
	now := time.Now().UTC()
	for i, candidate := range candidates {
		if i >= MaxImagesPerMessage {
			break
		}
		candidate.URL = normalizeURL(candidate.URL)
		if !validRemoteImageURL(candidate.URL) {
			continue
		}
		hash := Hash(candidate.URL)
		if existing, err := c.Store.GetRemoteImageCacheByHash(ctx, req.UserID, hash); err == nil && store.RemoteImageCacheFresh(existing, now.Unix()) {
			continue
		}
		req.URL = candidate.URL
		req.Source = candidate.Source
		if c.Allow != nil {
			decision, err := c.Allow(ctx, req)
			if err != nil {
				return err
			}
			if !decision.Allow {
				_, _ = c.Store.UpsertRemoteImageCache(ctx, store.RemoteImageCache{
					UserID:    req.UserID,
					URLHash:   hash,
					URL:       candidate.URL,
					Status:    store.RemoteImageStatusBlocked,
					Error:     firstNonEmpty(decision.Reason, "blocked"),
					ExpiresAt: now.Add(BlockedTTL),
				})
				continue
			}
		}
		data, contentType, err := fetch(ctx, candidate.URL)
		if err != nil {
			_, _ = c.Store.UpsertRemoteImageCache(ctx, store.RemoteImageCache{
				UserID:    req.UserID,
				URLHash:   hash,
				URL:       candidate.URL,
				Status:    store.RemoteImageStatusError,
				Error:     err.Error(),
				ExpiresAt: now.Add(ErrorTTL),
			})
			continue
		}
		saved, err := c.Blobs.SaveRemoteImage(req.UserID, hash, data)
		if err != nil {
			return err
		}
		blobRec, err := c.Store.CreateBlob(ctx, store.BlobRecord{UserID: req.UserID, Kind: "remote-image", Path: saved.Path, SHA256: saved.SHA256, Size: saved.Size})
		if err != nil {
			return err
		}
		_, _ = c.Store.UpsertRemoteImageCache(ctx, store.RemoteImageCache{
			UserID:      req.UserID,
			URLHash:     hash,
			URL:         candidate.URL,
			BlobID:      blobRec.ID,
			BlobPath:    saved.Path,
			ContentType: contentType,
			Size:        saved.Size,
			Status:      store.RemoteImageStatusOK,
			FetchedAt:   now,
			ExpiresAt:   now.Add(SuccessTTL),
		})
	}
	return nil
}

func fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	client := &http.Client{
		Timeout: FetchTimeout,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: safeDialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			if !validRemoteImageURL(req.URL.String()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/*,*/*;q=0.2")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, "", errors.New("remote image returned non-2xx")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > MaxImageBytes {
		return nil, "", errors.New("remote image is too large")
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = mediaType
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", errors.New("remote resource is not an image")
	}
	return data, contentType, nil
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var firstErr error
	dialer := &net.Dialer{Timeout: FetchTimeout}
	for _, ip := range ips {
		if privateIP(ip.IP) {
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
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
	return nil, errors.New("remote image host resolves only to private addresses")
}

func privateIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 169 && v4[1] == 254:
			// Link-local (cloud metadata) — also caught by IsLinkLocalUnicast.
			return true
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			// CGNAT 100.64.0.0/10.
			return true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			// Benchmarking 198.18.0.0/15.
			return true
		case v4[0] == 255:
			// Limited broadcast.
			return true
		case v4[0] == 0:
			// "This network" range beyond the unspecified address.
			return true
		}
	}
	return false
}

func validRemoteImageURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func normalizeCandidates(raw string) []string {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	raw = strings.Trim(raw, `"'`)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	if !validRemoteImageURL(raw) {
		return nil
	}
	return []string{normalizeURL(raw)}
}

func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func ReplaceCached(bodyHTML string, cache map[string]string) string {
	if len(cache) == 0 || strings.TrimSpace(bodyHTML) == "" {
		return bodyHTML
	}
	rewrite := func(raw string) string {
		key := Hash(raw)
		if replacement := cache[key]; replacement != "" {
			return replacement
		}
		return raw
	}
	bodyHTML = imageAttrRE.ReplaceAllStringFunc(bodyHTML, func(attr string) string {
		m := imageAttrRE.FindStringSubmatch(attr)
		if len(m) == 0 {
			return attr
		}
		name := m[1]
		value := firstNonEmpty(m[3], m[4], m[5])
		quote := `"`
		if strings.Contains(attr, "'") && !strings.Contains(attr, `"`) {
			quote = `'`
		}
		if strings.EqualFold(name, "srcset") {
			parts := strings.Split(value, ",")
			for i, part := range parts {
				fields := strings.Fields(strings.TrimSpace(part))
				if len(fields) > 0 {
					fields[0] = rewrite(fields[0])
					parts[i] = strings.Join(fields, " ")
				}
			}
			return name + "=" + quote + strings.Join(parts, ", ") + quote
		}
		return name + "=" + quote + rewrite(value) + quote
	})
	return cssURLRE.ReplaceAllStringFunc(bodyHTML, func(value string) string {
		m := cssURLRE.FindStringSubmatch(value)
		if len(m) == 0 {
			return value
		}
		raw := firstNonEmpty(m[2], m[3], m[4])
		return "url(" + rewrite(raw) + ")"
	})
}

func CachedURL(hash string) string {
	return "/remote-images/" + hash
}
