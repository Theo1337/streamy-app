// Package sound plays the short cues that mark the start and end of a session.
//
// The files are embedded so the tray executable keeps working when it is moved
// on its own, and playback is best effort: a machine without an audio device
// must never fail a stream.
package sound

import (
	"bytes"
	"embed"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

//go:embed assets/*.mp3
var assets embed.FS

var (
	extractMu sync.Mutex
	extracted = map[string]string{}
)

// PlayStart plays the cue used when a session starts. It returns immediately.
func PlayStart() { play("start.mp3") }

// PlayStop plays the cue used when a session stops. It returns immediately.
func PlayStop() { play("stop.mp3") }

func play(name string) {
	go func() {
		if err := playFile(name); err != nil {
			slog.Debug("could not play sound cue", "sound", name, "error", err)
		}
	}()
}

// materialize writes an embedded sound to the user's cache directory and
// returns its path. Windows' MCI player works on files, not byte slices.
func materialize(name string) (string, error) {
	extractMu.Lock()
	defer extractMu.Unlock()
	if path, ok := extracted[name]; ok {
		return path, nil
	}

	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return "", err
	}

	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "Streamy", "sounds")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	path := filepath.Join(dir, name)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
		extracted[name] = path
		return path, nil
	}

	// Write beside the destination first so a half-written file is never
	// handed to the player.
	tmp, err := os.CreateTemp(dir, ".streamy-sound-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	extracted[name] = path
	return path, nil
}
