package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"streamy/internal/config"
)

// fakeStreamer records what the app asked it to do, so the tests can check the
// lifecycle without ffmpeg or a network. Implementing an interface is all it
// takes to stand in for the real pipeline — that is the payoff for defining
// Streamer as an interface back in phase 1.
type fakeStreamer struct {
	mu       sync.Mutex
	url      string
	startErr error
	stopErr  error
	starts   int
	stops    int
	lastReq  StartRequest
}

func (f *fakeStreamer) Start(_ context.Context, req StartRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.lastReq = req
	if f.startErr != nil {
		return "", f.startErr
	}
	return f.url, nil
}

func (f *fakeStreamer) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return f.stopErr
}

func (f *fakeStreamer) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// newTestApp builds a headless app with a substituted clipboard so running the
// tests never touches the developer's real clipboard.
func newTestApp(t *testing.T, streamer Streamer) (*App, *[]string) {
	t.Helper()

	var copied []string
	var mu sync.Mutex

	a, err := New(Options{
		Config:   config.Default(),
		Streamer: streamer,
		Headless: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.copyToClipboard = func(text string) error {
		mu.Lock()
		defer mu.Unlock()
		copied = append(copied, text)
		return nil
	}
	return a, &copied
}

func TestToggleSharingStartAndStop(t *testing.T) {
	streamer := &fakeStreamer{url: "https://viewer.example/?room=abc"}
	a, copied := newTestApp(t, streamer)

	if got := a.Status(); got.Sharing || got.Text != StateIdle.String() {
		t.Fatalf("initial status = %+v, want idle", got)
	}

	a.ToggleSharing()

	status := a.Status()
	if !status.Sharing {
		t.Fatalf("status after start = %+v, want sharing", status)
	}
	if status.ViewerURL != streamer.url {
		t.Errorf("viewer url = %q, want %q", status.ViewerURL, streamer.url)
	}
	if len(*copied) != 1 || (*copied)[0] != streamer.url {
		t.Errorf("clipboard = %v, want [%q]", *copied, streamer.url)
	}

	a.ToggleSharing()

	status = a.Status()
	if status.Sharing || status.ViewerURL != "" {
		t.Errorf("status after stop = %+v, want idle without a link", status)
	}

	starts, stops := streamer.counts()
	if starts != 1 || stops != 1 {
		t.Errorf("starts/stops = %d/%d, want 1/1", starts, stops)
	}
}

// Phase 1 ships the stub, which refuses to start. The app must stay idle and not
// pretend a stream is running.
func TestNotImplementedStreamerLeavesAppIdle(t *testing.T) {
	streamer := &fakeStreamer{startErr: ErrNotImplemented}
	a, copied := newTestApp(t, streamer)

	a.ToggleSharing()

	status := a.Status()
	if status.Sharing {
		t.Errorf("status = %+v, want idle after a failed start", status)
	}
	if status.ViewerURL != "" {
		t.Errorf("viewer url = %q, want empty", status.ViewerURL)
	}
	if len(*copied) != 0 {
		t.Errorf("clipboard = %v, want nothing copied", *copied)
	}
}

func TestStartFailureIsReportedAndRecoverable(t *testing.T) {
	streamer := &fakeStreamer{startErr: errors.New("ffmpeg not found")}
	a, _ := newTestApp(t, streamer)

	a.ToggleSharing()
	if got := a.Status(); got.Sharing {
		t.Fatalf("status = %+v, want idle after a failed start", got)
	}

	// A later attempt must be allowed: the failure did not wedge the state.
	streamer.mu.Lock()
	streamer.startErr = nil
	streamer.url = "https://viewer.example/?room=retry"
	streamer.mu.Unlock()

	a.ToggleSharing()
	if got := a.Status(); !got.Sharing {
		t.Errorf("status = %+v, want sharing on the retry", got)
	}
}

func TestCopyViewerLinkWithoutStream(t *testing.T) {
	streamer := &fakeStreamer{url: "https://viewer.example/?room=abc"}
	a, copied := newTestApp(t, streamer)

	a.CopyViewerLink()
	if len(*copied) != 0 {
		t.Errorf("clipboard = %v, want nothing copied while idle", *copied)
	}
}

func TestQuitIsIdempotent(t *testing.T) {
	a, _ := newTestApp(t, &fakeStreamer{})

	done := make(chan struct{})
	go func() {
		a.Quit()
		a.Quit() // must not panic on a double close
		close(done)
	}()
	<-done

	select {
	case <-a.quitCh:
	default:
		t.Error("quitCh was not closed by Quit()")
	}
}

func TestShutdownStopsAnActiveStream(t *testing.T) {
	streamer := &fakeStreamer{url: "https://viewer.example/?room=abc"}
	a, _ := newTestApp(t, streamer)

	a.ToggleSharing()
	if !a.Status().Sharing {
		t.Fatal("expected to be sharing before shutdown")
	}

	if err := a.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	_, stops := streamer.counts()
	if stops != 1 {
		t.Errorf("stops = %d, want 1", stops)
	}
	if got := a.Status(); got.Sharing {
		t.Errorf("status after shutdown = %+v, want idle", got)
	}
}

func TestStateString(t *testing.T) {
	// Guards the strings the tray shows; if one changes, the test says so.
	if got := StateIdle.String(); got != "Not sharing" {
		t.Errorf("StateIdle = %q, want \"Not sharing\"", got)
	}
	if got := StateStreaming.String(); got != "Sharing" {
		t.Errorf("StateStreaming = %q, want \"Sharing\"", got)
	}
}

// TestRunShutsDownOnContextCancel walks the whole headless lifecycle: onReady
// runs, the hotkey is registered, the context is cancelled and everything is
// released again. This is the phase 1 "clean shutdown" requirement in test form.
func TestRunShutsDownOnContextCancel(t *testing.T) {
	streamer := &fakeStreamer{url: "https://viewer.example/?room=abc"}
	a, err := New(Options{
		Config:          config.Default(), // hotkey enabled
		Streamer:        streamer,
		Headless:        true,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.copyToClipboard = func(string) error { return nil }

	// Pretend the user already pressed the hotkey once.
	a.ToggleSharing()
	if !a.Status().Sharing {
		t.Fatal("expected to be sharing before Run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Give onReady a moment to register the hotkey, then ask the app to stop.
	// (Cancelling before onReady would race shutdown against startup; the app
	// is only reachable through a signal or the tray in real life, so that race
	// cannot happen in practice.)
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	starts, stops := streamer.counts()
	if starts != 1 || stops != 1 {
		t.Errorf("starts/stops = %d/%d, want 1/1", starts, stops)
	}
	if got := a.hotkeys.Spec(); got != "" {
		t.Errorf("hotkey %q is still registered after shutdown", got)
	}
	if got := a.Status(); got.Sharing {
		t.Errorf("status after shutdown = %+v, want idle", got)
	}
}
