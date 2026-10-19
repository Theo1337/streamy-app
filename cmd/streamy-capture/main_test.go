package main

import (
	"strings"
	"testing"
)

// A windowed capture has to hand gdigrab the window's title, because a title is
// the only way gdigrab can name a window. Printed output, though, ends up in
// terminals and bug reports, so by default the title is counted, not shown.
func TestFormatCommandRedactsWindowTitles(t *testing.T) {
	args := []string{
		"-hide_banner",
		"-f", "gdigrab",
		"-framerate", "30",
		"-i", `title=divorce-papers-DRAFT.docx`,
		"-c:v", "libx264",
	}

	t.Run("hidden by default", func(t *testing.T) {
		got := formatCommand(`C:\ffmpeg\bin\ffmpeg.exe`, args, false)
		if strings.Contains(got, "divorce-papers") {
			t.Errorf("the command leaks the window title:\n%s", got)
		}
		if !strings.Contains(got, "character window title, hidden") {
			t.Errorf("the redaction does not say what was hidden:\n%s", got)
		}
		// It must say how much was hidden, so the output still shows that a
		// title was passed at all.
		if !strings.Contains(got, "title=<25 character") {
			t.Errorf("the redaction does not report the title's length:\n%s", got)
		}
		// Everything else has to survive, or the command is not useful.
		for _, want := range []string{"gdigrab", "-framerate", "30", "libx264"} {
			if !strings.Contains(got, want) {
				t.Errorf("the redacted command lost %q:\n%s", want, got)
			}
		}
	})

	t.Run("shown with -titles", func(t *testing.T) {
		got := formatCommand(`C:\ffmpeg\bin\ffmpeg.exe`, args, true)
		if !strings.Contains(got, "divorce-papers-DRAFT.docx") {
			t.Errorf("-titles should print the title verbatim:\n%s", got)
		}
		if strings.Contains(got, "hidden") {
			t.Errorf("-titles still redacted something:\n%s", got)
		}
	})

	t.Run("a monitor capture has no title to hide", func(t *testing.T) {
		got := formatCommand("ffmpeg", []string{"-f", "lavfi", "-i", "ddagrab=output_idx=0:framerate=30"}, false)
		if strings.Contains(got, "hidden") {
			t.Errorf("a monitor capture was edited:\n%s", got)
		}
	})
}

func TestQuoteArg(t *testing.T) {
	for _, tc := range []struct{ name, arg, want string }{
		{"plain", "-c:v", "-c:v"},
		{"a value with a space", "ddagrab=output_idx=0:framerate=30", "ddagrab=output_idx=0:framerate=30"},
		{"a path with spaces", `C:\Program Files\ffmpeg.exe`, `"C:\Program Files\ffmpeg.exe"`},
		{"empty", "", `""`},
		{"an embedded quote", `a"b`, `"a\"b"`},
		{"a filter graph with commas", "hwdownload,format=bgra", "hwdownload,format=bgra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteArg(tc.arg); got != tc.want {
				t.Errorf("quoteArg(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

func TestDirOf(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"empty", "", ""},
		{"no separator", "out.mp4", ""},
		{"windows", `C:\Users\me\Videos\a.mp4`, `C:\Users\me\Videos`},
		{"mixed separators", `C:\Users/me\Videos/a.mp4`, `C:\Users/me\Videos`},
		{"posix", "/home/me/videos/a.mp4", "/home/me/videos"},
		{"trailing separator only", `\a.mp4`, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dirOf(tc.path); got != tc.want {
				t.Errorf("dirOf(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter than the limit", "Notepad", 20, "Notepad"},
		{"exactly the limit", "Notepad", 7, "Notepad"},
		{"truncated", "Notepad", 4, "Not\u2026"},
		{"one character", "Notepad", 1, "N"},
		// Counting runes, not bytes: cutting a multi-byte character in half
		// would produce mojibake in the log.
		{"multi-byte", "caf\u00e9 \u00e9\u00e9\u00e9", 5, "caf\u00e9\u2026"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.n); got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

// ffmpeg's failure output is many lines and the interesting part is at the end,
// so it is folded into one line before being shown in an error.
func TestOneLine(t *testing.T) {
	got := oneLine("first line\r\nsecond line\nthird line\n\n")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("oneLine left a line break in %q", got)
	}
	if !strings.Contains(got, "first line") || !strings.Contains(got, "third line") {
		t.Errorf("oneLine lost content: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("oneLine did not trim: %q", got)
	}
}
