// Package rtc sends Streamy's encoded H.264 video to a browser over WebRTC.
package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Logger is the structured logging surface used by Sender.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Sender owns a single WebRTC peer connection and its H.264 video track.
//
// Phase 3 deliberately has one viewer and manual SDP exchange. The peer
// connection is kept separate from capture so signaling can be replaced in
// phase 4 without changing how encoded samples are sent.
type Sender struct {
	pc     *webrtc.PeerConnection
	track  *webrtc.TrackLocalStaticSample
	logger Logger

	failure     chan error
	failureOnce sync.Once
	closeOnce   sync.Once
}

// NewSender creates an H.264 peer connection using the supplied ICE servers.
func NewSender(iceServers []webrtc.ICEServer, logger Logger) (*Sender, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, fmt.Errorf("create WebRTC peer connection: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		"video",
		"streamy",
	)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create H.264 video track: %w", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add H.264 video track: %w", err)
	}

	s := &Sender{
		pc:      pc,
		track:   track,
		logger:  logger,
		failure: make(chan error, 1),
	}
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if s.logger != nil {
			s.logger.Info("WebRTC ICE state changed", "state", state.String())
		}
		if state == webrtc.ICEConnectionStateFailed {
			s.reportFailure(errors.New("WebRTC ICE connection failed; check firewall and NAT settings"))
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if s.logger != nil {
			s.logger.Info("WebRTC connection state changed", "state", state.String())
		}
		if state == webrtc.PeerConnectionStateFailed {
			s.reportFailure(errors.New("WebRTC peer connection failed"))
		}
	})
	return s, nil
}

// CreateOffer creates an SDP offer and waits until its ICE candidates are
// gathered, so the offer can be copied as one complete document.
func (s *Sender) CreateOffer(ctx context.Context) (string, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create WebRTC offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local WebRTC offer: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return "", fmt.Errorf("gather ICE candidates: %w", ctx.Err())
	}
	local := s.pc.LocalDescription()
	if local == nil || strings.TrimSpace(local.SDP) == "" {
		return "", errors.New("WebRTC produced an empty offer")
	}
	return local.SDP, nil
}

// SetAnswer applies a browser answer. It accepts raw SDP or the JSON object
// copied by the test viewer.
func (s *Sender) SetAnswer(answer string) error {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return errors.New("the pasted WebRTC answer is empty")
	}

	var description webrtc.SessionDescription
	if strings.HasPrefix(answer, "{") {
		if err := json.Unmarshal([]byte(answer), &description); err != nil {
			return fmt.Errorf("parse the pasted WebRTC answer JSON: %w", err)
		}
	} else {
		description = webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}
	}
	if description.Type != webrtc.SDPTypeAnswer {
		return fmt.Errorf("the pasted description has type %q, want answer", description.Type.String())
	}
	if strings.TrimSpace(description.SDP) == "" {
		return errors.New("the pasted WebRTC answer has no SDP")
	}
	description.SDP = strings.TrimSpace(description.SDP) + "\r\n"
	if err := s.pc.SetRemoteDescription(description); err != nil {
		return fmt.Errorf("apply WebRTC answer: %w", err)
	}
	return nil
}

// WriteSample sends one complete encoded H.264 access unit.
func (s *Sender) WriteSample(data []byte, duration time.Duration) error {
	if len(data) == 0 {
		return errors.New("cannot send an empty H.264 access unit")
	}
	if duration <= 0 {
		return errors.New("H.264 sample duration must be positive")
	}
	if err := s.track.WriteSample(media.Sample{Data: data, Duration: duration}); err != nil {
		return fmt.Errorf("send H.264 sample over WebRTC: %w", err)
	}
	return nil
}

// Wait returns when the peer connection fails or ctx is cancelled.
func (s *Sender) Wait(ctx context.Context) error {
	select {
	case err := <-s.failure:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Failure is signalled when ICE or the peer connection enters a failed state.
func (s *Sender) Failure() <-chan error { return s.failure }

// Close tears down the peer connection. It is safe to call more than once.
func (s *Sender) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.pc.Close()
	})
	return err
}

func (s *Sender) reportFailure(err error) {
	s.failureOnce.Do(func() {
		s.failure <- err
	})
}
