//go:build !windows

package executil

import "os/exec"

func hideConsoleWindow(*exec.Cmd) {}

func noConsoleWindow(*exec.Cmd) {}
