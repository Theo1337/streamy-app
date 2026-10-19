// Package logging builds the structured logger used everywhere in Streamy.
//
// Streamy is a background process with no console of its own, so "logging" has
// to mean two things at once: keep a readable trail in a file next to the
// config, and print to stderr so running it from a terminal during development
// shows what is going on.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Options controls how the logger is built.
type Options struct {
	// Level is one of debug, info, warn or error. Anything else means info.
	Level string
	// File is an optional path of a log file to append to. Empty disables
	// file logging (stderr only).
	File string
}

// Setup builds the application logger. The returned closer is never nil and
// must be called on shutdown so the log file is flushed.
func Setup(opts Options) (*slog.Logger, func() error, error) {
	writers := []io.Writer{os.Stderr}

	file, err := openLogFile(opts.File)
	if err != nil {
		return nil, nil, err
	}
	if file != nil {
		writers = append(writers, file)
	}

	// A TextHandler prints key=value pairs, which is easy to read in a
	// terminal; swap it for slog.NewJSONHandler if you later ship logs
	// somewhere that parses them.
	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: ParseLevel(opts.Level),
	})

	closeFn := func() error {
		if file == nil {
			return nil
		}
		return file.Close()
	}
	return slog.New(handler), closeFn, nil
}

// ParseLevel turns a config string into a slog.Level, defaulting to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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

func openLogFile(path string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	// O_APPEND keeps previous runs instead of truncating them, which is what
	// you want when debugging something that already happened.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	return f, nil
}
