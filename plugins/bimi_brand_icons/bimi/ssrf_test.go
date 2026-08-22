package bimi

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestPublicIPBlocksAdditionalSensitiveRanges(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.0.0.5":        false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"100.127.0.1":     false,
		"198.18.1.1":      false,
		"255.255.255.255": false,
		"0.0.0.0":         false,
		"0.2.3.4":         false,
		"224.0.0.5":       false,
	}
	for ip, want := range cases {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %t, want %t", ip, got, want)
		}
	}
}

func TestSafeDialContextRefusesPrivateOnlyHosts(t *testing.T) {
	_, err := safeDialContext(context.Background(), net.DefaultResolver, "tcp", "127.0.0.1:443")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("safeDialContext loopback error = %v", err)
	}
	_, err = safeDialContext(context.Background(), net.DefaultResolver, "tcp", "10.20.30.40:443")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("safeDialContext private error = %v", err)
	}
}
