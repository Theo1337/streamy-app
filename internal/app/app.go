// Package app wires the background process together: it owns the settings, the
// global hotkey, the tray icon and the streaming pipeline, and defines the
// order everything starts and stops in.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"streamy/internal/clipboard"
	"streamy/internal/config"
	"streamy/internal/hotkey"
	"streamy/internal/tray"
	"streamy/internal/version"
)

// State is the lifecycle of a sharing session. Naming the states (instead of
// using a bool) is what makes "ignore a second hotkey press while we are still
// starting" easy to express.
type State int

const (
	// StateIdle means nothing is being shared.
	StateIdle State = iota
	// StateStarting means a stream is being brought up.
	StateStarting
	// StateStreaming means viewers can watch.
	StateStreaming
	// StateStopping means the pipeline is being torn down.
	StateStopping
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "Starting\u2026"
	case StateStreaming:
		return "Sharing"
	case StateStopping:
		return "Stopping\u2026"
	default:
		return "Not sharing"
	}
}

// Options configures an App.
type Options struct {
	Logger     *slog.Logger
	Config     config.Config
	ConfigPath string
	// Streamer is the capture pipeline. Nil means StubStreamer (phase 1).
	Streamer Streamer
	// Headless runs without a tray icon, which is handy in tests and on CI.
	Headless bool
	// ShutdownTimeout bounds how long Stop may take. Zero means 10 seconds.
	ShutdownTimeout time.Duration
}

// App is the long lived background process.
type App struct {
	logger   *slog.Logger
	cfg      config.Config
	cfgPath  string
	streamer Streamer
	hotkeys  *hotkey.Manager
	tray     *tray.Tray

	// mu guards the state below. Everything that touches them goes through a
	// short critical section and then calls refresh() outside of it, because
	// refresh takes the same lock in Status().
	mu        sync.Mutex
	state     State
	viewerURL string

	// copyToClipboard is a field rather than a direct call so tests can
	// substitute it: a unit test must not overwrite the real clipboard.
	copyToClipboard func(string) error

	timeout  time.Duration
	quitOnce sync.Once
	quitCh   chan struct{}
}

// New builds an App. It validates the config so a typo is reported at startup
// rather than in the middle of a stream.
func New(opts Options) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	streamer := opts.Streamer
	if streamer == nil {
		streamer = NewStubStreamer(logger)
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	a := &App{
		logger:          logger,
		cfg:             opts.Config,
		cfgPath:         opts.ConfigPath,
		streamer:        streamer,
		hotkeys:         hotkey.NewManager(logger),
		state:           StateIdle,
		copyToClipboard: clipboard.WriteText,
		timeout:         timeout,
		quitCh:          make(chan struct{}),
	}

	if !opts.Headless {
		a.tray = tray.New(tray.Options{
			Logger:     logger,
			Controller: a, // App implements tray.Controller
			Title:      "Streamy " + version.Version,
			OnReady:    a.onReady,
		})
	}
	return a, nil
}

// Config returns a copy of the settings in use.
func (a *App) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// Run starts everything and blocks until the app quits. Shutdown always runs,
// even when the run loop itself failed, so no ffmpeg process or hotkey can be
// left behind.
func (a *App) Run(ctx context.Context) error {
	runErr := a.runLoop(ctx)
	return errors.Join(runErr, a.shutdown(context.WithoutCancel(ctx)))
}

func (a *App) runLoop(ctx context.Context) error {
	if a.tray != nil {
		return a.tray.Run(ctx)
	}

	// Headless: nothing owns the main thread, so block on the context directly.
	if err := a.onReady(); err != nil {
		a.logger.Error("startup failed", "error", err)
	}
	a.logger.Info("running headless, press Ctrl+C to quit")
	<-ctx.Done()
	return nil
}

// onReady runs once the tray icon is on screen (or immediately in headless
// mode). This is where the global hotkey is registered.
func (a *App) onReady() error {
	a.logger.Info("streamy started",
		"version", version.String(),
		"config", a.cfgPath,
		"pid", os.Getpid(),
	)

	go a.watchHotkeys()

	if a.cfg.Hotkey.Enabled {
		if err := a.hotkeys.Register(a.cfg.Hotkey.Toggle); err != nil {
			// Not fatal: the tray menu still works. The usual cause is another
			// application owning the same combination, so point at the config.
			a.logger.Error("global hotkey unavailable",
				"shortcut", a.cfg.Hotkey.Toggle, "error", err)
			a.notify("Hotkey unavailable",
				fmt.Sprintf("Could not use %s: %v\nStart sharing from the tray menu, or change hotkey.toggle in:\n%s",
					a.cfg.Hotkey.Toggle, err, a.cfgPath))
		}
	} else {
		a.logger.Info("global hotkey disabled in settings")
	}

	a.refresh()
	a.notify("Streamy is running", fmt.Sprintf("Press %s to start sharing.", a.cfg.Hotkey.Toggle))
	return nil
}

// watchHotkeys turns every registered shortcut press into a toggle.
func (a *App) watchHotkeys() {
	for {
		select {
		case <-a.quitCh:
			return
		case _, ok := <-a.hotkeys.Events():
			if !ok {
				return
			}
			a.ToggleSharing()
		}
	}
}

// Status implements tray.Controller.
func (a *App) Status() tray.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return tray.Status{
		Sharing:   a.state == StateStreaming,
		ViewerURL: a.viewerURL,
		Text:      a.state.String(),
	}
}

// ToggleSharing starts a stream when idle and stops it when streaming. Both the
// global hotkey and the tray menu item call this.
func (a *App) ToggleSharing() {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()

	switch state {
	case StateStreaming:
		a.stopSharing()
	case StateIdle:
		a.startSharing()
	default:
		// Starting or stopping: a second press this fast is almost certainly
		// the user being impatient, so ignore it instead of queueing a stop.
		a.logger.Debug("ignoring toggle while busy", "state", state.String())
	}
}

func (a *App) startSharing() {
	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return
	}
	a.state = StateStarting
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	url, err := a.streamer.Start(ctx, StartRequest{Config: a.cfg})
	if err != nil {
		a.mu.Lock()
		a.state, a.viewerURL = StateIdle, ""
		a.mu.Unlock()
		a.refresh()

		if errors.Is(err, ErrNotImplemented) {
			// Phase 1: the notification is itself the test. Seeing it pop up
			// proves the hotkey, the app and the tray are all wired together.
			a.logger.Info("capture pipeline not implemented yet", "phase", 2)
			a.notify("Hotkey works", "Tray, hotkey and settings are wired up. Screen capture and WebRTC arrive in phase 2.")
			return
		}
		a.logger.Error("could not start sharing", "error", err)
		a.notify("Could not start sharing", err.Error())
		return
	}

	a.mu.Lock()
	a.state, a.viewerURL = StateStreaming, url
	a.mu.Unlock()
	a.refresh()

	a.logger.Info("sharing started", "viewer", url)

	// Copying the link is a nicety: if the clipboard is locked by another
	// process we still share, we just say so.
	if copyErr := a.copyToClipboard(url); copyErr != nil {
		a.logger.Warn("could not copy viewer link", "error", copyErr)
		a.notify("Sharing started", "Viewer link:\n"+url)
		return
	}
	a.notify("Sharing started", "Viewer link copied to your clipboard:\n"+url)
}

func (a *App) stopSharing() {
	a.mu.Lock()
	if a.state != StateStreaming {
		a.mu.Unlock()
		return
	}
	a.state = StateStopping
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	if err := a.streamer.Stop(ctx); err != nil {
		a.logger.Error("problem stopping sharing", "error", err)
		a.notify("Problem stopping sharing", err.Error())
	}

	a.mu.Lock()
	a.state, a.viewerURL = StateIdle, ""
	a.mu.Unlock()
	a.refresh()

	a.logger.Info("sharing stopped")
}

// OpenSettings launches the settings window.
//
// Wails arrives in phase 6 and will be a separate executable next to this one
// (systray does not play nicely with a webview on the same thread). Until then
// this reports where the config file is.
func (a *App) OpenSettings() {
	if path := a.uiExecutable(); path != "" {
		cmd := exec.Command(path)
		if err := cmd.Start(); err != nil {
			a.logger.Error("could not launch the settings UI", "path", path, "error", err)
			a.notify("Could not open settings", err.Error())
			return
		}
		// Reap the child so it does not stay a zombie if it exits on its own.
		go func() { _ = cmd.Wait() }()
		a.logger.Info("settings UI launched", "path", path)
		return
	}

	a.logger.Info("settings window not built yet", "phase", 6, "config", a.cfgPath)
	a.notify("Settings", "The settings window arrives in phase 6.\nFor now, edit:\n"+a.cfgPath)
}

// uiExecutable returns the path of the Wails UI next to this binary, or "".
func (a *App) uiExecutable() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), "streamy-ui.exe")
	if _, err := os.Stat(candidate); err != nil {
		return ""
	}
	return candidate
}

// CopyViewerLink implements tray.Controller.
func (a *App) CopyViewerLink() {
	a.mu.Lock()
	url := a.viewerURL
	a.mu.Unlock()

	if url == "" {
		a.notify("No link yet", "Start sharing first: the viewer link appears once a stream is running.")
		return
	}
	if err := a.copyToClipboard(url); err != nil {
		a.logger.Error("could not copy viewer link", "error", err)
		a.notify("Could not copy link", err.Error())
		return
	}
	a.notify("Viewer link copied", url)
}

// Quit implements tray.Controller and is the single "please stop" entry point.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		a.logger.Info("quit requested")
		close(a.quitCh)
		if a.tray != nil {
			a.tray.Quit()
		}
	})
}

// shutdown releases everything in reverse order of acquisition. Using a fresh
// context (not the cancelled one) is deliberate: cleanup needs to be able to
// wait even though the run context is already done.
func (a *App) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var errs []error

	if err := a.hotkeys.Unregister(); err != nil {
		errs = append(errs, err)
	}

	a.mu.Lock()
	wasActive := a.state == StateStreaming || a.state == StateStarting
	a.state, a.viewerURL = StateIdle, ""
	a.mu.Unlock()

	if wasActive {
		if err := a.streamer.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop streamer: %w", err))
		}
	}

	a.logger.Info("streamy stopped")
	return errors.Join(errs...)
}

// refresh asks the tray to redraw. It is nil safe so the same code paths work in
// headless mode.
func (a *App) refresh() {
	if a.tray != nil {
		a.tray.Refresh()
	}
}

// notify shows a balloon notification, or logs it when there is no tray.
func (a *App) notify(title, message string) {
	if a.tray != nil {
		a.tray.Notify(title, message)
		return
	}
	a.logger.Info("notification", "title", title, "message", message)
}
