package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"

	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type Check struct {
	Name     string
	OK       bool
	Required bool
	Detail   string
}

type Checker struct {
	Dir    string
	Runner subprocess.Runner
}

// Run returns the global (cwd-independent) checks followed by the per-repo
// checks for the Checker's Dir, treated as a single checkout. This is the
// `gitmoot doctor --repo <dir>` view.
func (c Checker) Run(ctx context.Context) []Check {
	checks := c.GlobalChecks(ctx)
	return append(checks, c.RepoChecks(ctx, c.Dir)...)
}

// GlobalChecks returns the diagnostics that do not depend on any repo checkout:
// required/optional runtime binaries and the auth checks. They can run from
// anywhere, so the dashboard renders them once.
func (c Checker) GlobalChecks(ctx context.Context) []Check {
	runner := c.runner()
	return []Check{
		c.command(ctx, runner, "git", true, "--version"),
		c.command(ctx, runner, "gh", true, "--version"),
		c.command(ctx, runner, "codex", true, "--version"),
		c.command(ctx, runner, "claude", false, "--help"),
		c.command(ctx, runner, "kimi", false, "--version"),
		c.claudeAuthEnv(),
		c.ghAuth(ctx, runner),
	}
}

// RepoChecks returns the per-repo diagnostics (origin remote resolves, base
// branch present) run against checkoutPath. A repo that has no checkout path yet
// (subscribed but never delivered to) reports a single non-required "no checkout"
// check rather than failing the git-dependent checks.
func (c Checker) RepoChecks(ctx context.Context, checkoutPath string) []Check {
	if strings.TrimSpace(checkoutPath) == "" {
		return []Check{{Name: "checkout", OK: false, Required: false, Detail: "no checkout yet"}}
	}
	runner := c.runner()
	return []Check{
		c.repoRemote(ctx, runner, checkoutPath),
		c.baseBranch(ctx, runner, checkoutPath),
	}
}

func (c Checker) runner() subprocess.Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return subprocess.ExecRunner{}
}

func (c Checker) command(ctx context.Context, runner subprocess.Runner, name string, required bool, args ...string) Check {
	if _, err := runner.LookPath(name); err != nil {
		return Check{Name: name, Required: required, Detail: err.Error()}
	}
	result, err := runner.Run(ctx, "", name, args...)
	if err != nil {
		return Check{Name: name, Required: required, Detail: strings.TrimSpace(result.Stderr)}
	}
	return Check{Name: name, OK: true, Required: required, Detail: firstLine(result.Stdout, result.Stderr)}
}

func (c Checker) ghAuth(ctx context.Context, runner subprocess.Runner) Check {
	result, err := runner.Run(ctx, "", "gh", "auth", "status")
	if err != nil {
		return Check{Name: "gh auth", Required: true, Detail: strings.TrimSpace(result.Stderr)}
	}
	return Check{Name: "gh auth", OK: true, Required: true, Detail: firstLine(result.Stdout, result.Stderr)}
}

func (c Checker) repoRemote(ctx context.Context, runner subprocess.Runner, dir string) Check {
	result, err := runner.Run(ctx, dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: strings.TrimSpace(result.Stderr)}
	}
	remote := strings.TrimSpace(result.Stdout)
	repo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: err.Error()}
	}

	view, err := runner.Run(ctx, dir, "gh", "repo", "view", repo.String(), "--json", "nameWithOwner")
	if err != nil {
		return Check{Name: "repo remote", Required: true, Detail: strings.TrimSpace(view.Stderr)}
	}
	return Check{Name: "repo remote", OK: true, Required: true, Detail: repo.String()}
}

func (c Checker) baseBranch(ctx context.Context, runner subprocess.Runner, dir string) Check {
	result, err := runner.Run(ctx, dir, "git", "branch", "--show-current")
	if err != nil {
		return Check{Name: "base branch", Required: true, Detail: strings.TrimSpace(result.Stderr)}
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "" {
		return Check{Name: "base branch", Required: true, Detail: "detached HEAD"}
	}
	return Check{Name: "base branch", OK: true, Required: true, Detail: branch}
}

func (c Checker) claudeAuthEnv() Check {
	auth := runtime.InspectClaudeAuthEnv(os.LookupEnv)
	detail := auth.MaskedDetail()
	if warning := auth.Warning(); warning != "" {
		detail += "; " + warning
	}
	return Check{Name: "claude auth", OK: auth.Ready(), Required: false, Detail: detail}
}

func FailedRequired(checks []Check) error {
	var failed []string
	for _, check := range checks {
		if check.Required && !check.OK {
			failed = append(failed, check.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("failed required checks: %s", strings.Join(failed, ", "))
}

func firstLine(values ...string) string {
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				return line
			}
		}
	}
	return ""
}
