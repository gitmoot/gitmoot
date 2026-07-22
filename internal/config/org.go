package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// OrgRole is one role in the local organization registry. MergeRule is
// deliberately advisory in phase 1a; scope is enforced at dispatch.
type OrgRole struct {
	Name      string
	Parent    string
	Scope     []string
	MergeRule string
	// Pane optionally binds this role to a Herdr pane for event-rule wakes. It is
	// advisory and unused unless an enabled event rule targets the role.
	Pane string
}

// OrgConfig is the local organization registry. Its fields stay private so the
// loader remains the single place that establishes its invariants.
type OrgConfig struct {
	enforce string
	roles   map[string]OrgRole
}

func (c OrgConfig) Enabled() bool { return len(c.roles) > 0 }

func (c OrgConfig) Enforce() string {
	if c.enforce == "" {
		return "block"
	}
	return c.enforce
}

func (c OrgConfig) Role(name string) (OrgRole, bool) {
	r, ok := c.roles[strings.ToLower(strings.TrimSpace(name))]
	return r, ok
}

// Ancestors returns name's parent chain, nearest parent first. The cycle guard
// keeps this safe for callers holding a manually constructed config too, even
// though LoadOrg rejects parent cycles.
func (c OrgConfig) Ancestors(name string) []string {
	seen := map[string]bool{}
	var out []string
	role, ok := c.Role(name)
	for ok && role.Parent != "" && !seen[role.Parent] {
		seen[role.Parent] = true
		out = append(out, role.Parent)
		role, ok = c.Role(role.Parent)
	}
	return out
}

func (c OrgConfig) Roots() []string {
	roots := make([]string, 0, len(c.roles))
	for name, role := range c.roles {
		if role.Parent == "" {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	return roots
}

func (c OrgConfig) Roles() []OrgRole {
	names := make([]string, 0, len(c.roles))
	for name := range c.roles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]OrgRole, 0, len(names))
	for _, name := range names {
		out = append(out, c.roles[name])
	}
	return out
}

// LoadOrg reads the optional [org] registry. A missing file means org has not
// been configured; any other IO or parse error is intentionally returned so the
// enqueue resolver can fail closed.
func LoadOrg(paths Paths) (OrgConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return OrgConfig{roles: map[string]OrgRole{}}, nil
		}
		return OrgConfig{}, err
	}
	cfg := OrgConfig{roles: map[string]OrgRole{}}
	current := ""
	inOrg := false
	lines := strings.Split(string(content), "\n")
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(stripOrgConfigComment(lines[index]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && !strings.HasSuffix(line, "]") {
			// Match the other narrowly-scoped config loaders: a typo in an
			// unrelated section must not make an absent org policy fail closed.
			inOrg, current = false, ""
			// Fail closed ONLY for a genuinely org-shaped header — "org" as a
			// complete token (end, a "." path separator, or a stray-space typo
			// of [org.roles...]) — so a typo in the registry cannot silently
			// disable the gate. Unrelated sections whose name merely starts with
			// "org" ([organization], [orgs], [org_settings]) stay tolerated: an
			// unrelated typo must not brick dispatch.
			body := strings.TrimSpace(strings.TrimPrefix(line, "["))
			if strings.HasPrefix(body, "org") && (len(body) == 3 || body[3] == '.' || unicode.IsSpace(rune(body[3]))) {
				return OrgConfig{}, fmt.Errorf("parse org section: missing closing ]")
			}
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name, role, ok, err := parseOrgSection(strings.TrimSpace(line[1 : len(line)-1]))
			if err != nil {
				return OrgConfig{}, err
			}
			inOrg, current = name, role
			if current != "" && ok {
				if _, exists := cfg.roles[current]; exists {
					return OrgConfig{}, fmt.Errorf("org role %q: duplicate section", current)
				}
				cfg.roles[current] = OrgRole{Name: current}
			}
			continue
		}
		if !inOrg {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return OrgConfig{}, fmt.Errorf("parse [org]: expected key = value")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if current == "" {
			if key == "enforce" {
				v, err := parseOrgTOMLString(value)
				if err != nil {
					return OrgConfig{}, fmt.Errorf("parse [org].enforce: %w", err)
				}
				cfg.enforce = v
			}
			continue
		}
		role := cfg.roles[current]
		switch key {
		case "parent":
			v, err := parseOrgTOMLString(value)
			if err != nil {
				return OrgConfig{}, fmt.Errorf("org role %q: parse parent: %w", current, err)
			}
			role.Parent = v
		case "scope":
			for !strings.HasSuffix(strings.TrimSpace(value), "]") {
				index++
				if index >= len(lines) {
					return OrgConfig{}, fmt.Errorf("org role %q: parse scope: unterminated array", current)
				}
				value += "\n" + strings.TrimSpace(stripOrgConfigComment(lines[index]))
			}
			v, err := parseOrgScopeArray(value)
			if err != nil {
				return OrgConfig{}, fmt.Errorf("org role %q: parse scope: %w", current, err)
			}
			for i := range v {
				normalized, err := normalizeOrgScope(v[i])
				if err != nil {
					return OrgConfig{}, fmt.Errorf("org role %q: %w", current, err)
				}
				v[i] = normalized
			}
			role.Scope = v
		case "merge_rule":
			v, err := parseOrgTOMLString(value)
			if err != nil {
				return OrgConfig{}, fmt.Errorf("org role %q: parse merge_rule: %w", current, err)
			}
			role.MergeRule = v
		case "pane":
			v, err := parseOrgTOMLString(value)
			if err != nil {
				return OrgConfig{}, fmt.Errorf("org role %q: parse pane: %w", current, err)
			}
			role.Pane = strings.TrimSpace(v)
		}
		cfg.roles[current] = role
	}
	if err := ValidateOrg(cfg); err != nil {
		return OrgConfig{}, err
	}
	return cfg, nil
}

func parseOrgSection(section string) (inOrg bool, role string, ok bool, err error) {
	if section == "org" {
		return true, "", true, nil
	}
	const prefix = "org.roles."
	if !strings.HasPrefix(section, prefix) {
		return false, "", false, nil
	}
	tail := strings.TrimSpace(strings.TrimPrefix(section, prefix))
	name, err := parseOrgTOMLString(tail)
	if err != nil {
		return false, "", false, fmt.Errorf("parse org role section: %w", err)
	}
	if name == "" {
		return false, "", false, fmt.Errorf("parse org role section: empty role name")
	}
	return true, name, true, nil
}

func parseOrgScopeArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, fmt.Errorf("scope must be a single-line array")
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return []string{}, nil
	}
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	for index, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" && index == len(parts)-1 {
			continue // TOML permits a trailing comma in an array.
		}
		item, err := parseOrgTOMLString(part)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func parseOrgTOMLString(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], nil
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return strconv.Unquote(value)
	}
	return "", fmt.Errorf("expected quoted TOML string")
}

// stripOrgConfigComment understands both TOML basic and literal strings. The
// repo's older scanners only need basic strings; org also accepts TOML literals.
func stripOrgConfigComment(value string) string {
	var quote rune
	escaped := false
	for index, r := range value {
		switch {
		case quote != 0 && quote == '\'' && r == '\'':
			quote = 0
		case quote != 0 && quote == '"' && escaped:
			escaped = false
		case quote != 0 && quote == '"' && r == '\\':
			escaped = true
		case quote != 0 && quote == '"' && r == '"':
			quote = 0
		case quote == 0 && (r == '\'' || r == '"'):
			quote = r
		case quote == 0 && r == '#':
			return value[:index]
		}
	}
	return value
}

// ValidateOrg verifies the registry invariants before it is exposed to a
// permission gate.
func ValidateOrg(cfg OrgConfig) error {
	if cfg.Enforce() != "block" && cfg.Enforce() != "warn" {
		return fmt.Errorf("org enforce must be \"block\" or \"warn\"")
	}
	for name, role := range cfg.roles {
		if !validOrgRoleName(name) {
			return fmt.Errorf("org role %q: invalid name", name)
		}
		if role.Name != "" && role.Name != name {
			return fmt.Errorf("org role %q: name mismatch", name)
		}
		if role.MergeRule != "" && role.MergeRule != "owner" && role.MergeRule != "self" && role.MergeRule != "none" {
			return fmt.Errorf("org role %q: invalid merge_rule %q", name, role.MergeRule)
		}
		for _, scope := range role.Scope {
			if _, err := normalizeOrgScope(scope); err != nil {
				return fmt.Errorf("org role %q: %w", name, err)
			}
		}
		if role.Parent != "" {
			if _, ok := cfg.roles[role.Parent]; !ok {
				return fmt.Errorf("org role %q: parent %q is not declared", name, role.Parent)
			}
		}
	}
	if len(cfg.roles) == 0 {
		return nil
	}
	if len(cfg.Roots()) != 1 {
		return fmt.Errorf("org: expected exactly one root role, got %d", len(cfg.Roots()))
	}
	for name := range cfg.roles {
		seen := map[string]bool{}
		for current := name; current != ""; {
			if seen[current] {
				return fmt.Errorf("org role %q: parent cycle", name)
			}
			seen[current] = true
			current = cfg.roles[current].Parent
		}
	}
	for name, role := range cfg.roles {
		if role.Parent == "" {
			continue
		}
		if !scopeSubset(role.Scope, cfg.roles[role.Parent].Scope) {
			return fmt.Errorf("org role %q: scope is not a subset of parent %q", name, role.Parent)
		}
	}
	return nil
}

func validOrgRoleName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func normalizeOrgScope(scope string) (string, error) {
	if strings.TrimSpace(scope) != scope || scope == "" {
		return "", fmt.Errorf("invalid scope %q", scope)
	}
	if scope == "*" {
		return scope, nil
	}
	parts := strings.Split(scope, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid scope %q", scope)
	}
	if strings.Contains(parts[0], "*") || (parts[1] != "*" && strings.Contains(parts[1], "*")) {
		return "", fmt.Errorf("invalid scope %q", scope)
	}
	for _, part := range parts {
		for _, r := range part {
			if unicode.IsSpace(r) {
				return "", fmt.Errorf("invalid scope %q", scope)
			}
		}
	}
	return strings.ToLower(scope), nil
}

// ScopeMatches expects LoadOrg-normalized scopes. It remains harmless for raw
// case variants, but callers must validate scopes through LoadOrg before using
// this as an authorization decision.
func ScopeMatches(scope []string, repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, raw := range scope {
		entry := strings.ToLower(raw)
		if entry == "*" || entry == repo || entry == parts[0]+"/*" {
			return true
		}
	}
	return false
}

func scopeSubset(child, parent []string) bool {
	for _, rawChild := range child {
		c, err := normalizeOrgScope(rawChild)
		if err != nil {
			return false
		}
		covered := false
		for _, rawParent := range parent {
			p, err := normalizeOrgScope(rawParent)
			if err != nil {
				continue
			}
			if p == "*" || p == c {
				covered = true
				break
			}
			if strings.HasSuffix(p, "/*") && strings.HasPrefix(c, strings.TrimSuffix(p, "*")) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}
