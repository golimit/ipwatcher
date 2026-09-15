package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime settings for ipwatcher.
type Config struct {
	Collector CollectorConfig `yaml:"collector"`
	Database  DatabaseConfig  `yaml:"database"`
	Logging   LoggingConfig   `yaml:"logging"`
	Providers []string        `yaml:"providers"`
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
	Providers *[]string `yaml:"providers"`
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
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
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
	return nil
}
