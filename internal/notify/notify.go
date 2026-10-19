// Package notify shows a short balloon notification anchored to the tray icon.
//
// The Go standard library has no way to talk to the Windows notification area,
// and fyne.io/systray (the library that draws our tray icon) does not expose
// balloon tips either. So on Windows we send Shell_NotifyIcon ourselves to the
// very same icon systray created — see notify_windows.go. Everywhere else this
// is a quiet no-op that just logs.
package notify

import (
	"log/slog"
)

// Show displays a notification. It never blocks and never returns an error:
// notifications are decoration, and a background app must not stall because
// the shell was busy. Failures are logged at warn level.
func Show(logger *slog.Logger, title, message string) {
	if logger == nil {
		logger = slog.Default()
	}
	// Showing a balloon means waiting for the tray window to exist, so it runs
	// on its own goroutine and the caller carries on.
	go func() {
		if err := show(title, message); err != nil {
			logger.Warn("could not show notification", "title", title, "error", err)
		}
	}()
}
