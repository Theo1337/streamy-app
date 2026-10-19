//go:build windows

package capture

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// rawMonitor is what EnumDisplayMonitors reports: the desktop's view of a
// screen.
type rawMonitor struct {
	Handle     uintptr
	DeviceName string
	Bounds     Rect
	WorkArea   Rect
	Primary    bool
}

// enumerateMonitors lists the screens the desktop is drawn on.
//
// EnumDisplayMonitors is the API the rest of Windows uses, so its rectangles
// are the ones a window's position should be compared against. What it does not
// know is anything about the graphics adapter.
func enumerateMonitors() ([]rawMonitor, error) {
	var out []rawMonitor

	// The callback must use only pointer sized parameters, so the rectangle
	// pointer arrives as a uintptr. The returned 1 means "keep enumerating".
	cb := windows.NewCallback(func(hmon, hdc, lprc, data uintptr) uintptr {
		mi := monitorInfoEx{CbSize: uint32(unsafe.Sizeof(monitorInfoEx{}))}
		if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return 1
		}
		out = append(out, rawMonitor{
			Handle:     hmon,
			DeviceName: windows.UTF16ToString(mi.SzDevice[:]),
			Bounds:     rectFromWin32(mi.RcMonitor),
			WorkArea:   rectFromWin32(mi.RcWork),
			Primary:    mi.DwFlags&monitorInfoPrimary != 0,
		})
		return 1
	})

	if r, _, _ := procEnumDisplayMonitors.Call(0, 0, cb, 0); r == 0 {
		return nil, fmt.Errorf("EnumDisplayMonitors returned no monitors")
	}
	return out, nil
}

// monitors returns the monitor list with the DXGI output index attached to each
// one.
//
// This join is what makes fullscreen capture work at all.
// EnumDisplayMonitors knows the desktop rectangle and the HMONITOR but nothing
// about the graphics adapter; DXGI knows the adapter, the output index, the
// rotation and the HMONITOR. The HMONITOR is the join key, and it is exact:
// both APIs hand out the same handle for the same screen.
//
// If a monitor cannot be matched, its DXGIIndex stays -1 and capture falls back
// to GDI for it rather than guessing an output number and recording somebody
// else's screen. The reason is reported in the returned warnings, which the
// caller logs once.
func monitors() (list []Monitor, warnings []string, err error) {
	raw, err := enumerateMonitors()
	if err != nil {
		return nil, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("no monitors found")
	}

	adapters, outputs, dxgiErr := enumerateDXGI()
	return joinMonitors(raw, adapters, outputs, dxgiErr)
}

// joinMonitors pairs the desktop's monitors with the graphics adapter's outputs.
//
// It is separate from monitors() so that the interesting part, the matching,
// can be tested without a graphics card or a screen.
func joinMonitors(raw []rawMonitor, adapters []dxgiAdapter, outputs []dxgiOutput, dxgiErr error) ([]Monitor, []string, error) {
	var warnings []string
	if dxgiErr != nil {
		warnings = append(warnings, fmt.Sprintf(
			"the graphics outputs could not be enumerated (%v); every monitor will be captured with GDI, which cannot capture fullscreen games reliably", dxgiErr))
	}

	adapter, skipped, haveAdapter := captureAdapter(adapters)
	if !haveAdapter && dxgiErr == nil {
		warnings = append(warnings, "no hardware graphics adapter was found, so Desktop Duplication is unavailable")
	}
	for _, s := range skipped {
		warnings = append(warnings, fmt.Sprintf("ignoring the software graphics adapter %q", s.Name))
	}

	// Only the adapter ffmpeg's D3D11 device will be created on is usable. An
	// output on any other adapter cannot be reached by output_idx, so those
	// outputs are dropped here and the monitors they drive fall back to GDI.
	byMonitor := make(map[uintptr]dxgiOutput, len(outputs))
	byRect := make(map[Rect]dxgiOutput, len(outputs))
	var elsewhere []dxgiOutput
	for _, o := range outputs {
		if !o.Attached {
			continue
		}
		if haveAdapter && o.AdapterLUID == adapter.LUID {
			byMonitor[o.Monitor] = o
			byRect[o.Desktop] = o
			continue
		}
		elsewhere = append(elsewhere, o)
	}

	list := make([]Monitor, 0, len(raw))
	for i, rm := range raw {
		m := Monitor{
			Index:     i,
			Bounds:    rm.Bounds,
			WorkArea:  rm.WorkArea,
			Primary:   rm.Primary,
			Name:      rm.DeviceName,
			DXGIIndex: -1,
		}

		output, ok := byMonitor[rm.Handle]
		if !ok {
			// A handle mismatch should not happen, but the rectangle is an
			// equally good key when it does.
			output, ok = byRect[rm.Bounds]
		}

		if ok {
			m.DXGIIndex = output.Index
			m.Rotation = output.Rotation
			m.SignalSize = output.frameSize()
		} else if dxgiErr == nil {
			for _, o := range elsewhere {
				if o.Monitor == rm.Handle || o.Desktop == rm.Bounds {
					warnings = append(warnings, fmt.Sprintf(
						"monitor %s (%s) is driven by adapter LUID %#x, but ffmpeg's capture device will be created on %q (LUID %#x), so this monitor has to be captured with GDI",
						rm.DeviceName, rm.Bounds, o.AdapterLUID, adapter.Name, adapter.LUID))
					break
				}
			}
			if len(elsewhere) == 0 {
				warnings = append(warnings, fmt.Sprintf(
					"monitor %s (%s) has no matching graphics output, so it has to be captured with GDI", rm.DeviceName, rm.Bounds))
			}
		}

		list = append(list, m)
	}
	return list, warnings, nil
}

// frameSize is the size Desktop Duplication will hand out for this output.
//
// DXGI describes the output as the desktop sees it, which for a rotated screen
// is already the rotated shape: a portrait 1080x1920 monitor reports 1080x1920
// here. The surface behind it is unrotated, so a quarter turn has to be undone
// in the filter graph, and this is the size before that happens.
func (o dxgiOutput) frameSize() Rect {
	r := o.Desktop
	if o.Rotation == 90 || o.Rotation == 270 {
		r.Width, r.Height = r.Height, r.Width
	}
	return r
}

// monitorUnderCursor returns the index into ms of the screen the mouse pointer
// is on, or -1 when the pointer position is unavailable.
func monitorUnderCursor(ms []Monitor) int {
	var pt pointL
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
		return -1
	}
	return monitorAtPoint(ms, int(pt.X), int(pt.Y))
}

// monitorAtPoint finds the monitor containing a point, falling back to the
// nearest one.
//
// The fallback matters because monitors are usually rectangles with gaps
// between them in virtual desktop coordinates, and a pointer can be in one of
// those gaps.
func monitorAtPoint(ms []Monitor, x, y int) int {
	for i, m := range ms {
		b := m.Bounds
		if x >= b.X && x < b.X+b.Width && y >= b.Y && y < b.Y+b.Height {
			return i
		}
	}

	best, bestDist := -1, 0
	for i, m := range ms {
		dx := (m.Bounds.X + m.Bounds.Width/2) - x
		dy := (m.Bounds.Y + m.Bounds.Height/2) - y
		dist := dx*dx + dy*dy
		if best < 0 || dist < bestDist {
			best, bestDist = i, dist
		}
	}
	return best
}

// rectFromWin32 converts a Windows RECT to our Rect.
func rectFromWin32(r windows.Rect) Rect {
	return Rect{
		X:      int(r.Left),
		Y:      int(r.Top),
		Width:  int(r.Right - r.Left),
		Height: int(r.Bottom - r.Top),
	}
}
