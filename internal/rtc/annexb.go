package rtc

import (
	"bufio"
	"errors"
	"io"
)

var errIncompleteNAL = errors.New("incomplete H.264 NAL unit")

// AccessUnitReader groups Annex-B NAL units into the video frames WebRTC
// expects. ffmpeg's raw H.264 output is a byte stream, not one frame per read.
type AccessUnitReader struct {
	nals *nalReader

	current []byte
	hasVCL  bool
	eof     bool
}

// NewAccessUnitReader reads Annex-B H.264 from r.
func NewAccessUnitReader(r io.Reader) *AccessUnitReader {
	return &AccessUnitReader{nals: &nalReader{reader: bufio.NewReaderSize(r, 64*1024)}}
}

// Next returns the next access unit, or io.EOF after the final one.
func (r *AccessUnitReader) Next() ([]byte, error) {
	if r.eof {
		return nil, io.EOF
	}

	for {
		nal, err := r.nals.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.eof = true
				if len(r.current) == 0 {
					return nil, io.EOF
				}
				out := r.current
				r.current = nil
				r.hasVCL = false
				return out, nil
			}
			return nil, err
		}

		nalType := nal[4] & 0x1f
		newAccessUnit := false
		switch {
		case nalType == 9: // Access unit delimiter.
			newAccessUnit = r.hasVCL
		case r.hasVCL && (nalType == 6 || nalType == 7 || nalType == 8 || nalType == 14 || nalType == 15 || nalType == 16 || nalType == 17 || nalType == 18):
			// These non-VCL NAL units precede the next picture in Annex-B
			// streams, so leave them with that picture rather than the prior one.
			newAccessUnit = true
		case r.hasVCL && isVCL(nalType):
			firstMB, ok := firstMacroblock(nal)
			newAccessUnit = ok && firstMB == 0
		}

		if newAccessUnit {
			out := r.current
			r.current = nal
			r.hasVCL = isVCL(nalType)
			return out, nil
		}
		r.current = append(r.current, nal...)
		if isVCL(nalType) {
			r.hasVCL = true
		}
	}
}

// Drain consumes the rest of the encoded stream. It lets ffmpeg finish
// writing stdout cleanly if a downstream peer stops accepting samples.
func (r *AccessUnitReader) Drain() error {
	_, err := io.Copy(io.Discard, r.nals.reader)
	return err
}

func isVCL(nalType byte) bool {
	return nalType >= 1 && nalType <= 5
}

// nalReader returns individual NAL units, normalizing each start code to four
// bytes. Emulation prevention in H.264 guarantees 00 00 01 cannot occur inside
// a NAL payload.
type nalReader struct {
	reader     *bufio.Reader
	started    bool
	nextStart  bool
	currentNAL []byte
	zeroRun    int
}

func (r *nalReader) next() ([]byte, error) {
	if r.nextStart {
		r.currentNAL = []byte{0, 0, 0, 1}
		r.zeroRun = 0
		r.nextStart = false
	}

	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, err
			}
			if !r.started {
				return nil, io.EOF
			}
			if len(r.currentNAL) == 4 {
				return nil, errIncompleteNAL
			}
			r.currentNAL = appendZeros(r.currentNAL, r.zeroRun)
			r.zeroRun = 0
			result := r.currentNAL
			r.currentNAL = nil
			r.started = false
			return result, nil
		}

		if !r.started {
			if b == 0 {
				r.zeroRun++
				continue
			}
			if b == 1 && r.zeroRun >= 2 {
				r.started = true
				r.nextStart = false
				r.currentNAL = []byte{0, 0, 0, 1}
			}
			r.zeroRun = 0
			continue
		}

		if b == 0 {
			r.zeroRun++
			continue
		}
		if b == 1 && r.zeroRun >= 2 {
			if len(r.currentNAL) == 4 {
				return nil, errIncompleteNAL
			}
			r.nextStart = true
			result := r.currentNAL
			r.currentNAL = nil
			return result, nil
		}

		r.currentNAL = appendZeros(r.currentNAL, r.zeroRun)
		r.zeroRun = 0
		r.currentNAL = append(r.currentNAL, b)
	}
}

func appendZeros(dst []byte, count int) []byte {
	for range count {
		dst = append(dst, 0)
	}
	return dst
}

// firstMacroblock reads first_mb_in_slice, the first Exp-Golomb value in a
// slice header, after removing H.264 emulation-prevention bytes.
func firstMacroblock(nal []byte) (uint64, bool) {
	if len(nal) < 6 {
		return 0, false
	}
	rbsp := make([]byte, 0, len(nal)-5)
	zeros := 0
	for _, b := range nal[5:] {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return readUnsignedExpGolomb(rbsp)
}

func readUnsignedExpGolomb(data []byte) (uint64, bool) {
	var leadingZeros uint
	for bit := 0; bit < len(data)*8; bit++ {
		value := (data[bit/8] >> (7 - uint(bit%8))) & 1
		if value == 1 {
			if leadingZeros == 0 {
				return 0, true
			}
			var suffix uint64
			for i := uint(0); i < leadingZeros; i++ {
				next := bit + 1 + int(i)
				if next >= len(data)*8 {
					return 0, false
				}
				suffix = (suffix << 1) | uint64((data[next/8]>>(7-uint(next%8)))&1)
			}
			return (uint64(1) << leadingZeros) - 1 + suffix, true
		}
		leadingZeros++
		if leadingZeros > 31 {
			return 0, false
		}
	}
	return 0, false
}
