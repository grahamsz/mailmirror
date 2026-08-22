package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestLoginThrottle(now func() time.Time) *loginThrottle {
	t := newLoginThrottle()
	t.now = now
	return t
}

func TestLoginThrottleAllowsUntilFailureThreshold(t *testing.T) {
	current := time.Unix(0, 0)
	throttle := newTestLoginThrottle(func() time.Time { return current })
	for i := 0; i < loginMaxFailuresPerEmail; i++ {
		if !throttle.allow("user@example.test", "10.0.0.1") {
			t.Fatalf("attempt %d blocked before threshold", i+1)
		}
		throttle.recordFailure("user@example.test", "10.0.0.1")
	}
	if throttle.allow("user@example.test", "10.0.0.2") {
		t.Fatal("lockout not applied after repeated failures from a second IP")
	}
}

func TestLoginThrottleUnlocksAfterWindowAndSuccessClears(t *testing.T) {
	current := time.Unix(0, 0)
	now := func() time.Time { return current }
	throttle := newTestLoginThrottle(now)
	for i := 0; i < loginMaxFailuresPerEmail; i++ {
		throttle.recordFailure("user@example.test", "10.0.0.1")
	}
	if throttle.allow("user@example.test", "10.0.0.1") {
		t.Fatal("locked account still allowed")
	}
	current = current.Add(loginEmailLockout + time.Minute)
	if !throttle.allow("user@example.test", "10.0.0.1") {
		t.Fatal("account still locked after lockout window")
	}
	throttle.recordSuccess("user@example.test")
	for i := 0; i < 3; i++ {
		throttle.recordFailure("user@example.test", "10.0.0.1")
	}
	if !throttle.allow("user@example.test", "10.0.0.1") {
		t.Fatal("success did not clear earlier failures")
	}
}

func TestLoginThrottleKeysByEmailSoOtherAccountsKeepWorking(t *testing.T) {
	current := time.Unix(0, 0)
	throttle := newTestLoginThrottle(func() time.Time { return current })
	for i := 0; i < loginMaxFailuresPerEmail*2; i++ {
		throttle.recordFailure("victim@example.test", "203.0.113.9")
	}
	if throttle.allow("victim@example.test", "203.0.113.9") {
		t.Fatal("victim account should be locked")
	}
	if !throttle.allow("other@example.test", "203.0.113.9") {
		t.Fatal("unrelated account on the same IP should not be locked by email-keyed failures")
	}
}

func TestLoginThrottleIPFloodCapBlocksAllAccounts(t *testing.T) {
	current := time.Unix(0, 0)
	now := func() time.Time { return current }
	throttle := newTestLoginThrottle(now)
	ip := "198.51.100.7"
	for i := 0; i < loginMaxFailuresPerIP; i++ {
		email := "acct" + string(rune('a'+i%26)) + "@example.test"
		throttle.recordFailure(email, ip)
	}
	if throttle.allow("fresh@example.test", ip) {
		t.Fatal("IP flood cap did not block a fresh account")
	}
	if !throttle.allow("fresh@example.test", "192.0.2.5") {
		t.Fatal("IP flood cap leaked to a different IP")
	}
	current = current.Add(loginIPCooldown + time.Minute)
	if !throttle.allow("fresh@example.test", ip) {
		t.Fatal("IP flood cap did not expire")
	}
}

func TestClientIPFromRequestStripsPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "192.0.2.1:55444"
	if got := clientIPFromRequest(r); got != "192.0.2.1" {
		t.Fatalf("client IP = %q", got)
	}
}
