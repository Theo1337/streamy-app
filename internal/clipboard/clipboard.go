// Package clipboard writes text to the system clipboard.
//
// The hotkey flow copies the viewer link automatically, so this tiny package
// sits right on the critical path. It is implemented per platform instead of
// pulling in a dependency, because Windows only needs three user32 calls.
package clipboard

// WriteText replaces the clipboard contents with text.
func WriteText(text string) error {
	return writeText(text)
}
