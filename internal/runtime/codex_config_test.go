package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfiguredCodexModel(t *testing.T) {
	t.Run("reads the top-level model key", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"),
			[]byte("model = \"gpt-5.5\"\nmodel_reasoning_effort = \"high\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)
		got, err := ConfiguredCodexModel()
		if err != nil {
			t.Fatalf("ConfiguredCodexModel: %v", err)
		}
		if got != "gpt-5.5" {
			t.Fatalf("model = %q, want gpt-5.5", got)
		}
	})

	t.Run("empty when the file is missing", func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir()) // no config.toml inside
		got, err := ConfiguredCodexModel()
		if err != nil || got != "" {
			t.Fatalf("missing config: got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("empty when the model key is absent", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model_reasoning_effort = \"high\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", dir)
		got, err := ConfiguredCodexModel()
		if err != nil || got != "" {
			t.Fatalf("no model key: got (%q, %v), want (\"\", nil)", got, err)
		}
	})
}
