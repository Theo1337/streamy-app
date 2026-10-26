//go:build !windows

package app

import "errors"

func loadDiscordIdentity() (string, error) { return "", nil }

func saveDiscordIdentity(string) error {
	return errors.New("secure Discord identity storage is only supported on Windows")
}

func deleteDiscordIdentity() error {
	return errors.New("secure Discord identity storage is only supported on Windows")
}
