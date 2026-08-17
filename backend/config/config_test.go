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

func TestLoadValidatesLogLevel(t *testing.T) {
	t.Setenv("ROLLTOP_MASTER_KEY", testMasterKey)
	t.Setenv("ROLLTOP_LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("default log level = %q", cfg.LogLevel)
	}

	t.Setenv("ROLLTOP_LOG_LEVEL", "Debug")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("log level = %q", cfg.LogLevel)
	}

	t.Setenv("ROLLTOP_LOG_LEVEL", "verbose")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown log level")
	}
}
