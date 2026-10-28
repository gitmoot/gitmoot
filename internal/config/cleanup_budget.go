package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CleanupBudget is a producer-owned, explicitly approved positive byte cap for the recognized
// worktree resource class, plus the bounds of the measurement it is compared with (#2360 v2,
// agreement 356319). It exists only when ONE config section supplies all six keys with valid
// values; owner and approval are provenance from this trusted file, never from a caller.
//
//	[cleanup_budget]                     # applies to every repository
//	[repos."owner/repo".cleanup_budget]  # replaces the global budget for that repository
//	limit_bytes = 1073741824             # > 0, allocated bytes
//	owner       = "jerry"
//	approval    = "owner decision id"
//	max_entries = 200000                 # > 0
//	max_depth   = 64                     # > 0
//	timeout_ms  = 30000                  # > 0
//
// A section that is present but incomplete or invalid is a load ERROR, so the caller treats the
// budget as unknown and refuses; it is never filled in from defaults or from another scope.
type CleanupBudget struct {
	LimitBytes int64
	Owner      string
	Approval   string
	MaxEntries int64
	MaxDepth   int
	TimeoutMS  int64
}

// CleanupBudgetConfig resolves the budget for a repository.
type CleanupBudgetConfig struct {
	global *CleanupBudget
	repos  map[string]*CleanupBudget
}

// For returns the repository's budget, else the global one, else nil (UNKNOWN). Scopes never mix.
func (c CleanupBudgetConfig) For(repo string) *CleanupBudget {
	if budget, ok := c.repos[strings.TrimSpace(repo)]; ok {
		return budget
	}
	return c.global
}

type cleanupBudgetSection struct {
	scope  string
	values map[string]string
}

// LoadCleanupBudget parses [cleanup_budget] and [repos."owner/repo".cleanup_budget]. A missing
// config file means no budget anywhere. A malformed header naming cleanup_budget refuses the
// whole file: a cap must never be read past an unreadable boundary.
func LoadCleanupBudget(paths Paths) (CleanupBudgetConfig, error) {
	cfg := CleanupBudgetConfig{repos: map[string]*CleanupBudget{}}
	content, err := os.ReadFile(paths.ConfigFile)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return CleanupBudgetConfig{}, err
	}
	var sections []*cleanupBudgetSection
	var current *cleanupBudgetSection
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "cleanup_budget") {
				return CleanupBudgetConfig{}, fmt.Errorf("parse cleanup_budget section: missing closing ]")
			}
			current = nil
			if scope, ok := parseCleanupBudgetSection(section); ok {
				current = &cleanupBudgetSection{scope: scope, values: map[string]string{}}
				sections = append(sections, current)
			}
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return CleanupBudgetConfig{}, fmt.Errorf("parse cleanup_budget %q: line %q is not key = value", current.scope, line)
		}
		key = strings.TrimSpace(key)
		if _, dup := current.values[key]; dup {
			return CleanupBudgetConfig{}, fmt.Errorf("parse cleanup_budget %q: %s set twice", current.scope, key)
		}
		current.values[key] = strings.TrimSpace(value)
	}
	for _, section := range sections {
		budget, err := buildCleanupBudget(section)
		if err != nil {
			return CleanupBudgetConfig{}, err
		}
		if section.scope == "" {
			if cfg.global != nil {
				return CleanupBudgetConfig{}, fmt.Errorf("parse cleanup_budget: [cleanup_budget] appears twice")
			}
			cfg.global = budget
			continue
		}
		if _, dup := cfg.repos[section.scope]; dup {
			return CleanupBudgetConfig{}, fmt.Errorf("parse cleanup_budget: %q appears twice", section.scope)
		}
		cfg.repos[section.scope] = budget
	}
	return cfg, nil
}

func parseCleanupBudgetSection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if section == "cleanup_budget" {
		return "", true
	}
	if !strings.HasPrefix(section, "repos.") || !strings.HasSuffix(section, ".cleanup_budget") {
		return "", false
	}
	quoted := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(section, "repos."), ".cleanup_budget"))
	repo, err := strconv.Unquote(quoted)
	if err != nil || strings.TrimSpace(repo) == "" || strings.Count(repo, "/") != 1 {
		return "", false
	}
	return strings.TrimSpace(repo), true
}

var cleanupBudgetKeys = []string{"limit_bytes", "owner", "approval", "max_entries", "max_depth", "timeout_ms"}

func buildCleanupBudget(section *cleanupBudgetSection) (*CleanupBudget, error) {
	where := "[cleanup_budget]"
	if section.scope != "" {
		where = fmt.Sprintf("[repos.%q.cleanup_budget]", section.scope)
	}
	for key := range section.values {
		known := false
		for _, k := range cleanupBudgetKeys {
			known = known || k == key
		}
		if !known {
			return nil, fmt.Errorf("%s: unknown key %q", where, key)
		}
	}
	for _, key := range cleanupBudgetKeys {
		if _, ok := section.values[key]; !ok {
			return nil, fmt.Errorf("%s: %s is required (a budget is all six keys or nothing)", where, key)
		}
	}
	positive := func(key string) (int64, error) {
		n, err := strconv.ParseInt(section.values[key], 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s: %s must be a positive integer, got %q", where, key, section.values[key])
		}
		return n, nil
	}
	text := func(key string) (string, error) {
		s, err := strconv.Unquote(section.values[key])
		if err != nil || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("%s: %s must be a non-empty quoted string", where, key)
		}
		return strings.TrimSpace(s), nil
	}
	var b CleanupBudget
	var err error
	if b.LimitBytes, err = positive("limit_bytes"); err != nil {
		return nil, err
	}
	if b.MaxEntries, err = positive("max_entries"); err != nil {
		return nil, err
	}
	depth, err := positive("max_depth")
	if err != nil {
		return nil, err
	}
	if depth > 4096 {
		return nil, fmt.Errorf("%s: max_depth %d is above 4096", where, depth)
	}
	b.MaxDepth = int(depth)
	if b.TimeoutMS, err = positive("timeout_ms"); err != nil {
		return nil, err
	}
	if b.Owner, err = text("owner"); err != nil {
		return nil, err
	}
	if b.Approval, err = text("approval"); err != nil {
		return nil, err
	}
	return &b, nil
}
