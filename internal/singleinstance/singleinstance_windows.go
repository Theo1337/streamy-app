//go:build windows

package singleinstance

import (
	"errors"

	"golang.org/x/sys/windows"
)

// mutexName lives in the "Local\" namespace, so it is per user session: two
// people logged into the same machine can each run their own tray.
const mutexName = `Local\streamy-tray`

func acquire() (func(), bool) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		// Cannot build the name: better to run than to refuse to start.
		return func() {}, true
	}

	handle, err := windows.CreateMutex(nil, false, name)
	// CreateMutex succeeds and reports ERROR_ALREADY_EXISTS when the mutex was
	// created by someone else — that is the "another instance is running" case.
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(handle)
		return func() {}, false
	}
	if err != nil || handle == 0 {
		return func() {}, true
	}

	// The mutex is released by the kernel when the process exits, even on a
	// crash, so there is no stale lock to clean up.
	return func() { windows.CloseHandle(handle) }, true
}
