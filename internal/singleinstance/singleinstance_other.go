//go:build !windows

package singleinstance

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func acquire() (func(), bool) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return func() {}, true
	}
	path := filepath.Join(dir, "streamy", "tray.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, true
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, true
	}

	// LOCK_NB means "do not block": if another process holds the lock we find
	// out immediately. The lock disappears with the file descriptor, so there
	// is no stale state after a crash.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return func() {}, false
	}

	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
	}, true
}
