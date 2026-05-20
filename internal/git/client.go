package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

type Client struct {
	Runner subprocess.Runner
	Dir    string
}

func (c Client) CreateBranch(ctx context.Context, branch string, base string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	args := []string{"switch", "-c", branch}
	if strings.TrimSpace(base) != "" {
		args = append(args, base)
	}
	_, err := c.run(ctx, args...)
	return err
}

func (c Client) CurrentBranch(ctx context.Context) (string, error) {
	result, err := c.run(ctx, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(result.Stdout)
	if branch == "" {
		return "", errors.New("current git branch is empty")
	}
	return branch, nil
}

func (c Client) PushBranch(ctx context.Context, remote string, branch string) error {
	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}
	if err := validateBranch(branch); err != nil {
		return err
	}
	_, err := c.run(ctx, "push", "-u", remote, branch)
	return err
}

func (c Client) run(ctx context.Context, args ...string) (subprocess.Result, error) {
	runner := c.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	result, err := runner.Run(ctx, c.Dir, "git", args...)
	if err != nil {
		return result, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return result, nil
}

func validateBranch(branch string) error {
	trimmed := strings.TrimSpace(branch)
	switch {
	case trimmed == "":
		return errors.New("branch is required")
	case trimmed != branch:
		return fmt.Errorf("branch %q must not contain leading or trailing whitespace", branch)
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("branch %q must not start with '-'", branch)
	case strings.ContainsAny(branch, " \t\r\n"):
		return fmt.Errorf("branch %q must not contain whitespace", branch)
	case strings.ContainsAny(branch, ":~^?*[\\"):
		return fmt.Errorf("branch %q contains invalid git ref characters", branch)
	case strings.Contains(branch, ".."):
		return fmt.Errorf("branch %q must not contain '..'", branch)
	case strings.Contains(branch, "@{"):
		return fmt.Errorf("branch %q must not contain '@{'", branch)
	case strings.Contains(branch, "//"):
		return fmt.Errorf("branch %q must not contain '//'", branch)
	case strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/"):
		return fmt.Errorf("branch %q must not start or end with '/'", branch)
	case strings.HasSuffix(branch, ".lock"):
		return fmt.Errorf("branch %q must not end with .lock", branch)
	}
	return nil
}
