package config

import (
	"net/netip"
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
	if len(cfg.Providers6) != 0 {
		t.Fatalf("providers_v6 should default empty, got %v", cfg.Providers6)
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
		t.Fatalf("interval = %v, want 5m", cfg.Collector.Interval)
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
providers_v6:
  - https://ipv6.example.com/ip
ignore_ips:
  - 203.0.113.66
  - 203.0.113.0/24
notify:
  webhook_url: https://hooks.example/x
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
	if len(cfg.Providers6) != 1 {
		t.Fatalf("providers_v6 = %v", cfg.Providers6)
	}
	if len(cfg.IgnoreIPs) != 2 {
		t.Fatalf("ignore_ips = %v", cfg.IgnoreIPs)
	}
	if cfg.Notify.WebhookURL != "https://hooks.example/x" {
		t.Fatalf("webhook = %q", cfg.Notify.WebhookURL)
	}
	if cfg.Collector.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s", cfg.Collector.Timeout)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("IPWATCHER_INTERVAL", "1m")
	t.Setenv("IPWATCHER_DB_PATH", "./env.db")
	t.Setenv("IPWATCHER_LOG_LEVEL", "warn")
	t.Setenv("IPWATCHER_PROVIDERS", "https://a.example,https://b.example")
	t.Setenv("IPWATCHER_PROVIDERS_V6", "https://v6.example")
	t.Setenv("IPWATCHER_IGNORE_IPS", "10.0.0.0/8,198.51.100.1")
	t.Setenv("IPWATCHER_WEBHOOK_URL", "https://hook.example/y")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.Interval != time.Minute {
		t.Fatalf("interval = %v, want 1m", cfg.Collector.Interval)
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
	if len(cfg.Providers6) != 1 {
		t.Fatalf("providers_v6 = %v", cfg.Providers6)
	}
	if len(cfg.IgnoreIPs) != 2 {
		t.Fatalf("ignore_ips = %v", cfg.IgnoreIPs)
	}
	if cfg.Notify.WebhookURL != "https://hook.example/y" {
		t.Fatalf("webhook = %q", cfg.Notify.WebhookURL)
	}
}

func TestLoadYAMLOverrideRetriesZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
collector:
  retries: 0
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Collector.Retries != 0 {
		t.Fatalf("retries = %d, want 0 (explicit zero must be preserved)", cfg.Collector.Retries)
	}
}

func TestValidateRejectsBadLevel(t *testing.T) {
	cfg := Default()
	cfg.Logging.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for bad log level")
	}
}

func TestValidateRejectsBadIgnoreIP(t *testing.T) {
	cfg := Default()
	cfg.IgnoreIPs = []string{"not-an-ip"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid ignore IP")
	}
	cfg.IgnoreIPs = []string{"10.0.0.0/99"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid prefix length")
	}
}

func TestValidateRejectsBadWebhook(t *testing.T) {
	cfg := Default()
	cfg.Notify.WebhookURL = "ftp://x"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for non-http webhook")
	}
}

func TestParseIgnoreFilterCIDR(t *testing.T) {
	f, err := ParseIgnoreFilter([]string{
		"203.0.113.66",
		"203.0.113.0/24",
		"2001:db8::1",
		"2001:db8::/32",
	})
	if err != nil {
		t.Fatalf("ParseIgnoreFilter: %v", err)
	}
	if f.Len() != 4 {
		t.Fatalf("len = %d, want 4", f.Len())
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"203.0.113.66", true},
		{"203.0.113.99", true},
		{"154.4.0.1", false},
		{"198.51.100.10", false},
		{"2001:db8::1", true},
		{"2001:db8:1::1", true},
		{"2001:db9::1", false},
	}
	for _, tc := range cases {
		addr := netip.MustParseAddr(tc.ip)
		if got := f.Contains(addr); got != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
