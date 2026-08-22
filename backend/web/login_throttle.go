// File overview: In-memory login throttling and timing equalization.
//
// Two defenses live here:
//  1. Failed-attempt tracking per account email (and a coarse per-IP flood
//     cap) so online password guessing is rate limited. Lockouts are keyed by
//     email so one attacker's failures do not lock other accounts behind the
//     same NAT, and the per-IP cap is high enough that shared networks only
//     trip it under genuine floods.
//  2. A dummy Argon2id verification on unknown emails so the response time of
//     "Invalid email or password" does not reveal whether an account exists.

package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginFailureWindow       = 15 * time.Minute
	loginMaxFailuresPerEmail = 8
	loginEmailLockout        = 15 * time.Minute
	loginMaxFailuresPerIP    = 100
	loginIPCooldown          = 15 * time.Minute
	loginThrottleMaxKeys     = 4096
)

type loginAttemptRecord struct {
	failures    []time.Time
	lockedUntil time.Time
	ipFailures  map[string]time.Time
}

// loginThrottle tracks failed logins for one Server. It is process-local by
// design: rolltop runs as a single instance guarded by an flock, and durable
// lockouts would let an attacker permanently lock victims by writing state.
type loginThrottle struct {
	mu       sync.Mutex
	window   time.Duration
	now      func() time.Time
	byEmail  map[string]*loginAttemptRecord
	byIP     map[string][]time.Time
	dummy    func(password string)
	dummySet sync.Once
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		window:  loginFailureWindow,
		now:     time.Now,
		byEmail: map[string]*loginAttemptRecord{},
		byIP:    map[string][]time.Time{},
	}
}

// setDummyVerifier installs the constant-work callback used to equalize timing
// for unknown accounts. It is injected rather than computed at construction so
// tests can substitute a cheap function.
func (t *loginThrottle) setDummyVerifier(fn func(password string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dummy = fn
}

func normalizeLoginKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// allow reports whether a login attempt may proceed right now.
func (t *loginThrottle) allow(email, ip string) bool {
	t.pruneLocked()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if times := t.byIP[ip]; len(times) >= loginMaxFailuresPerIP {
		if len(times) > 0 && now.Sub(times[0]) < loginIPCooldown {
			return false
		}
	}
	rec := t.byEmail[email]
	if rec == nil {
		return true
	}
	return now.After(rec.lockedUntil)
}

// recordFailure registers a failed attempt for both the account and source IP.
func (t *loginThrottle) recordFailure(email, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()

	times := append(t.byIP[ip], now)
	cutoff := now.Add(-loginIPCooldown)
	for len(times) > 0 && times[0].Before(cutoff) {
		times = times[1:]
	}
	t.byIP[ip] = times

	rec := t.byEmail[email]
	if rec == nil {
		rec = &loginAttemptRecord{}
		t.byEmail[email] = rec
	}
	rec.failures = append(rec.failures, now)
	windowCutoff := now.Add(-t.window)
	recent := rec.failures[:0]
	for _, ts := range rec.failures {
		if ts.After(windowCutoff) {
			recent = append(recent, ts)
		}
	}
	rec.failures = recent
	if len(rec.failures) >= loginMaxFailuresPerEmail {
		rec.lockedUntil = now.Add(loginEmailLockout)
	}
	t.enforceBoundsLocked()
}

// recordSuccess clears failure state after a successful login so legitimate
// users are never locked out by their own earlier typos.
func (t *loginThrottle) recordSuccess(email string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byEmail, email)
}

func (t *loginThrottle) pruneLocked() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	windowCutoff := now.Add(-t.window)
	for email, rec := range t.byEmail {
		if now.After(rec.lockedUntil) && (len(rec.failures) == 0 || rec.failures[len(rec.failures)-1].Before(windowCutoff)) {
			delete(t.byEmail, email)
		}
	}
	ipCutoff := now.Add(-loginIPCooldown)
	for ip, times := range t.byIP {
		kept := times[:0]
		for _, ts := range times {
			if ts.After(ipCutoff) {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(t.byIP, ip)
		} else {
			t.byIP[ip] = kept
		}
	}
	t.enforceBoundsLocked()
}

// enforceBoundsLocked caps memory use from distributed guessing attacks.
func (t *loginThrottle) enforceBoundsLocked() {
	if len(t.byEmail) > loginThrottleMaxKeys {
		t.byEmail = map[string]*loginAttemptRecord{}
	}
	if len(t.byIP) > loginThrottleMaxKeys {
		t.byIP = map[string][]time.Time{}
	}
}
