//go:build !windows

package sound

// playFile has no implementation outside Windows yet. Cues are decoration, so
// the rest of the application carries on silently.
func playFile(string) error { return nil }
