//go:build windows

package sound

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mciMu      sync.Mutex
	mciCurrent string
	soundSeq   uint64

	winmm                 = windows.NewLazySystemDLL("winmm.dll")
	procMCISendString     = winmm.NewProc("mciSendStringW")
	procMCIGetErrorString = winmm.NewProc("mciGetErrorStringW")
)

func playFile(name string) error {
	path, err := materialize(name)
	if err != nil {
		return err
	}

	mciMu.Lock()
	defer mciMu.Unlock()

	// Only one cue at a time: pressing stop right after start replaces the
	// start cue instead of playing both over each other.
	closeCurrent()

	soundSeq++
	alias := fmt.Sprintf("streamy%d", soundSeq)
	if err := mciSend(fmt.Sprintf(`open "%s" type mpegvideo alias %s`, path, alias)); err != nil {
		return err
	}
	if err := mciSend("play " + alias); err != nil {
		_ = mciSend("close " + alias)
		return err
	}
	mciCurrent = alias

	wait := mciDuration(alias) + 200*time.Millisecond
	go func(alias string, wait time.Duration) {
		time.Sleep(wait)
		mciMu.Lock()
		if mciCurrent == alias {
			_ = mciSend("close " + alias)
			mciCurrent = ""
		}
		mciMu.Unlock()
	}(alias, wait)

	return nil
}

// closeCurrent frees the alias of the previous cue. The caller holds mciMu.
func closeCurrent() {
	if mciCurrent == "" {
		return
	}
	_ = mciSend("close " + mciCurrent)
	mciCurrent = ""
}

// mciSend runs a command and reports the player's error text on failure. The
// caller holds mciMu.
func mciSend(command string) error {
	cmd, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return err
	}
	ret, _, _ := procMCISendString.Call(
		uintptr(unsafe.Pointer(cmd)),
		0, 0, 0,
	)
	if ret == 0 {
		return nil
	}
	return fmt.Errorf("mci %q: %s", command, mciErrorText(uint32(ret)))
}

// mciDuration returns the clip length reported by the player, falling back to
// two seconds when the status query is unavailable. The caller holds mciMu.
func mciDuration(alias string) time.Duration {
	command := "status " + alias + " length"
	cmd, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return 2 * time.Second
	}
	buf := make([]uint16, 32)
	ret, _, _ := procMCISendString.Call(
		uintptr(unsafe.Pointer(cmd)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		0,
	)
	if ret != 0 {
		return 2 * time.Second
	}
	ms, err := strconv.Atoi(strings.TrimSpace(windows.UTF16ToString(buf)))
	if err != nil || ms <= 0 {
		return 2 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

func mciErrorText(code uint32) string {
	buf := make([]uint16, 128)
	ok, _, _ := procMCIGetErrorString.Call(
		uintptr(code),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if ok == 0 {
		return fmt.Sprintf("error code %d", code)
	}
	return windows.UTF16ToString(buf)
}
