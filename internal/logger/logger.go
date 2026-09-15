package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// New builds a slog.Logger writing text to stderr at the given level.
func New(level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler), nil
}
