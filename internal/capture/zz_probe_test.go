//go:build windows

package capture

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestBitBltFlags(t *testing.T) {
	screenDC, _, errDC := procGetDC.Call(0)
	fmt.Printf("screenDC=%x, errDC=%v\n", screenDC, errDC)
	defer procReleaseDC.Call(0, screenDC)

	sourceDC, _, errSDC := procCreateCompatibleDC.Call(screenDC)
	defer procDeleteDC.Call(sourceDC)

	sourceBitmap, _, errBmp := procCreateCompatibleBitmap.Call(screenDC, 100, 100)
	defer procDeleteObject.Call(sourceBitmap)

	old, _, errSel := procSelectObject.Call(sourceDC, sourceBitmap)
	defer procSelectObject.Call(sourceDC, old)

	fmt.Printf("sourceDC=%x (%v), sourceBitmap=%x (%v), old=%x (%v)\n",
		sourceDC, errSDC, sourceBitmap, errBmp, old, errSel)

	r1, _, err1 := procBitBlt.Call(sourceDC, 0, 0, 100, 100, screenDC, 0, 0, srccopy|captureLayered)
	fmt.Printf("BitBlt with CAPTUREBLT: res=%v, err=%v\n", r1, err1)

	r2, _, err2 := procBitBlt.Call(sourceDC, 0, 0, 100, 100, screenDC, 0, 0, srccopy)
	fmt.Printf("BitBlt without CAPTUREBLT (plain srccopy): res=%v, err=%v\n", r2, err2)
}

func TestCheckDesktop(t *testing.T) {
	procOpenInputDesktop := user32.NewProc("OpenInputDesktop")
	h, _, err := procOpenInputDesktop.Call(0, 0, 0x0100)
	fmt.Printf("OpenInputDesktop: handle=%x, err=%v\n", h, err)
}

func TestPreviewMonitor(t *testing.T) {
	d, err := newResolver(nil).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Monitors) == 0 {
		t.Fatal("no monitors")
	}
	for i, m := range d.Monitors {
		target := Target{Kind: KindMonitor, Monitor: m}
		data, err := Preview(context.Background(), target, 360, 203)
		if err != nil {
			t.Errorf("Preview monitor %d (%s) failed: %v", i, m.Bounds, err)
		} else {
			t.Logf("Preview monitor %d (%s) OK, %d bytes", i, m.Bounds, len(data))
		}
	}
}

func TestPrintMonitors(t *testing.T) {
	mons, warns, err := monitors()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		t.Logf("WARN: %s", w)
	}
	for i, m := range mons {
		t.Logf("Monitor %d: Name=%q Primary=%v Bounds=%+v DXGIIndex=%d Rotation=%d",
			i, m.Name, m.Primary, m.Bounds, m.DXGIIndex, m.Rotation)
	}
}

func TestPrintAdapters(t *testing.T) {
	adapters, outputs, err := enumerateDXGI()
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range adapters {
		fmt.Printf("Adapter %d: %q (LUID=0x%x, Software=%v)\n", i, a.Name, a.LUID, a.Software)
	}
	for i, o := range outputs {
		fmt.Printf("Output %d: %q AdapterIdx=%d Index=%d Desktop=%+v Attached=%v Monitor=%x\n",
			i, o.DeviceName, o.AdapterIndex, o.Index, o.Desktop, o.Attached, o.Monitor)
	}
	chosen, _, ok := captureAdapter(adapters)
	fmt.Printf("captureAdapter chose: %+v (ok=%v)\n", chosen, ok)
}

func probeWindowCapture(t *testing.T) {

	needle := os.Getenv("PROBE_TITLE")
	if needle == "" {
		needle = "Discord"
	}
	var target Window
	for _, w := range topLevelWindows() {
		if strings.Contains(w.Title, needle) {
			target = w
			break
		}
	}
	if target.Handle == 0 {
		t.Skipf("no %q window", needle)
	}

	var wr windows.Rect
	if r, _, _ := procGetWindowRect.Call(target.Handle, uintptr(unsafe.Pointer(&wr))); r == 0 {
		t.Fatal("GetWindowRect failed")
	}
	winRect := rectFromWin32(wr)
	dwmRect, err := windowFrame(target.Handle)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Printf("title=%q\n", target.Title)
	fmt.Printf("windowRect=%+v dwmRect=%+v\n", winRect, dwmRect)
	fmt.Printf("delta: left=%d top=%d right=%d bottom=%d\n",
		dwmRect.X-winRect.X,
		dwmRect.Y-winRect.Y,
		(winRect.X+winRect.Width)-(dwmRect.X+dwmRect.Width),
		(winRect.Y+winRect.Height)-(dwmRect.Y+dwmRect.Height),
	)

	save := func(tag string, rect Rect) {
		g, err := newWindowGrabber(target.Handle, rect.X, rect.Y, rect.Width, rect.Height,
			rect.X, rect.Y, rect.Width, rect.Height, rect.Width, rect.Height, false)
		if err != nil {
			t.Errorf("%s: %v", tag, err)
			return
		}
		defer g.close()
		frame, err := g.grab()
		if err != nil {
			t.Errorf("%s: %v", tag, err)
			return
		}
		img := image.NewRGBA(image.Rect(0, 0, rect.Width, rect.Height))
		for y := 0; y < rect.Height; y++ {
			for x := 0; x < rect.Width; x++ {
				off := (y*rect.Width + x) * 4
				img.SetRGBA(x, y, color.RGBA{R: frame[off+2], G: frame[off+1], B: frame[off], A: 255})
			}
		}
		path := filepath.Join(os.TempDir(), "streamy-probe-"+tag+".png")
		f, err := os.Create(path)
		if err != nil {
			t.Errorf("%s: %v", tag, err)
			return
		}
		if err := png.Encode(f, img); err != nil {
			t.Errorf("%s: %v", tag, err)
		}
		_ = f.Close()
		fmt.Printf("wrote %s (%dx%d)\n", path, rect.Width, rect.Height)
	}

	save("dwm", dwmRect)
	save("win", winRect)
}
