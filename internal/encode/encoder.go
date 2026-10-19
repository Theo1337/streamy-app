package encode

import (
	"context"
	"fmt"

	"streamy/internal/config"
)

// Encoder is an H.264 encoder ffmpeg can drive.
type Encoder struct {
	// Name is the value passed to -c:v, for example "h264_nvenc".
	Name string
	// Label is the encoding.encoder value that names it, for example "nvenc".
	Label string
	// Hardware marks the GPU encoders. They cost almost no CPU, which is the
	// whole point of this app, but they are not present on every machine and
	// their quality per bit is slightly worse than x264's.
	Hardware bool
}

func (e Encoder) String() string {
	if e.Hardware {
		return fmt.Sprintf("%s (%s, hardware)", e.Name, e.Label)
	}
	return fmt.Sprintf("%s (%s, software)", e.Name, e.Label)
}

// The encoders, named after the config values that select them.
var (
	EncoderNVENC = Encoder{Name: "h264_nvenc", Label: config.EncoderNVENC, Hardware: true}
	EncoderQSV   = Encoder{Name: "h264_qsv", Label: config.EncoderQSV, Hardware: true}
	EncoderAMF   = Encoder{Name: "h264_amf", Label: config.EncoderAMF, Hardware: true}
	EncoderX264  = Encoder{Name: "libx264", Label: config.EncoderX264, Hardware: false}
)

// PreferenceOrder is the order "auto" tries encoders in.
//
// NVIDIA first, then Intel, then AMD, then the software encoder. The order
// matters only on machines with more than one capable GPU, which is rare; what
// matters far more is that every candidate is probed rather than assumed.
var PreferenceOrder = []Encoder{EncoderNVENC, EncoderQSV, EncoderAMF, EncoderX264}

// ForSetting returns the encoders to try for an encoding.encoder value, best
// first. An explicit choice yields exactly one candidate: if the user asked for
// nvenc, silently recording with x264 instead would be a lie.
func ForSetting(setting string) ([]Encoder, error) {
	switch setting {
	case config.EncoderNVENC:
		return []Encoder{EncoderNVENC}, nil
	case config.EncoderQSV:
		return []Encoder{EncoderQSV}, nil
	case config.EncoderAMF:
		return []Encoder{EncoderAMF}, nil
	case config.EncoderX264:
		return []Encoder{EncoderX264}, nil
	case config.EncoderAuto, "":
		return PreferenceOrder, nil
	default:
		return nil, fmt.Errorf("unknown encoding.encoder %q (want auto, nvenc, qsv, amf or x264)", setting)
	}
}

// Probe encodes one synthetic frame with the exact arguments a real capture
// would use.
//
// The synthetic source matters: probing with a screen capture would be slower,
// would depend on the desktop, and on some platforms would flash a recording
// permission prompt at startup. Something that only proves the encoder
// initialises is enough, because that is where hardware encoders fail.
//
// Using the real argument builder matters more. It means an option this
// particular encoder does not support is caught here, at selection time, rather
// than halfway through a stream. The AMF encoder on the development machine is
// a good example: it rejects `-profile:v baseline` at the option parsing stage.
func (f FFmpeg) Probe(ctx context.Context, cfg config.Config, enc Encoder) error {
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=30",
		"-frames:v", "1",
	}
	args = append(args, videoArgs(cfg, enc, probeFPS)...)
	args = append(args, "-f", "null", "-")

	if _, err := f.output(ctx, args...); err != nil {
		return fmt.Errorf("%s cannot encode: %w", enc.Name, err)
	}
	return nil
}

// probeFPS is the frame rate used for probes. It does not have to match the
// capture frame rate; it only has to be a rate the synthetic source can make.
const probeFPS = 30

// probeAll probes the candidates in order and returns the first that works.
//
// The failures are returned alongside the winner so the log can say why each
// one was rejected, which is the difference between "it fell back to x264" and
// "it fell back to x264 because h264_nvenc reported no capable devices".
func (f FFmpeg) probeAll(ctx context.Context, cfg config.Config, candidates []Encoder, log Logger) (Encoder, []error) {
	var failures []error

	for _, enc := range candidates {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return Encoder{}, failures
		}
		if log != nil {
			log.Debug("probing encoder", "encoder", enc.Name)
		}
		if err := f.Probe(ctx, cfg, enc); err != nil {
			failures = append(failures, err)
			continue
		}
		return enc, failures
	}
	return Encoder{}, failures
}

// Select decides which encoder to use, using the cache when it can.
//
// With encoding.encoder set to "auto" an encoder that was proven to work
// earlier in this ffmpeg version is used straight away: probing costs roughly
// half a second, and paying that on every hotkey press would be felt. With an
// explicit setting there is nothing to decide, and the probe is the only way to
// tell the user their GPU cannot do what they asked for.
func Select(ctx context.Context, f FFmpeg, cfg config.Config, log Logger) (Encoder, error) {
	candidates, err := ForSetting(cfg.Encoding.Encoder)
	if err != nil {
		return Encoder{}, err
	}

	explicit := cfg.Encoding.Encoder != config.EncoderAuto && cfg.Encoding.Encoder != ""

	if !explicit {
		if enc, ok := loadCache(f.Version()); ok {
			if containsEncoder(candidates, enc) {
				if log != nil {
					log.Debug("using the cached encoder", "encoder", enc.Name)
				}
				return enc, nil
			}
		}
	}

	enc, failures := f.probeAll(ctx, cfg, candidates, log)
	if enc.Name == "" {
		return Encoder{}, noEncoderError(cfg.Encoding.Encoder, failures)
	}

	for _, failure := range failures {
		if log != nil {
			log.Warn("encoder unavailable, trying the next one", "reason", failure)
		}
	}

	if !explicit {
		if err := saveCache(f.Version(), enc); err != nil && log != nil {
			// A cache that cannot be written is not worth failing a stream
			// over; it only costs the next start half a second.
			log.Debug("could not write the encoder cache", "error", err)
		}
	}
	return enc, nil
}

// noEncoderError explains a total failure in terms the user can act on.
func noEncoderError(setting string, failures []error) error {
	if setting == config.EncoderAuto || setting == "" {
		return fmt.Errorf("no working H.264 encoder found; ffmpeg was asked for nvenc, qsv, amf and x264 and all of them failed: %w", joinErrors(failures))
	}
	return fmt.Errorf("encoding.encoder is set to %q, which does not work on this machine: %w; set it to \"auto\", or to \"x264\" to use the CPU encoder", setting, joinErrors(failures))
}

func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return fmt.Errorf("no candidates were tried")
	case 1:
		return errs[0]
	}
	out := errs[0]
	for _, err := range errs[1:] {
		out = fmt.Errorf("%w; also: %v", out, err)
	}
	return out
}

func containsEncoder(encs []Encoder, want Encoder) bool {
	for _, e := range encs {
		if e.Name == want.Name {
			return true
		}
	}
	return false
}

// Lookup finds an encoder by its encoding.encoder label.
func Lookup(label string) (Encoder, bool) {
	for _, e := range PreferenceOrder {
		if e.Label == label {
			return e, true
		}
	}
	return Encoder{}, false
}
