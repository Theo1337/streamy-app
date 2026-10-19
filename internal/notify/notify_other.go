//go:build !windows

package notify

import (
	"log/slog"
)

// show has no platform implementation outside Windows yet. Rather than failing
// silently, the message is written to the log so nothing is lost while the
// macOS and Linux builds are being worked on.
func show(title, message string) error {
	slog.Info("notification", "title", title, "message", message)
	return nil
}
