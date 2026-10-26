package rtc

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestAccessUnitReaderGroupsSlicesIntoFrames(t *testing.T) {
	stream := joinNALs(
		nal(7, 0x42, 0xe0, 0x1f), // SPS
		nal(8, 0xce, 0x3c, 0x80), // PPS
		nal(5, 0x80),             // IDR, first_mb_in_slice = 0
		nal(1, 0x40),             // another slice in the same frame (first_mb = 1)
		nal(1, 0x80),             // next frame (first_mb = 0)
	)
	reader := NewAccessUnitReader(bytes.NewReader(stream))

	first, err := reader.Next()
	if err != nil {
		t.Fatalf("first access unit: %v", err)
	}
	if got, want := countNALs(first), 4; got != want {
		t.Errorf("first access unit has %d NALs, want %d", got, want)
	}

	second, err := reader.Next()
	if err != nil {
		t.Fatalf("second access unit: %v", err)
	}
	if got, want := countNALs(second), 1; got != want {
		t.Errorf("second access unit has %d NALs, want %d", got, want)
	}

	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream error = %v, want EOF", err)
	}
}

func TestAccessUnitReaderAcceptsThreeAndFourByteStartCodesAndAUD(t *testing.T) {
	stream := append([]byte{0, 0, 1}, 9, 0xf0)
	stream = append(stream, 0, 0, 0, 1, 5, 0x80)
	stream = append(stream, 0, 0, 1, 9, 0xf0)
	stream = append(stream, 0, 0, 1, 1, 0x80)
	reader := NewAccessUnitReader(bytes.NewReader(stream))

	first, err := reader.Next()
	if err != nil {
		t.Fatalf("first access unit: %v", err)
	}
	if got, want := countNALs(first), 2; got != want {
		t.Errorf("first access unit has %d NALs, want %d", got, want)
	}

	second, err := reader.Next()
	if err != nil {
		t.Fatalf("second access unit: %v", err)
	}
	if got, want := countNALs(second), 2; got != want {
		t.Errorf("second access unit has %d NALs, want %d", got, want)
	}
}

func TestAccessUnitReaderHandlesSmallUnderlyingReads(t *testing.T) {
	reader := NewAccessUnitReader(&limitedReader{
		reader: bytes.NewReader(joinNALs(nal(5, 0x80), nal(1, 0x80))),
		limit:  1,
	})
	for range 2 {
		if _, err := reader.Next(); err != nil {
			t.Fatalf("Next with one-byte reads: %v", err)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream error = %v, want EOF", err)
	}
}

func TestAccessUnitReaderRejectsEmptyNALWithoutPanicking(t *testing.T) {
	reader := NewAccessUnitReader(bytes.NewReader([]byte{0, 0, 1, 0, 0, 1, 0x65, 0x80}))
	if _, err := reader.Next(); !errors.Is(err, errIncompleteNAL) {
		t.Errorf("empty NAL error = %v, want %v", err, errIncompleteNAL)
	}
}

func TestUnsignedExpGolomb(t *testing.T) {
	for _, tc := range []struct {
		bits []byte
		want uint64
	}{
		{[]byte{0x80}, 0}, // 1
		{[]byte{0x40}, 1}, // 010
		{[]byte{0x60}, 2}, // 011
		{[]byte{0x20}, 3}, // 00100
	} {
		got, ok := readUnsignedExpGolomb(tc.bits)
		if !ok || got != tc.want {
			t.Errorf("readUnsignedExpGolomb(%08b) = %d, %t; want %d, true", tc.bits, got, ok, tc.want)
		}
	}
}

func joinNALs(nals ...[]byte) []byte {
	var stream []byte
	for i, n := range nals {
		prefix := []byte{0, 0, 0, 1}
		if i%2 == 1 {
			prefix = []byte{0, 0, 1}
		}
		stream = append(stream, prefix...)
		stream = append(stream, n...)
	}
	return stream
}

func nal(nalType byte, payload ...byte) []byte {
	return append([]byte{0x60 | nalType}, payload...)
}

func countNALs(accessUnit []byte) int {
	count := 0
	for i := 0; i+3 < len(accessUnit); i++ {
		if accessUnit[i] == 0 && accessUnit[i+1] == 0 && accessUnit[i+2] == 0 && accessUnit[i+3] == 1 {
			count++
			i += 3
		}
	}
	return count
}

type limitedReader struct {
	reader io.Reader
	limit  int
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if len(p) > r.limit {
		p = p[:r.limit]
	}
	return r.reader.Read(p)
}
