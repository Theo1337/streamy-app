//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"streamy/internal/config"
)

const discordIdentityFile = "discord.identity"

func loadDiscordIdentity() (string, error) {
	path, err := discordIdentityPath()
	if err != nil {
		return "", err
	}
	protected, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read saved Discord identity: %w", err)
	}
	plain, err := unprotectDiscordIdentity(protected)
	if err != nil {
		return "", fmt.Errorf("decrypt saved Discord identity: %w", err)
	}
	return string(plain), nil
}

func saveDiscordIdentity(token string) error {
	if token == "" {
		return errors.New("Discord identity token must not be empty")
	}
	protected, err := protectDiscordIdentity([]byte(token))
	if err != nil {
		return fmt.Errorf("encrypt Discord identity: %w", err)
	}
	path, err := discordIdentityPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".discord-identity-*")
	if err != nil {
		return fmt.Errorf("create temporary identity file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure temporary identity file: %w", err)
	}
	if _, err := temp.Write(protected); err != nil {
		temp.Close()
		return fmt.Errorf("write encrypted Discord identity: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close encrypted Discord identity: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("save encrypted Discord identity: %w", err)
	}
	return nil
}

func deleteDiscordIdentity() error {
	path, err := discordIdentityPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove saved Discord identity: %w", err)
	}
	return nil
}

func discordIdentityPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("locate Discord identity storage: %w", err)
	}
	return filepath.Join(dir, discordIdentityFile), nil
}

func protectDiscordIdentity(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("Discord identity token must not be empty")
	}
	input := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	result := append([]byte(nil), unsafe.Slice(output.Data, output.Size)...)
	runtime.KeepAlive(value)
	return result, nil
}

func unprotectDiscordIdentity(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("encrypted identity file is empty")
	}
	input := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	result := append([]byte(nil), unsafe.Slice(output.Data, output.Size)...)
	runtime.KeepAlive(value)
	return result, nil
}
