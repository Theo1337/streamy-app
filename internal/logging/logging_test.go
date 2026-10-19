package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo, // unknown means info, never a crash
		"noisy":   slog.LevelInfo,
	}
	for in, want := range tests {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSetupWritesToTheLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "streamy.log")

	logger, closeLog, err := Setup(Options{Level: "debug", File: path})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	logger.Info("hello from the test")
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if want := "hello from the test"; !contains(string(data), want) {
		t.Errorf("log file does not contain %q:\n%s", want, data)
	}
	if want := "msg="; !contains(string(data), want) {
		t.Errorf("log file is not in slog text format (no %q):\n%s", want, data)
	}
}

func TestSetupWithoutFileOnlyUsesStderr(t *testing.T) {
	logger, closeLog, err := Setup(Options{})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if logger == nil {
		t.Fatal("logger is nil")
	}
	if err := closeLog(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
