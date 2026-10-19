//go:build windows

package capture

import (
	"testing"
	"unsafe"
)

// TestDisplayConfigStructSizes pins the layout of the structures handed to
// QueryDisplayConfig.
//
// This is the safety net for the hand written bindings. A wrong size or a
// missing pad byte makes Windows write past the end of the Go slice we pass it,
// which is memory corruption rather than a returned error. The expected numbers
// come straight from the Windows SDK headers.
func TestDisplayConfigStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"LUID", unsafe.Sizeof(luID{}), 8},
		{"DISPLAYCONFIG_PATH_SOURCE_INFO", unsafe.Sizeof(displayConfigPathSourceInfo{}), 20},
		{"DISPLAYCONFIG_PATH_TARGET_INFO", unsafe.Sizeof(displayConfigPathTargetInfo{}), 48},
		{"DISPLAYCONFIG_PATH_INFO", unsafe.Sizeof(displayConfigPathInfo{}), 72},
		{"DISPLAYCONFIG_2DREGION", unsafe.Sizeof(displayConfig2DRegion{}), 8},
		{"DISPLAYCONFIG_VIDEO_SIGNAL_INFO", unsafe.Sizeof(displayConfigVideoSignalInfo{}), 48},
		{"DISPLAYCONFIG_TARGET_MODE", unsafe.Sizeof(displayConfigTargetMode{}), 48},
		{"DISPLAYCONFIG_SOURCE_MODE", unsafe.Sizeof(displayConfigSourceMode{}), 20},
		{"DISPLAYCONFIG_DESKTOP_IMAGE_INFO", unsafe.Sizeof(displayConfigDesktopImageInfo{}), 40},
		{"DISPLAYCONFIG_MODE_INFO", unsafe.Sizeof(displayConfigModeInfo{}), 64},
		{"MONITORINFOEXW", unsafe.Sizeof(monitorInfoEx{}), 104},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestDisplayConfigStructOffsets checks the offsets inside the structures where
// the union makes a mistake easy to make.
func TestDisplayConfigStructOffsets(t *testing.T) {
	var mode displayConfigModeInfo
	if got := unsafe.Offsetof(mode.TargetMode); got != 16 {
		t.Errorf("offsetof(DISPLAYCONFIG_MODE_INFO.TargetMode) = %d, want 16 (the size of InfoType+ID+AdapterID)", got)
	}

	var src displayConfigSourceMode
	if got := unsafe.Offsetof(src.Position); got != 12 {
		t.Errorf("offsetof(DISPLAYCONFIG_SOURCE_MODE.Position) = %d, want 12", got)
	}

	var path displayConfigPathInfo
	if got := unsafe.Offsetof(path.TargetInfo); got != 20 {
		t.Errorf("offsetof(DISPLAYCONFIG_PATH_INFO.TargetInfo) = %d, want 20", got)
	}
	if got := unsafe.Offsetof(path.Flags); got != 68 {
		t.Errorf("offsetof(DISPLAYCONFIG_PATH_INFO.Flags) = %d, want 68", got)
	}
}

// TestSourceModeAlias proves that reading the union as a source mode gives back
// what was written into it, which is the whole point of the C union trick.
func TestSourceModeAlias(t *testing.T) {
	var mode displayConfigModeInfo
	want := displayConfigSourceMode{
		Width:       1080,
		Height:      1920,
		PixelFormat: 2,
		Position:    pointL{X: -1920, Y: 293},
	}
	// Write through the alias, read back through the same view, and check that
	// the other view sees the same bytes: that overlap is the C union.
	*mode.sourceMode() = want

	if mode.TargetMode.VideoSignalInfo.PixelRate == 0 {
		t.Fatal("the target mode view does not see the bytes written to the source mode view")
	}
	got := *mode.sourceMode()
	if got != want {
		t.Errorf("source mode alias = %+v, want %+v", got, want)
	}
}
