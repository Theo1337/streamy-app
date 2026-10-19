//go:build windows

package capture

import (
	"strings"
	"testing"
	"unsafe"
)

// The structures in win_dxgi.go are handed to C code that writes into them, so
// a wrong field offset does not fail a test, it corrupts memory. These
// assertions are the cheapest way to catch that.
func TestDXGIStructLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(dxgiAdapterDesc1{}), uintptr(312); got != want {
		t.Errorf("dxgiAdapterDesc1 is %d bytes, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(dxgiOutputDesc{}), uintptr(96); got != want {
		t.Errorf("dxgiOutputDesc is %d bytes, want %d", got, want)
	}

	// The fields after the variable-length name are the ones that matter, so
	// check the offsets rather than only the total size: a shift that happens
	// to keep the size would still read the wrong numbers.
	var adapter dxgiAdapterDesc1
	base := uintptr(unsafe.Pointer(&adapter))
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"AdapterLuid", uintptr(unsafe.Pointer(&adapter.AdapterLuid)) - base, 296},
		{"Flags", uintptr(unsafe.Pointer(&adapter.Flags)) - base, 304},
		{"VendorID", uintptr(unsafe.Pointer(&adapter.VendorID)) - base, 256},
	} {
		if tc.got != tc.want {
			t.Errorf("dxgiAdapterDesc1.%s is at offset %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	var output dxgiOutputDesc
	base = uintptr(unsafe.Pointer(&output))
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"DesktopCoordinates", uintptr(unsafe.Pointer(&output.DesktopCoordinates)) - base, 64},
		{"AttachedToDesktop", uintptr(unsafe.Pointer(&output.AttachedToDesktop)) - base, 80},
		{"Rotation", uintptr(unsafe.Pointer(&output.Rotation)) - base, 84},
		{"Monitor", uintptr(unsafe.Pointer(&output.Monitor)) - base, 88},
	} {
		if tc.got != tc.want {
			t.Errorf("dxgiOutputDesc.%s is at offset %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestDXGIRotationDegrees(t *testing.T) {
	for _, tc := range []struct {
		rotation uint32
		want     int
	}{
		{dxgiModeRotationIdentity, 0},
		{dxgiModeRotation90, 90},
		{dxgiModeRotation180, 180},
		{dxgiModeRotation270, 270},
		// DXGI_MODE_ROTATION_UNSPECIFIED, which DXGI also uses for "no
		// rotation"; treating it as 0 is what desktop duplication does.
		{0, 0},
		// An unknown value must not be read as a rotation.
		{99, 0},
	} {
		if got := dxgiRotationDegrees(tc.rotation); got != tc.want {
			t.Errorf("dxgiRotationDegrees(%d) = %d, want %d", tc.rotation, got, tc.want)
		}
	}
}

// TestDXGIFrameSize covers the portrait monitor this was developed against: the
// desktop is 1080x1920, but Desktop Duplication hands out the unrotated
// 1920x1080 surface.
func TestDXGIFrameSize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output dxgiOutput
		want   Rect
	}{
		{
			name:   "landscape",
			output: dxgiOutput{Desktop: Rect{X: 0, Y: 0, Width: 1920, Height: 1080}},
			want:   Rect{X: 0, Y: 0, Width: 1920, Height: 1080},
		},
		{
			name:   "portrait, quarter turn",
			output: dxgiOutput{Desktop: Rect{X: 1920, Y: -232, Width: 1080, Height: 1920}, Rotation: 90},
			want:   Rect{X: 1920, Y: -232, Width: 1920, Height: 1080},
		},
		{
			name:   "portrait, three quarter turn",
			output: dxgiOutput{Desktop: Rect{X: 0, Y: 0, Width: 1080, Height: 1920}, Rotation: 270},
			want:   Rect{X: 0, Y: 0, Width: 1920, Height: 1080},
		},
		{
			name:   "upside down is still landscape",
			output: dxgiOutput{Desktop: Rect{X: 0, Y: 0, Width: 1920, Height: 1080}, Rotation: 180},
			want:   Rect{X: 0, Y: 0, Width: 1920, Height: 1080},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.output.frameSize(); got != tc.want {
				t.Errorf("frameSize() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCaptureAdapter(t *testing.T) {
	hardware := dxgiAdapter{LUID: 0xE106, Name: "NVIDIA GeForce RTX 3060"}
	software := dxgiAdapter{LUID: 0x5000, Name: "Microsoft Basic Render Driver", Software: true}

	t.Run("skips the software rasteriser", func(t *testing.T) {
		got, skipped, ok := captureAdapter([]dxgiAdapter{software, hardware})
		if !ok || got.LUID != hardware.LUID {
			t.Fatalf("captureAdapter picked %+v (ok=%t), want %+v", got, ok, hardware)
		}
		if len(skipped) != 1 || skipped[0].LUID != software.LUID {
			t.Errorf("skipped = %+v, want just the software adapter", skipped)
		}
	})

	t.Run("first hardware adapter wins", func(t *testing.T) {
		second := dxgiAdapter{LUID: 0x2222, Name: "second"}
		got, _, ok := captureAdapter([]dxgiAdapter{hardware, second})
		if !ok || got.LUID != hardware.LUID {
			t.Errorf("captureAdapter picked %+v, want the first hardware adapter", got)
		}
	})

	t.Run("no adapter at all", func(t *testing.T) {
		if _, _, ok := captureAdapter(nil); ok {
			t.Error("captureAdapter claimed to find an adapter in an empty list")
		}
	})
}

// rawMonitorAt and dxgiOutputAt build fixtures. The handles are arbitrary but
// stable, which is all the join needs: it compares them for equality.
func rawMonitorAt(handle uintptr, name string, bounds Rect, primary bool) rawMonitor {
	work := bounds
	work.Height -= 32 // a taskbar, so work area is not simply the bounds
	return rawMonitor{Handle: handle, DeviceName: name, Bounds: bounds, WorkArea: work, Primary: primary}
}

func dxgiOutputAt(luid uint64, index int, monitor uintptr, desktop Rect) dxgiOutput {
	return dxgiOutput{
		AdapterLUID: luid,
		Index:       index,
		DeviceName:  "\\\\.\\DISPLAY1",
		Desktop:     desktop,
		Attached:    true,
		Monitor:     monitor,
	}
}

// TestJoinMonitorsTheRealMachine is the layout of the machine this was written
// on, pinned down: three monitors, the middle one portrait and rotated, all on
// one adapter, and DXGI's output indices in a different order from Windows'
// monitor order.
func TestJoinMonitorsTheRealMachine(t *testing.T) {
	const luid = 0xE106
	left := Rect{X: -1920, Y: 293, Width: 1920, Height: 1080}
	portrait := Rect{X: 1920, Y: -232, Width: 1080, Height: 1920}
	primary := Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

	raw := []rawMonitor{
		rawMonitorAt(101, "\\\\.\\DISPLAY2", left, false),
		rawMonitorAt(102, "\\\\.\\DISPLAY3", portrait, false),
		rawMonitorAt(103, "\\\\.\\DISPLAY1", primary, true),
	}
	outputs := []dxgiOutput{
		dxgiOutputAt(luid, 0, 103, primary),
		dxgiOutputAt(luid, 1, 101, left),
		dxgiOutputAt(luid, 2, 102, portrait),
	}
	outputs[2].Rotation = 90

	list, warnings, err := joinMonitors(raw, []dxgiAdapter{{LUID: luid, Name: "RTX 3060"}}, outputs, nil)
	if err != nil {
		t.Fatalf("joinMonitors: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(list) != 3 {
		t.Fatalf("got %d monitors, want 3", len(list))
	}

	// Every monitor must be matched. A -1 here would silently downgrade that
	// screen to GDI capture.
	for _, m := range list {
		if m.DXGIIndex < 0 {
			t.Errorf("monitor %d (%s) was not matched to an output", m.Index, m.Name)
		}
	}

	if got, want := list[0].DXGIIndex, 1; got != want {
		t.Errorf("DISPLAY2 joined to output %d, want %d", got, want)
	}
	if got, want := list[1].DXGIIndex, 2; got != want {
		t.Errorf("DISPLAY3 joined to output %d, want %d", got, want)
	}
	if got, want := list[2].DXGIIndex, 0; got != want {
		t.Errorf("DISPLAY1 joined to output %d, want %d", got, want)
	}

	// The preserved details: monitor order, primary flag, the work area that
	// tells a maximised window apart from a fullscreen one, and the rotation
	// that drives the transpose filter.
	if !list[2].Primary {
		t.Error("the monitor at the desktop origin should be primary")
	}
	if list[1].Rotation != 90 {
		t.Errorf("portrait rotation = %d, want 90", list[1].Rotation)
	}
	if got, want := list[1].SignalSize, (Rect{X: 1920, Y: -232, Width: 1920, Height: 1080}); got != want {
		t.Errorf("portrait signal size = %s, want %s", got, want)
	}
	if got, want := list[0].WorkArea.Height, 1048; got != want {
		t.Errorf("work area height = %d, want %d", got, want)
	}
}

// A monitor whose handle does not match any output is still matched on the
// rectangle. Windows hands out fresh HMONITORs when the display layout changes,
// so this path matters in practice even though it should be rare.
func TestJoinMonitorsFallsBackToTheRectangle(t *testing.T) {
	const luid = 0xE106
	bounds := Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

	raw := []rawMonitor{rawMonitorAt(999, "\\\\.\\DISPLAY1", bounds, true)}
	outputs := []dxgiOutput{dxgiOutputAt(luid, 3, 111, bounds)}

	list, warnings, _ := joinMonitors(raw, []dxgiAdapter{{LUID: luid, Name: "gpu"}}, outputs, nil)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if got, want := list[0].DXGIIndex, 3; got != want {
		t.Errorf("DXGIIndex = %d, want %d", got, want)
	}
}

func TestJoinMonitorsWithoutAnOutputFallsBackToGDI(t *testing.T) {
	const luid = 0xE106
	bounds := Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

	raw := []rawMonitor{rawMonitorAt(1, "\\\\.\\DISPLAY1", bounds, true)}

	list, warnings, _ := joinMonitors(raw, []dxgiAdapter{{LUID: luid, Name: "gpu"}}, nil, nil)
	if got, want := list[0].DXGIIndex, -1; got != want {
		t.Errorf("DXGIIndex = %d, want %d so capture falls back to GDI", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no matching graphics output") {
		t.Errorf("warnings = %v, want one about a missing graphics output", warnings)
	}
}

// An output that exists but is not attached to the desktop must not be handed
// to ffmpeg: capturing it would produce frames nobody asked for, or fail.
func TestJoinMonitorsIgnoresUnattachedOutputs(t *testing.T) {
	const luid = 0xE106
	bounds := Rect{X: 0, Y: 0, Width: 1920, Height: 1080}

	detached := dxgiOutputAt(luid, 7, 1, bounds)
	detached.Attached = false

	raw := []rawMonitor{rawMonitorAt(1, "\\\\.\\DISPLAY1", bounds, true)}
	list, _, _ := joinMonitors(raw, []dxgiAdapter{{LUID: luid, Name: "gpu"}}, []dxgiOutput{detached}, nil)

	if got, want := list[0].DXGIIndex, -1; got != want {
		t.Errorf("DXGIIndex = %d, want %d: a detached output is not capturable", got, want)
	}
}

// A monitor on an adapter other than the one ffmpeg will build its device on
// cannot be reached by output_idx, so it has to be reported rather than
// silently pointed at the wrong output number.
func TestJoinMonitorsReportsAnOutputOnAnotherAdapter(t *testing.T) {
	const otherLUID, ffmpegLUID = 0x1111, 0xE106

	raw := []rawMonitor{
		rawMonitorAt(1, "\\\\.\\DISPLAY1", Rect{X: 0, Y: 0, Width: 1920, Height: 1080}, true),
		rawMonitorAt(2, "\\\\.\\DISPLAY9", Rect{X: 1920, Y: 0, Width: 1920, Height: 1080}, false),
	}
	// ffmpeg's adapter has one output; the second monitor is on another card.
	outputs := []dxgiOutput{
		dxgiOutputAt(ffmpegLUID, 0, 1, raw[0].Bounds),
		dxgiOutputAt(otherLUID, 0, 2, raw[1].Bounds),
	}

	list, warnings, _ := joinMonitors(raw, []dxgiAdapter{{LUID: ffmpegLUID, Name: "gpu"}}, outputs, nil)
	if got, want := list[0].DXGIIndex, 0; got != want {
		t.Errorf("first monitor DXGIIndex = %d, want %d", got, want)
	}
	if got, want := list[1].DXGIIndex, -1; got != want {
		t.Errorf("second monitor DXGIIndex = %d, want %d", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "is driven by adapter LUID") {
		t.Errorf("warnings = %v, want one naming the other adapter", warnings)
	}
}

// When DXGI cannot be reached at all, every monitor falls back to GDI and there
// is exactly one warning explaining why.
func TestJoinMonitorsWhenDXGIFails(t *testing.T) {
	raw := []rawMonitor{
		rawMonitorAt(1, "\\\\.\\DISPLAY1", Rect{X: 0, Y: 0, Width: 1920, Height: 1080}, true),
		rawMonitorAt(2, "\\\\.\\DISPLAY2", Rect{X: 1920, Y: 0, Width: 1920, Height: 1080}, false),
	}

	list, warnings, _ := joinMonitors(raw, nil, nil, errTestDXGI)
	if len(list) != 2 {
		t.Fatalf("got %d monitors, want 2", len(list))
	}
	for _, m := range list {
		if m.DXGIIndex != -1 {
			t.Errorf("monitor %d got DXGIIndex %d, want -1", m.Index, m.DXGIIndex)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "could not be enumerated") {
		t.Errorf("warnings = %v, want one about enumeration failing", warnings)
	}
}

var errTestDXGI = &testError{"dxgi is unavailable"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
