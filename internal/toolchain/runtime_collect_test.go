package toolchain

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectDoesNotRemoveStagedRuntimes pins a coupling that broke staging
// deterministically and would have been invisible in review.
//
// Collect() removes EVERY entry under Root() that is not a pinned toolchain
// IDENTITY. The first version of the runtime root lived at
// <home>/toolchains/runtimes, so a collection pass deleted staged runtimes - and
// deleted them mid-stage, surfacing as "mkdirat <name>.staging-…: no such file
// or directory" from an already-open root handle whose directory had been
// unlinked. Runtime copies are not toolchain versions and must never be judged
// by a toolchain retention list.
//
// The assertion is deliberately about SURVIVAL AND RUNNABILITY, not about the
// path: a future layout change is free to move the root anywhere Collect does
// not govern.
func TestCollectDoesNotRemoveStagedRuntimes(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nprintf 'tool-ran\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, err := StageRuntime(home, "tool", source)
	if err != nil {
		t.Fatal(err)
	}

	// Collect with an EMPTY keep list is the most aggressive pass possible: it
	// removes every toolchain the engine no longer pins.
	if err := Collect(home, nil); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("a staged runtime was removed by the toolchain collector: %v", err)
	}
	output, runErr := exec.Command(staged).CombinedOutput()
	if runErr != nil || strings.TrimSpace(string(output)) != "tool-ran" {
		t.Fatalf("staged runtime no longer runs after collection: err=%v output=%q", runErr, output)
	}

	// CONTROL: the collector still does its job in the same call, so this test
	// cannot pass because Collect became a no-op. An unpinned toolchain-shaped
	// directory under Root() must be gone.
	unpinned := filepath.Join(Root(home), "go1.0.0-deadbeefdeadbeef")
	if err := os.MkdirAll(unpinned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Collect(home, nil); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if _, err := os.Stat(unpinned); err == nil {
		t.Error("Collect left an unpinned toolchain directory in place, so the survival assertion above proves nothing")
	}
}
