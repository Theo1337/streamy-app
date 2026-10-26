package output

import (
	"time"

	"github.com/pion/rtp"

	"streamy/internal/rtc"
)

// WebRtcOutput sends media through the existing shared Pion broadcaster. One
// encoded stream is fanned out to every browser peer.
type WebRtcOutput struct {
	broadcaster *rtc.Broadcaster
}

// NewWebRtcOutput wraps a broadcaster.
func NewWebRtcOutput(broadcaster *rtc.Broadcaster) *WebRtcOutput {
	return &WebRtcOutput{broadcaster: broadcaster}
}

// Name identifies the output in logs.
func (o *WebRtcOutput) Name() string { return "webrtc" }

// WriteVideo sends one access unit to every peer.
func (o *WebRtcOutput) WriteVideo(sample []byte, _ time.Time, duration time.Duration) error {
	return o.broadcaster.WriteSample(sample, duration)
}

// WriteAudio forwards one RTP packet to every peer.
func (o *WebRtcOutput) WriteAudio(packet *rtp.Packet, _ time.Time) error {
	return o.broadcaster.WriteAudioRTP(packet)
}

// Close tears down every viewer peer.
func (o *WebRtcOutput) Close() error {
	o.broadcaster.Close()
	return nil
}
