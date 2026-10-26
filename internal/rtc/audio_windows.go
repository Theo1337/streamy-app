//go:build windows

package rtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"golang.org/x/sys/windows"

	"streamy/internal/executil"
)

type DesktopAudio struct {
	cancel         context.CancelFunc
	pcmWriter      *io.PipeWriter
	conn           *net.UDPConn
	captureDone    chan struct{}
	processDone    chan struct{}
	rtpDone        chan struct{}
	rtpStarted     chan struct{}
	closeOnce      sync.Once
	rtpStartOnce   sync.Once
	stderr         *audioTail
	logger         Logger
	processStarted atomic.Bool
}

func StartDesktopAudio(ctx context.Context, ffmpegPath string, excludeDiscord bool, logger Logger, send func(*rtp.Packet) error) (*DesktopAudio, error) {
	if strings.TrimSpace(ffmpegPath) == "" {
		return nil, errors.New("desktop audio requires a configured FFmpeg executable")
	}
	if send == nil {
		return nil, errors.New("desktop audio requires an RTP packet destination")
	}
	if err := requireFFmpegFeature(ctx, ffmpegPath, "-encoders", "libopus", "libopus audio encoder"); err != nil {
		return nil, err
	}

	excludePID := uint32(windows.GetCurrentProcessId())
	if excludeDiscord {
		discordPID, err := discordRootProcessID()
		if err != nil {
			return nil, err
		}
		if discordPID != 0 {
			excludePID = discordPID
			if logger != nil {
				logger.Info("excluding Discord desktop audio from loopback", "process_id", discordPID)
			}
		} else if logger != nil {
			logger.Warn("Discord desktop app was not found; capturing system audio without Discord exclusion")
		}
	}
	if logger != nil {
		logger.Info("starting low-latency WASAPI process-loopback audio", "excluded_process_id", excludePID)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("open local RTP socket for desktop audio: %w", err)
	}
	addr := conn.LocalAddr().(*net.UDPAddr)
	pcmReader, pcmWriter := io.Pipe()
	audioCtx, cancel := context.WithCancel(ctx)
	stderr := &audioTail{max: 8192}
	audio := &DesktopAudio{
		cancel:      cancel,
		pcmWriter:   pcmWriter,
		conn:        conn,
		captureDone: make(chan struct{}),
		processDone: make(chan struct{}),
		rtpDone:     make(chan struct{}),
		rtpStarted:  make(chan struct{}),
		stderr:      stderr,
		logger:      logger,
	}
	go audio.readRTP(audioCtx, send)

	ready := make(chan error, 1)
	go func() {
		defer close(audio.captureDone)
		defer pcmWriter.Close()
		err := captureProcessLoopback(audioCtx, excludePID, func(format pcmFormat) error {
			args := opusEncoderArgs(format, net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port)))
			cmd := exec.CommandContext(audioCtx, ffmpegPath, args...)
			executil.HideConsoleWindow(cmd)
			cmd.Stdin = pcmReader
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("start FFmpeg Opus encoder: %w", err)
			}
			audio.processStarted.Store(true)
			go func() {
				processErr := cmd.Wait()
				stderr.setProcessError(processErr)
				close(audio.processDone)
				cancel()
			}()
			return nil
		}, pcmWriter, ready)
		if err != nil && !errors.Is(err, context.Canceled) && logger != nil {
			logger.Warn("desktop audio capture stopped", "error", err)
		}
		if !audio.processStarted.Load() {
			close(audio.processDone)
		}
		cancel()
	}()

	startupTimer := time.NewTimer(5 * time.Second)
	defer startupTimer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			audio.Close()
			return nil, fmt.Errorf("start WASAPI process-loopback capture: %w", err)
		}
		select {
		case <-audio.processDone:
			audio.Close()
			return nil, fmt.Errorf("FFmpeg Opus encoder exited during startup: %s", stderr.String())
		default:
		}
	case <-ctx.Done():
		audio.Close()
		return nil, ctx.Err()
	case <-startupTimer.C:
		audio.Close()
		return nil, errors.New("WASAPI process-loopback capture did not initialize within 5 seconds")
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-audio.rtpStarted:
	case <-audio.processDone:
		audio.Close()
		return nil, fmt.Errorf("FFmpeg Opus encoder exited before producing audio: %s", stderr.String())
	case <-ctx.Done():
		audio.Close()
		return nil, ctx.Err()
	case <-timer.C:
		audio.Close()
		return nil, errors.New("WASAPI capture initialized but the Opus encoder produced no audio packets within 2 seconds")
	}

	return audio, nil
}

func (a *DesktopAudio) readRTP(ctx context.Context, send func(*rtp.Packet) error) {
	defer close(a.rtpDone)
	buf := make([]byte, 2048)
	for {
		if err := a.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			if ctx.Err() == nil && a.logger != nil {
				a.logger.Warn("setting desktop-audio socket deadline", "error", err)
			}
			return
		}
		n, _, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if a.logger != nil {
				a.logger.Warn("reading desktop-audio RTP", "error", err)
			}
			return
		}
		packet := new(rtp.Packet)
		if err := packet.Unmarshal(buf[:n]); err != nil {
			if a.logger != nil {
				a.logger.Warn("parsing desktop-audio RTP packet", "error", err)
			}
			continue
		}
		if err := send(packet); err != nil && a.logger != nil {
			a.logger.Warn("sending desktop audio to viewers", "error", err)
		}
		a.rtpStartOnce.Do(func() { close(a.rtpStarted) })
	}
}

func (a *DesktopAudio) Close() error {
	var closeErr error
	a.closeOnce.Do(func() {
		a.cancel()
		if err := a.pcmWriter.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			closeErr = err
		}
		if err := a.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = errors.Join(closeErr, err)
		}
		<-a.captureDone
		<-a.processDone
		<-a.rtpDone
	})
	return closeErr
}

// ffmpegFeatures caches the encoder-list check. Launching ffmpeg to ask it is
// cheap in absolute terms but it is on the path of every stream start, and the
// answer for one binary cannot change while the app runs.
var (
	ffmpegFeaturesMu sync.Mutex
	ffmpegFeatures   = map[string]error{}
)

func requireFFmpegFeature(ctx context.Context, ffmpegPath, flag, feature, label string) error {
	key := ffmpegPath + "\x00" + flag + "\x00" + feature
	ffmpegFeaturesMu.Lock()
	cached, ok := ffmpegFeatures[key]
	ffmpegFeaturesMu.Unlock()
	if ok {
		return cached
	}

	err := checkFFmpegFeature(ctx, ffmpegPath, flag, feature, label)
	// A cancelled check says nothing about the binary, so it is not worth
	// remembering.
	if ctx.Err() == nil {
		ffmpegFeaturesMu.Lock()
		ffmpegFeatures[key] = err
		ffmpegFeaturesMu.Unlock()
	}
	return err
}

func checkFFmpegFeature(ctx context.Context, ffmpegPath, flag, feature, label string) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", flag)
	executil.HideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("check FFmpeg %s support: %w", label, err)
	}
	if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(feature)) {
		return fmt.Errorf("desktop audio needs FFmpeg with %s support; this FFmpeg build does not list %s. Install a Windows FFmpeg build with libopus encoding, or turn off Desktop audio in Streamy settings", label, feature)
	}
	return nil
}

type audioTail struct {
	mu         sync.Mutex
	max        int
	data       []byte
	processErr error
}

func (t *audioTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.max {
		t.data = append([]byte(nil), t.data[len(t.data)-t.max:]...)
	}
	return len(p), nil
}

func (t *audioTail) setProcessError(err error) {
	t.mu.Lock()
	t.processErr = err
	t.mu.Unlock()
}

func (t *audioTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	message := strings.TrimSpace(string(t.data))
	if message == "" && t.processErr != nil && !errors.Is(t.processErr, context.Canceled) {
		message = t.processErr.Error()
	}
	if message == "" {
		return "no diagnostic output from FFmpeg"
	}
	return message
}

var _ io.Writer = (*audioTail)(nil)
