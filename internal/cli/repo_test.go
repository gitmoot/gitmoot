package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
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
