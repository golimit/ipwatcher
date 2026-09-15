package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.Collector.Interval != 5*time.Minute {
		t.Fatalf("interval = %v, want 5m", cfg.Collector.Interval)
	}
	if cfg.Collector.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", cfg.Collector.Timeout)
	}
	if cfg.Collector.Retries != 2 {
		t.Fatalf("retries = %d, want 2", cfg.Collector.Retries)
	}
	if len(cfg.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(cfg.Providers))
	}
	if cfg.Database.Path != "./data/ipwatcher.db" {
		t.Fatalf("db path = %q", cfg.Database.Path)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.Interval != 5*time.Minute {
		t.Fatalf("interval = %v", cfg.Collector.Interval)
	}
}

func TestLoadYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
collector:
  interval: 10m
database:
  path: /tmp/custom.db
logging:
  level: debug
providers:
  - https://example.com/ip
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.Interval != 10*time.Minute {
		t.Fatalf("interval = %v, want 10m", cfg.Collector.Interval)
	}
	if cfg.Database.Path != "/tmp/custom.db" {
		t.Fatalf("path = %q", cfg.Database.Path)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("level = %q", cfg.Logging.Level)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0] != "https://example.com/ip" {
		t.Fatalf("providers = %v", cfg.Providers)
	}
	// Unset fields keep defaults.
	if cfg.Collector.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v", cfg.Collector.Timeout)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("IPWATCHER_INTERVAL", "1m")
	t.Setenv("IPWATCHER_DB_PATH", "./env.db")
	t.Setenv("IPWATCHER_LOG_LEVEL", "warn")
	t.Setenv("IPWATCHER_PROVIDERS", "https://a.example,https://b.example")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.Interval != time.Minute {
		t.Fatalf("interval = %v", cfg.Collector.Interval)
	}
	if cfg.Database.Path != "./env.db" {
		t.Fatalf("path = %q", cfg.Database.Path)
	}
	if cfg.Logging.Level != "warn" {
		t.Fatalf("level = %q", cfg.Logging.Level)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %v", cfg.Providers)
	}
}

func TestValidateRejectsBadLevel(t *testing.T) {
	cfg := Default()
	cfg.Logging.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for bad log level")
	}
}
