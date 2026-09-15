package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime settings for ipwatcher.
type Config struct {
	Collector  CollectorConfig `yaml:"collector"`
	Database   DatabaseConfig  `yaml:"database"`
	Logging    LoggingConfig   `yaml:"logging"`
	Providers  []string        `yaml:"providers"`
	Providers6 []string        `yaml:"providers_v6"`
	// IgnoreIPs lists IPv4/IPv6 addresses or CIDR prefixes that must never
	// be recorded (captive portal, misbehaving provider, known-bad ranges).
	IgnoreIPs []string     `yaml:"ignore_ips"`
	Notify    NotifyConfig `yaml:"notify"`
}

type CollectorConfig struct {
	Interval   time.Duration `yaml:"interval"`
	Timeout    time.Duration `yaml:"timeout"`
	Retries    int           `yaml:"retries"`
	RetryDelay time.Duration `yaml:"retry_delay"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type NotifyConfig struct {
	WebhookURL string        `yaml:"webhook_url"`
	Timeout    time.Duration `yaml:"timeout"`
}

// Default returns the recommended production defaults from the plan.
func Default() Config {
	return Config{
		Collector: CollectorConfig{
			Interval:   5 * time.Minute,
			Timeout:    5 * time.Second,
			Retries:    2,
			RetryDelay: 2 * time.Second,
		},
		Database: DatabaseConfig{
			Path: "./data/ipwatcher.db",
		},
		Logging: LoggingConfig{
			Level: "info",
		},
		// Prefer IPv4-only hostnames so dual-stack networks still see IPv4.
		Providers: []string{
			"https://ipv4.icanhazip.com",
			"https://api.ipify.org",
			"https://4.ident.me",
		},
		// Empty by default: IPv6 collection is opt-in.
		Providers6: nil,
		Notify: NotifyConfig{
			Timeout: 5 * time.Second,
		},
	}
}

// Load reads optional YAML from path, applies defaults, then env overrides.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("failed to read config file %s: %w", path, err)
			}
		} else {
			fileCfg, err := parseFile(raw)
			if err != nil {
				return cfg, fmt.Errorf("failed to parse config file %s: %w", path, err)
			}
			cfg = mergeFile(cfg, fileCfg)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// fileConfig uses pointers so an explicit zero (e.g. retries: 0) is preserved.
type fileConfig struct {
	Collector *struct {
		Interval   *time.Duration `yaml:"interval"`
		Timeout    *time.Duration `yaml:"timeout"`
		Retries    *int           `yaml:"retries"`
		RetryDelay *time.Duration `yaml:"retry_delay"`
	} `yaml:"collector"`
	Database *struct {
		Path *string `yaml:"path"`
	} `yaml:"database"`
	Logging *struct {
		Level *string `yaml:"level"`
	} `yaml:"logging"`
	Providers  *[]string `yaml:"providers"`
	Providers6 *[]string `yaml:"providers_v6"`
	IgnoreIPs  *[]string `yaml:"ignore_ips"`
	Notify     *struct {
		WebhookURL *string        `yaml:"webhook_url"`
		Timeout    *time.Duration `yaml:"timeout"`
	} `yaml:"notify"`
}

func parseFile(raw []byte) (fileConfig, error) {
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return fc, err
	}
	return fc, nil
}

// mergeFile overlays present (non-nil) file fields onto base defaults.
func mergeFile(base Config, fc fileConfig) Config {
	out := base
	if fc.Collector != nil {
		if fc.Collector.Interval != nil && *fc.Collector.Interval > 0 {
			out.Collector.Interval = *fc.Collector.Interval
		}
		if fc.Collector.Timeout != nil && *fc.Collector.Timeout > 0 {
			out.Collector.Timeout = *fc.Collector.Timeout
		}
		if fc.Collector.Retries != nil && *fc.Collector.Retries >= 0 {
			out.Collector.Retries = *fc.Collector.Retries
		}
		if fc.Collector.RetryDelay != nil && *fc.Collector.RetryDelay >= 0 {
			out.Collector.RetryDelay = *fc.Collector.RetryDelay
		}
	}
	if fc.Database != nil && fc.Database.Path != nil && *fc.Database.Path != "" {
		out.Database.Path = *fc.Database.Path
	}
	if fc.Logging != nil && fc.Logging.Level != nil && *fc.Logging.Level != "" {
		out.Logging.Level = *fc.Logging.Level
	}
	if fc.Providers != nil && len(*fc.Providers) > 0 {
		out.Providers = append([]string(nil), *fc.Providers...)
	}
	if fc.Providers6 != nil {
		out.Providers6 = append([]string(nil), *fc.Providers6...)
	}
	if fc.IgnoreIPs != nil {
		out.IgnoreIPs = append([]string(nil), *fc.IgnoreIPs...)
	}
	if fc.Notify != nil {
		if fc.Notify.WebhookURL != nil {
			out.Notify.WebhookURL = *fc.Notify.WebhookURL
		}
		if fc.Notify.Timeout != nil && *fc.Notify.Timeout > 0 {
			out.Notify.Timeout = *fc.Notify.Timeout
		}
	}
	return out
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("IPWATCHER_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Collector.Interval = d
		}
	}
	if v := os.Getenv("IPWATCHER_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Collector.Timeout = d
		}
	}
	if v := os.Getenv("IPWATCHER_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Collector.Retries = n
		}
	}
	if v := os.Getenv("IPWATCHER_RETRY_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			cfg.Collector.RetryDelay = d
		}
	}
	if v := os.Getenv("IPWATCHER_DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("IPWATCHER_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("IPWATCHER_PROVIDERS"); v != "" {
		cfg.Providers = splitCSV(v)
	}
	if v := os.Getenv("IPWATCHER_PROVIDERS_V6"); v != "" {
		cfg.Providers6 = splitCSV(v)
	}
	if v := os.Getenv("IPWATCHER_IGNORE_IPS"); v != "" {
		cfg.IgnoreIPs = splitCSV(v)
	}
	if v := os.Getenv("IPWATCHER_WEBHOOK_URL"); v != "" {
		cfg.Notify.WebhookURL = v
	}
	if v := os.Getenv("IPWATCHER_WEBHOOK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.Notify.Timeout = d
		}
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Validate checks invariants required at runtime.
func (c Config) Validate() error {
	if c.Collector.Interval <= 0 {
		return fmt.Errorf("collector.interval must be positive")
	}
	if c.Collector.Timeout <= 0 {
		return fmt.Errorf("collector.timeout must be positive")
	}
	if c.Collector.Retries < 0 {
		return fmt.Errorf("collector.retries must be >= 0")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database.path is required")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider is required")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug|info|warn|error")
	}
	if _, err := ParseIgnoreFilter(c.IgnoreIPs); err != nil {
		return err
	}
	if c.Notify.WebhookURL != "" {
		if !strings.HasPrefix(c.Notify.WebhookURL, "http://") && !strings.HasPrefix(c.Notify.WebhookURL, "https://") {
			return fmt.Errorf("notify.webhook_url must be http(s)")
		}
		if c.Notify.Timeout <= 0 {
			return fmt.Errorf("notify.timeout must be positive")
		}
	}
	return nil
}

// IgnoreFilter matches addresses against exact IPs and CIDR prefixes.
type IgnoreFilter struct {
	prefixes []netip.Prefix
}

// ParseIgnoreFilter validates entries as IP or CIDR (v4/v6) and builds a filter.
func ParseIgnoreFilter(entries []string) (*IgnoreFilter, error) {
	f := &IgnoreFilter{prefixes: make([]netip.Prefix, 0, len(entries))}
	for _, s := range entries {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := ParseIPOrPrefix(s)
		if err != nil {
			return nil, fmt.Errorf("ignore_ips: %w", err)
		}
		f.prefixes = append(f.prefixes, p)
	}
	return f, nil
}

// ParseIPOrPrefix accepts "1.2.3.4", "1.2.3.0/24", "2001:db8::1", "2001:db8::/32".
func ParseIPOrPrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid IP or CIDR", s)
		}
		return p.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not a valid IP or CIDR", s)
	}
	bits := 32
	if addr.Is6() {
		bits = 128
	}
	return netip.PrefixFrom(addr.Unmap(), bits), nil
}

// Contains reports whether addr falls inside any ignored prefix.
func (f *IgnoreFilter) Contains(addr netip.Addr) bool {
	if f == nil || !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	for _, p := range f.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Len returns how many prefixes are configured.
func (f *IgnoreFilter) Len() int {
	if f == nil {
		return 0
	}
	return len(f.prefixes)
}

// Prefixes returns a copy of the configured prefixes.
func (f *IgnoreFilter) Prefixes() []netip.Prefix {
	if f == nil {
		return nil
	}
	out := make([]netip.Prefix, len(f.prefixes))
	copy(out, f.prefixes)
	return out
}
