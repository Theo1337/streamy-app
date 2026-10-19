package encode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"streamy/internal/config"
)

// The encoder cache remembers which encoder was proven to work, so that the
// half second of probing is paid once per ffmpeg version rather than on every
// hotkey press.
//
// It is a separate file rather than a field in config.json on purpose: the
// config file belongs to the user and is meant to be hand edited, and a
// machine-generated answer rewritten into it would be surprising.

// cacheFileName is the file inside the config directory.
const cacheFileName = "encoder-cache.json"

// cacheEntry is what is stored.
type cacheEntry struct {
	// FFmpegVersion is the first line of `ffmpeg -version`. Upgrading ffmpeg
	// changes it, which invalidates the entry: a new build may have fixed or
	// broken an encoder.
	FFmpegVersion string `json:"ffmpegVersion"`
	// Encoder is the encoding.encoder label that worked.
	Encoder string `json:"encoder"`
	// EncoderName is the -c:v value, kept so the file is readable.
	EncoderName string `json:"encoderName"`
	// ProbedAt is when the probe ran, for humans looking at the file.
	ProbedAt time.Time `json:"probedAt"`
}

// loadCache returns the cached encoder, if it is still valid.
func loadCache(ffmpegVersion string) (Encoder, bool) {
	path, err := cachePath()
	if err != nil {
		return Encoder{}, false
	}
	return loadCacheFrom(path, ffmpegVersion)
}

func loadCacheFrom(path, ffmpegVersion string) (Encoder, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Encoder{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		// A corrupt cache is not worth reporting: it only means probing again.
		return Encoder{}, false
	}
	if entry.FFmpegVersion != ffmpegVersion {
		return Encoder{}, false
	}
	enc, ok := Lookup(entry.Encoder)
	if !ok {
		return Encoder{}, false
	}
	return enc, true
}

// saveCache records the encoder that worked.
func saveCache(ffmpegVersion string, enc Encoder) error {
	path, err := cachePath()
	if err != nil {
		return err
	}
	return saveCacheTo(path, cacheEntry{
		FFmpegVersion: ffmpegVersion,
		Encoder:       enc.Label,
		EncoderName:   enc.Name,
		ProbedAt:      time.Now(),
	})
}

func saveCacheTo(path string, entry cacheEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// cachePath is the cache file inside the user's config directory.
func cachePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheFileName), nil
}
