package config

import (
	"path/filepath"
	"testing"
)

const testMasterKey = "12345678901234567890123456789012"

func TestLoadUsesRolltopDefaults(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != filepath.Join("/data", "rolltop.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
	if cfg.DataDir != "/data" {
		t.Fatalf("data dir = %q", cfg.DataDir)
	}
	wantPluginDir, err := filepath.Abs("plugins")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PluginDir != wantPluginDir {
		t.Fatalf("plugin dir = %q", cfg.PluginDir)
	}
}

func TestLoadUsesRolltopDatabasePath(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_DB_PATH", "/rolltop-data/custom.db")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath != "/rolltop-data/custom.db" {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
}

func TestLoadUsesRolltopPluginDir(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_PLUGIN_DIR", "/rolltop-plugins")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PluginDir != "/rolltop-plugins" {
		t.Fatalf("plugin dir = %q", cfg.PluginDir)
	}
}

func TestParsePublicBaseURLAcceptsOriginOnly(t *testing.T) {
	t.Setenv("ROLLTOP_PUBLIC_BASE_URL", "https://mail.example.com/")
	got, err := parsePublicBaseURL("ROLLTOP_PUBLIC_BASE_URL")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://mail.example.com" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestParsePublicBaseURLRejectsPathsAndSchemes(t *testing.T) {
	for _, value := range []string{
		"https://mail.example.com/app",
		"ftp://mail.example.com",
		"//mail.example.com",
		"https://user:pass@mail.example.com",
		"https://mail.example.com/?x=1",
	} {
		t.Setenv("ROLLTOP_PUBLIC_BASE_URL", value)
		if _, err := parsePublicBaseURL("ROLLTOP_PUBLIC_BASE_URL"); err == nil {
			t.Fatalf("parsePublicBaseURL(%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseInt64FallbackAndErrors(t *testing.T) {
	got, err := parseInt64("ROLLTOP_TEST_INT64", 42)
	if err != nil || got != 42 {
		t.Fatalf("parseInt64 fallback = %d, %v", got, err)
	}
	t.Setenv("ROLLTOP_TEST_INT64", "1048576")
	got, err = parseInt64("ROLLTOP_TEST_INT64", 42)
	if err != nil || got != 1048576 {
		t.Fatalf("parseInt64 value = %d, %v", got, err)
	}
	t.Setenv("ROLLTOP_TEST_INT64", "huge")
	if _, err := parseInt64("ROLLTOP_TEST_INT64", 42); err == nil {
		t.Fatal("parseInt64 accepted a non-numeric value")
	}
}

func TestLoadRejectsTinyMaxMessageBytes(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", "12345678901234567890123456789012")
	t.Setenv("ROLLTOP_MAX_MESSAGE_BYTES", "100")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a max message size below the floor")
	}
	t.Setenv("ROLLTOP_MAX_MESSAGE_BYTES", "2097152")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxMessageBytes != 2097152 {
		t.Fatalf("max message bytes = %d", cfg.MaxMessageBytes)
	}
	if cfg.PublicBaseURL != "" {
		t.Fatalf("default public base URL = %q", cfg.PublicBaseURL)
	}
}
