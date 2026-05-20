package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(nil, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run(nil) exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "gitmoot <command>") {
		t.Fatalf("usage output missing command help:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"nope"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("unknown command exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "nope"`) {
		t.Fatalf("stderr missing unknown command message:\n%s", stderr.String())
	}
}

func TestRunInitCreatesState(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	code := Run([]string{"init", "--home", home}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("init exit code = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".gitmoot", "gitmoot.db")); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
}

func TestRunSubcommandHelpSucceeds(t *testing.T) {
	for _, command := range []string{"init", "doctor"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run([]string{command, "--help"}, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("%s --help exit code = %d, stderr=%s", command, code, stderr.String())
			}
		})
	}
}
