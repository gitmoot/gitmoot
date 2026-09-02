package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// This file holds the small cross-file test helpers that used to live in test files
// deleted with the SkillOpt loop in #1752 (skillopt_test.go,
// skillopt_recover_generation_test.go, hard_verifier_test.go,
// train_init_ui_e2e_test.go). Dozens of unrelated test files across this package
// call them, so they moved to a neutral home rather than being duplicated.

// runGitOutput runs a git command in dir and returns its stdout, failing the test
// on a non-zero exit (with stderr in the message).
func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	result, err := subprocess.ExecRunner{}.Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, result.Stderr)
	}
	return result.Stdout
}

// gitFixtureRepo initializes a throwaway git repo containing a single marker file
// and returns its directory and the HEAD sha.
func gitFixtureRepo(t *testing.T, marker string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "marker.txt"), []byte(marker), 0o600); err != nil {
		t.Fatalf("write fixture marker: %v", err)
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "fixture")
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return repoDir, strings.TrimSpace(string(out))
}

// deadPID returns a PID high enough that no process should occupy it, so a
// liveness probe reads it as dead.
func deadPID(t *testing.T) int64 {
	t.Helper()
	return 2147480000
}

// thisHostname returns the current hostname, failing the test if it cannot be read.
func thisHostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname returned error: %v", err)
	}
	return host
}

// containsString reports whether values contains want.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// contains reports whether values contains target.
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
