//go:build windows

package capture

import (
	"context"
	"fmt"
	"os"
	"sync"

	"streamy/internal/config"
)

// Resolver is the Windows implementation of TargetResolver.
type Resolver struct {
	log Logger
	// warnOnce keeps the setup warnings to one occurrence. Snapshots happen on
	// every hotkey press and every couple of seconds while streaming, and the
	// same warning repeated hundreds of times is worse than no warning.
	warnOnce sync.Once
}

func newResolver(logger Logger) TargetResolver {
	// DPI awareness has to be set before any window exists, and the soonest
	// this package can be sure of that is when the resolver is built, which
	// main does first thing.
	EnableDPIAwareness()
	return &Resolver{log: logger}
}

// Snapshot describes the desktop as it is right now.
func (r *Resolver) Snapshot(ctx context.Context) (Desktop, error) {
	if err := ctx.Err(); err != nil {
		return Desktop{}, err
	}

	list, warnings, err := monitors()
	if err != nil {
		return Desktop{}, fmt.Errorf("list monitors: %w", err)
	}
	if len(warnings) > 0 && r.log != nil {
		r.warnOnce.Do(func() {
			for _, w := range warnings {
				r.log.Warn("capture: " + w)
			}
		})
	}

	return Desktop{
		Monitors:     list,
		Foreground:   foregroundWindow(),
		MouseMonitor: monitorUnderCursor(list),
		SelfPID:      uint32(os.Getpid()),
	}, nil
}

// Resolve applies the automatic target selection rules to a fresh snapshot.
func (r *Resolver) Resolve(ctx context.Context, cfg config.Config) (Target, error) {
	d, err := r.Snapshot(ctx)
	if err != nil {
		return Target{}, err
	}
	return Decide(d, cfg)
}

// Windows lists the windows the picker can offer. It is not part of
// TargetResolver because only the picker needs it, and enumerating every
// top level window is far more work than finding the foreground one.
func (r *Resolver) Windows(ctx context.Context) ([]Window, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return topLevelWindows(), nil
}
