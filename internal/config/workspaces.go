package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// WorkspacesPolicy is the [workspaces] section (#2678): remote workspaces for
// coding seats. A workspace is a provider sandbox bound to one seat and one
// issue or pull request; the seat runs its builds and tests there through
// `gitmoot workspace exec`.
type WorkspacesPolicy struct {
	// Enabled admits `gitmoot workspace open`. Default false. Turning it off
	// never strands a running workspace: the daemon keeps settling and closing
	// the ones already open.
	Enabled bool
	// Provider is the remote execution provider workspaces are created on, one
	// of the [remote_exec] providers. Its template, API key and
	// cost_per_hour_usd come from that provider's [remote_exec] configuration.
	Provider string
	// MaxTotalUSD is the global pilot cap: every workspace's accrued cost plus
	// the open workspaces' outstanding reservations never exceeds it.
	MaxTotalUSD float64
	// MaxWorkspaceUSD caps one workspace's reserved cost over its lifetime.
	MaxWorkspaceUSD float64
	// IdleTimeout closes a ready workspace this long after its last exec.
	IdleTimeout time.Duration
	// Template is the provider template workspaces are created from. Empty
	// falls back to the provider's [remote_exec] e2b_template, which remote
	// jobs also use, so a workspace image (with the Go toolchain) can differ
	// from the job image without changing it for everyone.
	Template string
}

// WorkspacesLocalProvider runs workspaces on the local execution backend.
const WorkspacesLocalProvider = "local"

// DefaultWorkspacesPolicy is the policy of a config without [workspaces].
func DefaultWorkspacesPolicy() WorkspacesPolicy {
	return WorkspacesPolicy{
		Provider:        RemoteExecProviderE2B,
		MaxTotalUSD:     50,
		MaxWorkspaceUSD: 10,
		IdleTimeout:     4 * time.Hour,
	}
}

// LoadWorkspacesPolicy reads [workspaces]. A missing file or section is the
// default (disabled). Unknown keys, invalid values and a malformed
// [workspaces header are refused, so a typo cannot silently lift a cost cap.
func LoadWorkspacesPolicy(paths Paths) (WorkspacesPolicy, error) {
	policy := DefaultWorkspacesPolicy()
	content, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return policy, nil
	}
	if err != nil {
		return WorkspacesPolicy{}, err
	}
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "workspaces") {
				return WorkspacesPolicy{}, fmt.Errorf("malformed section header %q", line)
			}
			current = section == "workspaces"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return WorkspacesPolicy{}, fmt.Errorf("parse [workspaces]: expected key = value, got %q", line)
		}
		key = strings.TrimSpace(key)
		if err := applyWorkspacesField(&policy, key, strings.TrimSpace(value)); err != nil {
			return WorkspacesPolicy{}, fmt.Errorf("parse [workspaces].%s: %w", key, err)
		}
	}
	if policy.MaxWorkspaceUSD > policy.MaxTotalUSD {
		return WorkspacesPolicy{}, fmt.Errorf("[workspaces].max_workspace_usd (%v) must not exceed max_total_usd (%v)", policy.MaxWorkspaceUSD, policy.MaxTotalUSD)
	}
	return policy, nil
}

func applyWorkspacesField(policy *WorkspacesPolicy, key, value string) error {
	switch key {
	case "enabled":
		parsed, err := parseConfigBool(value)
		policy.Enabled = parsed
		return err
	case "provider":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		parsed = strings.TrimSpace(parsed)
		// "local" runs workspaces on the local execution backend: no cost and no
		// isolation, for development and tests.
		if parsed != WorkspacesLocalProvider && !IsRemoteExecProvider(parsed) {
			return fmt.Errorf("unknown provider %q (allowed: %q or %s)", parsed, WorkspacesLocalProvider, RemoteExecProviderChoices())
		}
		policy.Provider = parsed
		return nil
	case "max_total_usd", "max_workspace_usd":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("expected a number: %w", err)
		}
		if parsed <= 0 || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return errors.New("must be a positive number of dollars")
		}
		if key == "max_total_usd" {
			policy.MaxTotalUSD = parsed
		} else {
			policy.MaxWorkspaceUSD = parsed
		}
		return nil
	case "template":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		if !validWorkspacesTemplate(parsed) {
			return fmt.Errorf("invalid template %q: 1-128 characters of letters, digits and . _ - : /, starting with a letter or digit", parsed)
		}
		policy.Template = parsed
		return nil
	case "idle_timeout":
		parsed, err := parseConfigDuration(value)
		if err != nil {
			return err
		}
		if parsed < time.Minute {
			return errors.New("must be at least 1m")
		}
		policy.IdleTimeout = parsed
		return nil
	default:
		return errors.New("unknown key")
	}
}

// validWorkspacesTemplate accepts a provider template name or ID (E2B:
// "gitmoot-omp-go126-2c4g-v1", an ID, or "team/name:tag"): 1-128 letters,
// digits and . _ - : /, starting with a letter or digit. Whitespace, quotes
// and control characters are refused, so a typo cannot reach the provider.
func validWorkspacesTemplate(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-' || r == ':' || r == '/'):
		default:
			return false
		}
	}
	return true
}

// RepoWorkspace is one repository's [repos."owner/repo".workspace] table
// (#2704): what that repository's workspaces are created from and what
// `gitmoot workspace open` requires of them before it reports one ready.
type RepoWorkspace struct {
	// Template overrides [workspaces] template for this repository's
	// workspaces. Empty inherits it (and through it [remote_exec]
	// e2b_template).
	Template string
	// Require are shell commands open runs in order in the workspace root,
	// each as `bash -lc '<command>'` through the exec path. The first that
	// exits non-zero or times out fails the open (requirements_unmet) and
	// closes the workspace.
	Require []string
}

// MaxWorkspaceRequire caps [repos."owner/repo".workspace] require.
const MaxWorkspaceRequire = 16

// RepoWorkspaces is every [repos."owner/repo".workspace] table, keyed by the
// lower-cased repository name so "Owner/Repo" and "owner/repo" are one
// repository. An invalid table is kept on its repository only (For returns
// the error), like a repository's review checks routing, so one repository's
// typo never refuses another repository's open.
type RepoWorkspaces struct {
	repos map[string]repoWorkspaceEntry
}

type repoWorkspaceEntry struct {
	workspace RepoWorkspace
	errs      []error
}

// For returns repo's table, the zero value (no template, no checks) for a
// repository without one, or the table's validation error.
func (c RepoWorkspaces) For(repo string) (RepoWorkspace, error) {
	entry, ok := c.repos[strings.ToLower(strings.TrimSpace(repo))]
	if !ok {
		return RepoWorkspace{}, nil
	}
	if err := errors.Join(entry.errs...); err != nil {
		return RepoWorkspace{}, err
	}
	workspace := entry.workspace
	workspace.Require = append([]string(nil), workspace.Require...)
	return workspace, nil
}

// LoadRepoWorkspaces reads every [repos."owner/repo".workspace] table. A
// missing file is no tables. A malformed header naming a workspace table, or
// a workspace header whose repository name cannot be read, is refused for
// the whole file, so a typo cannot silently drop a repository's checks.
// Unknown keys are ignored, as in the other per-repository tables.
func LoadRepoWorkspaces(paths Paths) (RepoWorkspaces, error) {
	cfg := RepoWorkspaces{repos: map[string]repoWorkspaceEntry{}}
	content, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return RepoWorkspaces{}, err
	}
	repo, key := "", ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "workspace") {
				return RepoWorkspaces{}, fmt.Errorf("malformed section header %q", line)
			}
			repo, key, err = parseRepoWorkspaceSection(section)
			if err != nil {
				return RepoWorkspaces{}, err
			}
			if key != "" {
				if _, ok := cfg.repos[key]; !ok {
					cfg.repos[key] = repoWorkspaceEntry{}
				}
			}
			continue
		}
		if key == "" {
			continue
		}
		field, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		field = strings.TrimSpace(field)
		entry := cfg.repos[key]
		if err := applyRepoWorkspaceField(&entry.workspace, field, strings.TrimSpace(value)); err != nil {
			entry.errs = append(entry.errs, fmt.Errorf("[repos.%q.workspace].%s: %w", repo, field, err))
		}
		cfg.repos[key] = entry
	}
	return cfg, nil
}

// parseRepoWorkspaceSection returns the repository a [repos."owner/repo".workspace]
// header names and its lower-cased key, or two empty strings for any other
// section. A workspace header whose repository cannot be read (a broken
// quote, an empty name) is an error for the whole file: which repository's
// checks it meant is unknown, so no repository may open without them.
func parseRepoWorkspaceSection(section string) (string, string, error) {
	body, ok := strings.CutPrefix(section, "repos.")
	if !ok || !strings.HasSuffix(body, ".workspace") {
		return "", "", nil
	}
	rest := strings.TrimSpace(strings.TrimSuffix(body, ".workspace"))
	if strings.HasPrefix(rest, "\"") {
		unquoted, err := strconv.Unquote(rest)
		if err != nil {
			return "", "", fmt.Errorf("section [%s]: the repository name does not unquote: %v; write [repos.\"owner/repo\".workspace]", section, err)
		}
		rest = strings.TrimSpace(unquoted)
	} else if strings.ContainsAny(rest, "\"'") {
		return "", "", fmt.Errorf("section [%s]: the repository name has a stray quote; write [repos.\"owner/repo\".workspace]", section)
	}
	if rest == "" {
		return "", "", fmt.Errorf("section [%s] names no repository; write [repos.\"owner/repo\".workspace]", section)
	}
	return rest, strings.ToLower(rest), nil
}

func applyRepoWorkspaceField(workspace *RepoWorkspace, key, value string) error {
	switch key {
	case "template":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		if !validWorkspacesTemplate(parsed) {
			return fmt.Errorf("invalid template %q: 1-128 characters of letters, digits and . _ - : /, starting with a letter or digit", parsed)
		}
		workspace.Template = parsed
		return nil
	case "require":
		parsed, err := parseWorkspaceRequire(value)
		if err != nil {
			return err
		}
		workspace.Require = parsed
		return nil
	default:
		return nil
	}
}

// parseWorkspaceRequire reads a single-line array of double-quoted shell
// commands. Commas inside a command are part of it. Each entry must be
// non-blank and free of NUL bytes (no argv can carry one), and there are at
// most MaxWorkspaceRequire.
func parseWorkspaceRequire(value string) ([]string, error) {
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, errors.New(`expected a single-line array of strings, like ["go version"]`)
	}
	commands := []string{}
	rest := strings.TrimSpace(value[1 : len(value)-1])
	for rest != "" {
		if rest[0] != '"' {
			return nil, fmt.Errorf("entry %d: expected a double-quoted string", len(commands)+1)
		}
		end := 1
		for end < len(rest) && rest[end] != '"' {
			if rest[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(rest) {
			return nil, fmt.Errorf("entry %d: unterminated string", len(commands)+1)
		}
		command, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", len(commands)+1, err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("entry %d is empty", len(commands)+1)
		}
		if strings.ContainsRune(command, 0) {
			return nil, fmt.Errorf("entry %d contains a NUL byte", len(commands)+1)
		}
		commands = append(commands, command)
		if len(commands) > MaxWorkspaceRequire {
			return nil, fmt.Errorf("at most %d commands", MaxWorkspaceRequire)
		}
		rest = strings.TrimSpace(rest[end+1:])
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil, fmt.Errorf("entry %d: expected , after the string", len(commands))
		}
		// TOML permits a trailing comma.
		rest = strings.TrimSpace(rest[1:])
	}
	return commands, nil
}
