package capture

import (
	"strings"
	"testing"

	"streamy/internal/config"
)

// These tests cover the target selection rules. They need no desktop, no game
// and no Windows: they feed Decide a hand written Desktop and check the Target
// that comes back, which is exactly the split the package is built around.

func monitor(index, dxgi int, x, y, w, h int, primary bool) Monitor {
	return Monitor{
		Index:     index,
		DXGIIndex: dxgi,
		Bounds:    Rect{X: x, Y: y, Width: w, Height: h},
		WorkArea:  Rect{X: x, Y: y, Width: w, Height: h - 48},
		Primary:   primary,
		Name:      "screen",
	}
}

// desktop returns the three monitor layout that was on the development machine:
// a 1920x1080 screen to the left, a portrait 1080x1920 screen to the right, and
// the primary 1920x1080 in the middle.
func desktop() Desktop {
	return Desktop{
		Monitors: []Monitor{
			monitor(0, 0, -1920, 293, 1920, 1080, false),
			monitor(1, 1, 1920, -232, 1080, 1920, false),
			monitor(2, 2, 0, 0, 1920, 1080, true),
		},
		MouseMonitor: -1,
		SelfPID:      4242,
	}
}

func window(title, class string, pid uint32, r Rect) *Window {
	return &Window{
		Title:   title,
		Class:   class,
		PID:     pid,
		Rect:    r,
		Visible: true,
	}
}

func TestRectCoveredBy(t *testing.T) {
	screen := Rect{Width: 1920, Height: 1080}

	cases := []struct {
		name      string
		other     Rect
		tolerance int
		want      bool
	}{
		{"exact", Rect{Width: 1920, Height: 1080}, 8, true},
		{"larger than the screen", Rect{X: -2, Y: -2, Width: 1924, Height: 1084}, 8, true},
		{"a couple of pixels short", Rect{Width: 1918, Height: 1078}, 8, true},
		{"taskbar still visible", Rect{Width: 1920, Height: 1032}, 8, false},
		{"half the screen", Rect{Width: 960, Height: 1080}, 8, false},
		{"offset by a lot", Rect{X: 400, Width: 1920, Height: 1080}, 8, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := screen.CoveredBy(c.other, c.tolerance); got != c.want {
				t.Errorf("CoveredBy(%v, %d) = %v, want %v", c.other, c.tolerance, got, c.want)
			}
		})
	}
}

func TestDecideFullscreenWindowCapturesItsMonitor(t *testing.T) {
	d := desktop()
	// A game on the third monitor: a different monitor from the primary, so a
	// bug that always picks the primary is visible.
	d.Foreground = window("Some Game", "UnityWndClass", 999, Rect{Width: 1920, Height: 1080})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if target.Kind != KindMonitor {
		t.Errorf("Kind = %v, want monitor", target.Kind)
	}
	if !target.Fullscreen {
		t.Error("Fullscreen = false, want true")
	}
	if target.Monitor.Index != 2 {
		t.Errorf("Monitor.Index = %d, want 2", target.Monitor.Index)
	}
	if target.Monitor.DXGIIndex != 2 {
		t.Errorf("Monitor.DXGIIndex = %d, want 2", target.Monitor.DXGIIndex)
	}
	if target.Reason == "" {
		t.Error("Reason is empty; every decision must explain itself")
	}
}

func TestDecideFullscreenOnSecondaryMonitor(t *testing.T) {
	d := desktop()
	// Fullscreen on the left hand monitor.
	d.Foreground = window("Video", "mpv", 999, Rect{X: -1920, Y: 293, Width: 1920, Height: 1080})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Monitor.Index != 0 || target.Monitor.DXGIIndex != 0 {
		t.Errorf("got monitor %d (dxgi %d), want monitor 0 (dxgi 0)", target.Monitor.Index, target.Monitor.DXGIIndex)
	}
}

func TestDecideMaximisedWindowIsNotFullscreen(t *testing.T) {
	// This is the case FullscreenTolerance exists for. A maximised window is
	// the work area: full width but 48 pixels short of the bottom.
	d := desktop()
	d.Foreground = window("Notepad", "Notepad", 999, Rect{Width: 1920, Height: 1032})

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Fullscreen {
		t.Errorf("a maximised window was treated as fullscreen: %s", target.Reason)
	}
	if target.Kind != KindMonitor || !target.Monitor.Primary {
		t.Errorf("got %s, want the primary monitor", target.Describe())
	}
}

func TestDecideFocusedWindow(t *testing.T) {
	d := desktop()
	d.Foreground = window("Notes - Editor", "Notepad", 999, Rect{X: 100, Y: 100, Width: 800, Height: 600})

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetFocused

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Kind != KindWindow {
		t.Fatalf("Kind = %v, want window", target.Kind)
	}
	if target.Window.Title != "Notes - Editor" {
		t.Errorf("Window.Title = %q", target.Window.Title)
	}
	// The monitor is recorded too, because gdigrab needs it for nothing but
	// logging does.
	if target.Monitor.Index != 2 {
		t.Errorf("Monitor.Index = %d, want 2", target.Monitor.Index)
	}
}

func TestDecideMouseMonitor(t *testing.T) {
	d := desktop()
	d.MouseMonitor = 1
	d.Foreground = window("Notes", "Notepad", 999, Rect{X: 100, Y: 100, Width: 800, Height: 600})

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetMouse

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Kind != KindMonitor || target.Monitor.Index != 1 {
		t.Errorf("got %s, want monitor 1", target.Describe())
	}
	if target.Monitor.DXGIIndex != 1 {
		t.Errorf("DXGIIndex = %d, want 1", target.Monitor.DXGIIndex)
	}
}

func TestDecideExplicitMonitor(t *testing.T) {
	d := desktop()

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetMonitor
	cfg.Capture.MonitorIndex = 1

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Monitor.Index != 1 {
		t.Errorf("Monitor.Index = %d, want 1", target.Monitor.Index)
	}
}

func TestDecideMonitorIndexOutOfRangeFallsBackToPrimary(t *testing.T) {
	d := desktop()

	cfg := config.Default()
	cfg.Capture.DefaultTarget = config.TargetMonitor
	cfg.Capture.MonitorIndex = 7

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !target.Monitor.Primary {
		t.Errorf("got monitor %d, want the primary", target.Monitor.Index)
	}
	if target.Reason == "" {
		t.Error("the fallback must say so in Reason")
	}
}

// TestDecideIgnoresUnusableWindows walks the exclusion rules. Each case is a
// foreground window that must not become the target even when it fills its
// monitor.
func TestDecideIgnoresUnusableWindows(t *testing.T) {
	full := Rect{Width: 1920, Height: 1080}

	cases := []struct {
		name string
		win  *Window
	}{
		{"the desktop itself", window("Program Manager", "Progman", 999, full)},
		{"the wallpaper host", window("", "WorkerW", 999, full)},
		{"the taskbar", window("", "Shell_TrayWnd", 999, full)},
		{"a secondary taskbar", window("", "Shell_SecondaryTrayWnd", 999, full)},
		{"our own tray window", window("Streamy", "StreamyTray", 4242, full)},
		{"an untitled window", window("   ", "SomeClass", 999, full)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := desktop()
			d.Foreground = c.win

			target, err := Decide(d, config.Default())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if target.Fullscreen {
				t.Errorf("%s was treated as a fullscreen app: %s", c.name, target.Reason)
			}
			if !target.Monitor.Primary {
				t.Errorf("%s: got %s, want the primary monitor", c.name, target.Describe())
			}
		})
	}
}

func TestDecideHiddenOrMinimisedForegroundIsIgnored(t *testing.T) {
	cases := []struct {
		name string
		win  *Window
	}{
		{"minimised", &Window{Title: "Game", Class: "Game", PID: 999, Rect: Rect{Width: 1920, Height: 1080}, Visible: true, Minimized: true}},
		{"hidden", &Window{Title: "Game", Class: "Game", PID: 999, Rect: Rect{Width: 1920, Height: 1080}, Visible: false}},
		{"zero sized", &Window{Title: "Game", Class: "Game", PID: 999, Visible: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := desktop()
			d.Foreground = c.win

			target, err := Decide(d, config.Default())
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if target.Fullscreen {
				t.Errorf("%s: treated as fullscreen: %s", c.name, target.Reason)
			}
		})
	}
}

func TestDecideNoForegroundWindow(t *testing.T) {
	d := desktop()
	d.Foreground = nil

	target, err := Decide(d, config.Default())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !target.Monitor.Primary {
		t.Errorf("got %s, want the primary monitor", target.Describe())
	}
}

func TestDecideAlwaysShowPickerIsNoted(t *testing.T) {
	// The picker arrives in phase 6. Until then the setting must not silently
	// do nothing: it says so in the reason, which is logged.
	d := desktop()

	cfg := config.Default()
	cfg.Capture.AlwaysShowPicker = true

	target, err := Decide(d, cfg)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if target.Reason == "" {
		t.Fatal("Reason is empty")
	}
	if !contains(target.Reason, "alwaysShowPicker") {
		t.Errorf("Reason %q does not mention the ignored setting", target.Reason)
	}
}

func TestDecideNoMonitors(t *testing.T) {
	if _, err := Decide(Desktop{}, config.Default()); err == nil {
		t.Fatal("Decide with no monitors returned no error")
	}
}

func TestMonitorAtPoint(t *testing.T) {
	d := desktop()

	cases := []struct {
		name string
		x, y int
		want int
	}{
		{"primary", 100, 100, 2},
		{"left hand monitor", -1800, 400, 0},
		{"portrait monitor", 2000, 100, 1},
		// A point below the left hand monitor is in the gap between screens,
		// so the nearest monitor wins.
		{"in the gap", -1800, 2000, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := monitorAtPoint(d.Monitors, c.x, c.y); got != c.want {
				t.Errorf("monitorAtPoint(%d, %d) = %d, want %d", c.x, c.y, got, c.want)
			}
		})
	}
}

func TestChooseGrabber(t *testing.T) {
	mon := Target{Kind: KindMonitor}
	win := Target{Kind: KindWindow, Window: Window{Title: "x"}}

	withDDA := Capabilities{DDAGrab: true}
	withoutDDA := Capabilities{DDAGrab: false}

	if got := mon.ChooseGrabber(withDDA); got != GrabberDDAGrab {
		t.Errorf("monitor with ddagrab available: %q, want %q", got, GrabberDDAGrab)
	}
	if got := mon.ChooseGrabber(withoutDDA); got != GrabberGDigrab {
		t.Errorf("monitor without ddagrab: %q, want %q", got, GrabberGDigrab)
	}
	// ddagrab cannot target a single window, whatever the capabilities say.
	if got := win.ChooseGrabber(withDDA); got != GrabberGDigrab {
		t.Errorf("window: %q, want %q", got, GrabberGDigrab)
	}
}

func TestInputSpecDDAGrab(t *testing.T) {
	target := Target{
		Kind:    KindMonitor,
		Monitor: Monitor{Index: 1, DXGIIndex: 4, Bounds: Rect{Width: 2560, Height: 1440}, Rotation: 90},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab, FPS: 60, DrawMouse: true, Scale: "1280x720"})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	if !contains(join(spec.Args), "ddagrab=output_idx=4:framerate=60:draw_mouse=1") {
		t.Errorf("args %v do not select dxgi output 4 at 60fps with the cursor", spec.Args)
	}
	if !contains(join(spec.Args), "d3d11va") {
		t.Errorf("args %v do not create the D3D11 device that hwdownload needs", spec.Args)
	}
	// A rotated monitor must be transposed back, the frames downloaded, and the
	// scale applied, in that order.
	for _, want := range []string{"hwdownload", "format=bgra", "transpose=1", "scale=1280x720"} {
		if !contains(spec.Filters, want) {
			t.Errorf("filters %q do not contain %q", spec.Filters, want)
		}
	}
	if index(spec.Filters, "hwdownload") > index(spec.Filters, "transpose") {
		t.Errorf("filters %q transpose before downloading, which is impossible", spec.Filters)
	}
}

func TestInputSpecGDigrabMonitor(t *testing.T) {
	target := Target{
		Kind:    KindMonitor,
		Monitor: Monitor{Index: 1, Bounds: Rect{X: -1920, Y: 293, Width: 1920, Height: 1080}},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab, FPS: 30, DrawMouse: false})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	args := join(spec.Args)
	for _, want := range []string{"-f gdigrab", "-offset_x -1920", "-offset_y 293", "-video_size 1920x1080", "-i desktop", "-draw_mouse 0"} {
		if !contains(args, want) {
			t.Errorf("args %q do not contain %q", args, want)
		}
	}
	// GDI already sees the desktop the way the user does, so there is no
	// transpose even on a rotated monitor.
	if contains(spec.Filters, "transpose") {
		t.Errorf("filters %q should not transpose a GDI capture", spec.Filters)
	}
}

func TestInputSpecGDigrabWindow(t *testing.T) {
	target := Target{
		Kind:   KindWindow,
		Window: Window{Title: `Notes: C:\tmp`, Class: "Notepad"},
	}

	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab, FPS: 30, DrawMouse: true})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}

	if len(spec.Args) == 0 || spec.Args[len(spec.Args)-1] != `title=Notes\: C\:\\tmp` {
		t.Errorf("last arg = %q, want the window title with the syntax characters escaped", spec.Args[len(spec.Args)-1])
	}
}

func TestInputSpecDDAGrabRejectsWindow(t *testing.T) {
	// Asking for a window with ddagrab is a programming error, not a user
	// error, so it must be caught rather than quietly capturing the monitor.
	target := Target{Kind: KindWindow, Window: Window{Title: "x"}}
	if _, err := target.InputSpec(InputOptions{Grabber: GrabberDDAGrab}); err == nil {
		t.Fatal("ddagrab accepted a window target")
	}
}

func TestInputSpecWindowWithoutTitle(t *testing.T) {
	target := Target{Kind: KindWindow, Window: Window{}}
	if _, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab}); err == nil {
		t.Fatal("a window with no title was accepted; gdigrab would match the wrong window")
	}
}

func TestInputSpecUnknownGrabber(t *testing.T) {
	if _, err := (Target{Kind: KindMonitor}).InputSpec(InputOptions{Grabber: "magic"}); err == nil {
		t.Fatal("an unknown grabber was accepted")
	}
}

func TestInputSpecDefaultsFPS(t *testing.T) {
	target := Target{Kind: KindMonitor, Monitor: Monitor{Bounds: Rect{Width: 100, Height: 100}}}
	spec, err := target.InputSpec(InputOptions{Grabber: GrabberGDigrab})
	if err != nil {
		t.Fatalf("InputSpec: %v", err)
	}
	if !contains(join(spec.Args), "-framerate 30") {
		t.Errorf("args %q do not contain a default framerate", spec.Args)
	}
}

func TestScaleFiltersKeepsDimensionsEven(t *testing.T) {
	// H.264 with 4:2:0 chroma needs even dimensions, and users type odd ones.
	got := scaleFilters("1921x1081")
	if len(got) != 2 {
		t.Fatalf("scaleFilters = %v, want a scale plus an even-dimension correction", got)
	}
	if !contains(got[1], "trunc(iw/2)*2") {
		t.Errorf("second filter = %q, want the trunc correction", got[1])
	}
	if len(scaleFilters("")) != 0 {
		t.Error("an empty scale should add no filters at all")
	}
}

func TestRotationFilters(t *testing.T) {
	cases := []struct {
		degrees int
		want    []string
	}{
		{0, nil},
		{90, []string{"transpose=1"}},
		{180, []string{"transpose=1", "transpose=1"}},
		{270, []string{"transpose=2"}},
		{45, nil}, // not a real rotation, must not blow up
	}

	for _, c := range cases {
		got := rotationFilters(c.degrees)
		if len(got) != len(c.want) {
			t.Errorf("rotationFilters(%d) = %v, want %v", c.degrees, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("rotationFilters(%d) = %v, want %v", c.degrees, got, c.want)
				break
			}
		}
	}
}

func TestEscapeFFmpegValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a:b", `a\:b`},
		{`C:\tmp`, `C\:\\tmp`},
		{"a,b", `a\,b`},
		{"100%", `100\%`},
	}
	for _, c := range cases {
		if got := escapeFFmpegValue(c.in); got != c.want {
			t.Errorf("escapeFFmpegValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTargetDescribe(t *testing.T) {
	mon := Target{Kind: KindMonitor, Monitor: Monitor{Index: 2, DXGIIndex: 3, Bounds: Rect{Width: 1920, Height: 1080}}}
	if got := mon.Describe(); !contains(got, "monitor 2") || !contains(got, "dxgi output 3") {
		t.Errorf("monitor Describe = %q", got)
	}

	win := Target{Kind: KindWindow, Window: Window{
		Title: "Quarterly layoffs - Mail", Class: "Notepad", PID: 4321,
		Rect: Rect{Width: 800, Height: 600},
	}}
	got := win.Describe()
	if !contains(got, "Notepad") || !contains(got, "4321") {
		t.Errorf("window Describe = %q, want the class and the pid", got)
	}
	if contains(got, "layoffs") {
		t.Errorf("window Describe = %q, which leaks the window title into the log", got)
	}
}

// Nothing the user might read later may contain a window title. Describe and
// Reason end up in streamy.log, in a tray notification and in bug reports, so
// this is checked for both ways a window can be chosen.
func TestDiagnosticsNeverContainAWindowTitle(t *testing.T) {
	const secret = "divorce-papers-DRAFT-final-v2.docx"

	cfgFor := func(defaultTarget string) config.Config {
		cfg := config.Default()
		cfg.Capture.DefaultTarget = defaultTarget
		return cfg
	}

	t.Run("a fullscreen window", func(t *testing.T) {
		full := Rect{Width: 1920, Height: 1080}
		d := Desktop{
			Monitors:   []Monitor{{Index: 0, Bounds: full, Primary: true, DXGIIndex: 0}},
			Foreground: window(secret, "Notepad", 4321, full),
			SelfPID:    1,
		}

		target, err := Decide(d, cfgFor(config.TargetPrimary))
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if contains(target.Reason, secret) {
			t.Errorf("Reason leaks the title: %q", target.Reason)
		}
		if contains(target.Describe(), secret) {
			t.Errorf("Describe leaks the title: %q", target.Describe())
		}
		if !contains(target.Reason, "Notepad") {
			t.Errorf("Reason = %q, want it to identify the window by class", target.Reason)
		}
	})

	t.Run("the focused window", func(t *testing.T) {
		// Deliberately not fullscreen, so the fallback target is the one that
		// picks it up.
		d := desktop()
		d.Foreground = window(secret, "Notepad", 4321, Rect{X: 100, Y: 100, Width: 800, Height: 600})

		target, err := Decide(d, cfgFor(config.TargetFocused))
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if target.Kind != KindWindow {
			t.Fatalf("Kind = %v, want window", target.Kind)
		}
		// The target must still keep the title: gdigrab needs it to find the
		// window. It is only the human-readable strings that must not have it.
		if target.Window.Title != secret {
			t.Error("the target lost the title, so gdigrab cannot find the window")
		}
		if contains(target.Reason, secret) {
			t.Errorf("Reason leaks the title: %q", target.Reason)
		}
		if contains(target.Describe(), secret) {
			t.Errorf("Describe leaks the title: %q", target.Describe())
		}
		if !contains(target.Describe(), "4321") {
			t.Errorf("Describe = %q, want it to name the owning process", target.Describe())
		}
	})
}

// containsStrings is here so the assertions above read like sentences.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func index(haystack, needle string) int { return strings.Index(haystack, needle) }

func join(parts []string) string { return strings.Join(parts, " ") }
