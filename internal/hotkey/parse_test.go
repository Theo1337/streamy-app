package hotkey

import (
	"testing"

	"golang.design/x/hotkey"
)

func TestParse(t *testing.T) {
	// Table driven tests are the Go idiom for "many inputs, same expectation".
	tests := []struct {
		in        string
		want      string // canonical form
		wantMods  []hotkey.Modifier
		wantKey   hotkey.Key
		expectErr bool
	}{
		{in: "Ctrl+Shift+F9", want: "Ctrl+Shift+F9", wantKey: hotkey.KeyF9, wantMods: []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}},
		// Case and spacing do not matter.
		{in: "ctrl + shift + f9", want: "Ctrl+Shift+F9", wantKey: hotkey.KeyF9, wantMods: []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}},
		// Order is normalised to Ctrl, Shift, Alt, Win.
		{in: "Shift+Ctrl+A", want: "Ctrl+Shift+A", wantKey: hotkey.KeyA, wantMods: []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}},
		// Aliases.
		{in: "control+space", want: "Ctrl+Space", wantKey: hotkey.KeySpace, wantMods: []hotkey.Modifier{hotkey.ModCtrl}},
		{in: "Super+Alt+Esc", want: "Alt+Win+Esc", wantKey: hotkey.KeyEscape, wantMods: []hotkey.Modifier{hotkey.ModAlt, hotkey.ModWin}},
		{in: "Win+Alt+S", want: "Alt+Win+S", wantKey: hotkey.KeyS, wantMods: []hotkey.Modifier{hotkey.ModAlt, hotkey.ModWin}},
		{in: "Ctrl+5", want: "Ctrl+5", wantKey: hotkey.Key5, wantMods: []hotkey.Modifier{hotkey.ModCtrl}},
		{in: "Ctrl+F20", want: "Ctrl+F20", wantKey: hotkey.KeyF20, wantMods: []hotkey.Modifier{hotkey.ModCtrl}},
		// Duplicated modifiers collapse.
		{in: "Ctrl+Control+X", want: "Ctrl+X", wantKey: hotkey.KeyX, wantMods: []hotkey.Modifier{hotkey.ModCtrl}},

		// Errors: each of these must be rejected with a message the user can
		// act on.
		{in: "", expectErr: true},
		{in: "F9", expectErr: true},             // no modifier
		{in: "Ctrl", expectErr: true},           // no key
		{in: "Ctrl+", expectErr: true},          // empty part
		{in: "Ctrl+F99", expectErr: true},       // no such function key
		{in: "Ctrl+Shift+", expectErr: true},    // trailing plus
		{in: "Hyper+Z", expectErr: true},        // unknown modifier
		{in: "Ctrl+Shift+F9X", expectErr: true}, // unknown key
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			spec, err := Parse(tc.in)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, want an error", tc.in, spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", tc.in, err)
			}
			if got := spec.String(); got != tc.want {
				t.Errorf("Parse(%q).String() = %q, want %q", tc.in, got, tc.want)
			}
			if spec.Key != tc.wantKey {
				t.Errorf("Parse(%q).Key = %#x, want %#x", tc.in, spec.Key, tc.wantKey)
			}
			if len(spec.Mods) != len(tc.wantMods) {
				t.Fatalf("Parse(%q).Mods = %v, want %v", tc.in, spec.Mods, tc.wantMods)
			}
			for i, mod := range tc.wantMods {
				if spec.Mods[i] != mod {
					t.Errorf("Parse(%q).Mods[%d] = %v, want %v", tc.in, i, spec.Mods[i], mod)
				}
			}
		})
	}
}
