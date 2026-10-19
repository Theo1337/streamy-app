// Package encode turns "record this" into a working ffmpeg command line.
//
// It does three things the rest of the app should not have to think about:
//
//   - find the ffmpeg binary and remember its version;
//   - work out which H.264 encoder the machine really has, rather than which
//     ones the build claims to have;
//   - run the subprocess and stop it in a way that leaves a playable file.
package encode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Logger is the part of log/slog this package uses. Depending on the interface
// instead of the concrete type keeps the package testable and documents which
// methods are actually called.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// FFmpeg is the ffmpeg binary Streamy runs.
type FFmpeg struct {
	// Path is the executable that was found.
	Path string

	// version is the first line of `ffmpeg -version`. It is used to invalidate
	// the encoder cache, so a new ffmpeg does not inherit the old one's
	// answers.
	version string
}

// Version returns the ffmpeg version string, empty if it was never read.
func (f FFmpeg) Version() string { return f.version }

// Find locates ffmpeg and reads its version.
//
// configured is the encoding.ffmpegPath setting. It may point at the executable
// or at the directory containing it, because people reasonably do both. When it
// is empty, ffmpeg is looked up on PATH.
func Find(ctx context.Context, configured string) (FFmpeg, error) {
	path, err := resolvePath(configured)
	if err != nil {
		return FFmpeg{}, err
	}

	f := FFmpeg{Path: path}
	out, err := f.output(ctx, "-hide_banner", "-version")
	if err != nil {
		return FFmpeg{}, fmt.Errorf("%s does not look like a working ffmpeg: %w", path, err)
	}
	f.version = firstLine(out)
	return f, nil
}

func resolvePath(configured string) (string, error) {
	configured = strings.TrimSpace(configured)

	if configured == "" {
		found, err := exec.LookPath("ffmpeg")
		if err != nil {
			return "", errors.New("ffmpeg was not found on PATH; install it (on Windows: winget install Gyan.FFmpeg) or point encoding.ffmpegPath at it in the config")
		}
		return found, nil
	}

	info, err := os.Stat(configured)
	if err != nil {
		return "", fmt.Errorf("encoding.ffmpegPath %q: %w", configured, err)
	}
	if info.IsDir() {
		// LookPath appends the platform's executable extensions, so this finds
		// ffmpeg.exe on Windows and ffmpeg elsewhere without a GOOS test.
		joined := filepath.Join(configured, "ffmpeg")
		found, err := exec.LookPath(joined)
		if err != nil {
			return "", fmt.Errorf("no ffmpeg executable in %q", configured)
		}
		return found, nil
	}
	return configured, nil
}

// output runs ffmpeg to completion and returns everything it wrote.
//
// It is only for short, non-interactive calls such as -version and the encoder
// probe. A real capture goes through Start, which needs to stream and to be
// stopped gracefully.
func (f FFmpeg) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, f.Path, args...)
	// ffmpeg writes all of its diagnostics to stderr, including the version
	// banner, so both streams are wanted.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), errorWithOutput(err, string(out))
	}
	return string(out), nil
}

// ProbeDDAGrab reports whether the Desktop Duplication grabber works here.
//
// It captures a single frame of the first display and throws it away. The
// single frame is real: ddagrab needs a D3D11 device and a genuine display
// output, and fails on adapters that do not support duplication, which includes
// some virtual machines and remote desktop sessions. That is exactly why this
// is probed rather than assumed.
//
// ddagrab only exists on Windows. Elsewhere this returns an error, which the
// caller reads as "Desktop Duplication is unavailable", which is true.
func (f FFmpeg) ProbeDDAGrab(ctx context.Context) error {
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		// Desktop Duplication hands out D3D11 textures, so a D3D11 device has
		// to exist for hwdownload to pull them back to system memory.
		"-init_hw_device", "d3d11va",
		"-f", "lavfi", "-i", "ddagrab=framerate=30",
		"-vf", "hwdownload,format=bgra",
		"-frames:v", "1",
		"-f", "null", "-",
	}

	if _, err := f.output(ctx, args...); err != nil {
		return fmt.Errorf("test capture with ddagrab: %w", err)
	}
	return nil
}

// errorWithOutput attaches the tail of ffmpeg's output to an error.
//
// ffmpeg reports "Conversion failed" and then explains why a few lines further
// up; without the output the error is useless, and with all of it the error is
// unreadable, so the last few lines win.
func errorWithOutput(err error, out string) error {
	tail := tailTail(strings.TrimSpace(stripProgress(out)))
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, tail)
}

// stripProgress removes ffmpeg's in-place progress redraws.
//
// ffmpeg writes its progress as a series of carriage-return terminated updates
// on the same physical line, and real messages separated by newlines. A real
// stream therefore looks like
//
//	frame=  1\rframe=  2\r[in#0/gdigrab] Stream #0: not enough frames\n
//
// so the text before the last carriage return on each line is redraw history,
// and the text after it is what actually happened.
func stripProgress(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// tailTail keeps the end of a multi-line message, which is where ffmpeg puts
// the reason for a failure.
func tailTail(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}
