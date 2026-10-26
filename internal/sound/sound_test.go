package sound

import (
	"bytes"
	"os"
	"testing"
)

func TestMaterializeWritesEmbeddedCue(t *testing.T) {
	// Keep the test out of the real user cache.
	t.Setenv("LocalAppData", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	path, err := materialize("start.mp3")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized cue: %v", err)
	}
	want, err := assets.ReadFile("assets/start.mp3")
	if err != nil {
		t.Fatalf("read embedded cue: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("materialized cue differs from the embedded file")
	}
}
