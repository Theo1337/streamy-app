//go:build !windows

package clipboard

import "errors"

// writeText is not implemented off Windows yet. Returning an error (instead of
// pretending it worked) means the caller logs a warning and the user can still
// copy the link by hand from the tray menu.
func writeText(string) error {
	return errors.New("clipboard support is only implemented on Windows for now")
}
