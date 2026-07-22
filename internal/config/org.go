package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
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
	Pane         string
	RecycleAfter time.Duration
}

// OrgConfig is the local organization registry. Its fields stay private so the
// loader remains the single place that establishes its invariants.
type OrgConfig struct {
	enforce      string
	recycleAfter time.Duration
	roles        map[string]OrgRole
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

func (c OrgConfig) RecycleAfterFor(role string) time.Duration {
	if configured, ok := c.Role(role); ok && configured.RecycleAfter != 0 {
		return configured.RecycleAfter
	}
	return c.recycleAfter
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

func (c OrgConfig) Children(name string) []OrgRole {
	var children []OrgRole
	for _, role := range c.roles {
		if role.Parent == name {
			children = append(children, role)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return children
}

func (c OrgConfig) Path(name string) []string {
	role, ok := c.Role(name)
	if !ok {
		return nil
	}
	path := []string{role.Name}
	visited := map[string]bool{role.Name: true}
	for role.Parent != "" {
		parent, ok := c.Role(role.Parent)
		if !ok {
			return nil
		}
		if visited[parent.Name] {
			break
		}
		visited[parent.Name] = true
		path = append(path, parent.Name)
		role = parent
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
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
	roleFields := map[string]map[string]bool{}
	seenOrgSection := false
	seenEnforce := false
	seenRecycleAfter := false
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
			if inOrg && current == "" && ok {
				if seenOrgSection {
					return OrgConfig{}, fmt.Errorf("duplicate [org] section")
				}
				seenOrgSection = true
			}
			if current != "" && ok {
				if _, exists := cfg.roles[current]; exists {
					return OrgConfig{}, fmt.Errorf("org role %q: duplicate section", current)
				}
				cfg.roles[current] = OrgRole{Name: current}
				roleFields[current] = map[string]bool{}
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
			// recycle_after is binary-first: binaries predating this allowlist
			// fail closed on a config that uses the field.
			if key != "enforce" && key != "recycle_after" {
				return OrgConfig{}, fmt.Errorf("unknown [org] field %q", key)
			}
			switch key {
			case "enforce":
				if seenEnforce {
					return OrgConfig{}, fmt.Errorf("duplicate [org].enforce")
				}
				seenEnforce = true
				v, err := parseOrgTOMLString(value)
				if err != nil {
					return OrgConfig{}, fmt.Errorf("parse [org].enforce: %w", err)
				}
				cfg.enforce = v
			case "recycle_after":
				if seenRecycleAfter {
					return OrgConfig{}, fmt.Errorf("duplicate [org].recycle_after")
				}
				seenRecycleAfter = true
				v, err := parseOrgDuration(value)
				if err != nil {
					return OrgConfig{}, fmt.Errorf("parse [org].recycle_after: %w", err)
				}
				cfg.recycleAfter = v
			}
			continue
		}
		if key != "parent" && key != "scope" && key != "merge_rule" && key != "pane" && key != "recycle_after" {
			return OrgConfig{}, fmt.Errorf("unknown field %q for org role %q", key, current)
		}
		if roleFields[current][key] {
			return OrgConfig{}, fmt.Errorf("duplicate field %q for org role %q", key, current)
		}
		roleFields[current][key] = true
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
		case "recycle_after":
			v, err := parseOrgDuration(value)
			if err != nil {
				return OrgConfig{}, fmt.Errorf("org role %q: parse recycle_after: %w", current, err)
			}
			role.RecycleAfter = v
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
	if section == "org.roles" {
		return false, "", false, fmt.Errorf("parse org role section: expected [org.roles.\"name\"]")
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

func parseOrgDuration(value string) (time.Duration, error) {
	parsed, err := parseOrgTOMLString(value)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(parsed)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", parsed, err)
	}
	return duration, nil
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
	if cfg.recycleAfter < 0 {
		return fmt.Errorf("org recycle_after must not be negative")
	}
	// Validate in sorted, structural passes so malformed registries return the
	// same error regardless of Go map iteration order. Root naming deliberately
	// precedes parent-reference checks, matching the retired Registry validator.
	names := make([]string, 0, len(cfg.roles))
	for name := range cfg.roles {
		names = append(names, name)
	}
	sort.Strings(names)

	roots := 0
	for _, name := range names {
		role := cfg.roles[name]
		if !validOrgRoleName(name) {
			return fmt.Errorf("org role %q: invalid name", name)
		}
		if role.Name != "" && role.Name != name {
			return fmt.Errorf("org role %q: name mismatch", name)
		}
		if len(role.Scope) == 0 {
			return fmt.Errorf("org role %q: scope must not be empty", name)
		}
		if role.MergeRule != "" && role.MergeRule != "owner" && role.MergeRule != "self" && role.MergeRule != "none" {
			return fmt.Errorf("org role %q: invalid merge_rule %q", name, role.MergeRule)
		}
		if role.RecycleAfter < 0 {
			return fmt.Errorf("org role %q: recycle_after must not be negative", name)
		}
		seenScope := map[string]bool{}
		for _, scope := range role.Scope {
			if _, err := normalizeOrgScope(scope); err != nil {
				return fmt.Errorf("org role %q: %w", name, err)
			}
			if seenScope[scope] {
				return fmt.Errorf("org role %q: duplicate scope %q", name, scope)
			}
			seenScope[scope] = true
		}
		if role.Parent == "" {
			roots++
			if name != "owner" {
				return fmt.Errorf("root org role must be named %q; got %q", "owner", name)
			}
		}
	}
	if len(cfg.roles) == 0 {
		return nil
	}
	if roots != 1 {
		return fmt.Errorf("org: expected exactly one root role, got %d", roots)
	}
	for _, name := range names {
		role := cfg.roles[name]
		if role.Parent != "" {
			if _, ok := cfg.roles[role.Parent]; !ok {
				return fmt.Errorf("org role %q: parent %q is not declared", name, role.Parent)
			}
		}
	}
	for _, name := range names {
		seen := map[string]bool{}
		for current := name; current != ""; {
			if seen[current] {
				return fmt.Errorf("org role %q: parent cycle", name)
			}
			seen[current] = true
			current = cfg.roles[current].Parent
		}
	}
	for _, name := range names {
		role := cfg.roles[name]
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
