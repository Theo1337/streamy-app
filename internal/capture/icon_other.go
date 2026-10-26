//go:build !windows

package capture

// WindowIcon is only meaningful on Windows. Other platforms return nil and the
// picker falls back to the first letter of the title.
func WindowIcon(uintptr) []byte { return nil }
