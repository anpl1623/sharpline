// Package logging builds the structured JSON loggers every Sharpline binary
// uses (CLAUDE.md §9: "Structured JSON logging via log/slog").
//
// There is no package-level logger and no global mutable state (CLAUDE.md §12).
// Callers construct a logger and inject it; the platform packages accept one.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ErrUnknownLevel is returned by ParseLevel for a value outside the accepted
// set. Callers match it with errors.Is.
var ErrUnknownLevel = errors.New("logging: unknown log level")

// Levels lists every accepted SHARPLINE_LOG_LEVEL value, in ascending
// verbosity-of-severity order. Exported so config validation can quote the
// accepted set back at the operator instead of failing silently.
func Levels() []string {
	return []string{"debug", "info", "warn", "error"}
}

// ParseLevel maps a SHARPLINE_LOG_LEVEL value onto a slog.Level. Matching is
// case-insensitive and surrounding whitespace is ignored, because environment
// variables arrive from YAML and .env files where both are common.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%w: %q (want one of %s)", ErrUnknownLevel, s, strings.Join(Levels(), ", "))
	}
}

// New returns a JSON slog.Logger writing to w, stamped with the service and
// deployment environment so log lines are attributable once every binary is
// shipping to one collector.
//
// env may be empty (nothing is stamped for it) so that a caller which has not
// yet loaded its configuration can still get a usable logger.
func New(w io.Writer, level slog.Level, service, env string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})

	attrs := make([]slog.Attr, 0, 2)
	if service != "" {
		attrs = append(attrs, slog.String("service", service))
	}
	if env != "" {
		attrs = append(attrs, slog.String("env", env))
	}
	if len(attrs) == 0 {
		return slog.New(handler)
	}
	return slog.New(handler.WithAttrs(attrs))
}

// Bootstrap returns the logger a binary uses before its configuration has been
// parsed — for reporting the configuration failure itself. It writes at info
// level because SHARPLINE_LOG_LEVEL is, by definition, not yet trustworthy.
func Bootstrap(w io.Writer, service string) *slog.Logger {
	return New(w, slog.LevelInfo, service, "")
}
