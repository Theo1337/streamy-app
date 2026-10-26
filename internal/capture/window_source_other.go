//go:build !windows

package capture

import (
	"context"
	"errors"
)

// WindowCaptureSupported is false here: the PrintWindow frame source is a
// Windows API. Other platforms fall back to the ordinary desktop grabber.
func WindowCaptureSupported() bool { return false }

// WindowFrameSource is only implementable on Windows.
type WindowFrameSource struct{}

func NewWindowFrameSource(context.Context, Window, string, int, bool) (*WindowFrameSource, error) {
	return nil, errors.New("per-window capture is only available on Windows")
}

func (s *WindowFrameSource) InputArgs() []string { return nil }

func (s *WindowFrameSource) Close() error { return nil }
