package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTranscriptsConfigDefaultsAndEnabledValues(t *testing.T) {
	home := t.TempDir()
	paths := PathsForHome(home)
	if got := LoadTranscriptsConfig(paths); got != DefaultTranscriptsConfig() {
		t.Fatalf("missing config = %+v, want %+v", got, DefaultTranscriptsConfig())
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`[other]
enabled = true

[transcripts]
enabled = true
retain = "24h"
max_total_bytes = 4096
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadTranscriptsConfig(paths)
	if !got.Enabled || got.Retain.Hours() != 24 || got.MaxTotalBytes != 4096 {
		t.Fatalf("enabled config = %+v", got)
	}
}

func TestLoadTranscriptsConfigInvalidDegradesToDisabled(t *testing.T) {
	for _, body := range []string{
		"[transcripts]\nenabled = nope\n",
		"[transcripts]\nenabled = true\nretain = \"0s\"\n",
		"[transcripts]\nenabled = true\nmax_total_bytes = 0\n",
		"[transcripts]\nenabled = true\nretain = \"not-a-duration\"\n",
	} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			paths := PathsForHome(home)
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := LoadTranscriptsConfig(paths); got.Enabled || got.Retain != DefaultTranscriptRetain || got.MaxTotalBytes != DefaultTranscriptMaxTotalBytes {
				t.Fatalf("invalid config = %+v, want disabled defaults", got)
			}
		})
	}
}
