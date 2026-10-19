//go:build windows

package notify

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// How we reach the tray icon.
//
// fyne.io/systray creates a hidden window of class "SystrayClass" and registers
// its notification icon with uID 100 (see systray_windows.go in the module
// cache). A tray icon can be modified from anywhere in the system: we look the
// window up by class name and ask the shell to turn the balloon on.
//
// The upside is that the balloon is properly attached to our icon — same
// position, same behaviour, no second icon in the tray. The downside is that
// these two numbers are an implementation detail of the systray library. If a
// future version changes them, findSystrayWindow fails and Show() logs a
// warning instead of crashing: the status text in the tooltip still works.
const (
	systrayWindowClass = "SystrayClass"
	systrayIconID      = 100
)

// NOTIFYICONDATAW / Shell_NotifyIcon constants.
const (
	nimModify = 0x00000001 // NIM_MODIFY
	nifInfo   = 0x00000010 // NIF_INFO: we are setting a balloon, nothing else
	niifInfo  = 0x00000001 // NIIF_INFO: the informational balloon icon
)

var (
	user32  = windows.NewLazySystemDLL("user32")
	shell32 = windows.NewLazySystemDLL("shell32")

	procFindWindowW     = user32.NewProc("FindWindowW")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
)

// notifyIconData mirrors the C NOTIFYICONDATAW struct. Go lays the fields out
// with the same alignment the C compiler uses, so the memory image matches.
//
// We only ever fill in Size, Wnd, ID, Flags and the three text fields: NIF_INFO
// tells the shell to leave everything else about the icon alone.
type notifyIconData struct {
	Size                       uint32
	Wnd                        windows.Handle
	ID, Flags, CallbackMessage uint32
	Icon                       windows.Handle
	Tip                        [128]uint16
	State, StateMask           uint32
	Info                       [256]uint16
	Timeout, Version           uint32
	InfoTitle                  [64]uint16
	InfoFlags                  uint32
	GUIDItem                   windows.GUID
	BalloonIcon                windows.Handle
}

func show(title, message string) error {
	hwnd, err := findSystrayWindow()
	if err != nil {
		return err
	}

	var nid notifyIconData
	nid.Size = uint32(unsafe.Sizeof(nid))
	nid.Wnd = hwnd
	nid.ID = systrayIconID
	nid.Flags = nifInfo
	nid.InfoFlags = niifInfo

	copyUTF16(nid.Tip[:], title)
	copyUTF16(nid.Info[:], message)
	copyUTF16(nid.InfoTitle[:], title)

	ok, _, callErr := procShellNotifyIcon.Call(
		uintptr(nimModify),
		uintptr(unsafe.Pointer(&nid)),
	)
	if ok == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_MODIFY): %w", win32Err(callErr))
	}
	return nil
}

// findSystrayWindow waits briefly for the tray window to appear. The first
// notification is often fired from systray's own "ready" callback, which can
// run before the window is visible to other processes.
func findSystrayWindow() (windows.Handle, error) {
	class, err := windows.UTF16PtrFromString(systrayWindowClass)
	if err != nil {
		return 0, err
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(class)), 0)
		if hwnd != 0 {
			return windows.Handle(hwnd), nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("tray window (class " + systrayWindowClass + ") not found")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// copyUTF16 copies s into a fixed size UTF-16 buffer, truncating if needed.
func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0 // keep the string NUL terminated
	}
	copy(dst, u)
}

// win32Err turns the error value returned by a syscall into something readable.
// On success Windows still reports errno 0, which Go surfaces as a non nil
// error, so that case has to be filtered out.
func win32Err(err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return errors.New("the call failed")
	}
	return err
}
