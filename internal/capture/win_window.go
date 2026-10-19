//go:build windows

package capture

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowByHandle reads everything we need to know about one window.
//
// The rectangle comes from the DWM rather than GetWindowRect when it can. Since
// Windows Vista, a maximised window's real rectangle extends a few pixels past
// the screen on every side, hidden by the compositor; the DWM's "extended frame
// bounds" are the pixels the user can actually see. Without that correction a
// maximised window looks bigger than its monitor and every maximised window
// would be mistaken for a fullscreen one.
func windowByHandle(hwnd uintptr) (Window, bool) {
	if hwnd == 0 {
		return Window{}, false
	}

	visible := windowIsVisible(hwnd)
	minimized := windowIsMinimized(hwnd)
	if !visible || minimized {
		return Window{}, false
	}

	// Tool windows are the invisible plumbing of other applications.
	if ex := windowExStyle(hwnd); ex&wsExToolWindow != 0 {
		return Window{}, false
	}

	rect, err := windowFrame(hwnd)
	if err != nil || rect.Empty() {
		return Window{}, false
	}

	var pid uint32
	windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid)

	return Window{
		Handle:    hwnd,
		Title:     windowText(hwnd),
		Class:     windowClass(hwnd),
		PID:       pid,
		Rect:      rect,
		Minimized: minimized,
		Visible:   visible,
	}, true
}

// windowFrame returns the visible frame of a window, in physical pixels.
func windowFrame(hwnd uintptr) (Rect, error) {
	var r windows.Rect
	// DwmGetWindowAttribute hands back the composited frame bounds.
	if err := windows.DwmGetWindowAttribute(
		windows.HWND(hwnd),
		dwmwaExtendedFrameBounds,
		unsafe.Pointer(&r),
		uint32(unsafe.Sizeof(r)),
	); err == nil {
		return rectFromWin32(r), nil
	}

	// Fall back to the plain window rectangle for windows the DWM does not
	// manage.
	if ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		return Rect{}, errors.New("GetWindowRect failed")
	}
	return rectFromWin32(r), nil
}

func windowIsVisible(hwnd uintptr) bool {
	return windows.IsWindowVisible(windows.HWND(hwnd))
}

func windowIsMinimized(hwnd uintptr) bool {
	r, _, _ := procIsIconic.Call(hwnd)
	return r != 0
}

func windowExStyle(hwnd uintptr) uint32 {
	r, _, _ := procGetWindowLongW.Call(hwnd, windowLongExStyle)
	return uint32(r)
}

func windowText(hwnd uintptr) string {
	// A fixed buffer rather than a length probe: the length of another
	// process's window can change between the two calls, and a caption longer
	// than this is not a useful identifier anyway.
	buf := make([]uint16, 512)
	if r, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	if r, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// foregroundWindow returns the window with keyboard focus, if it is capturable.
func foregroundWindow() *Window {
	hwnd := windows.GetForegroundWindow()
	if hwnd == 0 {
		return nil
	}
	w, ok := windowByHandle(uintptr(hwnd))
	if !ok {
		return nil
	}
	return &w
}

// topLevelWindows lists the windows a picker should offer.
//
// The filtering mirrors what capturable does, minus the "is the foreground
// window" part: visible, not minimised, titled, and not one of the shell
// windows that make up the desktop itself.
func topLevelWindows() []Window {
	var out []Window

	cb := windows.NewCallback(func(hwnd, lparam uintptr) uintptr {
		w, ok := windowByHandle(hwnd)
		if !ok {
			return 1 // keep enumerating
		}
		if strings.TrimSpace(w.Title) == "" || ignoredWindowClasses[w.Class] {
			return 1
		}
		out = append(out, w)
		return 1
	})

	// EnumWindows returns an error when the callback stops early, which this
	// one never does, so the error is ignored on purpose.
	_ = windows.EnumWindows(cb, nil)
	return out
}
