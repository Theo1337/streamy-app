// Package app wires the background process together: it owns the settings, the
// global hotkey, the tray icon and the streaming pipeline, and defines the
// order everything starts and stops in.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"streamy/internal/capture"
	"streamy/internal/clipboard"
	"streamy/internal/config"
	"streamy/internal/executil"
	"streamy/internal/hotkey"
	"streamy/internal/output"
	"streamy/internal/pipeline"
	"streamy/internal/sound"
	"streamy/internal/stack"
	"streamy/internal/tray"
	"streamy/internal/update"
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
	// Streamer is the capture pipeline. Nil means the stub, which refuses to
	// start so the tray, the hotkey and the shutdown path can be exercised
	// without ffmpeg.
	Streamer pipeline.Streamer
	// Headless runs without a tray icon, which is handy in tests and on CI.
	Headless bool
	// ShutdownTimeout bounds how long Stop may take. Zero means 10 seconds.
	ShutdownTimeout time.Duration
}

// App is the long lived background process.
type App struct {
	logger        *slog.Logger
	cfg           config.Config
	cfgPath       string
	streamer      pipeline.Streamer
	hotkeys       *hotkey.Manager
	pickerHotkeys *hotkey.Manager
	tray          *tray.Tray
	stack         *stack.Manager
	updateChecker *update.Checker

	// mu guards the state below. Everything that touches them goes through a
	// short critical section and then calls refresh() outside of it, because
	// refresh takes the same lock in Status().
	mu                  sync.Mutex
	state               State
	session             pipeline.Session
	ipc                 *ipcServer
	configMu            sync.Mutex
	uiProcesses         map[*exec.Cmd]chan struct{}
	activeTarget        *capture.Target
	lastInteraction     time.Time
	shuttingDown        bool
	identityToken       string
	identityProfile     discordProfile
	identityError       string
	pendingDiscordState string

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
		streamer = pipeline.NewStub(logger)
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	// Initialize update checker if enabled
	var updateChecker *update.Checker
	if opts.Config.Update.Enabled && strings.TrimSpace(opts.Config.Update.Repo) != "" {
		updateChecker = update.NewChecker(opts.Config.Update.Repo)
	}

	a := &App{
		logger:          logger,
		cfg:             opts.Config,
		cfgPath:         opts.ConfigPath,
		streamer:        streamer,
		hotkeys:         hotkey.NewManager(logger),
		pickerHotkeys:   hotkey.NewManager(logger),
		stack:           stack.New(logger, opts.Config.Stream.Stack, opts.Config.Stream.Discord.NotifyURL, opts.Config.Stream.Discord.NotifyKey),
		updateChecker:   updateChecker,
		state:           StateIdle,
		copyToClipboard: clipboard.WriteText,
		timeout:         timeout,
		quitCh:          make(chan struct{}),
		uiProcesses:     make(map[*exec.Cmd]chan struct{}),
		lastInteraction: time.Now(),
	}

	if !opts.Headless {
		a.tray = tray.New(tray.Options{
			Logger:     logger,
			Controller: a, // App implements tray.Controller
			Title:      "Streamy " + version.Version,
			OnReady:    a.onReady,
		})
	}
	token, identityErr := loadDiscordIdentity()
	if identityErr != nil {
		a.identityError = identityErr.Error()
	} else if token != "" {
		profile, err := parseDiscordIdentity(token)
		if err != nil {
			a.identityError = "Saved Discord sign-in is invalid or expired. Connect Discord again."
		} else {
			a.identityToken = token
			a.identityProfile = profile
		}
	}
	return a, nil
}

// Config returns a copy of the settings in use.
func (a *App) Config() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg := a.cfg
	cfg.Stream.STUNServers = append([]string(nil), cfg.Stream.STUNServers...)
	cfg.Stream.TURNServers = append([]config.TURNServer(nil), cfg.Stream.TURNServers...)
	for i := range cfg.Stream.TURNServers {
		cfg.Stream.TURNServers[i].URLs = append([]string(nil), cfg.Stream.TURNServers[i].URLs...)
	}
	cfg.Encoding.ExtraEncoderArgs = append([]string(nil), cfg.Encoding.ExtraEncoderArgs...)
	return cfg
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
	if err := a.startIPC(); err != nil {
		a.logger.Error("settings IPC is unavailable", "error", err)
	} else {
		a.logger.Info("settings IPC ready")
	}
	if a.cfg.RunAtStartup {
		if err := setRunAtStartup(true, a.cfgPath); err != nil {
			a.logger.Error("could not update Windows startup setting", "error", err)
			a.notify("Startup setting unavailable", err.Error())
		}
	}

	go a.watchHotkeys()
	go a.watchBotIdle()

	// The local bot is only useful when it can post the public viewer link.
	if a.shouldStartBot(a.cfg) {
		a.stack.Start()
	}

	// Start update checker if enabled
	if a.updateChecker != nil {
		go a.checkForUpdates()
	}

	if err := a.registerHotkeys(a.cfg.Hotkey); err != nil {
		a.logger.Error("global hotkeys unavailable", "error", err)
		a.notify("Hotkey unavailable", fmt.Sprintf("%v\nChange the shortcuts in:\n%s", err, a.cfgPath))
	} else if !a.cfg.Hotkey.Enabled {
		a.logger.Info("global hotkeys disabled in settings")
	}

	a.refresh()
	a.notify("Streamy is running", fmt.Sprintf("Press %s to start capturing.", a.cfg.Hotkey.Toggle))
	if a.tray != nil && !a.cfg.SetupComplete {
		a.OpenSettings()
	}
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
		case _, ok := <-a.pickerHotkeys.Events():
			if !ok {
				return
			}
			a.OpenPicker()
		}
	}
}

// touchInteraction postpones the ten-minute idle shutdown when the user
// explicitly opens a window, picks a target or saves settings. Background status
// polling deliberately does not call it, so a forgotten open window still lets
// the bot go quiet.
func (a *App) touchInteraction() {
	a.mu.Lock()
	a.lastInteraction = time.Now()
	a.mu.Unlock()
}

// watchBotIdle stops the local bot after ten quiet minutes. Starting a stream
// or opening a window resets the timer, and the next stream starts it again
// lazily. This keeps a tray app that may sit unused all day from pinning a bot
// process in the background.
func (a *App) watchBotIdle() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-a.quitCh:
			return
		case <-ticker.C:
		}
		a.mu.Lock()
		idle := a.state == StateIdle && a.stack.Running() && time.Since(a.lastInteraction) >= 10*time.Minute
		a.mu.Unlock()
		if !idle {
			continue
		}
		a.logger.Info("stopping the local Discord bot after ten idle minutes")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := a.stack.Stop(ctx); err != nil {
			a.logger.Warn("could not stop the idle local bot", "error", err)
		}
		cancel()
	}
}

// checkForUpdates checks for updates on startup and then periodically.
func (a *App) checkForUpdates() {
	// Initial check after a short delay to not slow down startup
	select {
	case <-a.quitCh:
		return
	case <-time.After(5 * time.Second):
	}

	a.performUpdateCheck()

	// Then check periodically based on config interval
	intervalDays := a.cfg.Update.CheckIntervalDays
	if intervalDays <= 0 {
		intervalDays = 7
	}
	ticker := time.NewTicker(time.Duration(intervalDays) * 24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-a.quitCh:
			return
		case <-ticker.C:
			a.performUpdateCheck()
		}
	}
}

// performUpdateCheck runs a single update check and notifies if update available.
func (a *App) performUpdateCheck() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := a.updateChecker.Check(ctx)
	if err != nil {
		a.logger.Debug("update check failed", "error", err)
		return
	}

	if !result.UpdateAvailable {
		a.logger.Debug("no update available", "current", result.CurrentVersion, "latest", result.LatestVersion)
		return
	}

	a.logger.Info("update available", "current", result.CurrentVersion, "latest", result.LatestVersion)

	// Notify user about update
	message := fmt.Sprintf("Version %s is available (you have %s).", result.LatestVersion, result.CurrentVersion)
	if result.DownloadURL != "" && result.DownloadURL != result.ReleaseURL {
		message += "\nClick to download the installer."
	} else {
		message += "\nClick to view the release page."
	}

	a.notify("Streamy update available", message)

	// Store the download URL for the notification click handler
	// We'll need to add a way to handle notification clicks
	_ = result.DownloadURL
}

// Status implements tray.Controller.
func (a *App) Status() tray.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	var target *tray.Target
	if a.activeTarget != nil {
		target = &tray.Target{
			Kind:   a.activeTarget.Kind.String(),
			Index:  a.activeTarget.Monitor.Index,
			Handle: fmt.Sprintf("%d", a.activeTarget.Window.Handle),
		}
		if a.activeTarget.Kind == capture.KindWindow {
			target.Index = 0
		}
	}
	return tray.Status{
		Sharing:   a.state == StateStreaming,
		ViewerURL: a.session.ViewerURL,
		Text:      a.statusTextLocked(),
		Active:    a.session.Label,
		Target:    target,
	}
}

// statusTextLocked returns the line shown at the top of the tray menu. It
// prefers the pipeline's own word for what it is doing ("Recording", later
// "Sharing") over the bare state name. The caller must hold a.mu.
func (a *App) statusTextLocked() string {
	if a.state == StateStreaming && a.session.Label != "" {
		return a.session.Label
	}
	return a.state.String()
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
		a.startSharing(nil)
	default:
		// Starting or stopping: a second press this fast is almost certainly
		// the user being impatient, so ignore it instead of queueing a stop.
		a.logger.Debug("ignoring toggle while busy", "state", state.String())
	}
}

func (a *App) startSharing(target *capture.Target) {
	a.mu.Lock()
	if a.state != StateIdle {
		a.mu.Unlock()
		return
	}
	if target == nil && a.cfg.Capture.AlwaysShowPicker {
		a.mu.Unlock()
		a.OpenPicker()
		return
	}
	a.state = StateStarting
	cfg := a.cfg
	identityToken := a.identityToken
	identityName := a.identityProfile.Name
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Make sure the local bot is on its way. Waiting for it would block the
	// hotkey; the notification is best effort and the bot starts in parallel.
	if a.shouldStartBot(cfg) {
		a.stack.Start()
	}

	session, err := a.streamer.Start(ctx, pipeline.Request{
		Config: cfg, Target: target, IdentityToken: identityToken, UserName: identityName,
	})
	if err != nil {
		a.mu.Lock()
		a.state, a.session = StateIdle, pipeline.Session{}
		a.activeTarget = nil
		a.mu.Unlock()
		a.refresh()

		if errors.Is(err, pipeline.ErrNotImplemented) {
			// No pipeline is configured: the notification is itself the test.
			// Seeing it pop up proves the hotkey, the app and the tray are all
			// wired together.
			a.logger.Info("no capture pipeline configured", "phase", 2)
			a.notify("Hotkey works", "Tray, hotkey and settings are wired up, but no pipeline is configured.")
			return
		}
		a.logger.Error("could not start the capture", "error", err)
		a.notify("Could not start capturing", err.Error())
		return
	}

	a.mu.Lock()
	a.state, a.session = StateStreaming, session
	a.activeTarget = session.Target
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()

	a.logger.Info("session started",
		"label", session.Label,
		"description", session.Description,
		"viewer_origin", redactViewerURL(session.ViewerURL),
	)
	sound.PlayStart()

	// Copying the link is a nicety: if the clipboard is locked by another
	// process we still record, we just say so.
	var copyErr error
	if session.ViewerURL != "" {
		copyErr = a.copyToClipboard(session.ViewerURL)
		if copyErr != nil {
			a.logger.Warn("could not copy the link", "error", copyErr)
		}
	}
	a.announce(session, copyErr)
}

// SwitchSharing retargets a live stream when the picker selects a different
// window or display. Starting from idle is allowed too, which keeps the picker
// code to one path.
func (a *App) SwitchSharing(target capture.Target) error {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	if state == StateIdle {
		a.startSharing(&target)
		return nil
	}
	if state != StateStreaming {
		return errors.New("the stream is busy starting or stopping")
	}

	a.mu.Lock()
	a.state = StateStarting
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := a.streamer.SwitchTarget(ctx, target)
	if err != nil {
		a.logger.Error("could not switch the stream target", "error", err)
		a.mu.Lock()
		a.state = StateStreaming
		a.mu.Unlock()
		a.refresh()
		var rejected *pipeline.SwitchRejectedError
		if errors.As(err, &rejected) {
			// The target was rejected before ffmpeg was restarted, so the
			// current capture is still running and viewers stay connected.
			a.notify("Could not switch the stream", "The current target is still live. "+err.Error())
			return err
		}
		a.notify("Could not switch the stream", err.Error())
		// The old capture has already stopped, so leaving the room "live"
		// would show a frozen frame forever.
		go a.stopSharing()
		return err
	}

	a.mu.Lock()
	a.state = StateStreaming
	targetCopy := target
	session.Target = &targetCopy
	a.session = session
	a.activeTarget = &targetCopy
	a.mu.Unlock()
	a.refresh()
	a.logger.Info("stream target switched", "target", target.Describe())
	a.notify("Stream switched", target.Describe())
	return nil
}

// shouldStartBot decides whether the local Discord bot should run. It is only
// useful when notifications are configured. An explicit script is still a
// valid way to point at a development launcher; otherwise the built-in
// `streamy bot` subcommand is used.
func (a *App) shouldStartBot(cfg config.Config) bool {
	if !cfg.Stream.Stack.AutoStart {
		return false
	}
	return strings.TrimSpace(cfg.Stream.Stack.Script) != "" ||
		strings.TrimSpace(cfg.Stream.Discord.NotifyURL) != ""
}

// notifyBotStopped removes the bot's live link. It is best effort: a bot outage
// must never make stopping a stream slow or fail.
func (a *App) notifyBotStopped(ctx context.Context, cfg config.Config) {
	discord := cfg.Stream.Discord
	if discord.NotifyURL == "" {
		return
	}
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := output.NotifyBotStop(notifyCtx, discord.NotifyURL, discord.NotifyKey, output.BotNotice{
		ChannelID: discord.ChannelID,
	}); err != nil {
		a.logger.Warn("could not remove the Discord live link", "error", err)
	}
}

func redactViewerURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if parsed.Host != "" {
		return parsed.Scheme + "://" + parsed.Host
	}
	return parsed.Scheme + ":"
}

// announce reports a started session to the user.
//
// The wording is derived from the session rather than hard coded, because
// "sharing" and "recording to a file" are different enough that the same
// sentence would be wrong for one of them.
func (a *App) announce(session pipeline.Session, copyErr error) {
	title := sessionLabel(session, "Sharing")
	if session.LocalRecording != "" {
		title = "Recording started"
	}

	message := session.Description
	switch {
	case session.ViewerURL == "":
		// Nothing to copy; the description already says where it is going.

	case copyErr != nil:
		message += "\n\nLink (could not copy it automatically):\n" + session.ViewerURL

	case session.LocalRecording != "":
		// The link is the recording's own path, so repeating it would be
		// noise.
		message += "\n\nThe link is on your clipboard."

	default:
		message += "\n\nLink copied to your clipboard:\n" + session.ViewerURL
	}
	a.notify(title, message)
}

func sessionLabel(session pipeline.Session, fallback string) string {
	if session.Label == "" {
		return fallback
	}
	return session.Label
}

func (a *App) stopSharing() {
	a.mu.Lock()
	if a.state != StateStreaming {
		a.mu.Unlock()
		return
	}
	a.state = StateStopping
	cfg := a.cfg
	a.mu.Unlock()
	a.refresh()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	if err := a.streamer.Stop(ctx); err != nil {
		a.logger.Error("problem stopping the capture", "error", err)
		a.notify("Problem stopping", err.Error())
	}
	// Do not make the tray wait on Discord; the bot call is best effort.
	go a.notifyBotStopped(ctx, cfg)

	a.mu.Lock()
	a.state, a.session = StateIdle, pipeline.Session{}
	a.activeTarget = nil
	a.lastInteraction = time.Now()
	a.mu.Unlock()
	a.refresh()
	sound.PlayStop()

	a.logger.Info("session stopped")
}

// OpenSettings launches the settings window (sidebar, no capture picker).
func (a *App) OpenSettings() { a.openUI("settings") }

// OpenPicker launches the full-window capture picker. It deliberately has no
// sidebar, so choosing a window stays one click away.
func (a *App) OpenPicker() { a.openUI("share") }

func (a *App) openUI(view string) {
	path := a.uiExecutable()
	if path == "" {
		a.logger.Info("settings UI is not installed", "config", a.cfgPath)
		a.notify("Settings UI not found", "Build streamy-ui.exe and place it beside Streamy.\nFor now, edit:\n"+a.cfgPath)
		return
	}

	a.mu.Lock()
	ipc := a.ipc
	if a.shuttingDown || ipc == nil {
		a.mu.Unlock()
		if ipc == nil {
			a.notify("Settings unavailable", "The local settings connection is not running.")
		}
		return
	}
	a.lastInteraction = time.Now()
	a.mu.Unlock()

	cmd := exec.Command(path, "-ipc-address", ipc.address, "-ipc-token", ipc.token, "-view", view)
	// The settings UI is a Wails window. A console-subsystem build would
	// otherwise flash a terminal, but HideConsoleWindow's SW_HIDE would also
	// hide the first ShowWindow call. NoConsoleWindow suppresses only the
	// terminal.
	executil.NoConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		a.logger.Error("could not launch the settings UI", "path", path, "error", err)
		a.notify("Could not open settings", err.Error())
		return
	}

	done := make(chan struct{})
	a.mu.Lock()
	if a.shuttingDown || a.ipc != ipc {
		a.mu.Unlock()
		_ = cmd.Process.Kill()
		close(done)
		return
	}
	a.uiProcesses[cmd] = done
	a.mu.Unlock()

	// Reap the child so it does not stay a zombie if it exits on its own.
	go func() {
		waitErr := cmd.Wait()
		a.mu.Lock()
		delete(a.uiProcesses, cmd)
		expectedShutdown := a.shuttingDown
		a.mu.Unlock()
		close(done)
		if waitErr != nil && !errors.Is(waitErr, os.ErrProcessDone) && !expectedShutdown {
			a.logger.Warn("settings UI exited with an error", "error", waitErr)
		}
	}()
	a.logger.Info("settings UI launched", "path", path, "view", view)
}

// uiExecutable returns the path of the Wails UI next to this binary, or "".
func (a *App) uiExecutable() string {
	self, err := os.Executable()
	if err != nil {
		a.logger.Warn("could not resolve tray executable path", "error", err)
		return ""
	}
	workingDir, err := os.Getwd()
	if err != nil {
		a.logger.Warn("could not resolve current working directory", "error", err)
	}
	if workingDir == "" {
		return ""
	}
	return findUIExecutable(self, workingDir)
}

func findUIExecutable(self, workingDir string) string {
	sibling := filepath.Join(filepath.Dir(self), "streamy-ui.exe")
	if isFile(sibling) {
		return sibling
	}

	// `go run` puts the tray executable in a temporary directory. In a
	// development checkout, find the UI built by build.ps1 relative to the
	// module root instead of incorrectly reporting it as missing.
	if isFile(filepath.Join(workingDir, "go.mod")) &&
		isFile(filepath.Join(workingDir, "ui", "main.go")) {
		candidate := filepath.Join(workingDir, "out", "bin", "streamy-ui.exe")
		if isFile(candidate) {
			return candidate
		}
	}
	return ""
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// CopyViewerLink implements tray.Controller.
func (a *App) CopyViewerLink() {
	a.mu.Lock()
	url := a.session.ViewerURL
	a.mu.Unlock()

	if url == "" {
		a.notify("No link yet", "Start capturing first: the link appears once a session is running.")
		return
	}
	if err := a.copyToClipboard(url); err != nil {
		a.logger.Error("could not copy the link", "error", err)
		a.notify("Could not copy link", err.Error())
		return
	}
	a.notify("Link copied", url)
}

// CheckForUpdates implements tray.Controller.
func (a *App) CheckForUpdates() {
	if a.updateChecker == nil {
		a.notify("Update check unavailable", "Auto-update is not configured. Set a GitHub repo in settings to enable.")
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		result, err := a.updateChecker.Check(ctx)
		if err != nil {
			a.logger.Error("update check failed", "error", err)
			a.notify("Update check failed", err.Error())
			return
		}

		if !result.UpdateAvailable {
			a.notify("No updates", fmt.Sprintf("You are on the latest version (%s).", result.CurrentVersion))
			return
		}

		// Update available - open the download URL in the browser
		downloadURL := result.DownloadURL
		if downloadURL == "" {
			downloadURL = result.ReleaseURL
		}

		if err := openInBrowser(downloadURL); err != nil {
			a.logger.Error("could not open download URL", "error", err, "url", downloadURL)
			a.notify("Could not open download page", fmt.Sprintf("Please visit: %s", downloadURL))
			return
		}

		a.notify("Update available", fmt.Sprintf("Opening download for version %s...", result.LatestVersion))
	}()
}

// openInBrowser opens a URL in the default browser.
func openInBrowser(url string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
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
	a.quitOnce.Do(func() { close(a.quitCh) })
	a.mu.Lock()
	a.shuttingDown = true
	uiProcesses := make([]*exec.Cmd, 0, len(a.uiProcesses))
	uiDones := make([]chan struct{}, 0, len(a.uiProcesses))
	for cmd, done := range a.uiProcesses {
		uiProcesses = append(uiProcesses, cmd)
		uiDones = append(uiDones, done)
	}
	cfg := a.cfg
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var errs []error
	for _, cmd := range uiProcesses {
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("close settings UI: %w", err))
			}
		}
	}
	for _, done := range uiDones {
		select {
		case <-done:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("wait for settings UI to close: %w", ctx.Err()))
		}
	}

	if err := a.hotkeys.Unregister(); err != nil {
		errs = append(errs, err)
	}
	if err := a.pickerHotkeys.Unregister(); err != nil {
		errs = append(errs, err)
	}

	a.mu.Lock()
	wasActive := a.state == StateStreaming || a.state == StateStarting || a.state == StateStopping
	a.state, a.session = StateIdle, pipeline.Session{}
	a.mu.Unlock()

	if wasActive {
		if err := a.streamer.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop streamer: %w", err))
		}
		a.notifyBotStopped(ctx, cfg)
	}
	if err := a.stopIPC(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop settings IPC: %w", err))
	}
	if err := a.stack.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop local bot: %w", err))
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
