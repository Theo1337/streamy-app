package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"streamy/internal/capture"
	"streamy/internal/config"
	"streamy/internal/output"
	"streamy/internal/rtc"
	"streamy/internal/signal"
)

// LiveStreamer captures one screen and desktop audio, publishes them over
// WebRTC, and creates an authenticated room link for viewers.
type LiveStreamer struct {
	logger *slog.Logger

	// base is reused by every stream. Its one-time work (finding ffmpeg,
	// probing the encoder, checking Desktop Duplication) costs hundreds of
	// milliseconds, which is exactly the delay the user feels when pressing
	// the hotkey a second time.
	base *Recorder

	mu            sync.Mutex
	recorder      *Recorder
	audio         *rtc.DesktopAudio
	signal        signal.Signaler
	broadcast     *rtc.Broadcaster
	outputs       []output.Output
	reader        *io.PipeReader
	cancel        context.CancelFunc
	done          chan struct{}
	session       Session
	fps           int
	identityToken string
	starting      bool
	startDone     chan struct{}
	writer        *io.PipeWriter
	cfg           config.Config
}

// NewLiveStreamer creates the tray app's browser-sharing pipeline.
func NewLiveStreamer(logger *slog.Logger) *LiveStreamer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LiveStreamer{
		logger: logger,
		base:   NewRecorder(Options{Logger: logger, Format: "h264"}),
	}
}

// Start creates the room, starts capture, and returns a browser link.
func (s *LiveStreamer) Start(ctx context.Context, req Request) (Session, error) {
	s.mu.Lock()
	if s.recorder != nil || s.starting {
		s.mu.Unlock()
		return Session{}, errors.New("a live stream is already running")
	}
	s.starting = true
	s.startDone = make(chan struct{})
	s.mu.Unlock()
	defer s.finishStart()

	cfg := req.Config
	if cfg.Signaling.URL == "" {
		return Session{}, errors.New("signaling.url is empty; configure the Worker WebSocket URL in config.json")
	}
	if cfg.Signaling.ViewerURL == "" {
		return Session{}, errors.New("signaling.viewerURL is empty; configure the deployed viewer URL in config.json")
	}
	if req.IdentityToken == "" {
		return Session{}, errors.New("sign in with Discord from Streamy settings before starting a public stream")
	}

	// The outputs and capture live beyond the Start call. Detach cancellation
	// from the caller's request context; Stop cancels this context explicitly.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	running := false
	defer func() {
		if !running {
			cancel()
		}
	}()

	room, token, err := signal.NewRoom()
	if err != nil {
		return Session{}, err
	}
	lobbyICEServers := signalingICEServers(cfg)
	client, err := signal.NewWebSocketWithIdentity(
		ctx, cfg.Signaling.URL, room, token, req.IdentityToken, cfg.Stream.MaxViewers, lobbyICEServers,
	)
	if err != nil {
		return Session{}, err
	}
	broadcaster, err := rtc.NewBroadcaster(s.logger)
	if err != nil {
		_ = client.Close()
		return Session{}, err
	}
	viewerURL, err := signal.LobbyLink(cfg.Signaling.ViewerURL)
	if err != nil {
		broadcaster.Close()
		_ = client.Close()
		return Session{}, err
	}

	outputs := []output.Output{output.NewWebRtcOutput(broadcaster)}
	closeOutputs := func() {
		for _, out := range outputs {
			_ = out.Close()
		}
	}

	reader, writer := io.Pipe()
	recorder := s.base
	// The writer is deliberately not closed when one ffmpeg capture ends:
	// switching windows restarts ffmpeg around the same pipe, and the video
	// pump must stay alive across that gap. Stop closes it explicitly.
	recorder.SetStdout(writer, false)
	cleanupStart := func() {
		cancel()
		closeOutputs()
		if client != nil {
			_ = client.Close()
		}
		_ = reader.Close()
		_ = writer.Close()
	}

	// Desktop audio is encoded once to Opus and copied to every output. Today
	// there is one output, WebRTC, but keeping the fan-out makes the media path
	// independent of the transport.
	audioSink := func(packet *rtp.Packet) error {
		now := time.Now()
		var firstErr error
		for _, out := range outputs {
			if err := out.WriteAudio(packet, now); err != nil {
				s.logger.Warn("sending audio to an output", "output", out.Name(), "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}

	// Audio capture waits for WASAPI and for the first encoded packet, which is
	// most of a second. Recording the screen is independent of it, so both are
	// brought up at the same time instead of one after the other.
	var audioCapture *rtc.DesktopAudio
	var audioErr error
	var audioDone chan struct{}
	if cfg.Capture.DesktopAudio {
		ffmpeg, err := recorder.ensureFFmpeg(ctx, cfg)
		if err != nil {
			cleanupStart()
			return Session{}, fmt.Errorf("prepare desktop audio: %w", err)
		}
		audioDone = make(chan struct{})
		go func() {
			defer close(audioDone)
			audioCapture, audioErr = rtc.StartDesktopAudio(
				runCtx, ffmpeg.Path, cfg.Capture.ExcludeDiscordAudio, s.logger, audioSink,
			)
		}()
	}

	captureSession, captureErr := recorder.Start(ctx, req)
	if audioDone != nil {
		select {
		case <-audioDone:
		case <-ctx.Done():
			cancel()
			<-audioDone
		}
	}
	if captureErr != nil {
		if audioCapture != nil {
			if closeErr := audioCapture.Close(); closeErr != nil {
				s.logger.Warn("closing desktop-audio capture after screen startup failure", "error", closeErr)
			}
		}
		cleanupStart()
		return Session{}, fmt.Errorf("start screen capture: %w", captureErr)
	}
	if audioErr != nil {
		if stopErr := recorder.Stop(ctx); stopErr != nil {
			s.logger.Warn("stopping screen capture after audio startup failure", "error", stopErr)
		}
		cleanupStart()
		return Session{}, fmt.Errorf("start desktop audio: %w", audioErr)
	}
	session := Session{
		ViewerURL:   viewerURL,
		Label:       "Sharing",
		Description: fmt.Sprintf("%s; viewer link copied to clipboard", captureSession.Description),
		Target:      captureSession.Target,
	}
	done := make(chan struct{})

	s.mu.Lock()
	s.recorder = recorder
	s.audio = audioCapture
	s.signal = client
	s.broadcast = broadcaster
	s.outputs = outputs
	s.reader = reader
	s.cancel = cancel
	s.done = done
	s.session = session
	s.fps = cfg.Encoding.FPS
	s.identityToken = req.IdentityToken
	s.writer = writer
	s.cfg = cfg
	s.mu.Unlock()

	go s.forwardVideo(runCtx, reader)
	go s.handleSignals(runCtx, client, cfg, signalingICEServers(cfg))
	discord := cfg.Stream.Discord
	if discord.NotifyURL != "" {
		go func() {
			notifyCtx, notifyCancel := context.WithTimeout(runCtx, 10*time.Second)
			defer notifyCancel()
			err := output.NotifyBotStart(notifyCtx, discord.NotifyURL, discord.NotifyKey, output.BotNotice{
				ChannelID: discord.ChannelID,
				Streamer:  req.UserName,
				URL:       viewerURL,
			})
			if err != nil {
				s.logger.Warn("could not notify the Discord bot", "error", err)
			} else {
				s.logger.Info("notified the Discord bot", "channel", discord.ChannelID)
			}
		}()
	}
	s.logger.Info("live room ready", "room", room, "maxViewers", cfg.Stream.MaxViewers)
	running = true
	return session, nil
}

func (s *LiveStreamer) finishStart() {
	s.mu.Lock()
	s.starting = false
	if s.startDone != nil {
		close(s.startDone)
		s.startDone = nil
	}
	s.mu.Unlock()
}

func (s *LiveStreamer) forwardVideo(ctx context.Context, input io.Reader) {
	defer close(s.done)
	reader := rtc.NewAccessUnitReader(input)
	duration := time.Second / time.Duration(s.sessionFPS())
	var lastBandwidthLog time.Time
	var bytesSinceLog int64

	for {
		select {
		case <-ctx.Done():
			_ = reader.Drain()
			return
		default:
		}
		accessUnit, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if ctx.Err() != nil {
				// Stop closes the pipe under a pump that may be blocked in a
				// read. That is the end of the session, not a malformed stream:
				// logging it as a parse failure only makes shutdown look broken.
				s.logger.Debug("video pump stopped during shutdown", "error", err)
				return
			}
			s.logger.Error("could not parse ffmpeg H.264 output", "error", err)
			return
		}

		s.mu.Lock()
		outputs := append([]output.Output(nil), s.outputs...)
		broadcaster := s.broadcast
		s.mu.Unlock()
		if len(outputs) == 0 {
			continue
		}
		now := time.Now()
		sent := false
		for _, out := range outputs {
			if err := out.WriteVideo(accessUnit, now, duration); err != nil {
				s.logger.Warn("sending video to an output failed", "output", out.Name(), "error", err)
				continue
			}
			sent = true
		}
		if !sent {
			continue
		}
		if broadcaster != nil {
			bytesSinceLog += int64(len(accessUnit) * broadcaster.ViewerCount())
		}
		if lastBandwidthLog.IsZero() {
			lastBandwidthLog = time.Now()
		}
		if elapsed := time.Since(lastBandwidthLog); elapsed >= 10*time.Second {
			logArgs := []any{"estimated_kbps", float64(bytesSinceLog) * 8 / elapsed.Seconds() / 1000}
			if broadcaster != nil {
				logArgs = append(logArgs, "viewers", broadcaster.ViewerCount())
			}
			s.logger.Info("video output bandwidth", logArgs...)
			lastBandwidthLog = time.Now()
			bytesSinceLog = 0
		}
	}
}

func (s *LiveStreamer) sessionFPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fps < 1 {
		return 30
	}
	return s.fps
}

func (s *LiveStreamer) handleSignals(ctx context.Context, client signal.Signaler, cfg config.Config, lobbyICEServers []signal.ICEServer) {
	room, token := client.Room(), client.Token()
	backoff := time.Second
	for {
		if !s.consumeSignals(ctx, client, cfg) || ctx.Err() != nil {
			_ = client.Close()
			return
		}
		if err := client.Err(); err != nil {
			s.logger.Warn("signaling disconnected; attempting to reconnect", "error", err)
		}
		_ = client.Close()
		s.mu.Lock()
		broadcaster := s.broadcast
		s.mu.Unlock()
		if broadcaster != nil {
			broadcaster.Close()
			broadcaster.RequestKeyframe("signaling-reconnect")
		}

		for {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			reconnectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			reconnected, err := signal.NewWebSocketWithIdentity(
				reconnectCtx, cfg.Signaling.URL, room, token, s.identityToken, cfg.Stream.MaxViewers, lobbyICEServers,
			)
			cancel()
			if err != nil {
				s.logger.Warn("signaling reconnect failed", "retry_in", backoff, "error", err)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			client = reconnected
			s.mu.Lock()
			if ctx.Err() != nil {
				s.mu.Unlock()
				_ = reconnected.Close()
				return
			}
			s.signal = reconnected
			s.mu.Unlock()
			s.logger.Info("signaling connection restored", "room", room)
			backoff = time.Second
			break
		}
	}
}

func (s *LiveStreamer) consumeSignals(ctx context.Context, client signal.Signaler, cfg config.Config) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case message, ok := <-client.Messages():
			if !ok {
				if err := client.Err(); err != nil {
					s.logger.Error("signaling connection closed", "error", err)
				}
				return true
			}
			switch message.Type {
			case "welcome":
				s.logger.Info("connected to signaling room",
					"peer_id", message.PeerID,
					"viewers", message.ViewerCount,
					"viewer_limit", message.MaxViewers,
				)
			case "peer-joined":
				if message.Role == signal.RoleViewer {
					go s.startViewer(ctx, client, cfg, message.PeerID)
				}
			case "peer-left":
				if message.Role == signal.RoleViewer {
					s.mu.Lock()
					broadcaster := s.broadcast
					s.mu.Unlock()
					if broadcaster != nil {
						broadcaster.RemovePeer(message.PeerID)
					}
					s.logger.Info("viewer left", "peer_id", message.PeerID, "reason", message.Reason)
				}
			case "answer":
				s.mu.Lock()
				broadcaster := s.broadcast
				s.mu.Unlock()
				if broadcaster != nil {
					if peer := broadcaster.Peer(message.From); peer != nil {
						if err := peer.SetAnswer(message.SDP); err != nil {
							s.logger.Error("applying viewer answer", "peer_id", message.From, "error", err)
						}
					} else {
						s.logger.Debug("ignoring answer from an unknown viewer", "peer_id", message.From)
					}
				}
			case "candidate":
				s.logger.Debug("ignoring trickle candidate; phase 4 uses complete SDP", "peer_id", message.From)
			case "error":
				s.logger.Error("signaling server error", "code", message.Code, "message", message.ErrorMessage)
				if message.Fatal == nil || *message.Fatal {
					return false
				}
			case "pong":
			default:
				s.logger.Warn("ignoring unknown signaling message", "type", message.Type)
			}
		}
	}
}

func (s *LiveStreamer) startViewer(ctx context.Context, client signal.Signaler, cfg config.Config, peerID string) {
	s.mu.Lock()
	broadcaster := s.broadcast
	if broadcaster == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	if broadcaster.Peer(peerID) != nil {
		return
	}
	peer, err := broadcaster.AddPeer(peerID, iceServers(cfg))
	if err != nil {
		s.logger.Warn("could not add WebRTC peer for viewer", "peer_id", peerID, "error", err)
		return
	}
	offer, err := peer.CreateOffer(ctx)
	if err != nil {
		broadcaster.RemovePeer(peerID)
		s.logger.Error("create WebRTC offer", "peer_id", peerID, "error", err)
		return
	}
	if ctx.Err() != nil {
		broadcaster.RemovePeer(peerID)
		return
	}
	if err := client.Send(ctx, signal.Message{Type: "offer", To: peerID, SDP: offer}); err != nil {
		broadcaster.RemovePeer(peerID)
		s.logger.Error("send WebRTC offer", "peer_id", peerID, "error", err)
		return
	}

	s.logger.Info("sent WebRTC offer", "peer_id", peerID)
	go func() {
		select {
		case err := <-peer.Failure():
			if errors.Is(err, context.Canceled) {
				return
			}
			s.logger.Warn("viewer WebRTC connection ended", "peer_id", peerID, "error", err)
			broadcaster.RemovePeer(peerID)
		case <-ctx.Done():
		}
	}()
}

func iceServers(cfg config.Config) []webrtc.ICEServer {
	servers := make([]webrtc.ICEServer, 0, len(cfg.Stream.STUNServers)+len(cfg.Stream.TURNServers))
	for _, server := range cfg.Stream.STUNServers {
		servers = append(servers, webrtc.ICEServer{URLs: []string{server}})
	}
	for _, server := range cfg.Stream.TURNServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:       server.URLs,
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	return servers
}

func signalingICEServers(cfg config.Config) []signal.ICEServer {
	servers := make([]signal.ICEServer, 0, len(cfg.Stream.STUNServers)+len(cfg.Stream.TURNServers))
	for _, server := range cfg.Stream.STUNServers {
		servers = append(servers, signal.ICEServer{URLs: []string{server}})
	}
	for _, server := range cfg.Stream.TURNServers {
		servers = append(servers, signal.ICEServer{
			URLs: server.URLs, Username: server.Username, Credential: server.Credential,
		})
	}
	return servers
}

// SwitchTarget retargets the capture without closing the room or the outputs.
// Viewers stay connected; ffmpeg restarts its input and a keyframe is requested
// so the new window appears immediately.
func (s *LiveStreamer) SwitchTarget(ctx context.Context, target capture.Target) (Session, error) {
	s.mu.Lock()
	recorder := s.recorder
	broadcaster := s.broadcast
	session := s.session
	s.mu.Unlock()
	if recorder == nil {
		return Session{}, errors.New("no live stream is running")
	}
	if _, err := recorder.SwitchTarget(ctx, target); err != nil {
		return Session{}, err
	}
	if broadcaster != nil {
		broadcaster.RequestKeyframe("target-switch")
	}
	s.logger.Info("stream target switched", "target", target.Describe())
	return session, nil
}

// Stop closes signaling and capture, and waits for ffmpeg's output pump.
func (s *LiveStreamer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.starting {
		startDone := s.startDone
		s.mu.Unlock()
		select {
		case <-startDone:
			return s.Stop(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	recorder, audio, client, outputs := s.recorder, s.audio, s.signal, s.outputs
	cancel, reader, done, writer := s.cancel, s.reader, s.done, s.writer
	if cancel != nil {
		cancel()
	}
	s.recorder, s.audio, s.signal, s.broadcast, s.outputs = nil, nil, nil, nil, nil
	s.cancel, s.reader, s.done, s.session, s.fps = nil, nil, nil, Session{}, 0
	s.writer = nil
	s.mu.Unlock()
	if recorder == nil {
		return nil
	}

	if audio != nil {
		if err := audio.Close(); err != nil {
			s.logger.Warn("closing desktop-audio capture", "error", err)
		}
	}
	if client != nil {
		if err := client.Close(); err != nil {
			s.logger.Warn("closing signaling connection", "error", err)
		}
	}
	err := recorder.Stop(ctx)
	// Wait for the video pump to finish. recorder.Stop() stops ffmpeg, which
	// closes its stdout (our writer), so forwardVideo will see io.EOF and exit
	// cleanly. We wait for 'done' before closing the writer ourselves as a
	// safety net; if we closed the writer first, forwardVideo would get
	// io.ErrClosedPipe instead of EOF and log a spurious parse error.
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			err = errors.Join(err, ctx.Err())
			// Context deadline fired: force-unblock the pump by closing the
			// reader so it returns io.ErrClosedPipe (which forwardVideo treats
			// as a clean shutdown because ctx.Err() is set).
			if reader != nil {
				_ = reader.Close()
			}
		}
	}
	// Safety net: if recorder.Stop() didn't close the writer (e.g. ffmpeg
	// already exited), close it now. forwardVideo has already exited, so this
	// is a no-op for the pump.
	if writer != nil {
		_ = writer.Close()
	}
	if reader != nil {
		_ = reader.Close()
	}
	for _, out := range outputs {
		if closeErr := out.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s output: %w", out.Name(), closeErr))
		}
	}
	return err
}

var _ Streamer = (*LiveStreamer)(nil)
