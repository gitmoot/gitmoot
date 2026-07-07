package config

import (
	"os"
	"testing"
)

func TestLoadResultChecksModeDefaultsToWarn(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// No [workflow] section -> the documented warn-by-default (issue #526).
	mode, err := LoadResultChecksMode(paths)
	if err != nil {
		t.Fatalf("LoadResultChecksMode: %v", err)
	}
	if mode != ResultChecksWarn {
		t.Fatalf("mode = %q, want warn", mode)
	}
}

func TestLoadResultChecksModeMissingFileDefaultsToWarn(t *testing.T) {
	// A home whose config file was never written must still resolve to warn, not
	// error, mirroring LoadMemorySettings' missing-file handling.
	paths := PathsForHome(t.TempDir())
	mode, err := LoadResultChecksMode(paths)
	if err != nil {
		t.Fatalf("LoadResultChecksMode: %v", err)
	}
	if mode != ResultChecksWarn {
		t.Fatalf("mode = %q, want warn", mode)
	}
}

func TestLoadResultChecksModeMatrix(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  ResultChecksMode
	}{
		{"off", ResultChecksOff},
		{"warn", ResultChecksWarn},
		{"block", ResultChecksBlock},
		{"  BLOCK  ", ResultChecksBlock}, // trimmed + case-insensitive
		// Quoted TOML form — the exact style the docs and DefaultConfig writer
		// use (result_checks = "off"). Must parse identically to the bare form,
		// otherwise the feature cannot be disabled/blocked via the docs (#526).
		{`"off"`, ResultChecksOff},
		{`"warn"`, ResultChecksWarn},
		{`"block"`, ResultChecksBlock},
		{`  "BLOCK"  `, ResultChecksBlock}, // quoted + padded + case-insensitive
	} {
		paths := PathsForHome(t.TempDir())
		if err := Initialize(paths); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[workflow]\nresult_checks = "+tc.value+"\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		mode, err := LoadResultChecksMode(paths)
		if err != nil {
			t.Fatalf("LoadResultChecksMode(%q): %v", tc.value, err)
		}
		if mode != tc.want {
			t.Fatalf("value %q -> mode %q, want %q", tc.value, mode, tc.want)
		}
	}
}

func TestLoadResultChecksModeRejectsInvalid(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[workflow]\nresult_checks = loud\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadResultChecksMode(paths); err == nil {
		t.Fatal("expected an error for an invalid result_checks value")
	}
}

func TestLoadResultChecksModeIgnoresOtherSections(t *testing.T) {
	// A result_checks key under a DIFFERENT section must not be picked up.
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+"\n[memory]\nresult_checks = off\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	mode, err := LoadResultChecksMode(paths)
	if err != nil {
		t.Fatalf("LoadResultChecksMode: %v", err)
	}
	if mode != ResultChecksWarn {
		t.Fatalf("mode = %q, want warn (result_checks under [memory] must be ignored)", mode)
	}
}
