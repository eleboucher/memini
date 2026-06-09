// Package logging builds the application's slog logger from config.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger for the given level (debug|info|warn|error) and
// format ("json" default, or "text"). Logs go to stderr so they don't collide
// with the MCP protocol stream on stdout in stdio mode.
func New(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
