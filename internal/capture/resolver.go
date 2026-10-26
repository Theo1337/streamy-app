package capture

import (
	"context"

	"streamy/internal/config"
)

// TargetResolver answers the two questions the rest of the app asks about
// screens and windows.
//
// It is an interface rather than a set of functions so that the platform
// specific part stays behind one seam: Windows implements it today, and macOS
// (ScreenCaptureKit) and Linux (PipeWire or X11) will implement the same two
// methods later without any change to Decide or to the ffmpeg argument
// builders.
type TargetResolver interface {
	// Resolve picks the target for this config, applying the automatic rules
	// in Decide.
	Resolve(ctx context.Context, cfg config.Config) (Target, error)

	// Snapshot returns the current state of the desktop. The picker uses it to
	// draw its list, and the auto switch uses it to notice that the fullscreen
	// app has moved to another monitor.
	Snapshot(ctx context.Context) (Desktop, error)

	// Windows lists the capturable top-level windows for the on-demand picker.
	Windows(ctx context.Context) ([]Window, error)
}

// NewResolver returns the TargetResolver for the platform this binary was built
// for.
func NewResolver(logger Logger) TargetResolver {
	return newResolver(logger)
}

// Logger is the small part of log/slog this package needs. Depending on an
// interface instead of *slog.Logger keeps the package testable without a
// logger, and documents exactly which two methods are used.
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}
