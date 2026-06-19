package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
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
	withClaudeAuthEnv(t, map[string]string{runtime.ClaudeOAuthTokenEnv: "secret-token"})
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
	foundClaudeAuth := false
	for _, check := range checks {
		if check.Name != "claude auth" {
			continue
		}
		foundClaudeAuth = true
		if !check.OK || check.Required {
			t.Fatalf("claude auth check = %+v, want optional ok check", check)
		}
		if !strings.Contains(check.Detail, "CLAUDE_CODE_OAUTH_TOKEN=set") || strings.Contains(check.Detail, "secret-token") {
			t.Fatalf("claude auth detail = %q", check.Detail)
		}
	}
	if !foundClaudeAuth {
		t.Fatalf("checks missing claude auth: %+v", checks)
	}
}

func TestCheckerFailsOnMissingRequiredCommand(t *testing.T) {
	checks := Checker{Runner: fakeRunner{paths: map[string]bool{}}}.Run(context.Background())
	if err := FailedRequired(checks); err == nil {
		t.Fatal("FailedRequired returned nil, want error")
	}
}

func TestCheckerWarnsWhenClaudeAuthEnvMissing(t *testing.T) {
	withClaudeAuthEnv(t, nil)
	checks := Checker{Runner: fakeRunner{paths: map[string]bool{}}}.Run(context.Background())
	for _, check := range checks {
		if check.Name == "claude auth" {
			if check.OK || check.Required {
				t.Fatalf("claude auth check = %+v, want optional warning", check)
			}
			if !strings.Contains(check.Detail, "claude setup-token") {
				t.Fatalf("claude auth detail = %q, want setup-token guidance", check.Detail)
			}
			return
		}
	}
	t.Fatalf("checks missing claude auth: %+v", checks)
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

func withClaudeAuthEnv(t *testing.T, values map[string]string) {
	t.Helper()
	names := []string{
		runtime.ClaudeOAuthTokenEnv,
		runtime.AnthropicAPIKeyEnv,
		runtime.AnthropicAuthTokenEnv,
	}
	previous := make(map[string]string, len(names))
	present := make(map[string]bool, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			previous[name] = value
			present[name] = true
		}
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	for name, value := range values {
		if err := os.Setenv(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			if present[name] {
				_ = os.Setenv(name, previous[name])
			} else {
				_ = os.Unsetenv(name)
			}
		}
	})
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
		paths: map[string]bool{"git": true, "gh": true, "codex": true},
		runs: map[string]subprocess.Result{
			"git --version":   {Stdout: "git version 2\n"},
			"gh --version":    {Stdout: "gh version 2\n"},
			"codex --version": {Stdout: "codex 1\n"},
			"gh auth status":  {Stdout: "Logged in\n"},
		},
	}
	for _, c := range (Checker{Runner: runner}).GlobalChecks(context.Background()) {
		if c.Name == "repo remote" || c.Name == "base branch" || c.Name == "checkout" {
			t.Fatalf("GlobalChecks unexpectedly included per-repo check %q", c.Name)
		}
	}
}

func TestRepoChecksAgainstCheckoutPath(t *testing.T) {
	runner := pathSensitiveRunner{
		want: "/checkout",
		fakeRunner: fakeRunner{
			runs: map[string]subprocess.Result{
				"git remote get-url origin":                           {Stdout: "https://github.com/jerryfane/gitmoot.git\n"},
				"gh repo view jerryfane/gitmoot --json nameWithOwner": {Stdout: `{"nameWithOwner":"jerryfane/gitmoot"}`},
				"git branch --show-current":                           {Stdout: "main\n"},
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
