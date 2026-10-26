package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"streamy/internal/config"
)

func TestSettingsIPCRequiresTokenAndSavesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	a, err := New(Options{
		Config:     config.Default(),
		ConfigPath: configPath,
		Headless:   true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.startIPC(); err != nil {
		t.Fatalf("startIPC: %v", err)
	}
	t.Cleanup(func() {
		if err := a.stopIPC(context.Background()); err != nil {
			t.Errorf("stopIPC: %v", err)
		}
	})

	ipc := a.ipc
	response, err := http.Get("http://" + ipc.address + "/api/config")
	if err != nil {
		t.Fatalf("unauthorized GET: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %s, want 401", response.Status)
	}

	cfg := config.Default()
	cfg.Hotkey.Enabled = false
	cfg.LogLevel = "debug"
	payload, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, "http://"+ipc.address+"/api/config", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+ipc.token)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("authorized PUT: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorized PUT status = %s, want 200", response.Status)
	}

	loaded, created, err := config.LoadFrom(configPath)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if created || loaded.LogLevel != "debug" || loaded.Hotkey.Enabled {
		t.Fatalf("saved config = %+v, created = %v; wanted debug with hotkey disabled", loaded, created)
	}
	if got := a.Config().LogLevel; got != "debug" {
		t.Fatalf("in-memory log level = %q, want debug", got)
	}
}
