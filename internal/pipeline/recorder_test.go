package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"streamy/internal/capture"
)

// fileURL is what goes on the clipboard, so it has to be a URL that actually
// opens. Windows paths are the awkward case: a bare C:\... is not a URL, and
// anything reading it as one sees a scheme called "c".
func TestFileURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{
			name: "drive path",
			path: `C:\Users\me\Videos\Streamy\streamy-2026-01-02_03-04-05.mp4`,
			want: "file:///C:/Users/me/Videos/Streamy/streamy-2026-01-02_03-04-05.mp4",
		},
		{
			name: "spaces are escaped",
			path: `C:\Users\me\My Videos\a b.mp4`,
			want: "file:///C:/Users/me/My%20Videos/a%20b.mp4",
		},
		{
			name: "already slashed",
			path: "/home/me/videos/a.mp4",
			want: "file:///home/me/videos/a.mp4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileURL(tc.path); got != tc.want {
				t.Errorf("fileURL(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestRecordingPath(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)

	t.Run("names the file from the clock", func(t *testing.T) {
		dir := t.TempDir()
		r := NewRecorder(Options{OutputDir: dir})

		path, err := r.recordingPath(when)
		if err != nil {
			t.Fatalf("recordingPath: %v", err)
		}
		if got, want := filepath.Base(path), "streamy-2026-01-02_03-04-05.mp4"; got != want {
			t.Errorf("file name = %q, want %q", got, want)
		}
		if got, want := filepath.Dir(path), dir; got != want {
			t.Errorf("directory = %q, want %q", got, want)
		}
	})

	t.Run("the format picks the extension", func(t *testing.T) {
		for _, tc := range []struct{ format, ext string }{
			{"", "mp4"},
			{"mp4", "mp4"},
			{"mp4-frag", "mp4"}, // fragmented mp4 is still an mp4
			{"mkv", "mkv"},
			{"h264", "h264"},
		} {
			r := NewRecorder(Options{OutputDir: t.TempDir(), Format: tc.format})
			path, err := r.recordingPath(when)
			if err != nil {
				t.Fatalf("format %q: %v", tc.format, err)
			}
			if got := filepath.Ext(path); got != "."+tc.ext {
				t.Errorf("format %q gave a %q file, want .%s", tc.format, got, tc.ext)
			}
		}
	})

	t.Run("creates the folder", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "Streamy", "nested")
		r := NewRecorder(Options{OutputDir: dir})

		path, err := r.recordingPath(when)
		if err != nil {
			t.Fatalf("recordingPath: %v", err)
		}
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("the recordings folder was not created: %v", err)
		}
		if !info.IsDir() {
			t.Error("the recordings folder is not a directory")
		}
	})

	t.Run("defaults to Videos", func(t *testing.T) {
		// A recorder with no OutputDir must find somewhere sensible, not fail
		// and not write next to the config file.
		r := NewRecorder(Options{})
		path, err := r.recordingPath(when)
		if err != nil {
			t.Fatalf("recordingPath with no output directory: %v", err)
		}
		if !strings.Contains(filepath.ToSlash(path), "/Videos/Streamy/") {
			t.Errorf("path %q is not inside the Videos folder", path)
		}
		// The test just created a directory in the user's Videos folder; take
		// it away again rather than leaving litter behind.
		_ = os.Remove(filepath.Dir(path))
	})
}

func TestSameTarget(t *testing.T) {
	window := func(handle uintptr) capture.Target {
		return capture.Target{Kind: capture.KindWindow, Window: capture.Window{Handle: handle, Title: "changing"}}
	}
	monitor := func(index int) capture.Target {
		return capture.Target{Kind: capture.KindMonitor, Monitor: capture.Monitor{Index: index}}
	}

	for _, tc := range []struct {
		name string
		a, b capture.Target
		want bool
	}{
		{"the same monitor", monitor(1), monitor(1), true},
		{"a different monitor", monitor(1), monitor(2), false},
		{"the same window", window(10), window(10), true},
		{"a different window", window(10), window(11), false},
		{"a monitor and a window", monitor(1), window(1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameTarget(tc.a, tc.b); got != tc.want {
				t.Errorf("sameTarget = %t, want %t", got, tc.want)
			}
		})
	}

	// A retitled window is the same window. Comparing titles would restart the
	// capture every time a browser changed its tab.
	t.Run("a retitled window does not count as a change", func(t *testing.T) {
		a := window(10)
		a.Window.Title = "before"
		b := window(10)
		b.Window.Title = "after"
		if !sameTarget(a, b) {
			t.Error("a window that changed its title was treated as a different target")
		}
	})

	// A window that has been torn down and had its handle reused is not
	// something we can detect, but at least the handle must be compared.
	t.Run("a window handle is compared", func(t *testing.T) {
		if sameTarget(window(10), window(0)) {
			t.Error("windows with different handles compared equal")
		}
	})
}

// Stop has to be safe when nothing is running: the app calls it on shutdown and
// after a failed start, when there is no ffmpeg process to stop.
func TestStopWhenNothingIsRunning(t *testing.T) {
	r := NewRecorder(Options{OutputDir: t.TempDir()})
	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("Stop with nothing running: %v", err)
	}
	if got := r.Session(); got != (Session{}) {
		t.Errorf("Session() = %+v, want the zero session", got)
	}
}

// A nil logger must be replaced, not dereferenced.
func TestNewRecorderWithoutALogger(t *testing.T) {
	r := NewRecorder(Options{OutputDir: t.TempDir()})
	if r.logger == nil {
		t.Fatal("NewRecorder left the logger nil")
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// The stub exists so that a hotkey notification means "the hotkey worked"
// rather than "the stream worked". It must therefore fail loudly.
func TestStubRefusesToStart(t *testing.T) {
	stub := NewStub(nil)

	session, err := stub.Start(context.Background(), Request{})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Start error = %v, want %v", err, ErrNotImplemented)
	}
	if session != (Session{}) {
		t.Errorf("Start returned %+v, want the zero session so nothing looks live", session)
	}
	if err := stub.Stop(context.Background()); err != nil {
		t.Errorf("Stop = %v, want nil", err)
	}
}

// A Recorder is a Streamer. This is checked here rather than in the app so that
// breaking the interface fails in the package that has to satisfy it.
func TestRecorderIsAStreamer(t *testing.T) {
	var _ Streamer = NewRecorder(Options{})
	var _ Streamer = NewStub(nil)
}
