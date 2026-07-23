package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadToolCache(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantEnabled bool
		wantDir     string // empty means "expect the default <home>/cache/tools"
		wantErr     string
	}{
		{name: "missing config", wantEnabled: true},
		{name: "omitted", content: "[cache]\nenabled = true\n", wantEnabled: true},
		{name: "disabled", content: "[cache]\nenabled = false\n", wantEnabled: false},
		{name: "custom dir", content: "[cache]\ndir = \"/var/gitmoot-tool-cache\"\n", wantEnabled: true, wantDir: "/var/gitmoot-tool-cache"},
		{name: "relative dir rejected", content: "[cache]\ndir = \"relative/path\"\n", wantErr: "must be absolute"},
		{name: "garbage enabled", content: "[cache]\nenabled = maybe\n", wantErr: "invalid [cache].enabled"},
		{name: "other section", content: "[workflow]\nenabled = false\ndir = \"/should/not/apply\"\n", wantEnabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if test.content != "" {
				if err := os.MkdirAll(paths.Home, 0o700); err != nil {
					t.Fatalf("mkdir home: %v", err)
				}
				if err := os.WriteFile(paths.ConfigFile, []byte(test.content), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			got, err := LoadToolCache(paths)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("LoadToolCache error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadToolCache: %v", err)
			}
			if got.Enabled != test.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", got.Enabled, test.wantEnabled)
			}
			wantDir := test.wantDir
			if wantDir == "" {
				wantDir = filepath.Join(paths.Home, "cache", "tools")
			}
			if got.Dir != wantDir {
				t.Fatalf("Dir = %q, want %q", got.Dir, wantDir)
			}
		})
	}
}

// TestLoadToolCacheDefaultDirIsAbsoluteEvenWithRelativeHome pins a finder
// finding: pathsFromFlag does not itself require --home to be absolute, so
// paths.Home (and therefore the naive join of it with the default subdir) can
// be relative. A relative Dir is rejected outright by Landlock and, for
// unsandboxed jobs, resolves against the CHILD PROCESS's cwd (the worktree
// checkout) rather than the daemon's -- a different path per worktree,
// silently recreating the per-worktree cache duplication this feature exists
// to eliminate.
func TestLoadToolCacheDefaultDirIsAbsoluteEvenWithRelativeHome(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	paths := PathsForHome("relhome")
	if filepath.IsAbs(paths.Home) {
		t.Fatalf("test setup invalid: paths.Home %q is already absolute", paths.Home)
	}
	got, err := LoadToolCache(paths)
	if err != nil {
		t.Fatalf("LoadToolCache: %v", err)
	}
	if !filepath.IsAbs(got.Dir) {
		t.Fatalf("Dir = %q, want an absolute path", got.Dir)
	}
	want := filepath.Join(cwd, "relhome", ".gitmoot", "cache", "tools")
	if got.Dir != want {
		t.Fatalf("Dir = %q, want %q", got.Dir, want)
	}
}
