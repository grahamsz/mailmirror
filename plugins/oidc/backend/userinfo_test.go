package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchUserinfoParsesVerifiedFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"email":"user@example.test","name":"User","email_verified":false}`))
	}))
	defer server.Close()

	email, name, verified, err := fetchUserinfo(context.Background(), server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if email != "user@example.test" || name != "User" {
		t.Fatalf("userinfo = %q %q", email, name)
	}
	if verified == nil || *verified {
		t.Fatalf("verified = %v, want explicit false", verified)
	}
}

func TestFetchUserinfoWithoutClaimReturnsNilVerified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"email":"user@example.test"}`))
	}))
	defer server.Close()

	_, _, verified, err := fetchUserinfo(context.Background(), server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if verified != nil {
		t.Fatalf("verified = %v, want nil when claim absent", verified)
	}
}
