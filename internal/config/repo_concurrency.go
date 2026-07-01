package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RepoConcurrency is one per-repo daemon scheduler override parsed from a
// [repos."owner/repo"] config section (#576). It lets an operator cap a single
// repo's daemon parallelism — or flip its scheduler — WITHOUT restarting the
// shared daemon (the #559 restart footgun: a restart re-inherits the launching
// shell's env and resets in-flight scheduler state). The daemon re-reads this on
// every worker tick, so an edit takes effect on the next tick.
//
// It is OFF BY DEFAULT: a config with no [repos.*] sections yields an empty
// slice and a repo with no matching section behaves EXACTLY as today (the global
// --workers/--parallel + --scheduler apply). The loader mirrors LoadHeartbeats
// (Load/applyField/validate, config order preserved).
type RepoConcurrency struct {
	// Repo is the full name "owner/repo" the section keys on.
	Repo string
	// MaxParallel caps THIS repo's in-flight concurrency. <=0 (missing) means use
	// the global --workers/--parallel default — never "zero concurrency" (a
	// stalled repo). A positive value overrides the global for this repo only.
	MaxParallel int
	// Scheduler optionally overrides the queued-job scheduler for this repo:
	// "pool" (continuous worker pool) or "barrier" (per-tick). "" (missing) keeps
	// the daemon's global scheduler.
	Scheduler string
}

// LoadRepoConcurrency collects every [repos."owner/repo"] section from the config
// file into a stable, validated slice (config order preserved). It is OFF BY
// DEFAULT: a config with no [repos.*] subsections returns an empty slice and
// never errors, so callers that find an empty slice do no further work and every
// repo keeps today's global behavior.
//
// It reuses the same naive line-scanner shape as LoadHeartbeats. Unrelated
// sections (agents.*, admission, skillopt, …) are ignored; a repo name contains
// a '/', so the section key is the TOML quoted-key form repos."owner/repo".
func LoadRepoConcurrency(paths Paths) ([]RepoConcurrency, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	collected := map[string]*RepoConcurrency{}
	order := make([]string, 0)
	var current *RepoConcurrency
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = nil
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			repo, ok := parseRepoConcurrencySection(section)
			if !ok {
				continue
			}
			if collected[repo] == nil {
				collected[repo] = &RepoConcurrency{Repo: repo}
				order = append(order, repo)
			}
			current = collected[repo]
			continue
		}
		if current == nil {
			continue
		}
		field, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyRepoConcurrencyField(current, strings.TrimSpace(field), strings.TrimSpace(value)); err != nil {
			return nil, fmt.Errorf("parse [repos.%q].%s: %w", current.Repo, strings.TrimSpace(field), err)
		}
	}
	repos := make([]RepoConcurrency, 0, len(order))
	for _, repo := range order {
		entry := collected[repo]
		applyRepoConcurrencyDefaults(entry)
		if err := validateRepoConcurrency(*entry); err != nil {
			return nil, err
		}
		repos = append(repos, *entry)
	}
	return repos, nil
}

// RepoConcurrencyFor returns the override for repo, if any. Matching is exact on
// the full name; a caller that finds ok=false uses the global default.
func RepoConcurrencyFor(list []RepoConcurrency, repo string) (RepoConcurrency, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return RepoConcurrency{}, false
	}
	for _, entry := range list {
		if entry.Repo == repo {
			return entry, true
		}
	}
	return RepoConcurrency{}, false
}

// parseRepoConcurrencySection extracts the "owner/repo" from a section of the
// form repos."owner/repo" (or a bare repos.<key>). It returns ok=false for any
// other section so unrelated sections are ignored. A repo name contains a '/',
// which is not a legal TOML bare key, so the quoted-key form is the expected
// shape; a bare remainder is still accepted for leniency.
func parseRepoConcurrencySection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if !strings.HasPrefix(section, "repos.") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(section, "repos."))
	if rest == "" {
		return "", false
	}
	if strings.HasPrefix(rest, "\"") {
		unquoted, err := strconv.Unquote(rest)
		if err != nil || strings.TrimSpace(unquoted) == "" {
			return "", false
		}
		return strings.TrimSpace(unquoted), true
	}
	return rest, true
}

func applyRepoConcurrencyField(entry *RepoConcurrency, key string, value string) error {
	switch key {
	case "max_parallel":
		parsed, err := strconv.Atoi(value)
		entry.MaxParallel = parsed
		return err
	case "scheduler":
		parsed, err := parseConfigString(value)
		entry.Scheduler = parsed
		return err
	default:
		return nil
	}
}

func applyRepoConcurrencyDefaults(entry *RepoConcurrency) {
	entry.Repo = strings.TrimSpace(entry.Repo)
	entry.Scheduler = strings.TrimSpace(entry.Scheduler)
}

// validateRepoConcurrency enforces the contract with explicit errors (matching
// the heartbeat/admission validation style). A negative max_parallel is a
// mistake (0/missing already means "use the global default"); an unknown
// scheduler is rejected rather than silently ignored.
func validateRepoConcurrency(entry RepoConcurrency) error {
	if entry.MaxParallel < 0 {
		return fmt.Errorf("repo concurrency [repos.%q]: max_parallel must be 0 (use the global default) or positive", entry.Repo)
	}
	switch entry.Scheduler {
	case "", "pool", "barrier":
		return nil
	default:
		return fmt.Errorf("repo concurrency [repos.%q]: unsupported scheduler %q; use %q or %q", entry.Repo, entry.Scheduler, "pool", "barrier")
	}
}
