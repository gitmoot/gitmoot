package ompaddon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// bakedRegistryDir decodes the registry directory the add-on code will use,
// i.e. the value assigned in the TypeScript, not the informational header.
func bakedRegistryDir(t *testing.T, content []byte) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^const REGISTRY_DIR: string = (".*");$`).FindSubmatch(content)
	if match == nil {
		t.Fatalf("installed add-on has no REGISTRY_DIR assignment")
	}
	var dir string
	if err := json.Unmarshal(match[1], &dir); err != nil {
		t.Fatalf("REGISTRY_DIR literal %s is not a valid string literal: %v", match[1], err)
	}
	return dir
}

// shortDir keeps registry paths under the Unix socket limit that Render
// enforces; t.TempDir paths embed the test name and can exceed it.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gmo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestInstallBakesRegistryDirAndIsIdempotent(t *testing.T) {
	extensions := filepath.Join(t.TempDir(), "agent", "extensions")
	registry := filepath.Join(shortDir(t), "home", ".gitmoot", "run", "omp")

	path, changed, err := Install(extensions, registry)
	if err != nil || !changed {
		t.Fatalf("first install = changed %v, err %v; want a write", changed, err)
	}
	if path != filepath.Join(extensions, FileName) {
		t.Fatalf("installed at %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bakedRegistryDir(t, content); got != registry {
		t.Fatalf("baked registry dir = %q, want %q", got, registry)
	}
	if bytes.Contains(content, []byte("__GITMOOT_OMP_")) {
		t.Fatalf("installed add-on still contains an unfilled placeholder")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("installed mode = %v, want 0644", info.Mode().Perm())
	}

	// A running OMP watches nothing, but rewriting an identical file still
	// churns mtimes and races a starting session; the second install must not write.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := Install(extensions, registry); err != nil || changed {
		t.Fatalf("second install = changed %v, err %v; want no write", changed, err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(old) {
		t.Fatalf("second install rewrote the file (mtime %v, want %v)", info.ModTime(), old)
	}
	entries, err := os.ReadDir(extensions)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("extensions dir has %d entries, want only the add-on (temp files must not be left behind)", len(entries))
	}

	inspection, err := Inspect(extensions, registry)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != StatusInstalled || inspection.Version != Version || inspection.RegistryDir != registry {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func TestInstallRestoresModeOfIdenticalFile(t *testing.T) {
	extensions := t.TempDir()
	registry := filepath.Join(shortDir(t), "run", "omp")
	path, _, err := Install(extensions, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := Install(extensions, registry); err != nil || !changed {
		t.Fatalf("install over 0600 copy = changed %v, err %v", changed, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestInspectReportsAddonForAnotherHomeAsOutdated(t *testing.T) {
	extensions := t.TempDir()
	homeA := filepath.Join(shortDir(t), "a", ".gitmoot", "run", "omp")
	homeB := filepath.Join(shortDir(t), "b", ".gitmoot", "run", "omp")
	if _, _, err := Install(extensions, homeA); err != nil {
		t.Fatal(err)
	}

	inspection, err := Inspect(extensions, homeB)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != StatusOutdated {
		t.Fatalf("add-on baked for %s inspected for %s: status %s, want outdated", homeA, homeB, inspection.Status)
	}
	if inspection.RegistryDir != homeA || inspection.ExpectedRegistryDir != homeB {
		t.Fatalf("inspection registry dirs = %q / %q", inspection.RegistryDir, inspection.ExpectedRegistryDir)
	}

	if _, changed, err := Install(extensions, homeB); err != nil || !changed {
		t.Fatalf("reinstall for the other home = changed %v, err %v", changed, err)
	}
	content, _ := os.ReadFile(filepath.Join(extensions, FileName))
	if got := bakedRegistryDir(t, content); got != homeB {
		t.Fatalf("after reinstall the add-on uses %q, want %q", got, homeB)
	}
}

func TestInspectDetectsOlderAddonAndInstallReplacesIt(t *testing.T) {
	extensions := t.TempDir()
	registry := filepath.Join(shortDir(t), "run", "omp")
	want, err := Render(registry)
	if err != nil {
		t.Fatal(err)
	}
	// An add-on from an earlier Gitmoot: same home, different code and version.
	older := bytes.ReplaceAll(want, []byte(Version), []byte("0123456789ab"))
	older = bytes.Replace(older, []byte("const RECENT_KEY_MS = 3_000;"), []byte("const RECENT_KEY_MS = 1_000;"), 1)
	path := filepath.Join(extensions, FileName)
	if err := os.WriteFile(path, older, 0o644); err != nil {
		t.Fatal(err)
	}

	inspection, err := Inspect(extensions, registry)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != StatusOutdated || inspection.Version != "0123456789ab" || inspection.ExpectedVersion != Version {
		t.Fatalf("inspection of older add-on = %+v", inspection)
	}

	// A hand edit that keeps the current version header is still not what
	// this binary installs.
	edited := bytes.Replace(want, []byte("const RECENT_KEY_MS = 3_000;"), []byte("const RECENT_KEY_MS = 0;"), 1)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if inspection, _ := Inspect(extensions, registry); inspection.Status != StatusOutdated {
		t.Fatalf("hand-edited add-on status = %s, want outdated", inspection.Status)
	}

	if _, changed, err := Install(extensions, registry); err != nil || !changed {
		t.Fatalf("install over older add-on = changed %v, err %v", changed, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("install did not replace the older add-on")
	}
	if inspection, _ := Inspect(extensions, registry); inspection.Status != StatusInstalled {
		t.Fatalf("status after install = %s", inspection.Status)
	}
}

func TestInspectMissing(t *testing.T) {
	inspection, err := Inspect(filepath.Join(t.TempDir(), "absent"), "/srv/gitmoot/run/omp")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != StatusMissing {
		t.Fatalf("status = %s, want missing", inspection.Status)
	}
}

func TestRenderEscapesRegistryDirAsStringLiteral(t *testing.T) {
	registry := `/tmp/it's "quoted" \ dir/run/omp`
	content, err := Render(registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := bakedRegistryDir(t, content); got != registry {
		t.Fatalf("baked registry dir = %q, want %q", got, registry)
	}
	if got := headerString(headerRegistryDir, content); got != registry {
		t.Fatalf("header registry dir = %q, want %q", got, registry)
	}
}

func TestRenderRejectsUnusableRegistryDirs(t *testing.T) {
	for name, dir := range map[string]string{
		"relative": "run/omp",
		"newline":  "/tmp/a\nb",
		// The socket path inside it would exceed the Unix limit.
		"too long": "/" + strings.Repeat("d", maxRegistryDirBytes),
	} {
		if _, err := Render(dir); err == nil {
			t.Errorf("%s: Render(%q) succeeded", name, dir)
		}
	}
	if _, err := Render("/" + strings.Repeat("d", maxRegistryDirBytes-1)); err != nil {
		t.Errorf("Render at the length limit failed: %v", err)
	}
}

func TestExtensionsDirFollowsOMPAgentDir(t *testing.T) {
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }
	check := func(want string) {
		t.Helper()
		got, err := ExtensionsDir("/home/u", getenv)
		if err != nil || got != want {
			t.Fatalf("ExtensionsDir with %s=%q = %q, %v; want %q", AgentDirEnv, env[AgentDirEnv], got, err, want)
		}
	}
	check("/home/u/.omp/agent/extensions")
	env[AgentDirEnv] = "/srv/omp-agent/"
	check("/srv/omp-agent/extensions")
	// OMP resolves a relative value against the working directory.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env[AgentDirEnv] = "rel/agent"
	check(filepath.Join(cwd, "rel", "agent", "extensions"))
}
