package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// idleRuntimeHome stages one runtime copy in a fresh home and ages its stamp past
// DefaultRuntimeMinIdle; it points the collector at an empty process table.
func idleRuntimeHome(t *testing.T) (home string, paths config.Paths, published string) {
	t.Helper()
	home = t.TempDir()
	paths = config.PathsForHome(home)
	tool := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher, _, err := toolchain.StageRuntime(paths.Home, "prunetool", tool)
	if err != nil {
		t.Fatal(err)
	}
	published = filepath.Dir(filepath.Dir(launcher))
	old := time.Now().Add(-2 * toolchain.DefaultRuntimeMinIdle)
	if err := os.Chtimes(filepath.Join(toolchain.RuntimeRoot(paths.Home), ".last-used", filepath.Base(published)), old, old); err != nil {
		t.Fatal(err)
	}
	previous := runtimeCollectionProcRoot
	runtimeCollectionProcRoot = t.TempDir()
	t.Cleanup(func() { runtimeCollectionProcRoot = previous })
	return home, paths, published
}

func TestRuntimePruneFlagsAndJSONReport(t *testing.T) {
	home, _, published := idleRuntimeHome(t)
	for _, args := range [][]string{{"extra"}, {"--min-idle", "0s"}, {"--no-such-flag"}} {
		var stdout, stderr bytes.Buffer
		if code := runRuntimePrune(append([]string{"--home", home}, args...), &stdout, &stderr); code != 2 {
			t.Fatalf("runtime prune %v exit = %d, want 2 (stderr %q)", args, code, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runRuntimePrune([]string{"--home", home, "--dry-run", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exit = %d stderr=%q", code, stderr.String())
	}
	var report struct {
		Removed []struct {
			Name  string `json:"name"`
			Bytes int64  `json:"bytes"`
		} `json:"removed"`
		Kept         []json.RawMessage `json:"kept"`
		RemovedBytes int64             `json:"removed_bytes"`
		DryRun       bool              `json:"dry_run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("JSON report: %v\n%s", err, stdout.String())
	}
	if !report.DryRun || len(report.Removed) != 1 || report.Removed[0].Name != filepath.Base(published) || report.RemovedBytes <= 0 {
		t.Fatalf("dry-run report = %+v", report)
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("dry run removed the copy: %v", err)
	}

	stdout.Reset()
	if code := runRuntimePrune([]string{"--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("prune exit = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed "+filepath.Base(published)) {
		t.Fatalf("text report = %q", stdout.String())
	}
	if _, err := os.Stat(published); !os.IsNotExist(err) {
		t.Fatalf("idle copy still present: %v", err)
	}
}

// The daemon pass removes an idle copy and logs it; an unreadable process table
// keeps everything and says so.
func TestRuntimeCollectionPassRemovesAndReportsIncompleteScans(t *testing.T) {
	_, paths, published := idleRuntimeHome(t)
	runtimeCollectionProcRoot = filepath.Join(t.TempDir(), "missing")
	var out bytes.Buffer
	runRuntimeCollectionPass(paths, time.Now(), &out)
	if !strings.Contains(out.String(), "runtime collection") {
		t.Fatalf("unreadable process table was silent: %q", out.String())
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("copy removed despite an unreadable process table: %v", err)
	}

	runtimeCollectionProcRoot = t.TempDir()
	out.Reset()
	runRuntimeCollectionPass(paths, time.Now(), &out)
	if _, err := os.Stat(published); !os.IsNotExist(err) {
		t.Fatalf("idle copy still present: %v", err)
	}
}

func TestRuntimeCollectionPassRecordsNoRemovalAndIncompleteScan(t *testing.T) {
	_, paths, published := idleRuntimeHome(t)
	now := time.Now().UTC()
	stamp := filepath.Join(toolchain.RuntimeRoot(paths.Home), ".last-used", filepath.Base(published))
	if err := os.Chtimes(stamp, now, now); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runRuntimeCollectionPass(paths, now, &out)
	data, err := os.ReadFile(filepath.Join(paths.Home, "runtime-collection.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Decode the on-disk contract without depending on a new production type,
	// so this check also compiles on the old code.
	var state struct {
		LastPassAt time.Time `json:"last_pass_at"`
		Kept       int       `json:"kept"`
		Removed    int       `json:"removed"`
		Incomplete string    `json:"incomplete"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if !state.LastPassAt.Equal(now) || state.Kept != 1 || state.Removed != 0 || state.Incomplete != "" {
		t.Fatalf("no-removal pass record: %+v", state)
	}
	old := now.Add(-2 * toolchain.DefaultRuntimeMinIdle)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	runtimeCollectionProcRoot = filepath.Join(t.TempDir(), "missing")
	runRuntimeCollectionPass(paths, now.Add(time.Minute), &out)
	data, err = os.ReadFile(filepath.Join(paths.Home, "runtime-collection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if !state.LastPassAt.Equal(now.Add(time.Minute)) || state.Incomplete == "" || state.Removed != 0 {
		t.Fatalf("incomplete pass did not replace prior success: %+v", state)
	}
	if _, err := os.Stat(published); err != nil {
		t.Fatalf("collector removed a protected copy: %v", err)
	}
}
