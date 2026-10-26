//go:build windows

package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTmpPreviewChrome(t *testing.T) {
	EnableDPIAwareness()
	windows := topLevelWindows()
	index := 0
	for _, w := range windows {
		if !strings.Contains(w.Title, "Chrome") &&
			!strings.Contains(w.Title, "Discord") &&
			!strings.Contains(w.Title, "WindowsTerminal") &&
			!strings.Contains(w.Title, "Notepad") {
			continue
		}
		index++
		t.Logf("window %d: %q %dx%d handle=%d", index, w.Title, w.Rect.Width, w.Rect.Height, w.Handle)
		data, err := Preview(context.Background(), Target{Kind: KindWindow, Window: w}, 512, 512)
		if err != nil {
			t.Logf("preview failed: %v", err)
			continue
		}
		path := filepath.Join(os.TempDir(), fmt.Sprintf("streamy-printwindow-%d.jpg", index))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(data))
	}
}
