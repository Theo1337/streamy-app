package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
)

const maxIPCRequestBytes = 1 << 20

type ipcServer struct {
	listener net.Listener
	server   *http.Server
	address  string
	token    string
}

type targetList struct {
	Monitors []monitorItem `json:"monitors"`
	Windows  []windowItem  `json:"windows"`
}

type monitorItem struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Bounds    string `json:"bounds"`
	Primary   bool   `json:"primary"`
	DXGIIndex int    `json:"dxgiIndex"`
}

type windowItem struct {
	Handle string `json:"handle"`
	Title  string `json:"title"`
	Class  string `json:"class"`
	PID    uint32 `json:"pid"`
	Icon   string `json:"icon,omitempty"`
}

type targetChoice struct {
	Kind   string `json:"kind"`
	Index  int    `json:"index"`
	Handle string `json:"handle"`
}

func (a *App) startIPC() error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for settings IPC: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		_ = listener.Close()
		return fmt.Errorf("generate settings IPC token: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", a.ipcConfig)
	mux.HandleFunc("PUT /api/config", a.ipcSaveConfig)
	mux.HandleFunc("GET /api/targets", a.ipcTargets)
	mux.HandleFunc("GET /api/preview", a.ipcPreview)
	mux.HandleFunc("GET /api/status", a.ipcStatus)
	mux.HandleFunc("POST /api/share", a.ipcShare)
	mux.HandleFunc("POST /api/switch", a.ipcSwitch)
	mux.HandleFunc("POST /api/startup", a.ipcStartup)
	mux.HandleFunc("POST /api/stop", a.ipcStop)
	mux.HandleFunc("POST /api/discord/login", a.ipcDiscordLogin)
	mux.HandleFunc("GET /api/discord/profile", a.ipcDiscordProfile)
	mux.HandleFunc("DELETE /api/discord/profile", a.ipcDiscordLogout)
	mux.HandleFunc("GET /api/discord/callback", a.ipcDiscordCallback)
	mux.HandleFunc("GET /api/discord/done", ipcDiscordDone)
	server := &http.Server{
		Handler:           a.ipcAuth(hex.EncodeToString(secret), mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	ipc := &ipcServer{
		listener: listener,
		server:   server,
		address:  listener.Addr().String(),
		token:    hex.EncodeToString(secret),
	}
	a.mu.Lock()
	if a.ipc != nil {
		a.mu.Unlock()
		_ = listener.Close()
		return errors.New("settings IPC is already running")
	}
	a.ipc = ipc
	a.mu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("settings IPC stopped unexpectedly", "error", err)
		}
	}()
	return nil
}

func (a *App) ipcAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, OPTIONS")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet &&
			(r.URL.Path == "/api/discord/callback" || r.URL.Path == "/api/discord/done") {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) ipcConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.Config())
}

func (a *App) ipcSaveConfig(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := decodeIPCJSON(w, r, &cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.saveConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.touchInteraction()
	writeJSON(w, map[string]bool{"saved": true})
}

func (a *App) ipcTargets(w http.ResponseWriter, r *http.Request) {
	a.touchInteraction()
	resolver := capture.NewResolver(a.logger)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	desktop, err := resolver.Snapshot(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	windows, err := resolver.Windows(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	result := targetList{
		Monitors: make([]monitorItem, 0, len(desktop.Monitors)),
		Windows:  make([]windowItem, 0, len(windows)),
	}
	for _, monitor := range desktop.Monitors {
		result.Monitors = append(result.Monitors, monitorItem{
			Index: monitor.Index, Name: fmt.Sprintf("Display %d", monitor.Index+1),
			Bounds: monitor.Bounds.String(), Primary: monitor.Primary, DXGIIndex: monitor.DXGIIndex,
		})
	}
	for _, window := range windows {
		result.Windows = append(result.Windows, windowItem{
			Handle: strconv.FormatUint(uint64(window.Handle), 10),
			Title:  window.Title, Class: window.Class, PID: window.PID,
			Icon: pngDataURL(capture.WindowIcon(window.Handle)),
		})
	}
	sort.Slice(result.Monitors, func(i, j int) bool {
		return result.Monitors[i].Index < result.Monitors[j].Index
	})
	sort.Slice(result.Windows, func(i, j int) bool {
		return strings.ToLower(result.Windows[i].Title) < strings.ToLower(result.Windows[j].Title)
	})
	writeJSON(w, result)
}

func pngDataURL(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func (a *App) ipcStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, a.Status())
}

func (a *App) ipcPreview(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil {
		http.Error(w, "target index is invalid", http.StatusBadRequest)
		return
	}
	target, err := a.resolveChoice(r, targetChoice{
		Kind: r.URL.Query().Get("kind"), Index: index, Handle: r.URL.Query().Get("handle"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	preview, err := capture.Preview(ctx, target, 360, 203)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(preview)
}

func (a *App) ipcShare(w http.ResponseWriter, r *http.Request) {
	var choice targetChoice
	if err := decodeIPCJSON(w, r, &choice); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := a.resolveChoice(r, choice)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	go a.startSharing(&target)
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) ipcSwitch(w http.ResponseWriter, r *http.Request) {
	var choice targetChoice
	if err := decodeIPCJSON(w, r, &choice); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := a.resolveChoice(r, choice)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.SwitchSharing(target); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) ipcStartup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeIPCJSON(w, r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	cfg.RunAtStartup = body.Enabled
	if err := a.saveConfig(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"enabled": body.Enabled})
}

func (a *App) resolveChoice(r *http.Request, choice targetChoice) (capture.Target, error) {
	resolver := capture.NewResolver(a.logger)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	desktop, err := resolver.Snapshot(ctx)
	if err != nil {
		return capture.Target{}, err
	}
	switch choice.Kind {
	case "monitor":
		for _, monitor := range desktop.Monitors {
			if monitor.Index == choice.Index {
				return capture.Target{
					Kind: capture.KindMonitor, Monitor: monitor,
					Reason: "selected explicitly in the screen picker",
				}, nil
			}
		}
		return capture.Target{}, fmt.Errorf("monitor %d is no longer available", choice.Index)
	case "window":
		handle, err := strconv.ParseUint(choice.Handle, 10, 64)
		if err != nil || handle == 0 {
			return capture.Target{}, errors.New("window handle is invalid; refresh the target list")
		}
		windows, err := resolver.Windows(ctx)
		if err != nil {
			return capture.Target{}, err
		}
		for _, window := range windows {
			if uint64(window.Handle) == handle {
				if window.PID == desktop.SelfPID || !window.Visible || window.Minimized {
					return capture.Target{}, errors.New("that window is no longer capturable")
				}
				return capture.Target{
					Kind: capture.KindWindow, Window: window,
					Reason: "selected explicitly in the screen picker",
				}.WithDesktop(desktop), nil
			}
		}
		return capture.Target{}, errors.New("window is no longer available")
	default:
		return capture.Target{}, fmt.Errorf("unknown target kind %q", choice.Kind)
	}
}

func (a *App) ipcStop(w http.ResponseWriter, _ *http.Request) {
	go a.stopSharing()
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) saveConfig(cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.Schema = config.CurrentSchema
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.mu.Lock()
	old := a.cfg
	path := a.cfgPath
	a.mu.Unlock()

	if err := a.registerHotkeys(cfg.Hotkey); err != nil {
		return errors.Join(err, a.registerHotkeys(old.Hotkey))
	}
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			_ = a.registerHotkeys(old.Hotkey)
			return err
		}
	}
	startupChanged := cfg.RunAtStartup != old.RunAtStartup
	if startupChanged {
		if err := setRunAtStartup(cfg.RunAtStartup, path); err != nil {
			return errors.Join(fmt.Errorf("update run-at-startup setting: %w", err), a.registerHotkeys(old.Hotkey))
		}
	}
	if err := cfg.SaveTo(path); err != nil {
		rollbackErr := a.registerHotkeys(old.Hotkey)
		var startupErr error
		if startupChanged {
			startupErr = setRunAtStartup(old.RunAtStartup, path)
		}
		return errors.Join(fmt.Errorf("save settings: %w", err), rollbackErr, startupErr)
	}
	a.mu.Lock()
	a.cfg, a.cfgPath = cfg, path
	a.mu.Unlock()
	a.logger.Info("settings updated", "config", path)
	return nil
}

func (a *App) registerHotkeys(settings config.Hotkey) error {
	if err := a.hotkeys.Unregister(); err != nil {
		return fmt.Errorf("unregister toggle hotkey: %w", err)
	}
	if err := a.pickerHotkeys.Unregister(); err != nil {
		return fmt.Errorf("unregister picker hotkey: %w", err)
	}
	if !settings.Enabled {
		return nil
	}
	if err := a.hotkeys.Register(settings.Toggle); err != nil {
		return fmt.Errorf("register toggle hotkey %q: %w", settings.Toggle, err)
	}
	if err := a.pickerHotkeys.Register(settings.Picker); err != nil {
		_ = a.hotkeys.Unregister()
		return fmt.Errorf("register picker hotkey %q: %w", settings.Picker, err)
	}
	return nil
}

func (a *App) stopIPC(ctx context.Context) error {
	a.mu.Lock()
	ipc := a.ipc
	a.ipc = nil
	a.mu.Unlock()
	if ipc == nil {
		return nil
	}
	err := ipc.server.Shutdown(ctx)
	closeErr := ipc.listener.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(err, closeErr)
}

func decodeIPCJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxIPCRequestBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "could not encode response", http.StatusInternalServerError)
	}
}
