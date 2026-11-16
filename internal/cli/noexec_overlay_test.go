package cli

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOmpNoExecutionOverlayDisablesXdev is #2663: the overlay file a
// no-execution seat hands omp with --config must switch omp's xd:// device
// mounting off. With tools.xdev on (omp's default), a --tools list that grants
// read but omits write gets a device-only write tool. The overlay left on disk
// by an older daemon is replaced, and the file is read back the way omp reads
// settings: YAML, setting id `tools.xdev` as a nested path.
func TestOmpNoExecutionOverlayDisablesXdev(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(paths.Database), "runtimes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := "mcp:\n  enableProjectConfig: false\nextensions: []\nskills:\n  enabled: false\ndisabledProviders:\n  - native\n"
	if err := os.WriteFile(filepath.Join(dir, "omp-no-execution-v1.yml"), []byte(stale), 0o400); err != nil {
		t.Fatal(err)
	}

	path, err := ensureOmpNoExecutionOverlay(home)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("overlay is not YAML omp can parse: %v\n%s", err, raw)
	}
	tools, _ := settings["tools"].(map[string]any)
	if xdev, ok := tools["xdev"].(bool); !ok || xdev {
		t.Fatalf("overlay tools.xdev = %#v, want boolean false\n%s", tools["xdev"], raw)
	}
}
