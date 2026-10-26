package executil

import "os/exec"

// HideConsoleWindow stops Windows from creating a visible console for a
// subprocess. ffmpeg is a console program; started from the tray it would
// otherwise pop a window, steal focus, and knock exclusive-fullscreen games
// back to windowed mode. Other platforms do nothing.
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	hideConsoleWindow(cmd)
}

// NoConsoleWindow also keeps Windows from creating a console for a subprocess,
// but unlike HideConsoleWindow it leaves the child's initial show state alone.
// A console-subsystem settings UI gets no terminal, and a GUI child (Wails)
// still shows its window on the first ShowWindow call instead of inheriting
// SW_HIDE. Other platforms do nothing.
func NoConsoleWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	noConsoleWindow(cmd)
}
