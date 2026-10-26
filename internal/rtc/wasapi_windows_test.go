//go:build windows

package rtc

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestDiscordTreeRootIncludesRendererDescendants(t *testing.T) {
	processes := map[uint32]processEntry{
		10: {pid: 10, parent: 1, exeName: "Discord.exe"},
		11: {pid: 11, parent: 10, exeName: "Discord.exe"},
		12: {pid: 12, parent: 11, exeName: "Discord.exe"},
		13: {pid: 13, parent: 10, exeName: "Update.exe"},
	}
	if root, cyclic := discordTreeRoot(processes[12], processes); root.pid != 10 || cyclic {
		t.Fatalf("discordTreeRoot() = %d, cyclic %v, want root PID 10", root.pid, cyclic)
	}
	if root, cyclic := discordTreeRoot(processes[13], processes); root.pid != 10 || cyclic {
		t.Fatalf("discordTreeRoot() for a Discord descendant = %d, cyclic %v, want excluded tree root PID 10", root.pid, cyclic)
	}
}

// A recycled PID can leave two Discord processes naming each other as parent.
// Walking that used to spin forever, which hung every stream start.
func TestDiscordTreeRootStopsAtParentCycle(t *testing.T) {
	processes := map[uint32]processEntry{
		20: {pid: 20, parent: 21, exeName: "Discord.exe"},
		21: {pid: 21, parent: 20, exeName: "Discord.exe"},
		22: {pid: 22, parent: 20, exeName: "Discord.exe"},
	}
	if _, cyclic := discordTreeRoot(processes[22], processes); !cyclic {
		t.Fatal("discordTreeRoot() did not report the parent cycle")
	}

	entries := []processEntry{processes[20], processes[21], processes[22]}
	if root := widestDiscordRoot(entries, processes); root.pid != 20 {
		t.Fatalf("widestDiscordRoot() = %d, want PID 20", root.pid)
	}
}

func TestIsDiscordProcessMatchesDesktopVariants(t *testing.T) {
	for _, name := range []string{"Discord.exe", "DiscordCanary.exe", "DiscordPTB.exe", "DiscordDevelopment.exe"} {
		if !isDiscordProcess(name) {
			t.Errorf("isDiscordProcess(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"chrome.exe", "Discord Helper.exe"} {
		if isDiscordProcess(name) {
			t.Errorf("isDiscordProcess(%q) = true, want false", name)
		}
	}
}

func TestActivationCallbackSupportsAgileCOMInterfaces(t *testing.T) {
	for name, iid := range map[string]windows.GUID{
		"IUnknown": iunknownIID,
		"IActivateAudioInterfaceCompletionHandler": activateHandlerIID,
		"IAgileObject": agileObjectIID,
	} {
		if !supportsActivationCallbackIID(guidBytes(iid)) {
			t.Errorf("activation callback does not support %s", name)
		}
	}

	if supportsActivationCallbackIID(guidBytes(captureClientIID)) {
		t.Error("activation callback unexpectedly supports IAudioCaptureClient")
	}
}
