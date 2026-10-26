// Package stack owns the lifecycle of the local Discord bot.
//
// The bot is a subcommand of the same executable (`streamy bot`), so the tray
// starts a child process instead of a Node script. A configured script is still
// honoured for development and for people who point config.json at their own
// launcher; an empty script uses the built-in bot.
package stack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"streamy/internal/config"
	"streamy/internal/executil"
)

const (
	defaultBotPort = 8791
	startupTimeout = 30 * time.Second
	stopTimeout    = 10 * time.Second
)

// Manager is safe to call from several goroutines. Start is idempotent and
// Stop only stops a bot that this process started (or adopted after a restart).
type Manager struct {
	logger *slog.Logger
	script string
	port   int
	apiKey string

	mu         sync.Mutex
	started    bool
	running    bool
	generation int
	cancel     context.CancelFunc
	procDone   chan struct{}
	err        error
}

// New builds a manager. notifyURL supplies the port the bot must listen on;
// notifyKey is used to ask an already-running bot to quit gracefully.
func New(logger *slog.Logger, settings config.LocalBot, notifyURL, notifyKey string) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		logger: logger,
		script: strings.TrimSpace(settings.Script),
		port:   portFromNotifyURL(notifyURL),
		apiKey: strings.TrimSpace(notifyKey),
	}
}

// Start launches the bot in the background. It is safe to call repeatedly.
func (m *Manager) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.generation++
	generation := m.generation
	script := m.script
	m.mu.Unlock()

	if script != "" {
		if info, err := os.Stat(script); err == nil && !info.IsDir() {
			m.logger.Info("starting the local Discord bot from a script", "script", script)
			go m.runScript(script, generation)
			return
		}
		m.logger.Warn("configured bot script was not found; using the built-in bot", "script", script)
	}

	// A previous Streamy process may have left the bot running. Adopt it
	// instead of fighting over the port.
	if m.healthOK() {
		m.mu.Lock()
		if generation == m.generation {
			m.running = true
			m.err = nil
		}
		m.mu.Unlock()
		m.logger.Info("the local Discord bot is already running", "port", m.port)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		cancel()
		return
	}
	m.cancel = cancel
	m.procDone = done
	m.mu.Unlock()

	go m.runBuiltin(ctx, done, generation)
}

// Running reports whether the bot is answering on its port. The idle watchdog
// uses it to decide whether there is anything to stop.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// Stop shuts the bot down. A bot started by this process is asked to exit
// cleanly and then killed if it does not; an adopted bot is asked to exit too.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	started := m.started
	script := m.script
	cancel := m.cancel
	done := m.procDone
	m.started = false
	m.running = false
	m.cancel = nil
	m.procDone = nil
	m.generation++
	m.mu.Unlock()

	if !started {
		return nil
	}

	if script != "" {
		if info, err := os.Stat(script); err == nil && !info.IsDir() {
			if cancel != nil {
				cancel()
			}
			m.logger.Info("stopping the local Discord bot")
			runCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer stopCancel()
			args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Stop"}
			if err := m.exec(runCtx, args...); err != nil {
				return fmt.Errorf("stop Discord bot: %w", err)
			}
			return nil
		}
	}

	m.logger.Info("stopping the local Discord bot")
	m.requestQuit(ctx)
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(stopTimeout):
			m.logger.Warn("the local Discord bot did not stop in time")
		}
	}
	return nil
}

func (m *Manager) runBuiltin(ctx context.Context, done chan struct{}, generation int) {
	defer close(done)

	self, err := os.Executable()
	if err != nil {
		m.finish(generation, err, false)
		return
	}
	cmd := exec.CommandContext(ctx, self, "bot", "-port", strconv.Itoa(m.port))
	cmd.Dir = filepath.Dir(self)
	executil.HideConsoleWindow(cmd)
	if logFile := m.openLog(); logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	if err := cmd.Start(); err != nil {
		m.finish(generation, err, false)
		return
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	deadline := time.Now().Add(startupTimeout)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-waitCh:
			// The bot exited before it became healthy: a bad token, a port
			// already in use, or a missing .env.
			if err != nil {
				m.logger.Warn("the local Discord bot exited during startup", "error", err)
			}
			m.finish(generation, err, false)
			return
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-waitCh
			m.finish(generation, ctx.Err(), false)
			return
		case <-ticker.C:
			if m.healthOK() {
				m.mu.Lock()
				if generation == m.generation {
					m.running = true
					m.err = nil
				}
				m.mu.Unlock()
				m.logger.Info("the local Discord bot is ready", "port", m.port)

				err := <-waitCh
				if err != nil && ctx.Err() == nil {
					m.logger.Warn("the local Discord bot exited", "error", err)
				}
				m.finish(generation, err, false)
				return
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				<-waitCh
				m.finish(generation, fmt.Errorf("bot did not become healthy on port %d", m.port), false)
				return
			}
		}
	}
}

func (m *Manager) runScript(script string, generation int) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	m.mu.Lock()
	if generation != m.generation {
		m.mu.Unlock()
		cancel()
		return
	}
	m.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	err := m.exec(ctx, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Hidden")
	m.finish(generation, err, err == nil)
	if err != nil {
		m.logger.Warn("the local Discord bot did not start", "error", err)
		return
	}
	m.logger.Info("the local Discord bot is ready")
}

func (m *Manager) finish(generation int, err error, running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	m.running = running
	m.err = err
}

func (m *Manager) exec(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "powershell.exe", args...)
	executil.HideConsoleWindow(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if text != "" {
			return fmt.Errorf("%w: %s", err, tail(text, 800))
		}
		return err
	}
	return nil
}

func (m *Manager) healthOK() bool {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", m.port))
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (m *Manager) requestQuit(ctx context.Context) {
	if m.apiKey == "" {
		return
	}
	quitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(quitCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/quit", m.port), nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+m.apiKey)
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return
	}
	response.Body.Close()
}

func (m *Manager) openLog() *os.File {
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	dir := filepath.Join(filepath.Dir(self), "stack", ".run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	file, err := os.OpenFile(filepath.Join(dir, "bot.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return file
}

func portFromNotifyURL(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultBotPort
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return defaultBotPort
	}
	if value := parsed.Port(); value != "" {
		if port, err := strconv.Atoi(value); err == nil && port > 0 && port < 65536 {
			return port
		}
	}
	return defaultBotPort
}

func tail(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[len(text)-max:]
}
