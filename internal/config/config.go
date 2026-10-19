// Package config loads and stores Streamy's settings as JSON in the user's
// config directory.
//
// Two Go ideas are worth pointing out in this file:
//
//  1. We decode into a fully populated Default() value instead of into a zero
//     Config. encoding/json only overwrites the fields it finds, so settings
//     added in a later version automatically get their default value when an
//     older config.json is loaded. That is our migration story for free.
//
//  2. Saving is atomic: write a temporary file, then rename it over the real
//     one. A crash halfway through a write can therefore never leave you with a
//     half written config.json.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"streamy/internal/hotkey"
)

// CurrentSchema is stored in the file so a future version can recognise configs
// written by an older one.
const CurrentSchema = 1

// AppDirName is the folder created inside the operating system's config
// directory. On Windows that is %AppData%\streamy.
const AppDirName = "streamy"

// The values accepted by Capture.DefaultTarget.
const (
	TargetPrimary = "primary" // the primary monitor
	TargetMouse   = "mouse"   // the monitor under the mouse cursor
	TargetFocused = "focused" // whatever window currently has focus
	TargetMonitor = "monitor" // the monitor at Capture.MonitorIndex
)

// The values accepted by Encoding.Encoder.
const (
	EncoderAuto  = "auto" // detect the best available encoder
	EncoderNVENC = "nvenc"
	EncoderQSV   = "qsv"
	EncoderAMF   = "amf"
	EncoderX264  = "x264" // software fallback
)

// Config is the whole settings tree. Every field has a json tag so the file is
// readable and hand-editable.
type Config struct {
	Schema int `json:"schema"`

	LogLevel string `json:"logLevel"`

	Hotkey    Hotkey    `json:"hotkey"`
	Capture   Capture   `json:"capture"`
	Encoding  Encoding  `json:"encoding"`
	Stream    Stream    `json:"stream"`
	Signaling Signaling `json:"signaling"`
}

// Hotkey holds the global shortcuts. Both are parsed by internal/hotkey, e.g.
// "Ctrl+Shift+F9".
type Hotkey struct {
	Enabled bool `json:"enabled"`
	// Toggle starts a stream when idle and stops it when streaming.
	Toggle string `json:"toggle"`
	// Picker always opens the screen/window picker (phase 6).
	Picker string `json:"picker"`
}

// Capture holds the "what do we record" settings, including the automatic
// target selection rules described in INSTRUCTIONS.md.
type Capture struct {
	// DefaultTarget is used when the hotkey cannot find a fullscreen app.
	DefaultTarget string `json:"defaultTarget"`
	// MonitorIndex is the monitor used when DefaultTarget is "monitor".
	MonitorIndex int `json:"monitorIndex"`
	// AlwaysShowPicker disables auto-selection entirely.
	AlwaysShowPicker bool `json:"alwaysShowPicker"`
	// AutoSwitchTarget follows the fullscreen app while streaming. Off by
	// default because switching targets restarts the encoder.
	AutoSwitchTarget bool `json:"autoSwitchTarget"`
	// CaptureCursor draws the mouse pointer into the video.
	CaptureCursor bool `json:"captureCursor"`
}

// Encoding holds the ffmpeg related settings.
type Encoding struct {
	Encoder string `json:"encoder"`
	// FFmpegPath is an explicit path to ffmpeg.exe; empty means "look it up on
	// PATH".
	FFmpegPath string `json:"ffmpegPath"`
	// Resolution is "source" (no scaling) or an explicit "1920x1080".
	Resolution string `json:"resolution"`
	FPS        int    `json:"fps"`
	// BitrateKbps is the target video bitrate in kilobits per second.
	BitrateKbps int `json:"bitrateKbps"`
	// KeyframeSeconds is the GOP length. About one per second keeps new viewers
	// waiting at most a second before they can decode.
	KeyframeSeconds int `json:"keyframeSeconds"`
	// EncoderThreads is passed to libx264 (0 means "let ffmpeg decide").
	EncoderThreads int `json:"encoderThreads"`
	// ExtraEncoderArgs are appended to the ffmpeg command line, for tuning.
	ExtraEncoderArgs []string `json:"extraEncoderArgs"`
}

// Stream holds the WebRTC settings.
type Stream struct {
	MaxViewers  int          `json:"maxViewers"`
	STUNServers []string     `json:"stunServers"`
	TURNServers []TURNServer `json:"turnServers"`
}

// TURNServer is one relay, used when a direct peer-to-peer path is impossible.
type TURNServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// Signaling points at the Cloudflare Worker written in phase 4.
type Signaling struct {
	URL string `json:"url"`
}

// Default returns the settings used on first run, and the base that a loaded
// file is decoded on top of.
func Default() Config {
	return Config{
		Schema:   CurrentSchema,
		LogLevel: "info",
		Hotkey: Hotkey{
			Enabled: true,
			Toggle:  "Ctrl+Shift+F9",
			Picker:  "Ctrl+Shift+F10",
		},
		Capture: Capture{
			DefaultTarget: TargetPrimary,
			MonitorIndex:  0,
			CaptureCursor: true,
		},
		Encoding: Encoding{
			Encoder:         EncoderAuto,
			Resolution:      "source",
			FPS:             30,
			BitrateKbps:     6000,
			KeyframeSeconds: 1,
		},
		Stream: Stream{
			MaxViewers: 10,
			// Google's public STUN servers: free, no account, good enough to
			// discover your public address for most home networks.
			STUNServers: []string{
				"stun:stun.l.google.com:19302",
				"stun:stun1.l.google.com:19302",
			},
		},
	}
}

// Dir returns the directory Streamy keeps its config and logs in.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, AppDirName), nil
}

// DefaultPath returns the path of the default config file.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LogPath returns the path of the default log file.
func LogPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "streamy.log"), nil
}

// Load reads the config from the default location.
func Load() (Config, bool, error) {
	path, err := DefaultPath()
	if err != nil {
		return Config{}, false, err
	}
	return LoadFrom(path)
}

// LoadFrom reads the config from path. A missing file is not an error: the
// defaults are written there and returned, with created reporting that this
// happened.
func LoadFrom(path string) (cfg Config, created bool, err error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = Default()
		if err := cfg.SaveTo(path); err != nil {
			return cfg, false, err
		}
		return cfg, true, nil
	case err != nil:
		return Config{}, false, fmt.Errorf("read config: %w", err)
	}

	// The file is meant to be hand editable, and a good few Windows tools still
	// write a UTF-8 byte order mark at the start: PowerShell's Set-Content, and
	// any editor set to "UTF-8 with BOM". JSON does not allow one, and the
	// failure is nasty, because the settings look fine and are silently
	// ignored in favour of the defaults. Strip it instead of complaining.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	// Unknown fields are deliberately ignored, so a config written by a newer
	// version still loads in an older one.
	cfg = Default()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, false, err
	}
	return cfg, false, nil
}

// Save writes the config to the default location.
func (c Config) Save() error {
	path, err := DefaultPath()
	if err != nil {
		return err
	}
	return c.SaveTo(path)
}

// SaveTo writes the config to path atomically.
func (c Config) SaveTo(path string) error {
	c.Schema = CurrentSchema

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')

	// Write to a sibling temp file first so a partial write can never be
	// swapped in. os.Rename replaces the destination on both Windows and Unix.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best effort cleanup if we bail out before the rename.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// Validate checks the values a user could have typed by hand, so mistakes are
// reported at startup instead of later inside ffmpeg.
func (c Config) Validate() error {
	var errs []error

	if c.Schema > CurrentSchema {
		errs = append(errs, fmt.Errorf("config schema %d is newer than this build understands (%d)", c.Schema, CurrentSchema))
	}

	if c.Hotkey.Enabled {
		if _, err := hotkey.Parse(c.Hotkey.Toggle); err != nil {
			errs = append(errs, fmt.Errorf("hotkey.toggle: %w", err))
		}
	}
	if _, err := hotkey.Parse(c.Hotkey.Picker); err != nil {
		errs = append(errs, fmt.Errorf("hotkey.picker: %w", err))
	}

	switch c.Capture.DefaultTarget {
	case TargetPrimary, TargetMouse, TargetFocused, TargetMonitor:
	case "":
		errs = append(errs, errors.New("capture.defaultTarget: must not be empty"))
	default:
		errs = append(errs, fmt.Errorf("capture.defaultTarget: unknown value %q", c.Capture.DefaultTarget))
	}
	if c.Capture.MonitorIndex < 0 {
		errs = append(errs, errors.New("capture.monitorIndex: must not be negative"))
	}

	switch c.Encoding.Encoder {
	case EncoderAuto, EncoderNVENC, EncoderQSV, EncoderAMF, EncoderX264:
	case "":
		errs = append(errs, errors.New("encoding.encoder: must not be empty"))
	default:
		errs = append(errs, fmt.Errorf("encoding.encoder: unknown value %q (want auto, nvenc, qsv, amf or x264)", c.Encoding.Encoder))
	}
	if err := validateResolution(c.Encoding.Resolution); err != nil {
		errs = append(errs, err)
	}
	if c.Encoding.FPS < 1 || c.Encoding.FPS > 240 {
		errs = append(errs, fmt.Errorf("encoding.fps: %d is out of range (1-240)", c.Encoding.FPS))
	}
	if c.Encoding.BitrateKbps < 100 {
		errs = append(errs, fmt.Errorf("encoding.bitrateKbps: %d is too low (minimum 100)", c.Encoding.BitrateKbps))
	}
	if c.Encoding.KeyframeSeconds < 1 || c.Encoding.KeyframeSeconds > 10 {
		errs = append(errs, fmt.Errorf("encoding.keyframeSeconds: %d is out of range (1-10)", c.Encoding.KeyframeSeconds))
	}
	if c.Encoding.EncoderThreads < 0 {
		errs = append(errs, errors.New("encoding.encoderThreads: must not be negative"))
	}

	if c.Stream.MaxViewers < 0 {
		errs = append(errs, errors.New("stream.maxViewers: must not be negative"))
	}

	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		errs = append(errs, fmt.Errorf("logLevel: unknown level %q", c.LogLevel))
	}

	if u := strings.TrimSpace(c.Signaling.URL); u != "" {
		parsed, err := url.Parse(u)
		if err != nil {
			errs = append(errs, fmt.Errorf("signaling.url: %w", err))
		} else if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
			errs = append(errs, fmt.Errorf("signaling.url: scheme must be ws or wss, got %q", parsed.Scheme))
		}
	}

	return errors.Join(errs...)
}

// resolutionRe matches "1920x1080" and similar.
var resolutionRe = regexp.MustCompile(`^(\d{2,5})x(\d{2,5})$`)

func validateResolution(res string) error {
	value := strings.ToLower(strings.TrimSpace(res))
	if value == "source" {
		return nil
	}
	m := resolutionRe.FindStringSubmatch(value)
	if m == nil {
		return fmt.Errorf("encoding.resolution: %q is not \"source\" or WIDTHxHEIGHT", res)
	}
	for _, part := range m[1:] {
		n, err := strconv.Atoi(part)
		if err != nil || n == 0 {
			return fmt.Errorf("encoding.resolution: %q has a non positive dimension", res)
		}
	}
	return nil
}
