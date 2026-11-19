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
