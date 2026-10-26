package pipeline

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// blockingErrorReader behaves like the read side of the ffmpeg pipe: it blocks
// until the test releases it, then reports the error a closed *io.PipeReader
// returns.
type blockingErrorReader struct {
	entered chan struct{}
	unblock chan struct{}
}

func (r *blockingErrorReader) Read([]byte) (int, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	<-r.unblock
	return 0, io.ErrClosedPipe
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

// Stop closes the pipe while forwardVideo may be blocked in a read. The
// resulting io.ErrClosedPipe is the normal end of the session and must not be
// reported as a parse failure.
func TestForwardVideoTreatsAReadErrorDuringShutdownAsClean(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	streamer := NewLiveStreamer(logger)
	streamer.done = make(chan struct{})

	input := &blockingErrorReader{entered: make(chan struct{}, 1), unblock: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		streamer.forwardVideo(ctx, input)
	}()

	select {
	case <-input.entered:
	case <-time.After(time.Second):
		t.Fatal("forwardVideo never read from the input")
	}
	cancel()
	close(input.unblock)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("forwardVideo did not stop")
	}
	if strings.Contains(logs.String(), "could not parse ffmpeg H.264 output") {
		t.Errorf("shutdown logged a parse error:\n%s", logs.String())
	}
}

// The same error while the stream is live is a real failure and still has to
// be loud.
func TestForwardVideoReportsAReadErrorWhileLive(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	streamer := NewLiveStreamer(logger)
	streamer.done = make(chan struct{})

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		streamer.forwardVideo(context.Background(), errorReader{err: io.ErrClosedPipe})
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("forwardVideo did not stop")
	}
	if !strings.Contains(logs.String(), "could not parse ffmpeg H.264 output") {
		t.Errorf("live read failure was not logged:\n%s", logs.String())
	}
}
