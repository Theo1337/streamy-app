//go:build windows

package executil

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHideConsoleWindowSetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("ffmpeg", "-version")
	HideConsoleWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow = false, want true")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW", cmd.SysProcAttr.CreationFlags)
	}
}
