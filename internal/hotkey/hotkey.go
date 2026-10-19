// Package hotkey registers system wide keyboard shortcuts.
//
// It wraps golang.design/x/hotkey, which on Windows is built on RegisterHotKey
// and therefore needs no administrator rights and no keyboard hook of our own.
// The chosen shortcut is delivered to the process as a message, which keeps the
// idle cost at zero: no polling, no CPU while nobody presses anything.
package hotkey

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"golang.design/x/hotkey"
)

// Manager owns at most one registered global shortcut at a time. Keeping it a
// single object (instead of a package level variable) means tests can create
// their own and shutdown can be explicit.
type Manager struct {
	logger *slog.Logger

	mu       sync.Mutex
	hk       *hotkey.Hotkey
	spec     string
	stopPump chan struct{}

	// events is buffered with room for one press: if the user hammers the
	// shortcut faster than we can react, coalescing the presses into one toggle
	// is friendlier than queueing a burst of start/stop cycles.
	events chan struct{}
}

// NewManager creates a Manager that has nothing registered yet.
func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger, events: make(chan struct{}, 1)}
}

// Events returns the channel that receives one value per shortcut press. It is
// never closed, so callers should also select on their own shutdown channel.
func (m *Manager) Events() <-chan struct{} { return m.events }

// Spec returns the canonical form of the registered shortcut, or "".
func (m *Manager) Spec() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spec
}

// Register parses shortcut and registers it. It returns a descriptive error
// when the shortcut is malformed or when another application already owns it.
func (m *Manager) Register(shortcut string) error {
	spec, err := Parse(shortcut)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.hk != nil {
		registered := m.spec
		m.mu.Unlock()
		return fmt.Errorf("hotkey %s is already registered", registered)
	}
	m.mu.Unlock()

	hk := hotkey.New(spec.Mods, spec.Key)
	if err := hk.Register(); err != nil {
		// On Windows this is almost always "another program already uses this
		// combination", which is why the caller should suggest editing the
		// config rather than giving up.
		return fmt.Errorf("register global hotkey %s: %w", spec, err)
	}

	stop := make(chan struct{})
	m.mu.Lock()
	m.hk, m.spec, m.stopPump = hk, spec.String(), stop
	m.mu.Unlock()

	go m.pump(hk, stop)
	m.logger.Info("global hotkey registered", "shortcut", spec.String())
	return nil
}

// Unregister releases the shortcut. It is safe to call when nothing is
// registered, and safe to call twice.
func (m *Manager) Unregister() error {
	m.mu.Lock()
	hk, stop := m.hk, m.stopPump
	m.hk, m.stopPump, m.spec = nil, nil, ""
	m.mu.Unlock()

	if hk == nil {
		return nil
	}
	close(stop)
	if err := hk.Unregister(); err != nil {
		return fmt.Errorf("unregister hotkey: %w", err)
	}
	m.logger.Info("global hotkey unregistered")
	return nil
}

// pump forwards shortcut presses to Events.
//
// We act on the key *release* rather than the key press. Windows repeats
// WM_HOTKEY roughly every 30 ms while the keys stay down, so triggering on
// press would toggle sharing on and off many times for one long keypress.
// The release arrives exactly once.
func (m *Manager) pump(hk *hotkey.Hotkey, stop <-chan struct{}) {
	keyup := hk.Keyup()
	keydown := hk.Keydown()

	for {
		select {
		case <-stop:
			return
		case <-keydown:
			// Nothing to do: swallow the press, wait for the release.
		case <-keyup:
			select {
			case m.events <- struct{}{}:
			default: // a toggle is already waiting to be handled
			}
		}
	}
}

// ErrNotSupported is returned by future platform implementations that cannot
// register global shortcuts.
var ErrNotSupported = errors.New("global hotkeys are not supported on this platform")
