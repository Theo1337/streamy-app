package rtc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
)

type pcmFormat struct {
	name       string
	sampleRate uint32
	channels   uint16
	blockAlign uint16
}

func pcmWaveFormat(sampleRate uint32, channels uint16) []byte {
	const bitsPerSample = uint16(16)
	blockAlign := channels * bitsPerSample / 8
	format := make([]byte, 18)
	binary.LittleEndian.PutUint16(format, 1)
	binary.LittleEndian.PutUint16(format[2:], channels)
	binary.LittleEndian.PutUint32(format[4:], sampleRate)
	binary.LittleEndian.PutUint32(format[8:], sampleRate*uint32(blockAlign))
	binary.LittleEndian.PutUint16(format[12:], blockAlign)
	binary.LittleEndian.PutUint16(format[14:], bitsPerSample)
	return format
}

func opusEncoderArgs(format pcmFormat, address string) []string {
	return []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-f", format.name,
		"-ar", strconv.FormatUint(uint64(format.sampleRate), 10),
		"-ac", strconv.FormatUint(uint64(format.channels), 10),
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
		fmt.Sprintf("rtp://%s?pkt_size=1200", address),
	}
}

func rawPCMFormat(formatTag, bitsPerSample uint16, sampleRate uint32, channels, blockAlign uint16, subFormat [16]byte) (pcmFormat, error) {
	if formatTag == 0xfffe {
		pcmGUIDTail := [12]byte{0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
		if !bytes.Equal(subFormat[4:], pcmGUIDTail[:]) {
			return pcmFormat{}, fmt.Errorf("unsupported WASAPI extensible subtype %x", subFormat)
		}
		formatTag = uint16(subFormat[0]) | uint16(subFormat[1])<<8
	}
	name := ""
	switch {
	case formatTag == 3 && bitsPerSample == 32:
		name = "f32le"
	case formatTag == 3 && bitsPerSample == 64:
		name = "f64le"
	case formatTag == 1 && bitsPerSample == 8:
		name = "u8"
	case formatTag == 1 && bitsPerSample == 16:
		name = "s16le"
	case formatTag == 1 && bitsPerSample == 24:
		name = "s24le"
	case formatTag == 1 && bitsPerSample == 32:
		name = "s32le"
	default:
		return pcmFormat{}, fmt.Errorf("unsupported WASAPI sample format: format tag %#x, %d bits", formatTag, bitsPerSample)
	}
	if sampleRate == 0 || channels == 0 || blockAlign == 0 {
		return pcmFormat{}, fmt.Errorf("invalid WASAPI mix format: %d Hz, %d channels, %d-byte frames", sampleRate, channels, blockAlign)
	}
	return pcmFormat{
		name:       name,
		sampleRate: sampleRate,
		channels:   channels,
		blockAlign: blockAlign,
	}, nil
}
