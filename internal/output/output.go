// Package output is the boundary between the encoded media pipeline and the
// places it can go. WebRTC implements the interface today, so an additional
// transport can be added without touching capture or encoding.
package output

import (
	"time"

	"github.com/pion/rtp"
)

// Output receives the same encoded H.264 access units and Opus packets as every
// other output. Implementations must be safe for one video goroutine and one
// audio goroutine writing at the same time.
type Output interface {
	Name() string
	WriteVideo(sample []byte, timestamp time.Time, duration time.Duration) error
	WriteAudio(packet *rtp.Packet, timestamp time.Time) error
	Close() error
}
