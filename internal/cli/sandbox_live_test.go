package cli

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// TestLiveKimiProduceLandlockState is an opt-in live test because it consumes a
// real Kimi turn and requires the host's authenticated Kimi CLI. It proves both
// halves of the runtime-state grant: a complete wrapped session can persist its
// own state in an isolated HOME, while the same wrapper still gets EACCES for a
// sibling path outside every grant.
func TestLiveKimiProduceLandlockState(t *testing.T) {
	if os.Getenv("GITMOOT_LIVE_LANDLOCK_KIMI") != "1" {
		t.Skip("set GITMOOT_LIVE_LANDLOCK_KIMI=1 to run the authenticated Kimi Landlock test")
	}
	originalHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(repoRoot, ".gitmoot-live-kimi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	binary := filepath.Join(base, "gitmoot")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/gitmoot")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build live shim: %v\n%s", err, output)
	}

	home := filepath.Join(base, "home")
	work := filepath.Join(base, "work")
	data := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{home, work, data, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyLiveCredential(t, filepath.Join(originalHome, ".kimi-code", "config.toml"), filepath.Join(home, ".kimi-code", "config.toml"))
	copyLiveCredential(t, filepath.Join(originalHome, ".kimi-code", "credentials", "kimi-code.json"), filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	agent := runtime.Agent{
		Name:           "live-kimi-landlock",
		Role:           "producer",
		Runtime:        runtime.KimiRuntime,
		RuntimeRef:     runtime.FreshRefPrefix + "live-landlock",
		RepoScope:      "owner/repo",
		AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite,
		WritablePaths:  []string{data},
	}
	adapter, err := buildRuntimeAdapter("", agent, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	kimi := adapter.(runtime.KimiAdapter)
	wrapper, ok := kimi.Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("Kimi runner = %T, want WrappingRunner", kimi.Runner)
	}
	wrapper.Executable = binary
	kimi.Runner = wrapper

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := kimi.Deliver(ctx, agent, runtime.Job{Prompt: "Reply exactly OK. Do not use tools."})
	if err != nil {
		t.Fatalf("wrapped Kimi delivery: %v", err)
	}
	if !strings.Contains(strings.ToUpper(result.Summary), "OK") {
		t.Fatalf("wrapped Kimi summary = %q, want OK", result.Summary)
	}
	if !treeHasRegularFile(t, filepath.Join(home, ".kimi-code", "sessions")) {
		t.Fatal("wrapped Kimi delivery completed without persisting a session under HOME/.kimi-code")
	}

	outsideFile := filepath.Join(outside, "denied.txt")
	if _, err := wrapper.Run(context.Background(), work, "sh", "-c", `printf denied > "$1"`, "gitmoot-live", outsideFile); err == nil {
		t.Fatal("write outside runtime and declared grants unexpectedly succeeded")
	}
	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Fatalf("outside file exists after denied write: %v", err)
	}
}

func copyLiveCredential(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read live Kimi fixture %q: %v", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func treeHasRegularFile(t *testing.T, root string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect runtime session state %q: %v", root, err)
	}
	return found
}
