// Package tray draws the notification area icon and its menu.
//
// Design note: the tray does not know anything about streaming. It renders a
// Status snapshot and calls back into a Controller interface, which the app
// package implements. In Go it is idiomatic for the *consumer* to declare the
// interface it needs (rather than the provider declaring "here is my API"),
// because that keeps the dependency arrow pointing one way: app -> tray, never
// tray -> app.
package tray

import (
	"context"
	"log/slog"
	"sync"

	"fyne.io/systray"

	"streamy/internal/notify"
)

// Status is the snapshot the menu is rendered from.
type Status struct {
	// Sharing is true while a stream is live.
	Sharing bool
	// ViewerURL is the link shown to viewers, empty when nothing is running.
	ViewerURL string
	// Text is the free form line shown at the top of the menu and in the
	// tooltip, e.g. "Starting…" or "Not sharing".
	Text string
}

// Controller is what the tray needs from the application.
type Controller interface {
	Status() Status
	ToggleSharing()
	OpenSettings()
	CopyViewerLink()
	Quit()
}

// Options configures a Tray.
type Options struct {
	Logger     *slog.Logger
	Controller Controller
	Title      string
	// OnReady runs once the icon is on screen. Errors are logged and reported
	// to the user as a notification, but do not stop the tray.
	OnReady func() error
}

// Tray is the notification area icon.
type Tray struct {
	logger *slog.Logger
	ctrl   Controller
	title  string
	opts   Options

	mStatus    *systray.MenuItem
	mStartStop *systray.MenuItem
	mSettings  *systray.MenuItem
	mCopyLink  *systray.MenuItem
	mQuit      *systray.MenuItem

	// ready guards the menu item pointers: menu items only exist after
	// systray.Run has called onReady, and Refresh may be called before that.
	mu    sync.RWMutex
	ready bool
}

// New creates a Tray. Call Run to show it.
func New(opts Options) *Tray {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	title := opts.Title
	if title == "" {
		title = "Streamy"
	}
	return &Tray{logger: logger, ctrl: opts.Controller, title: title, opts: opts}
}

// Run shows the tray icon and blocks until the user picks Quit or ctx is
// cancelled.
//
// It must be called from the program's main goroutine. On Windows the tray owns
// a window and pumps its message loop; on macOS AppKit requires the main thread
// for anything that touches the UI. Keeping the blocking call on main also
// means the process has a natural "anchor" goroutine that the rest of the app
// hangs off, which makes shutdown ordering obvious.
func (t *Tray) Run(ctx context.Context) error {
	// A second goroutine watches for cancellation and tells the tray to close.
	// systray.Quit is safe to call from any goroutine.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			t.logger.Debug("context cancelled, closing tray")
			systray.Quit()
		case <-stopped:
		}
	}()

	systray.Run(t.onReady, t.onExit)
	close(stopped)
	return nil
}

// Quit asks the tray to close and Run to return.
func (t *Tray) Quit() { systray.Quit() }

// Notify shows a balloon notification next to the tray icon.
func (t *Tray) Notify(title, message string) {
	notify.Show(t.logger, title, message)
}

func (t *Tray) onReady() {
	systray.SetTitle(t.title)
	systray.SetTooltip(t.title)
	systray.SetIcon(IconIdle())

	// A disabled item is a tidy way to show plain text in a menu.
	t.mStatus = systray.AddMenuItem(t.title, "")
	t.mStatus.Disable()
	t.mStartStop = systray.AddMenuItem("Start sharing", "Start or stop sharing")
	t.mSettings = systray.AddMenuItem("Open settings\u2026", "Hotkey, encoder, servers")
	t.mCopyLink = systray.AddMenuItem("Copy viewer link", "Copy the link your viewers should open")
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("Quit", "Stop sharing and exit")

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()

	go t.clickLoop()
	t.Refresh()

	if t.opts.OnReady != nil {
		if err := t.opts.OnReady(); err != nil {
			t.logger.Error("startup failed", "error", err)
			t.Notify(t.title, "Startup problem: "+err.Error())
		}
	}
}

func (t *Tray) onExit() {
	t.logger.Debug("tray closed")
}

// clickLoop is one goroutine for all menu items: selecting on several channels
// is cheaper and easier to reason about than a goroutine per item.
func (t *Tray) clickLoop() {
	for {
		select {
		case <-t.mStartStop.ClickedCh:
			t.ctrl.ToggleSharing()
		case <-t.mSettings.ClickedCh:
			t.ctrl.OpenSettings()
		case <-t.mCopyLink.ClickedCh:
			t.ctrl.CopyViewerLink()
		case <-t.mQuit.ClickedCh:
			// Quit the app, not just the tray: the app stops the stream and
			// then closes the tray, so no resource is left behind.
			t.ctrl.Quit()
			return
		}
		t.Refresh()
	}
}

// Refresh re-reads the controller status and updates the icon, the menu labels
// and the tooltip. It is safe to call from any goroutine and before Run.
func (t *Tray) Refresh() {
	t.mu.RLock()
	ready := t.ready
	t.mu.RUnlock()
	if !ready {
		return // nothing to update yet; onReady will render the first state
	}

	status := t.ctrl.Status()

	if status.Sharing {
		systray.SetIcon(IconLive())
		t.mStartStop.SetTitle("Stop sharing")
	} else {
		systray.SetIcon(IconIdle())
		t.mStartStop.SetTitle("Start sharing")
	}

	text := status.Text
	if text == "" {
		if status.Sharing {
			text = "Sharing"
		} else {
			text = "Not sharing"
		}
	}
	t.mStatus.SetTitle(text)

	tooltip := t.title + " \u2014 " + text
	if status.ViewerURL != "" {
		tooltip += "\n" + status.ViewerURL
	}
	systray.SetTooltip(tooltip)
}
