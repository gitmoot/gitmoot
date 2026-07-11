package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestRunRepoAddListDoctorRemove(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/gitmoot.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"repo", "add", "jerryfane/gitmoot", "--home", home, "--path", repoDir, "--poll", "45s"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "registered jerryfane/gitmoot") {
		t.Fatalf("repo add output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"repo", "list", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repo list exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"jerryfane/gitmoot", "enabled", "45s", filepath.Clean(repoDir)} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("repo list missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"repo", "doctor", "jerryfane/gitmoot", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repo doctor exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"repo: jerryfane/gitmoot ok", "remote: https://github.com/jerryfane/gitmoot.git", "branch: main"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("repo doctor missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"repo", "remove", "jerryfane/gitmoot", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("repo remove exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed jerryfane/gitmoot") {
		t.Fatalf("repo remove output = %q", stdout.String())
	}
}

func TestRunRepoAddAcceptsFlagsBeforeOrAfterPositional(t *testing.T) {
	cases := []struct {
		name string
		args func(home, repoDir string) []string
	}{
		{
			name: "flags after positional",
			args: func(home, repoDir string) []string {
				return []string{"repo", "add", "jerryfane/gitmoot", "--home", home, "--path", repoDir}
			},
		},
		{
			name: "flags before positional",
			args: func(home, repoDir string) []string {
				return []string{"repo", "add", "--home", home, "--path", repoDir, "jerryfane/gitmoot"}
			},
		},
		{
			name: "positional between flags",
			args: func(home, repoDir string) []string {
				return []string{"repo", "add", "--home", home, "jerryfane/gitmoot", "--path", repoDir}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			repoDir := t.TempDir()
			runGit(t, repoDir, "init")
			runGit(t, repoDir, "branch", "-m", "main")
			runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/gitmoot.git")

			var stdout, stderr bytes.Buffer
			code := Run(tc.args(home, repoDir), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("repo add exit code = %d, stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "registered jerryfane/gitmoot") {
				t.Fatalf("repo add output = %q", stdout.String())
			}
		})
	}
}

func TestRunRepoAddRejectsWrongOrigin(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/jerryfane/other.git")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"repo", "add", "jerryfane/gitmoot", "--home", home, "--path", repoDir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("repo add exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not jerryfane/gitmoot") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRepoAddRefusesLinkedWorktreeUnlessForced(t *testing.T) {
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "add", "owner/repo", "--home", home, "--path", primary}, &stdout, &stderr); code != 0 {
		t.Fatalf("primary repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"repo", "add", "owner/repo", "--home", home, "--path", linked}, &stdout, &stderr); code != 1 {
		t.Fatalf("linked repo add exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"linked worktree", primary, "--force"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("linked refusal missing %q: %s", want, stderr.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"repo", "add", "owner/repo", "--home", home, "--path", linked, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("forced linked repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	record, err := store.GetRepo(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	if record.CheckoutPath != linked || record.PrimaryCheckoutPath != primary {
		t.Fatalf("forced record = %+v", record)
	}
}

func TestRunRepoDoctorReportsLinkedAndDanglingCheckout(t *testing.T) {
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "add", "owner/repo", "--home", home, "--path", linked, "--force"}, &stdout, &stderr); code != 0 {
		t.Fatalf("forced repo add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"repo", "doctor", "owner/repo", "--home", home}, &stdout, &stderr); code != 1 {
		t.Fatalf("linked repo doctor exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"repo: owner/repo warn", "linked worktree", "primary: " + primary} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("linked repo doctor missing %q: %s", want, stdout.String())
		}
	}
	runGit(t, primary, "worktree", "remove", "--force", linked)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"repo", "doctor", "owner/repo", "--home", home}, &stdout, &stderr); code != 1 {
		t.Fatalf("dangling repo doctor exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "is missing") || !strings.Contains(stdout.String(), "primary: "+primary) {
		t.Fatalf("dangling repo doctor output = %s", stdout.String())
	}
}

func TestRepoCheckoutDoctorChecksWarnAndLazyBackfill(t *testing.T) {
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	store := openCLIJobStore(t, home)
	if err := store.UpsertRepoForce(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: linked}); err != nil {
		t.Fatalf("UpsertRepoForce returned error: %v", err)
	}
	if _, err := store.HealRepoCheckout(context.Background(), "owner/repo", linked, linked, ""); err != nil {
		t.Fatalf("clear primary checkout returned error: %v", err)
	}
	store.Close()
	checks := repoCheckoutDoctorChecks(config.PathsForHome(home))
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "linked worktree") {
		t.Fatalf("checks = %+v", checks)
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	record, err := store.GetRepo(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("GetRepo returned error: %v", err)
	}
	if record.PrimaryCheckoutPath != primary {
		t.Fatalf("lazy primary backfill = %q, want %q", record.PrimaryCheckoutPath, primary)
	}
	runGit(t, primary, "worktree", "remove", "--force", linked)
	checks = repoCheckoutDoctorChecks(config.PathsForHome(home))
	if len(checks) != 1 || checks[0].OK || !strings.Contains(checks[0].Detail, "is missing") || !strings.Contains(checks[0].Detail, "is available") {
		t.Fatalf("dangling checks = %+v", checks)
	}
}

func setupLinkedWorktreeRepo(t *testing.T) (string, string) {
	t.Helper()
	primary := t.TempDir()
	runGit(t, primary, "init", "-b", "main")
	runGit(t, primary, "config", "user.email", "gitmoot@example.com")
	runGit(t, primary, "config", "user.name", "Gitmoot")
	runGit(t, primary, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, primary, "commit", "--allow-empty", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, primary, "worktree", "add", "-b", "task", linked)
	return filepath.Clean(primary), filepath.Clean(linked)
}

func TestRepoCheckoutDoctorChecksSkipMissingDatabase(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if checks := repoCheckoutDoctorChecks(paths); len(checks) != 0 {
		t.Fatalf("checks = %+v, want none", checks)
	}
	if _, err := os.Stat(paths.Database); !os.IsNotExist(err) {
		t.Fatalf("doctor sweep created database: %v", err)
	}
}
