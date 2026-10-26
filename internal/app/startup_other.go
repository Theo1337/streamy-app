//go:build !windows

package app

import "errors"

func setRunAtStartup(bool, string) error {
	return errors.New("run at system startup is only supported on Windows")
}
