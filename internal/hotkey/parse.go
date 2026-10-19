package hotkey

import (
	"errors"
	"fmt"
	"strings"

	"golang.design/x/hotkey"
)

// modifierNames holds every accepted spelling for the modifier keys. Values are
// the flags the operating system expects.
var modifierNames = map[string]hotkey.Modifier{
	"CTRL":    hotkey.ModCtrl,
	"CONTROL": hotkey.ModCtrl,
	"SHIFT":   hotkey.ModShift,
	"ALT":     hotkey.ModAlt,
	"WIN":     hotkey.ModWin,
	"SUPER":   hotkey.ModWin,
	"CMD":     hotkey.ModWin,
	"META":    hotkey.ModWin,
}

// keyNames maps an upper case name to the virtual key code, and keyDisplay maps
// back from the code to the nicest spelling of it. Both are built once at start
// up by the anonymous function below.
var keyNames, keyDisplay = func() (map[string]hotkey.Key, map[hotkey.Key]string) {
	names := make(map[string]hotkey.Key, 80)
	display := make(map[hotkey.Key]string, 80)

	add := func(name string, k hotkey.Key) {
		names[strings.ToUpper(name)] = k
		if _, seen := display[k]; !seen {
			display[k] = name // first spelling added wins
		}
	}

	add("Space", hotkey.KeySpace)
	add("Enter", hotkey.KeyReturn)
	add("Return", hotkey.KeyReturn)
	add("Esc", hotkey.KeyEscape)
	add("Escape", hotkey.KeyEscape)
	add("Tab", hotkey.KeyTab)
	add("Delete", hotkey.KeyDelete)
	add("Del", hotkey.KeyDelete)
	add("Left", hotkey.KeyLeft)
	add("Right", hotkey.KeyRight)
	add("Up", hotkey.KeyUp)
	add("Down", hotkey.KeyDown)

	// Media keys are handy for a "push to talk to my viewers" style shortcut.
	add("PlayPause", hotkey.KeyMediaPlayPause)
	add("MediaNext", hotkey.KeyMediaNext)
	add("MediaPrev", hotkey.KeyMediaPrev)
	add("MediaStop", hotkey.KeyMediaStop)
	add("VolumeUp", hotkey.KeyVolumeUp)
	add("VolumeDown", hotkey.KeyVolumeDown)
	add("VolumeMute", hotkey.KeyVolumeMute)
	add("Mute", hotkey.KeyVolumeMute)

	// Letters and digits. hotkey.KeyA is 0x41 ('A') and the letters are
	// contiguous, same for the digits starting at 0x30.
	for c := 'A'; c <= 'Z'; c++ {
		add(string(c), hotkey.Key(c))
	}
	for d := '0'; d <= '9'; d++ {
		add(string(d), hotkey.Key(d))
	}
	// Function keys: KeyF1 is 0x70 and they are contiguous up to F20.
	for i := 1; i <= 20; i++ {
		add(fmt.Sprintf("F%d", i), hotkey.KeyF1+hotkey.Key(i-1))
	}

	return names, display
}()

// modifierDisplay is the canonical order shortcuts are printed in.
var modifierDisplay = []struct {
	mod hotkey.Modifier
	pos int
}{
	{hotkey.ModCtrl, 0},
	{hotkey.ModShift, 1},
	{hotkey.ModAlt, 2},
	{hotkey.ModWin, 3},
}

// Spec is a shortcut that has been parsed and can be registered.
type Spec struct {
	// Mods are the modifier flags to register, in canonical order.
	Mods []hotkey.Modifier
	// Key is the main key of the shortcut.
	Key hotkey.Key
	// raw is the canonical spelling of the shortcut.
	raw string
}

// String returns the shortcut in its canonical form, e.g. "Ctrl+Shift+F9".
func (s Spec) String() string { return s.raw }

// Parse converts a human readable shortcut such as "Ctrl+Shift+F9", "win+alt+s"
// or "Ctrl + Shift + Space" into a Spec.
//
// At least one modifier is required on purpose: a global hotkey that swallows a
// bare key would break typing in every other application on the machine.
func Parse(shortcut string) (Spec, error) {
	raw := strings.TrimSpace(shortcut)
	if raw == "" {
		return Spec{}, errors.New("hotkey is empty")
	}

	parts := strings.Split(raw, "+")
	if len(parts) < 2 {
		return Spec{}, fmt.Errorf("hotkey %q needs at least one modifier, for example \"Ctrl+Shift+F9\"", raw)
	}

	var (
		mods    []hotkey.Modifier
		seen    = make(map[hotkey.Modifier]bool, 4)
		key     hotkey.Key
		haveKey bool
	)

	for i, part := range parts {
		name := strings.ToUpper(strings.TrimSpace(part))
		if name == "" {
			return Spec{}, fmt.Errorf("hotkey %q has an empty part around a \"+\"", raw)
		}

		// The key is always the last part; everything before it is a modifier.
		if i == len(parts)-1 {
			k, ok := keyNames[name]
			if !ok {
				return Spec{}, fmt.Errorf("hotkey %q: unknown key %q", raw, part)
			}
			key, haveKey = k, true
			continue
		}

		mod, ok := modifierNames[name]
		if !ok {
			return Spec{}, fmt.Errorf("hotkey %q: %q is not a modifier (use Ctrl, Shift, Alt or Win)", raw, part)
		}
		if seen[mod] {
			continue // "Ctrl+Control+X" is just "Ctrl+X"
		}
		seen[mod] = true
		mods = append(mods, mod)
	}

	if !haveKey {
		return Spec{}, fmt.Errorf("hotkey %q has no key after the modifiers", raw)
	}
	if len(mods) == 0 {
		return Spec{}, fmt.Errorf("hotkey %q needs at least one modifier", raw)
	}

	// Sort the modifiers into a fixed order so the same shortcut always prints
	// the same way, no matter how it was typed.
	ordered := make([]hotkey.Modifier, 0, len(mods))
	for _, m := range modifierDisplay {
		if seen[m.mod] {
			ordered = append(ordered, m.mod)
		}
	}

	var b strings.Builder
	for _, m := range modifierDisplay {
		if !seen[m.mod] {
			continue
		}
		b.WriteString(modifierLabel(m.mod))
		b.WriteByte('+')
	}
	b.WriteString(keyDisplay[key])

	return Spec{Mods: ordered, Key: key, raw: b.String()}, nil
}

func modifierLabel(m hotkey.Modifier) string {
	switch m {
	case hotkey.ModCtrl:
		return "Ctrl"
	case hotkey.ModShift:
		return "Shift"
	case hotkey.ModAlt:
		return "Alt"
	case hotkey.ModWin:
		return "Win"
	default:
		return "Mod"
	}
}
