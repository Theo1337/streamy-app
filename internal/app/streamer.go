package app

import (
	"context"
	"errors"
	"log/slog"

	"streamy/internal/config"
)

// Streamer is the capture -> encode -> WebRTC pipeline.
//
// Phase 1 ships StubStreamer so the tray, the hotkey and the shutdown path can
// be built and tested without ffmpeg or a network. Phase 2 swaps in a real
// implementation behind this same interface, which is the whole point of
// defining it up front: nothing in this package has to change.
type Streamer interface {
	// Start begins sharing and returns the URL viewers should open.
	Start(ctx context.Context, req StartRequest) (viewerURL string, err error)
	// Stop tears the pipeline down. It must be safe to call when not running,
	// and it must kill any subprocess (ffmpeg) it spawned.
	Stop(ctx context.Context) error
}

// StartRequest is everything the pipeline needs to know to start.
type StartRequest struct {
	// Config is a snapshot of the current settings.
	Config config.Config
	// Target is the resolved capture target. Phase 2 fills this in from
	// internal/capture's TargetResolver; empty means "decide it yourself".
	Target string
}

// ErrNotImplemented marks a Streamer that is still a placeholder.
var ErrNotImplemented = errors.New("the capture pipeline is not implemented yet")

// StubStreamer is the phase 1 placeholder. It refuses to start with a clear
// error instead of pretending that video is flowing, so a working notification
// on the hotkey means "the hotkey worked", not "the stream worked".
type StubStreamer struct {
	logger *slog.Logger
}

// NewStubStreamer returns a placeholder pipeline.
func NewStubStreamer(logger *slog.Logger) *StubStreamer {
	return &StubStreamer{logger: logger}
}

// Start always fails with ErrNotImplemented.
func (s *StubStreamer) Start(context.Context, StartRequest) (string, error) {
	return "", ErrNotImplemented
}

// Stop does nothing, and is happy about it.
func (s *StubStreamer) Stop(context.Context) error { return nil }
