package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
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
		paths: map[string]bool{"git": true, "gh": true, "omp": true},
		runs: map[string]subprocess.Result{
			"git --version":             {Stdout: "git version 2\n"},
			"gh --version":              {Stdout: "gh version 2\n"},
			"omp --version":             {Stdout: "omp/18.5.0\n"},
			"gh auth status":            {Stdout: "Logged in\n"},
			"git remote get-url origin": {Stdout: "https://github.com/gitmoot/gitmoot.git\n"},
			"gh repo view gitmoot/gitmoot --json nameWithOwner": {Stdout: `{"nameWithOwner":"gitmoot/gitmoot"}`},
			"git branch --show-current":                         {Stdout: "main\n"},
		},
	}

	checks := Checker{Dir: "/repo", Runner: runner}.Run(context.Background())
	if err := FailedRequired(checks); err != nil {
		t.Fatalf("FailedRequired returned error: %v\nchecks=%+v", err, checks)
	}
	for _, check := range checks {
		switch check.Name {
		case "codex", "claude", "kimi", "claude auth", "claude auth (daemon)":
			t.Fatalf("doctor still checks removed runtime %q: %+v", check.Name, check)
		}
	}
}

// TestCheckerFailsClearlyWithoutOmp: omp is the only job runtime (#2587), so a
// host without it fails doctor with a detail that says gitmoot requires omp.
func TestCheckerFailsClearlyWithoutOmp(t *testing.T) {
	runner := fakeRunner{
		paths: map[string]bool{"git": true, "gh": true},
		runs: map[string]subprocess.Result{
			"git --version":  {Stdout: "git version 2\n"},
			"gh --version":   {Stdout: "gh version 2\n"},
			"gh auth status": {Stdout: "Logged in\n"},
		},
	}
	checks := Checker{Runner: runner}.GlobalChecks(context.Background())
	err := FailedRequired(checks)
	if err == nil || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("FailedRequired = %v, want a failure naming omp", err)
	}
	for _, check := range checks {
		if check.Name != "omp" {
			continue
		}
		if check.OK || !check.Required || !strings.Contains(check.Detail, "requires omp") {
			t.Fatalf("omp check = %+v, want required failure saying gitmoot requires omp", check)
		}
		return
	}
	t.Fatalf("checks missing omp: %+v", checks)
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
			paths: map[string]bool{"git": true, "gh": true, "omp": true},
			runs: map[string]subprocess.Result{
				"git --version":             {Stdout: "git version 2\n"},
				"gh --version":              {Stdout: "gh version 2\n"},
				"omp --version":             {Stdout: "omp/18.5.0\n"},
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
		case "git", "gh", "omp", "gh auth":
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

func TestRepoChecksNoCheckoutWarns(t *testing.T) {
	checks := Checker{Runner: fakeRunner{paths: map[string]bool{}}}.RepoChecks(context.Background(), "")
	if len(checks) != 1 {
		t.Fatalf("RepoChecks(\"\") = %+v, want a single check", checks)
	}
	c := checks[0]
	if c.Name != "checkout" || c.OK || c.Required {
		t.Fatalf("no-checkout check = %+v, want non-required, not-ok 'checkout'", c)
	}
	if !strings.Contains(c.Detail, "no checkout") {
		t.Fatalf("no-checkout detail = %q", c.Detail)
	}
}

func TestGlobalChecksHaveNoRepoChecks(t *testing.T) {
	runner := fakeRunner{
		paths: map[string]bool{"git": true, "gh": true, "omp": true},
		runs: map[string]subprocess.Result{
			"git --version":  {Stdout: "git version 2\n"},
			"gh --version":   {Stdout: "gh version 2\n"},
			"omp --version":  {Stdout: "omp/18.5.0\n"},
			"gh auth status": {Stdout: "Logged in\n"},
		},
	}
	for _, c := range (Checker{Runner: runner}).GlobalChecks(context.Background()) {
		if c.Name == "repo remote" || c.Name == "base branch" || c.Name == "checkout" {
			t.Fatalf("GlobalChecks unexpectedly included per-repo check %q", c.Name)
		}
	}
}

func TestCheckStuckJobsTreatsOnlyConfirmedDeadPIDAsHardFinding(t *testing.T) {
	check := CheckStuckJobs(StuckJobsStatus{Jobs: []StuckJobProcess{
		{JobID: "dead", PID: 101, Known: true, Live: false},
		{JobID: "live", PID: 202, Known: true, Live: true},
		{JobID: "legacy-no-pid"},
		{JobID: "unknown", PID: 303, Known: false},
	}})
	if check.OK || !check.Required {
		t.Fatalf("check = %+v, want required hard finding", check)
	}
	if !strings.Contains(check.Detail, "dead (pid 101)") {
		t.Fatalf("detail = %q, want dead job identity", check.Detail)
	}
	for _, absent := range []string{"live", "legacy-no-pid", "unknown"} {
		if strings.Contains(check.Detail, absent) {
			t.Fatalf("detail = %q, must not flag %s", check.Detail, absent)
		}
	}

	healthy := CheckStuckJobs(StuckJobsStatus{Jobs: []StuckJobProcess{
		{JobID: "live", PID: 202, Known: true, Live: true},
		{JobID: "legacy-no-pid"},
	}})
	if !healthy.OK || !healthy.Required {
		t.Fatalf("healthy check = %+v, want required OK", healthy)
	}
}

func TestRepoChecksAgainstCheckoutPath(t *testing.T) {
	runner := pathSensitiveRunner{
		want: "/checkout",
		fakeRunner: fakeRunner{
			runs: map[string]subprocess.Result{
				"git remote get-url origin":                         {Stdout: "https://github.com/gitmoot/gitmoot.git\n"},
				"gh repo view gitmoot/gitmoot --json nameWithOwner": {Stdout: `{"nameWithOwner":"gitmoot/gitmoot"}`},
				"git branch --show-current":                         {Stdout: "main\n"},
			},
		},
	}
	checks := Checker{Runner: runner}.RepoChecks(context.Background(), "/checkout")
	for _, c := range checks {
		if !c.OK {
			t.Fatalf("repo check %q ran against wrong dir or failed: %+v", c.Name, c)
		}
	}
}

// pathSensitiveRunner asserts that git/gh repo commands run in the expected dir.
type pathSensitiveRunner struct {
	fakeRunner
	want string
}

func (r pathSensitiveRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	if dir != r.want {
		return subprocess.Result{}, fmt.Errorf("ran in %q, want %q", dir, r.want)
	}
	return r.fakeRunner.Run(ctx, dir, command, args...)
}
