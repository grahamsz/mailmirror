package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordResetLinkUsesCanonicalBaseURLWhenConfigured(t *testing.T) {
	server := &Server{publicBaseURL: "https://mail.example.com"}
	r := httptest.NewRequest(http.MethodPost, "/api/password-reset/request", nil)
	r.Host = "evil.example.net"
	link := server.passwordResetLink(r, "tok-123")
	if !strings.HasPrefix(link, "https://mail.example.com/reset-password?token=tok-123") {
		t.Fatalf("reset link = %q, want canonical host", link)
	}
	if strings.Contains(link, "evil.example.net") {
		t.Fatalf("reset link leaked spoofed Host: %q", link)
	}
}

func TestPasswordResetLinkFallsBackToRequestHostWithoutCanonicalBase(t *testing.T) {
	server := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/password-reset/request", nil)
	r.Host = "mail.local.test:8080"
	link := server.passwordResetLink(r, "tok-123")
	if link != "http://mail.local.test:8080/reset-password?token=tok-123" {
		t.Fatalf("fallback reset link = %q", link)
	}
}
