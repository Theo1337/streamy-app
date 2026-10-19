package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/encode"
)

// startupGrace is how long ffmpeg is given to prove it is alive.
//
// ffmpeg reports an unusable encoder, a missing capture device or a bad option
// by exiting within a few hundred milliseconds. Waiting before declaring
// success is the difference between "recording started" being true and being
// merely optimistic.
const startupGrace = 900 * time.Millisecond

// switchPollInterval is how often the auto switch looks for a changed target.
const switchPollInterval = 2 * time.Second

// Recorder records the resolved capture target to a file.
//
// It is the phase 2 pipeline. Its purpose is to prove the whole chain end to
// end — target resolution, ffmpeg arguments, encoder selection, the
// subprocess, and a shutdown that leaves a playable file — before WebRTC is
// layered on top. It can also hand the encoded stream to a writer instead of a
// file, which is the shape phase 3 needs and what the debug tool's -stdout flag
// exercises.
type Recorder struct {
	opts   Options
	logger *slog.Logger

	// Setup is done once, on the first Start, because finding ffmpeg and
	// probing encoders costs about a second and most runs of the tray process
	// never share anything at all.
	ffmpegOnce sync.Once
	ffmpeg     encode.FFmpeg
	ffmpegErr  error

	capsOnce sync.Once
	caps     capture.Capabilities

	resolverOnce sync.Once
	resolver     capture.TargetResolver

	// mu guards everything below.
	mu        sync.Mutex
	runner    *encode.Runner
	session   Session
	target    capture.Target
	encoder   encode.Encoder
	cancelRun context.CancelFunc
	watcher   chan struct{}
	pumpDone  chan struct{}
}

// Options configures a Recorder.
type Options struct {
	// Logger may be nil.
	Logger *slog.Logger

	// OutputDir is where recordings are written. Empty means the Streamy folder
	// inside the user's Videos directory.
	OutputDir string

	// Format is the container to write: "mp4", "mkv", "mp4-frag" or "h264".
	// Empty means "mp4", or "h264" when Stdout is set.
	Format string

	// Stdout, when set, receives the encoded stream instead of a file, and the
	// recorder writes nothing to disk.
	Stdout io.Writer
}

// NewRecorder returns a Recorder.
func NewRecorder(opts Options) *Recorder {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{opts: opts, logger: logger}
}

// Start resolves the target, starts ffmpeg and returns the session.
func (r *Recorder) Start(ctx context.Context, req Request) (Session, error) {
	r.mu.Lock()
	if r.runner != nil {
		current := r.session
		r.mu.Unlock()
		return current, fmt.Errorf("already recording to %s", current.LocalRecording)
	}
	r.mu.Unlock()

	// Everything up to here runs inside the caller's context, which bounds how
	// long starting may take.
	ffmpeg, err := r.ensureFFmpeg(ctx, req.Config)
	if err != nil {
		return Session{}, err
	}

	encoder, err := encode.Select(ctx, ffmpeg, req.Config, r.logger)
	if err != nil {
		return Session{}, err
	}

	// The context given to Start only bounds the start-up work: the caller
	// cancels it as soon as Start returns. The capture itself outlives it, so
	// the runner gets its own context, derived from the caller's values but not
	// from its cancellation, and Stop cancels that.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))

	session, err := r.startCapture(runCtx, req.Config, ffmpeg, encoder, true)
	if err != nil {
		cancelRun()
		return Session{}, err
	}

	r.mu.Lock()
	r.cancelRun = cancelRun
	r.mu.Unlock()

	if req.Config.Capture.AutoSwitchTarget {
		// Off by default, because switching targets restarts the encoder and
		// therefore starts a new file.
		r.startWatcher(runCtx, req.Config, ffmpeg, encoder)
	}

	return session, nil
}

// startCapture resolves a target and brings one ffmpeg process up for it.
//
// Start and the auto switch both use it, which is why it does not touch the
// shared cancel function or the watcher.
func (r *Recorder) startCapture(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder, announce bool) (Session, error) {
	capabilities := r.capabilities(ctx, ffmpeg, cfg)

	target, err := r.targetResolver().Resolve(ctx, cfg)
	if err != nil {
		return Session{}, err
	}
	grabber := target.ChooseGrabber(capabilities)

	// Requirement 8 from the spec: always say which target was chosen and why.
	// This line is the first thing to read when the wrong screen got recorded.
	r.logger.Info("capture target chosen",
		"target", target.Describe(),
		"reason", target.Reason,
		"grabber", string(grabber),
		"encoder", encoder.Name,
	)

	output, err := r.output()
	if err != nil {
		return Session{}, err
	}

	runner, err := encode.Start(ctx, encode.StartOptions{
		FFmpeg:       ffmpeg,
		Config:       cfg,
		Target:       target,
		Encoder:      encoder,
		Capabilities: capabilities,
		Output:       output,
		Logger:       r.logger,
	})
	if err != nil {
		return Session{}, err
	}

	if err := runner.WaitForStart(startupGrace); err != nil {
		// It is already dead: tidy up and report why, rather than leaving a
		// corpse behind and telling the user they are recording.
		_ = runner.Stop(context.Background())
		return Session{}, fmt.Errorf("ffmpeg could not capture %s: %w", target.Describe(), err)
	}

	session := Session{}
	if output.Pipe {
		session.Label = "Streaming"
		session.Description = fmt.Sprintf("Encoding %s with %s via %s to stdout",
			target.Describe(), encoder.Name, grabber)
	} else {
		// A file:// URL is what goes on the clipboard. It is a real link to a
		// real file, so the tray's "copy viewer link" does something honest
		// even before the viewer page exists.
		session.Label = "Recording"
		session.LocalRecording = output.Path
		session.ViewerURL = fileURL(output.Path)
		session.Description = fmt.Sprintf("Recording %s with %s via %s to %s",
			target.Describe(), encoder.Name, grabber, output.Path)
	}

	var pump chan struct{}
	if output.Pipe && r.opts.Stdout != nil {
		// The phase 3 shape: the encoded stream is handed to a reader instead
		// of being written to disk.
		pump = make(chan struct{})
		go func() {
			defer close(pump)
			if _, err := io.Copy(r.opts.Stdout, runner.Stdout()); err != nil && !errors.Is(err, io.EOF) {
				r.logger.Debug("copying the encoded stream to the writer", "error", err)
			}
		}()
	}

	r.mu.Lock()
	r.runner = runner
	r.target = target
	r.encoder = encoder
	r.session = session
	r.pumpDone = pump
	r.mu.Unlock()

	if announce {
		r.logger.Info("capture started", "target", target.Describe(), "encoder", encoder.Name)
	}
	return session, nil
}

// ensureFFmpeg finds ffmpeg once and remembers the answer, including a failure:
// if ffmpeg is not installed, retrying on every hotkey press would only make
// each press slower.
func (r *Recorder) ensureFFmpeg(ctx context.Context, cfg config.Config) (encode.FFmpeg, error) {
	r.ffmpegOnce.Do(func() {
		r.ffmpeg, r.ffmpegErr = encode.Find(ctx, cfg.Encoding.FFmpegPath)
		if r.ffmpegErr == nil {
			r.logger.Info("using ffmpeg", "path", r.ffmpeg.Path, "version", r.ffmpeg.Version())
		}
	})
	return r.ffmpeg, r.ffmpegErr
}

// capabilities probes what this machine can do, once per process.
func (r *Recorder) capabilities(ctx context.Context, ffmpeg encode.FFmpeg, cfg config.Config) capture.Capabilities {
	r.capsOnce.Do(func() {
		if err := ffmpeg.ProbeDDAGrab(ctx); err != nil {
			// Not fatal. It only means fullscreen captures fall back to GDI,
			// which is slower and can come out black for exclusive fullscreen
			// games, so it is worth saying out loud.
			r.logger.Warn("Desktop Duplication (ddagrab) is unavailable, falling back to GDI capture; exclusive fullscreen games may record as a black screen",
				"reason", err)
			r.caps = capture.Capabilities{DDAGrab: false}
			return
		}
		r.logger.Debug("Desktop Duplication (ddagrab) is available")
		r.caps = capture.Capabilities{DDAGrab: true}
	})
	return r.caps
}

func (r *Recorder) targetResolver() capture.TargetResolver {
	r.resolverOnce.Do(func() {
		r.resolver = capture.NewResolver(r.logger)
	})
	return r.resolver
}

// output returns where the encoded stream should go.
func (r *Recorder) output() (encode.Output, error) {
	if r.opts.Stdout != nil {
		return encode.Output{Format: r.opts.Format, Pipe: true}, nil
	}

	path, err := r.recordingPath(time.Now())
	if err != nil {
		return encode.Output{}, err
	}
	return encode.Output{Path: path, Format: r.opts.Format}, nil
}

// recordingPath builds a fresh path for a recording.
//
// Recordings are user content, so they go in the Videos folder rather than in
// the config directory next to the log. The timestamp is local time and sorts
// in order, which makes a folder of recordings easy to scan.
func (r *Recorder) recordingPath(now time.Time) (string, error) {
	format := r.opts.Format
	if format == "" {
		format = "mp4"
	}

	dir := r.opts.OutputDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory to put recordings in: %w", err)
		}
		dir = filepath.Join(home, "Videos", "Streamy")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create the recordings folder: %w", err)
	}

	ext := "mp4"
	switch format {
	case "mkv":
		ext = "mkv"
	case "h264":
		ext = "h264"
	}

	name := "streamy-" + now.Format("2006-01-02_15-04-05") + "." + ext
	return filepath.Join(dir, name), nil
}

// Stop stops the capture and waits for ffmpeg to finish writing.
func (r *Recorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	runner := r.runner
	cancel := r.cancelRun
	watcher := r.watcher
	pump := r.pumpDone
	session := r.session
	r.runner = nil
	r.cancelRun = nil
	r.watcher = nil
	r.pumpDone = nil
	r.session = Session{}
	r.mu.Unlock()

	if runner == nil {
		return nil
	}

	// Stop the watcher first, so it cannot start a new capture while this one
	// is being torn down. Cancelling the run context is what makes it exit.
	if cancel != nil {
		cancel()
	}
	if watcher != nil {
		select {
		case <-watcher:
		case <-ctx.Done():
		}
	}

	// runner.Stop asks ffmpeg to quit with "q", which is what makes it write
	// the index and leave a playable file behind.
	err := runner.Stop(ctx)

	if pump != nil {
		// Wait for the last bytes to reach the writer before reporting done.
		select {
		case <-pump:
		case <-ctx.Done():
		}
	}

	if session.LocalRecording != "" {
		r.logger.Info("recording finished", "file", session.LocalRecording)
	}
	return err
}

// Session returns the running session, if any.
func (r *Recorder) Session() Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// startWatcher polls for a changed target while the capture runs.
//
// Only started when capture.autoSwitchTarget is on. Switching means stopping
// one ffmpeg and starting another, which is why each target gets its own file
// and why this is off by default.
func (r *Recorder) startWatcher(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder) {
	done := make(chan struct{})

	r.mu.Lock()
	r.watcher = done
	current := r.target
	r.mu.Unlock()

	go func() {
		defer close(done)

		ticker := time.NewTicker(switchPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			target, err := r.targetResolver().Resolve(ctx, cfg)
			if err != nil {
				r.logger.Debug("auto switch: could not resolve a target", "error", err)
				continue
			}
			if sameTarget(target, current) {
				continue
			}

			r.logger.Info("auto switching capture target",
				"from", current.Describe(), "to", target.Describe(), "reason", target.Reason)

			if err := r.switchTarget(ctx, cfg, ffmpeg, encoder); err != nil {
				// A failed switch has already stopped the old capture, so it is
				// reported loudly and the watcher gives up rather than
				// thrashing.
				r.logger.Error("auto switch failed", "error", err)
				return
			}
			current = target
		}
	}()
}

// switchTarget stops the current capture and starts one for the new target.
func (r *Recorder) switchTarget(ctx context.Context, cfg config.Config, ffmpeg encode.FFmpeg, encoder encode.Encoder) error {
	r.mu.Lock()
	old := r.runner
	r.runner = nil
	previous := r.session.LocalRecording
	r.mu.Unlock()

	if old != nil {
		if err := old.Stop(ctx); err != nil {
			r.logger.Warn("stopping the previous capture", "error", err)
		}
	}

	session, err := r.startCapture(ctx, cfg, ffmpeg, encoder, false)
	if err != nil {
		return fmt.Errorf("previous recording is at %s: %w", previous, err)
	}
	r.logger.Info("auto switch finished", "file", session.LocalRecording)
	return nil
}

// sameTarget reports whether two resolutions would capture the same pixels.
func sameTarget(a, b capture.Target) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == capture.KindWindow {
		// Window titles change as applications update them, so the handle is
		// the stable identity.
		return a.Window.Handle == b.Window.Handle
	}
	return a.Monitor.Index == b.Monitor.Index
}

// fileURL turns a filesystem path into a file:// URL.
//
// Windows paths need the extra leading slash: C:\Users\me\a.mp4 has to become
// file:///C:/Users/me/a.mp4 for anything to open it.
func fileURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}
