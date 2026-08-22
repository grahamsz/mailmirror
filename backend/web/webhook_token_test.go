package web

import (
	"net/http/httptest"
	"testing"
)

func TestValidWebhookTokenAcceptsHeadersOnly(t *testing.T) {
	server := &Server{webhookToken: "secret-token"}

	r := httptest.NewRequest("POST", "/webhooks/sync", nil)
	r.Header.Set("X-Rolltop-Webhook-Token", "secret-token")
	if !server.validWebhookToken(r) {
		t.Fatal("header token rejected")
	}

	r = httptest.NewRequest("POST", "/webhooks/sync", nil)
	r.Header.Set("Authorization", "Bearer secret-token")
	if !server.validWebhookToken(r) {
		t.Fatal("bearer token rejected")
	}

	r = httptest.NewRequest("POST", "/webhooks/sync?token=secret-token", nil)
	if server.validWebhookToken(r) {
		t.Fatal("query-string token accepted; tokens in URLs leak into proxy logs")
	}

	r = httptest.NewRequest("POST", "/webhooks/sync", nil)
	if server.validWebhookToken(r) {
		t.Fatal("missing token accepted")
	}
}
