package capture

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Grabber is the ffmpeg input method used to read pixels off the screen.
type Grabber string

const (
	// GrabberDDAGrab uses the Desktop Duplication API through ffmpeg's ddagrab
	// filter. It hands out D3D11 textures straight from the compositor, which
	// is the only reliable way to capture an exclusive fullscreen game:
	// copying those frames with GDI tends to produce a black image.
	GrabberDDAGrab Grabber = "ddagrab"

	// GrabberGDigrab uses the GDI BitBlt path. It is slower, it cannot always
	// see fullscreen games, and it is the fallback. It is also the only
	// grabber that can target a single window.
	GrabberGDigrab Grabber = "gdigrab"
)

// Capabilities describes what the local ffmpeg can actually do. It is measured
// once at startup (see internal/encode) rather than assumed from the filter
// list, because "ddagrab is present" and "ddagrab works on this machine" are
// different statements.
type Capabilities struct {
	// DDAGrab is set when a test capture with ddagrab succeeded.
	DDAGrab bool
}

// ChooseGrabber picks the grabber to use for this target.
func (t Target) ChooseGrabber(caps Capabilities) Grabber {
	// ddagrab duplicates a whole output and has no concept of a single window.
	if t.Kind == KindWindow {
		return GrabberGDigrab
	}
	if caps.DDAGrab {
		return GrabberDDAGrab
	}
	return GrabberGDigrab
}

// InputOptions tunes how the capture source is opened.
type InputOptions struct {
	Grabber   Grabber
	FPS       int
	DrawMouse bool
	// Scale resizes the frames before they reach the encoder, for example
	// "1280x720". Doing it here rather than with encoder specific options keeps
	// the encoder arguments identical for every encoder.
	Scale string
}

// InputSpec is the ffmpeg side of a Target: the arguments that open the source,
// plus the video filter chain that prepares the frames.
type InputSpec struct {
	// Args are the input arguments, e.g. `-f gdigrab -i desktop`.
	Args []string
	// Filters is the video filter chain without the -vf flag. Empty when the
	// frames need no work at all.
	Filters string
	// Grabber records which grabber was used, for logging.
	Grabber Grabber
}

// InputSpec builds the ffmpeg arguments that open this target.
func (t Target) InputSpec(opts InputOptions) (InputSpec, error) {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}

	switch opts.Grabber {
	case GrabberDDAGrab:
		return t.ddagrabSpec(opts)
	case GrabberGDigrab:
		return t.gdigrabSpec(opts)
	case "":
		return InputSpec{}, errors.New("no grabber chosen")
	default:
		return InputSpec{}, fmt.Errorf("unknown grabber %q", opts.Grabber)
	}
}

// ddagrabSpec builds the Desktop Duplication input.
//
// ddagrab is a filter rather than a capture device, so it is attached to a
// lavfi source. It also needs a D3D11 device to hand its textures to, hence the
// `-init_hw_device d3d11va`, which is what makes `hwdownload` work afterwards.
func (t Target) ddagrabSpec(opts InputOptions) (InputSpec, error) {
	if t.Kind != KindMonitor {
		return InputSpec{}, errors.New("ddagrab can only capture a whole monitor")
	}

	args := []string{
		"-init_hw_device", "d3d11va",
		"-f", "lavfi",
		"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:draw_mouse=%d",
			t.Monitor.DXGIIndex, opts.FPS, boolToInt(opts.DrawMouse)),
	}

	// The frames live on the GPU, so they have to be downloaded before any
	// ordinary filter can touch them. `format=bgra` names the layout ddagrab
	// produces; from there ffmpeg converts to whatever the encoder wants.
	filters := []string{"hwdownload", "format=bgra"}
	filters = append(filters, rotationFilters(t.Monitor.Rotation)...)
	filters = append(filters, scaleFilters(opts.Scale)...)

	return InputSpec{Args: args, Filters: strings.Join(filters, ","), Grabber: GrabberDDAGrab}, nil
}

// gdigrabSpec builds the GDI input, for a monitor region or a single window.
func (t Target) gdigrabSpec(opts InputOptions) (InputSpec, error) {
	switch t.Kind {
	case KindMonitor:
		b := t.Monitor.Bounds
		if b.Empty() {
			return InputSpec{}, errors.New("monitor has an empty rectangle")
		}
		args := []string{
			"-f", "gdigrab",
			"-framerate", strconv.Itoa(opts.FPS),
			"-draw_mouse", strconv.Itoa(boolToInt(opts.DrawMouse)),
			// GDI grabs a rectangle of the whole virtual desktop, so the
			// monitor is selected purely by offset and size. That is why this
			// path needs no DXGI index at all.
			"-offset_x", strconv.Itoa(b.X),
			"-offset_y", strconv.Itoa(b.Y),
			"-video_size", fmt.Sprintf("%dx%d", b.Width, b.Height),
			"-i", "desktop",
		}
		// No rotation filter here: GDI reads the composited desktop, which DWM
		// has already rotated the way the user sees it, and so the frame comes
		// out portrait for a portrait monitor — the opposite of ddagrab.
		return InputSpec{Args: args, Filters: strings.Join(scaleFilters(opts.Scale), ","), Grabber: GrabberGDigrab}, nil

	case KindWindow:
		title := strings.TrimSpace(t.Window.Title)
		if title == "" {
			return InputSpec{}, errors.New("window has no title to match on")
		}
		// gdigrab selects a window by title, matched case insensitively and
		// allowing a partial match. The title needs ffmpeg's own escaping
		// because `:` separates options inside an input specification.
		args := []string{
			"-f", "gdigrab",
			"-framerate", strconv.Itoa(opts.FPS),
			"-draw_mouse", strconv.Itoa(boolToInt(opts.DrawMouse)),
			"-i", "title=" + escapeFFmpegValue(title),
		}
		return InputSpec{Args: args, Filters: strings.Join(scaleFilters(opts.Scale), ","), Grabber: GrabberGDigrab}, nil

	default:
		return InputSpec{}, fmt.Errorf("cannot capture a %s", t.Kind)
	}
}

// rotationFilters turns a display rotation into transpose filters.
//
// Desktop Duplication always hands out the display's unrotated surface. The
// portrait monitor used during development is reported by Windows as
// 1080x1920, while ddagrab produces a 1920x1080 image; these filters put it
// back the way the user sees it. 180 needs two transposes because ffmpeg has
// no "rotate half a turn" filter; `hflip,vflip` would work too.
func rotationFilters(rotation int) []string {
	switch rotation {
	case 90:
		return []string{"transpose=1"} // 90 degrees clockwise
	case 180:
		return []string{"transpose=1", "transpose=1"}
	case 270:
		return []string{"transpose=2"} // 90 degrees counter-clockwise
	default:
		return nil
	}
}

// scaleFilters resizes the frames when the settings ask for it.
//
// An explicit scale always ends with a rounding step because H.264 with
// 4:2:0 chroma needs even width and height, and users type odd numbers.
func scaleFilters(scale string) []string {
	scale = strings.TrimSpace(scale)
	if scale == "" {
		return nil
	}
	return []string{"scale=" + scale, "scale=trunc(iw/2)*2:trunc(ih/2)*2"}
}

// escapeFFmpegValue escapes the characters ffmpeg treats as syntax inside an
// option value, so a window title like "C:\tmp: notes" survives intact.
func escapeFFmpegValue(v string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`,`, `\,`,
		`'`, `\'`,
		`%`, `\%`,
	)
	return replacer.Replace(v)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
