package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type discordProfile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type discordIdentityStatus struct {
	Connected bool           `json:"connected"`
	Profile   discordProfile `json:"profile"`
	Error     string         `json:"error,omitempty"`
}

type discordOAuthStart struct {
	Callback string `json:"callback"`
	State    string `json:"clientState"`
}

type discordOAuthResponse struct {
	AuthorizationURL string `json:"authorizationURL"`
	Error            string `json:"error"`
}

type discordIdentityClaims struct {
	Issuer  string `json:"iss"`
	ID      string `json:"sub"`
	Name    string `json:"name"`
	Avatar  string `json:"avatar"`
	Expires int64  `json:"exp"`
}

func (a *App) ipcDiscordLogin(w http.ResponseWriter, r *http.Request) {
	authorizationURL, err := a.startDiscordLogin(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"authorizationURL": authorizationURL})
}

func (a *App) ipcDiscordCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	identityToken := r.URL.Query().Get("identity")
	a.mu.Lock()
	expected := a.pendingDiscordState
	a.pendingDiscordState = ""
	if expected == "" || len(state) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(state), []byte(expected)) != 1 {
		a.mu.Unlock()
		http.Error(w, "Discord login state did not match; return to Streamy and try again.", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("error") != "" {
		a.identityError = "Discord sign-in was cancelled."
		a.mu.Unlock()
		http.Redirect(w, r, "/api/discord/done", http.StatusSeeOther)
		return
	}
	profile, err := parseDiscordIdentity(identityToken)
	if err != nil {
		a.identityError = err.Error()
		a.mu.Unlock()
		http.Redirect(w, r, "/api/discord/done", http.StatusSeeOther)
		return
	}
	a.mu.Unlock()
	if err := saveDiscordIdentity(identityToken); err != nil {
		a.mu.Lock()
		a.identityError = fmt.Sprintf("Discord connected, but its sign-in could not be saved locally: %v", err)
		a.mu.Unlock()
		http.Redirect(w, r, "/api/discord/done", http.StatusSeeOther)
		return
	}
	a.mu.Lock()
	a.identityToken = identityToken
	a.identityProfile = profile
	a.identityError = ""
	a.mu.Unlock()
	http.Redirect(w, r, "/api/discord/done", http.StatusSeeOther)
}

func (a *App) ipcDiscordProfile(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	status := discordIdentityStatus{
		Connected: a.identityToken != "",
		Profile:   a.identityProfile,
		Error:     a.identityError,
	}
	a.mu.Unlock()
	writeJSON(w, status)
}

func (a *App) ipcDiscordLogout(w http.ResponseWriter, _ *http.Request) {
	if err := deleteDiscordIdentity(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.identityToken = ""
	a.identityProfile = discordProfile{}
	a.identityError = ""
	a.mu.Unlock()
	writeJSON(w, map[string]bool{"connected": false})
}

func ipcDiscordDone(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, "<!doctype html><title>Discord connected</title><p>Discord sign-in complete. Return to Streamy.</p>")
}

func discordOAuthStartURL(endpoint string) (string, error) {
	worker, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("parse signaling URL: %w", err)
	}
	switch worker.Scheme {
	case "wss":
		worker.Scheme = "https"
	case "ws":
		worker.Scheme = "http"
	default:
		return "", errors.New("signaling URL must use ws or wss")
	}
	if worker.Host == "" {
		return "", errors.New("signaling URL must include a host")
	}
	worker.Path = strings.TrimRight(worker.Path, "/") + "/v1/auth/discord/start"
	worker.RawPath = ""
	worker.RawQuery = ""
	worker.Fragment = ""
	return worker.String(), nil
}

func parseDiscordIdentity(token string) (discordProfile, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 4096 {
		return discordProfile{}, errors.New("the Worker returned an invalid Discord identity")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return discordProfile{}, errors.New("the Worker returned an invalid identity header")
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return discordProfile{}, errors.New("the Worker returned an unsupported identity token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return discordProfile{}, errors.New("the Worker returned an invalid identity profile")
	}
	var claims discordIdentityClaims
	if err := json.Unmarshal(payload, &claims); err != nil ||
		claims.Issuer != "streamy-signal" || claims.ID == "" || claims.Name == "" ||
		claims.Expires <= time.Now().Unix() {
		return discordProfile{}, errors.New("the Worker returned an expired or incomplete Discord identity")
	}
	if claims.Avatar != "" {
		avatarURL, err := url.Parse(claims.Avatar)
		if err != nil || avatarURL.Scheme != "https" || avatarURL.Host != "cdn.discordapp.com" {
			return discordProfile{}, errors.New("the Worker returned an invalid Discord avatar URL")
		}
	}
	return discordProfile{ID: claims.ID, Name: claims.Name, Avatar: claims.Avatar}, nil
}

func randomDiscordState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Discord OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (a *App) startDiscordLogin(ctx context.Context) (string, error) {
	a.mu.Lock()
	cfg, ipc := a.cfg, a.ipc
	a.mu.Unlock()
	if ipc == nil {
		return "", errors.New("settings IPC is not running")
	}
	state, err := randomDiscordState()
	if err != nil {
		return "", err
	}
	startURL, err := discordOAuthStartURL(cfg.Signaling.URL)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(discordOAuthStart{
		Callback: "http://" + ipc.address + "/api/discord/callback",
		State:    state,
	})
	if err != nil {
		return "", fmt.Errorf("encode Discord login request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("create Discord login request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", fmt.Errorf("contact Discord OAuth service: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		return "", fmt.Errorf("read Discord OAuth response: %w", err)
	}
	var result discordOAuthResponse
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if json.Unmarshal(responseBody, &result) == nil && result.Error != "" {
			return "", fmt.Errorf("Discord OAuth: %s", result.Error)
		}
		return "", fmt.Errorf("Discord OAuth service returned %s", response.Status)
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || result.AuthorizationURL == "" {
		return "", errors.New("Discord OAuth service returned an invalid authorization URL")
	}
	a.mu.Lock()
	a.pendingDiscordState = state
	a.identityError = ""
	a.mu.Unlock()
	return result.AuthorizationURL, nil
}
