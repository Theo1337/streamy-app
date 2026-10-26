//go:build !windows

package rtc

import (
	"context"
	"errors"

	"github.com/pion/rtp"
)

type DesktopAudio struct{}

func StartDesktopAudio(context.Context, string, bool, Logger, func(*rtp.Packet) error) (*DesktopAudio, error) {
	return nil, errors.New("desktop audio capture is supported only on Windows")
}

func (*DesktopAudio) Close() error { return nil }
