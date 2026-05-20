package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type fakeRunner struct {
	paths map[string]bool
	runs  map[string]subprocess.Result
	errs  map[string]error
}

func (f fakeRunner) LookPath(file string) (string, error) {
	if f.paths[file] {
		return "/bin/" + file, nil
	}
	return "", fmt.Errorf("%s missing", file)
}

func (f fakeRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	key := command + " " + strings.Join(args, " ")
	result := f.runs[key]
	return result, f.errs[key]
}

func TestCheckerRun(t *testing.T) {
	runner := fakeRunner{
		paths: map[string]bool{"git": true, "gh": true, "codex": true},
		runs: map[string]subprocess.Result{
			"git --version":             {Stdout: "git version 2\n"},
			"gh --version":              {Stdout: "gh version 2\n"},
			"codex --version":           {Stdout: "codex 1\n"},
			"gh auth status":            {Stdout: "Logged in\n"},
			"git remote get-url origin": {Stdout: "https://github.com/jerryfane/gitmoot.git\n"},
			"gh repo view jerryfane/gitmoot --json nameWithOwner": {Stdout: `{"nameWithOwner":"jerryfane/gitmoot"}`},
			"git branch --show-current":                           {Stdout: "main\n"},
		},
	}

	checks := Checker{Dir: "/repo", Runner: runner}.Run(context.Background())
	if err := FailedRequired(checks); err != nil {
		t.Fatalf("FailedRequired returned error: %v\nchecks=%+v", err, checks)
	}
}

func TestCheckerFailsOnMissingRequiredCommand(t *testing.T) {
	checks := Checker{Runner: fakeRunner{paths: map[string]bool{}}}.Run(context.Background())
	if err := FailedRequired(checks); err == nil {
		t.Fatal("FailedRequired returned nil, want error")
	}
}

func TestCheckerRunsGlobalChecksOutsideRepoDir(t *testing.T) {
	runner := dirSensitiveRunner{
		badDir: "/missing",
		fakeRunner: fakeRunner{
			paths: map[string]bool{"git": true, "gh": true, "codex": true},
			runs: map[string]subprocess.Result{
				"git --version":             {Stdout: "git version 2\n"},
				"gh --version":              {Stdout: "gh version 2\n"},
				"codex --version":           {Stdout: "codex 1\n"},
				"gh auth status":            {Stdout: "Logged in\n"},
				"git remote get-url origin": {Stderr: "not a repo\n"},
				"git branch --show-current": {Stderr: "not a repo\n"},
			},
			errs: map[string]error{
				"git remote get-url origin": fmt.Errorf("not a repo"),
				"git branch --show-current": fmt.Errorf("not a repo"),
			},
		},
	}

	checks := Checker{Dir: "/missing", Runner: runner}.Run(context.Background())

	for _, check := range checks {
		switch check.Name {
		case "git", "gh", "codex", "gh auth":
			if !check.OK {
				t.Fatalf("%s check failed even though it is global: %+v", check.Name, check)
			}
		}
	}
}

type dirSensitiveRunner struct {
	fakeRunner
	badDir string
}

func (r dirSensitiveRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	if dir == r.badDir {
		key := command + " " + strings.Join(args, " ")
		result := r.runs[key]
		if err := r.errs[key]; err != nil {
			return result, err
		}
		return subprocess.Result{}, fmt.Errorf("bad dir was used for %s", key)
	}
	return r.fakeRunner.Run(ctx, dir, command, args...)
}
