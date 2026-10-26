package encode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/executil"
)

// Runner is a running ffmpeg process.
//
// Its job is to make stopping safe. ffmpeg finalises the output file when it
// exits normally, so a recording written by a process that was killed is a file
// with no index and nothing that can play it. ffmpeg reads a "q" from stdin as
// "stop now, cleanly", and that works on Windows even when stdin is a pipe,
// which is what this runner relies on.
type Runner struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer

	// Args is the command line ffmpeg was started with, for logging.
	Args []string

	done     chan struct{}
	waitErr  error
	stopOnce sync.Once
	cancel   context.CancelFunc

	// gracePeriod is how long ffmpeg gets to finish writing after being asked
	// to quit, before it is killed.
	gracePeriod time.Duration

	log Logger
}

// StartOptions is everything Start needs.
type StartOptions struct {
	// FFmpeg is the binary to run.
	FFmpeg FFmpeg
	// Config supplies the encoder settings and the log level.
	Config config.Config
	// Target is what to record.
	Target capture.Target
	// Encoder is the encoder to use, usually from Select.
	Encoder Encoder
	// Capabilities says which grabber is usable.
	Capabilities capture.Capabilities
	// Output is where the stream goes.
	Output Output
	// Input optionally replaces the target-derived capture input. Window
	// capture uses it to feed raw frames from Go over a loopback socket.
	Input *InputOverride
	// Logger may be nil.
	Logger Logger
	// Stderr, when set, receives ffmpeg's diagnostics live in addition to the
	// tail kept for error messages. The debug tool points it at the console.
	Stderr io.Writer
}

// Start builds the command line and starts ffmpeg.
func Start(ctx context.Context, opts StartOptions) (*Runner, error) {
	args, err := ArgsWithInput(opts.Config, opts.Target, opts.Encoder, opts.Capabilities, opts.Output, opts.Input)
	if err != nil {
		return nil, err
	}

	// Not exec.CommandContext: its cancellation calls Kill, which is precisely
	// what must not happen to a process writing an MP4. The context is watched
	// below and turned into a graceful stop instead.
	cmd := exec.Command(opts.FFmpeg.Path, args...)
	executil.HideConsoleWindow(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open ffmpeg stdout: %w", err)
	}
	stderr := newTailBuffer(16 << 10)
	if opts.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, opts.Stderr)
	} else {
		cmd.Stderr = stderr
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	// stopCtx is separate from the caller's context so that stopping can still
	// take its full grace period after the caller's context has been
	// cancelled — which is exactly the shutdown case.
	stopCtx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		Args:        args,
		done:        make(chan struct{}),
		gracePeriod: 5 * time.Second,
		log:         opts.Logger,
		cancel:      cancel,
	}

	go func() {
		defer close(r.done)
		r.waitErr = cmd.Wait()
		if r.log != nil {
			if r.waitErr != nil {
				r.log.Debug("ffmpeg exited", "error", r.waitErr, "stderr", r.stderr.String())
			} else {
				r.log.Debug("ffmpeg exited cleanly")
			}
		}
	}()

	// Watch the caller's context and stop ffmpeg when it is cancelled.
	go func() {
		select {
		case <-ctx.Done():
			if err := r.Stop(stopCtx); err != nil && r.log != nil {
				r.log.Warn("stopping ffmpeg", "error", err)
			}
		case <-r.done:
		}
	}()

	return r, nil
}

// Stdout is the encoded stream. It is only useful when the output was a pipe;
// for a file, ffmpeg's stdout is empty. It returns nil for a file output so a
// caller cannot accidentally wait on a stream that will never produce anything.
func (r *Runner) Stdout() io.Reader {
	if r.stdout == nil {
		return nil
	}
	return r.stdout
}

// Done is closed once ffmpeg has exited.
func (r *Runner) Done() <-chan struct{} { return r.done }

// Err returns ffmpeg's exit status, valid once Done is closed.
func (r *Runner) Err() error { return r.waitErr }

// Stderr returns the tail of ffmpeg's diagnostics so far.
func (r *Runner) Stderr() string { return r.stderr.String() }

// Stop asks ffmpeg to quit and waits for it.
//
// The "q" is what turns a partial recording into a playable one, so it is
// always tried first. Only if ffmpeg ignores it past the grace period is it
// killed, and that is reported as an error because the output file will be
// suspect.
func (r *Runner) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		if r.stdin != nil {
			// ffmpeg reads this as "finish the file and exit". Closing the pipe
			// afterwards gives it the EOF it would otherwise wait for.
			if _, err := io.WriteString(r.stdin, "q"); err != nil && r.log != nil {
				r.log.Debug("could not send q to ffmpeg", "error", err)
			}
			_ = r.stdin.Close()
		}
	})

	// Give ffmpeg the shorter of its grace period and whatever the caller has
	// left.
	timer := time.NewTimer(r.gracePeriod)
	defer timer.Stop()

	select {
	case <-r.done:
		// Exited on its own, which is the good case.
		r.cancel()
		return r.cleanExitError()

	case <-timer.C:
		if r.log != nil {
			r.log.Warn("ffmpeg did not stop within the grace period; killing it, the recording may be truncated",
				"grace", r.gracePeriod.String())
		}

	case <-ctx.Done():
		// The caller is out of patience too.
	}

	if err := r.cmd.Process.Kill(); err != nil && !errors.Is(err, exec.ErrNotFound) {
		// Losing the race with a process that just exited is normal.
		if r.log != nil {
			r.log.Debug("killing ffmpeg", "error", err)
		}
	}
	<-r.done
	r.cancel()

	return fmt.Errorf("ffmpeg had to be killed after %s, so the recording may be truncated: %w", r.gracePeriod, r.waitErr)
}

// cleanExitError turns a graceful exit into an error only when something
// actually went wrong.
func (r *Runner) cleanExitError() error {
	if r.waitErr == nil {
		return nil
	}
	tail := strings.TrimSpace(stripProgress(r.stderr.String()))
	if tail == "" {
		return fmt.Errorf("ffmpeg exited: %w", r.waitErr)
	}
	return fmt.Errorf("ffmpeg exited: %w: %s", r.waitErr, tailTail(tail))
}

// WaitForStart gives ffmpeg a moment to fail.
//
// ffmpeg reports a bad encoder, an unusable capture device or a rejected option
// by exiting within a few hundred milliseconds. Without this check, Start would
// report success the instant the process was spawned, and the user would be
// told they were recording while ffmpeg was already gone.
func (r *Runner) WaitForStart(grace time.Duration) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-r.done:
		if err := r.cleanExitError(); err != nil {
			return err
		}
		// Exiting cleanly that fast is not a capture either.
		return errors.New("ffmpeg exited immediately")
	case <-timer.C:
		return nil
	}
}

// Wait blocks until ffmpeg exits.
func (r *Runner) Wait() error {
	<-r.done
	r.cancel()
	return r.cleanExitError()
}

// tailBuffer is an io.Writer that keeps only the most recent bytes written to
// it.
//
// ffmpeg's diagnostics can run to megabytes on a long stream, and the only part
// anyone reads is the end, which is where the reason for a failure is.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		// Drop from the front, keeping the tail.
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
