//go:build windows

package clipboard

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	cfUnicodeText = 13     // CF_UNICODETEXT
	gmemMoveable  = 0x0002 // GMEM_MOVEABLE
	openAttempts  = 10     // the clipboard is a shared, briefly locked resource
	openRetryWait = 20 * time.Millisecond
)

var (
	user32   = windows.NewLazySystemDLL("user32")
	kernel32 = windows.NewLazySystemDLL("kernel32")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

// writeText puts text on the clipboard as CF_UNICODETEXT.
//
// The Windows clipboard protocol is: open the clipboard (only one process at a
// time can own it), empty it, allocate global memory, copy the string in, hand
// that memory to the clipboard and close. After SetClipboardData succeeds the
// system owns the memory and we must not free it — freeing it on the failure
// paths is what the explicit GlobalFree calls are for.
func writeText(text string) error {
	if err := openClipboard(); err != nil {
		return err
	}
	// Closing the clipboard is required, otherwise every other application
	// hangs the next time it tries to paste.
	defer procCloseClipboard.Call()

	if ok, _, err := procEmptyClipboard.Call(); ok == 0 {
		return wrap("EmptyClipboard", err)
	}

	utf16, err := windows.UTF16FromString(text) // includes the trailing NUL
	if err != nil {
		return fmt.Errorf("encode clipboard text: %w", err)
	}
	size := uintptr(len(utf16) * 2) // 2 bytes per UTF-16 code unit

	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return wrap("GlobalAlloc", err)
	}

	ptr, _, err := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return wrap("GlobalLock", err)
	}
	// Copy with RtlMoveMemory (memmove) instead of dereferencing ptr through an
	// unsafe.Pointer. Going uintptr -> unsafe.Pointer would be a "possible
	// misuse of unsafe.Pointer" as far as `go vet` is concerned, and it has a
	// point: the Go runtime is free to move or collect things once the only
	// reference is an integer. Passing the address of the Go slice *to* the
	// system call is the safe direction, and KeepAlive keeps the slice alive
	// until the copy has happened.
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&utf16[0])), size)
	runtime.KeepAlive(utf16)
	procGlobalUnlock.Call(h)

	if ok, _, err := procSetClipboardData.Call(cfUnicodeText, h); ok == 0 {
		procGlobalFree.Call(h)
		return wrap("SetClipboardData", err)
	}
	return nil
}

// openClipboard retries for a moment: another application may be reading the
// clipboard right now, and OpenClipboard fails instead of waiting.
func openClipboard() error {
	for i := 0; i < openAttempts; i++ {
		if ok, _, _ := procOpenClipboard.Call(0); ok != 0 {
			return nil
		}
		time.Sleep(openRetryWait)
	}
	return errors.New("OpenClipboard: the clipboard is locked by another application")
}

// wrap gives a Win32 call failure a readable message. A successful call still
// reports errno 0, which Go surfaces as a non nil error, so that is filtered.
func wrap(api string, err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s failed", api)
	}
	return fmt.Errorf("%s: %w", api, err)
}
