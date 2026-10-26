//go:build !windows

package capture

import (
	"context"
	"errors"

	"streamy/internal/config"
)

// errUnsupported is returned on platforms whose capture support has not been
// written yet.
var errUnsupported = errors.New("screen capture is only implemented on Windows so far; macOS and Linux arrive with their own TargetResolver behind this same interface")

// Resolver is the placeholder for platforms without an implementation.
type Resolver struct{}

func newResolver(Logger) TargetResolver { return &Resolver{} }

// DisplayPaths is only implemented on Windows.
func DisplayPaths() ([]DisplayPath, error) { return nil, errUnsupported }

func (r *Resolver) Snapshot(context.Context) (Desktop, error) { return Desktop{}, errUnsupported }

func (r *Resolver) Resolve(context.Context, config.Config) (Target, error) {
	return Target{}, errUnsupported
}

func (r *Resolver) Windows(context.Context) ([]Window, error) { return nil, errUnsupported }

func Preview(context.Context, Target, int, int) ([]byte, error) { return nil, errUnsupported }
