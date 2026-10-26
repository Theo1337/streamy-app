package rtc

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestOpusEncoderArgsReadPCMAndWriteLowLatencyRTP(t *testing.T) {
	got := opusEncoderArgs(pcmFormat{
		name:       "f32le",
		sampleRate: 48000,
		channels:   2,
		blockAlign: 8,
	}, "127.0.0.1:5000")
	want := []string{
		"-hide_banner",
		"-nostats",
		"-loglevel", "error",
		"-f", "f32le",
		"-ar", "48000",
		"-ac", "2",
		"-i", "pipe:0",
		"-vn",
		"-c:a", "libopus",
		"-application", "lowdelay",
		"-frame_duration", "10",
		"-b:a", "128k",
		"-ar", "48000",
		"-ac", "2",
		"-flush_packets", "1",
		"-f", "rtp",
		"rtp://127.0.0.1:5000?pkt_size=1200",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opusEncoderArgs() = %#v, want %#v", got, want)
	}
}

func TestRawPCMFormatUnderstandsWASAPIMixFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  uint16
		bits uint16
		sub  [16]byte
		want string
	}{
		{name: "float32", tag: 3, bits: 32, want: "f32le"},
		{name: "pcm16", tag: 1, bits: 16, want: "s16le"},
		{
			name: "extensible float32", tag: 0xfffe, bits: 32,
			sub:  [16]byte{3, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71},
			want: "f32le",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rawPCMFormat(tc.tag, tc.bits, 48000, 2, 8, tc.sub)
			if err != nil {
				t.Fatalf("rawPCMFormat: %v", err)
			}
			if got.name != tc.want || got.sampleRate != 48000 || got.channels != 2 {
				t.Fatalf("rawPCMFormat = %+v, want %s, 48000 Hz, stereo", got, tc.want)
			}
		})
	}
}

func TestPCMWaveFormatIsStereoS16LE(t *testing.T) {
	got := pcmWaveFormat(48000, 2)
	if len(got) != 18 {
		t.Fatalf("pcmWaveFormat length = %d, want 18-byte WAVEFORMATEX", len(got))
	}
	format, err := rawPCMFormat(
		binary.LittleEndian.Uint16(got),
		binary.LittleEndian.Uint16(got[14:]),
		binary.LittleEndian.Uint32(got[4:]),
		binary.LittleEndian.Uint16(got[2:]),
		binary.LittleEndian.Uint16(got[12:]),
		[16]byte{},
	)
	if err != nil {
		t.Fatalf("rawPCMFormat: %v", err)
	}
	if format.name != "s16le" || format.sampleRate != 48000 || format.channels != 2 || format.blockAlign != 4 {
		t.Fatalf("process-loopback format = %+v, want s16le 48000 Hz stereo", format)
	}
}

func TestRawPCMFormatRejectsUnsupportedFormat(t *testing.T) {
	if _, err := rawPCMFormat(7, 16, 48000, 2, 4, [16]byte{}); err == nil {
		t.Fatal("rawPCMFormat accepted an unsupported sample format")
	}
}
