//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const startupRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const startupRegistryName = "Streamy"

func setRunAtStartup(enabled bool, configPath string) error {
	if !enabled {
		key, err := registry.OpenKey(registry.CURRENT_USER, startupRegistryPath, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open Windows startup settings: %w", err)
		}
		defer key.Close()
		if err := key.DeleteValue(startupRegistryName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("remove Streamy from Windows startup: %w", err)
		}
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find Streamy executable for startup: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve Streamy executable path: %w", err)
	}
	command := `"` + executable + `"`
	if configPath != "" {
		absoluteConfig, err := filepath.Abs(configPath)
		if err != nil {
			return fmt.Errorf("resolve Streamy config path: %w", err)
		}
		command += ` -config "` + absoluteConfig + `"`
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, startupRegistryPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Windows startup settings: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(startupRegistryName, command); err != nil {
		return fmt.Errorf("add Streamy to Windows startup: %w", err)
	}
	return nil
}
