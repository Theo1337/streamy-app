//go:build windows

package capture

import (
	"testing"
	"unsafe"
)

// GetCursorInfo writes into CURSORINFO, so a wrong field offset would corrupt
// memory rather than fail visibly. Assert the layout the Win32 header defines.
func TestCursorInfoLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(cursorInfo{}), uintptr(24); got != want {
		t.Errorf("cursorInfo is %d bytes, want %d", got, want)
	}
	var info cursorInfo
	base := uintptr(unsafe.Pointer(&info))
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Flags", uintptr(unsafe.Pointer(&info.Flags)) - base, 4},
		{"HCursor", uintptr(unsafe.Pointer(&info.HCursor)) - base, 8},
		{"PtScreenPos", uintptr(unsafe.Pointer(&info.PtScreenPos)) - base, 16},
	} {
		if tc.got != tc.want {
			t.Errorf("cursorInfo.%s is at offset %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestCursorDrawRect(t *testing.T) {
	tests := []struct {
		name                                     string
		relX, relY, hotspotX, hotspotY           int
		cursorWidth, cursorHeight                int
		srcWidth, srcHeight, outWidth, outHeight int
		wantX, wantY, wantWidth, wantHeight      int
		wantOK                                   bool
	}{
		{
			name: "unscaled frame keeps the icon at its native size",
			relX: 100, relY: 200, hotspotX: 5, hotspotY: 7,
			cursorWidth: 32, cursorHeight: 32,
			srcWidth: 1920, srcHeight: 1080, outWidth: 1920, outHeight: 1080,
			wantX: 95, wantY: 193, wantWidth: 32, wantHeight: 32, wantOK: true,
		},
		{
			name: "downscaled frame scales position and icon",
			relX: 100, relY: 200, hotspotX: 5, hotspotY: 7,
			cursorWidth: 32, cursorHeight: 32,
			srcWidth: 1920, srcHeight: 1080, outWidth: 960, outHeight: 540,
			wantX: 48, wantY: 97, wantWidth: 16, wantHeight: 16, wantOK: true,
		},
		{
			name: "pointer left of the window is not drawn",
			relX: -1, relY: 10, hotspotX: 0, hotspotY: 0,
			cursorWidth: 32, cursorHeight: 32,
			srcWidth: 1920, srcHeight: 1080, outWidth: 1920, outHeight: 1080,
			wantOK: false,
		},
		{
			name: "pointer past the right edge is not drawn",
			relX: 1920, relY: 10, hotspotX: 0, hotspotY: 0,
			cursorWidth: 32, cursorHeight: 32,
			srcWidth: 1920, srcHeight: 1080, outWidth: 1920, outHeight: 1080,
			wantOK: false,
		},
		{
			name: "unknown cursor size is skipped",
			relX: 100, relY: 100, hotspotX: 0, hotspotY: 0,
			cursorWidth: 0, cursorHeight: 0,
			srcWidth: 1920, srcHeight: 1080, outWidth: 1920, outHeight: 1080,
			wantOK: false,
		},
		{
			name: "empty source is skipped",
			relX: 0, relY: 0, hotspotX: 0, hotspotY: 0,
			cursorWidth: 32, cursorHeight: 32,
			srcWidth: 0, srcHeight: 0, outWidth: 1920, outHeight: 1080,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x, y, width, height, ok := cursorDrawRect(
				tc.relX, tc.relY, tc.hotspotX, tc.hotspotY,
				tc.cursorWidth, tc.cursorHeight,
				tc.srcWidth, tc.srcHeight, tc.outWidth, tc.outHeight,
			)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if x != tc.wantX || y != tc.wantY || width != tc.wantWidth || height != tc.wantHeight {
				t.Errorf("got (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					x, y, width, height, tc.wantX, tc.wantY, tc.wantWidth, tc.wantHeight)
			}
		})
	}
}
