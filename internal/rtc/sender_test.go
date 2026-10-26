package rtc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestSenderNegotiatesAndSendsH264Track(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sender, err := NewSender(nil, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer sender.Close()

	viewer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create test viewer: %v", err)
	}
	defer viewer.Close()
	if _, err := viewer.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		t.Fatalf("add receive-only video transceiver: %v", err)
	}

	trackReceived := make(chan struct{}, 1)
	viewer.OnTrack(func(*webrtc.TrackRemote, *webrtc.RTPReceiver) {
		trackReceived <- struct{}{}
	})

	offer, err := sender.CreateOffer(ctx)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if !strings.Contains(offer, "H264/90000") {
		t.Fatal("offer does not advertise H.264")
	}
	if err := viewer.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	}); err != nil {
		t.Fatalf("apply offer: %v", err)
	}
	answer, err := viewer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(viewer)
	if err := viewer.SetLocalDescription(answer); err != nil {
		t.Fatalf("set viewer answer: %v", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal("viewer ICE gathering timed out")
	}
	localAnswer := viewer.LocalDescription()
	if localAnswer == nil {
		t.Fatal("viewer produced no local answer")
	}
	answerJSON, err := json.Marshal(localAnswer)
	if err != nil {
		t.Fatalf("marshal browser answer: %v", err)
	}
	if err := sender.SetAnswer(string(answerJSON)); err != nil {
		t.Fatalf("SetAnswer: %v", err)
	}

	sample := []byte{0, 0, 0, 1, 0x65, 0x80}
	deadline := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	for {
		if err := sender.WriteSample(sample, time.Second/30); err != nil {
			t.Fatalf("WriteSample: %v", err)
		}
		select {
		case <-trackReceived:
			return
		case <-deadline.C:
		case <-ctx.Done():
			t.Fatal("viewer did not receive the H.264 track")
		}
	}
}
