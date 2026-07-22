// Package org defines the configured organization registry and its live-state
// provider contract. It deliberately has no config, database, CLI, or Herdr
// dependencies so parsing and policy validation stay deterministic and testable.
package org

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type EnforceMode string

const (
	EnforceWarn  EnforceMode = "warn"
	EnforceBlock EnforceMode = "block"
)

type Role struct {
	Name      string   `json:"name"`
	Parent    string   `json:"parent,omitempty"`
	Scope     []string `json:"scope"`
	MergeRule string   `json:"merge_rule"`
}

type Registry struct {
	Enforce EnforceMode     `json:"enforce,omitempty"`
	Roles   map[string]Role `json:"roles,omitempty"`
}

func (r Registry) Enabled() bool { return len(r.Roles) > 0 }

func (r Registry) Role(name string) (Role, bool) {
	role, ok := r.Roles[strings.TrimSpace(name)]
	return role, ok
}

func (r Registry) SortedRoles() []Role {
	roles := make([]Role, 0, len(r.Roles))
	for _, role := range r.Roles {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })
	return roles
}

func (r Registry) Children(name string) []Role {
	var children []Role
	for _, role := range r.Roles {
		if role.Parent == name {
			children = append(children, role)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
	return children
}

func (r Registry) Path(name string) []string {
	role, ok := r.Role(name)
	if !ok {
		return nil
	}
	path := []string{role.Name}
	visited := map[string]bool{role.Name: true}
	for role.Parent != "" {
		parent, ok := r.Role(role.Parent)
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

func (r Registry) Matches(roleName, repo string) bool {
	role, ok := r.Role(roleName)
	if !ok {
		return false
	}
	for _, pattern := range role.Scope {
		if ScopeMatches(pattern, repo) {
			return true
		}
	}
	return false
}

// ScopeMatches implements the frozen v1 scope grammar: *, owner/*, */repo, and
// owner/repo. Scope is display-only in #1059; this helper is the canonical
// matcher for the later enforcement change.
func ScopeMatches(pattern, repo string) bool {
	pattern = strings.TrimSpace(pattern)
	repo = strings.TrimSpace(repo)
	if validateScope(pattern) != nil {
		return false
	}
	if pattern == "*" {
		return validRepo(repo)
	}
	patternOwner, patternRepo, ok := splitScope(pattern)
	if !ok || !validRepo(repo) {
		return false
	}
	repoOwner, repoName, _ := strings.Cut(repo, "/")
	return (patternOwner == "*" || patternOwner == repoOwner) && (patternRepo == "*" || patternRepo == repoName)
}

// ParseRegistry parses only [org] and [org.roles."name"] sections. An absent
// org configuration is disabled/open. Once any org section is present, every
// malformed field or hierarchy is returned as an error so callers fail closed.
func ParseRegistry(data []byte) (Registry, error) {
	registry := Registry{Roles: map[string]Role{}}
	roleFields := map[string]map[string]bool{}
	seenOrg := false
	seenOrgSection := false
	seenEnforce := false
	sectionKind := ""
	sectionRole := ""

	for lineNumber, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			sectionKind, sectionRole = "", ""
			if !strings.HasSuffix(line, "]") {
				candidate := strings.TrimSpace(strings.TrimPrefix(line, "["))
				if candidate == "org.roles" || strings.HasPrefix(candidate, "org.roles.") {
					return Registry{}, fmt.Errorf("line %d: malformed org role section header", lineNumber+1)
				}
				continue
			}
			section := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			switch {
			case section == "org":
				if seenOrgSection {
					return Registry{}, fmt.Errorf("line %d: duplicate [org] section", lineNumber+1)
				}
				seenOrg, seenOrgSection = true, true
				sectionKind, sectionRole = "org", ""
			case section == "org.roles" || strings.HasPrefix(section, "org.roles."):
				seenOrg = true
				name, err := parseRoleSection(section)
				if err != nil {
					return Registry{}, fmt.Errorf("line %d: %w", lineNumber+1, err)
				}
				if _, exists := registry.Roles[name]; exists {
					return Registry{}, fmt.Errorf("line %d: duplicate org role %q", lineNumber+1, name)
				}
				registry.Roles[name] = Role{Name: name}
				roleFields[name] = map[string]bool{}
				sectionKind, sectionRole = "role", name
			default:
				continue
			}
			continue
		}

		if sectionKind == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return Registry{}, fmt.Errorf("line %d: malformed org field", lineNumber+1)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if sectionKind == "org" {
			if key != "enforce" {
				return Registry{}, fmt.Errorf("line %d: unknown [org] field %q", lineNumber+1, key)
			}
			if seenEnforce {
				return Registry{}, fmt.Errorf("line %d: duplicate [org].enforce", lineNumber+1)
			}
			seenEnforce = true
			parsed, err := parseString(value)
			if err != nil {
				return Registry{}, fmt.Errorf("line %d: parse [org].enforce: %w", lineNumber+1, err)
			}
			registry.Enforce = EnforceMode(parsed)
			continue
		}

		if key != "parent" && key != "scope" && key != "merge_rule" {
			return Registry{}, fmt.Errorf("line %d: unknown org role field %q", lineNumber+1, key)
		}
		if roleFields[sectionRole][key] {
			return Registry{}, fmt.Errorf("line %d: duplicate field %q for org role %q", lineNumber+1, key, sectionRole)
		}
		roleFields[sectionRole][key] = true
		role := registry.Roles[sectionRole]
		switch key {
		case "parent":
			parsed, err := parseString(value)
			if err != nil {
				return Registry{}, fmt.Errorf("line %d: parse org role %q parent: %w", lineNumber+1, sectionRole, err)
			}
			role.Parent = strings.TrimSpace(parsed)
		case "scope":
			parsed, err := parseStringArray(value)
			if err != nil {
				return Registry{}, fmt.Errorf("line %d: parse org role %q scope: %w", lineNumber+1, sectionRole, err)
			}
			role.Scope = parsed
		case "merge_rule":
			parsed, err := parseString(value)
			if err != nil {
				return Registry{}, fmt.Errorf("line %d: parse org role %q merge_rule: %w", lineNumber+1, sectionRole, err)
			}
			role.MergeRule = strings.TrimSpace(parsed)
		}
		registry.Roles[sectionRole] = role
	}

	if !seenOrg {
		return Registry{}, nil
	}
	// #1067/#1073 reconcile: presence of any [org.roles.*] turns the registry ON;
	// an explicit [org] section is OPTIONAL (its only key, enforce, is optional and
	// defaults to block — it cannot be an activation switch). This matches the
	// authoritative config.LoadOrg semantics (OrgConfig.Enforce() defaults to
	// "block"). A roles-only config is valid, not malformed. Genuinely malformed
	// input (bad enforce value, cycles, out-of-subset scope) still fails closed below.
	if registry.Enforce == "" {
		registry.Enforce = EnforceBlock
	}
	if registry.Enforce != EnforceWarn && registry.Enforce != EnforceBlock {
		return Registry{}, fmt.Errorf("[org].enforce must be %q or %q", EnforceWarn, EnforceBlock)
	}
	if err := registry.Validate(); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

func (r Registry) Validate() error {
	if len(r.Roles) == 0 {
		return fmt.Errorf("org registry must declare at least one role")
	}
	// Iterate in sorted name order across every pass so fail-closed errors are
	// deterministic regardless of Go's map iteration order. The passes are also
	// ordered by structural precedence: a role's own fields and the single-owner
	// root rule are validated before parent references, so a mis-named root
	// always surfaces as a root error rather than an incidental undeclared-parent
	// error on some child (whichever the map happened to visit first).
	names := make([]string, 0, len(r.Roles))
	for name := range r.Roles {
		names = append(names, name)
	}
	sort.Strings(names)

	// Pass 1: per-role field validity + root identification.
	roots := 0
	for _, name := range names {
		role := r.Roles[name]
		if strings.TrimSpace(name) == "" || role.Name != name {
			return fmt.Errorf("invalid org role name %q", name)
		}
		if len(role.Scope) == 0 {
			return fmt.Errorf("org role %q requires a non-empty scope", name)
		}
		seenScope := map[string]bool{}
		for _, scope := range role.Scope {
			if err := validateScope(scope); err != nil {
				return fmt.Errorf("org role %q scope %q: %w", name, scope, err)
			}
			if seenScope[scope] {
				return fmt.Errorf("org role %q has duplicate scope %q", name, scope)
			}
			seenScope[scope] = true
		}
		// #1057 reconcile: merge_rule is OPTIONAL — "deliberately advisory in
		// phase 1a" (see config.OrgConfig). A role may omit it, matching the
		// authoritative config.LoadOrg acceptance rule. A malformed merge_rule
		// line (key with no value) still fails closed in the field parser above.
		if role.Parent == "" {
			roots++
			if name != "owner" {
				return fmt.Errorf("root org role must be named %q; got %q", "owner", name)
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("org registry requires exactly one root role named owner; got %d roots", roots)
	}

	// Pass 2: parent references.
	for _, name := range names {
		role := r.Roles[name]
		if role.Parent == "" {
			continue
		}
		if role.Parent == name {
			return fmt.Errorf("org role %q cannot parent itself", name)
		}
		if _, ok := r.Roles[role.Parent]; !ok {
			return fmt.Errorf("org role %q references undeclared parent %q", name, role.Parent)
		}
	}

	// Pass 3: cycle detection.
	for _, name := range names {
		seen := map[string]bool{}
		for current := name; current != ""; current = r.Roles[current].Parent {
			if seen[current] {
				return fmt.Errorf("org role hierarchy contains a cycle at %q", current)
			}
			seen[current] = true
		}
	}

	// Pass 4: child scope must be a subset of its parent's scope.
	for _, name := range names {
		role := r.Roles[name]
		if role.Parent == "" {
			continue
		}
		parent := r.Roles[role.Parent]
		for _, childScope := range role.Scope {
			covered := false
			for _, parentScope := range parent.Scope {
				if scopeContains(parentScope, childScope) {
					covered = true
					break
				}
			}
			if !covered {
				return fmt.Errorf("org role %q scope %q is not a subset of parent %q scope", name, childScope, role.Parent)
			}
		}
	}
	return nil
}

func parseRoleSection(section string) (string, error) {
	raw := strings.TrimPrefix(section, "org.roles.")
	if raw == "" || !strings.HasPrefix(raw, "\"") || !strings.HasSuffix(raw, "\"") {
		return "", fmt.Errorf("org role section must be [org.roles.\"name\"]")
	}
	name, err := strconv.Unquote(raw)
	if err != nil || strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
		return "", fmt.Errorf("invalid org role section %q", section)
	}
	if strings.ContainsAny(name, "[]/\t\r\n") {
		return "", fmt.Errorf("invalid org role name %q", name)
	}
	return name, nil
}

func parseString(value string) (string, error) {
	if !strings.HasPrefix(value, "\"") || !strings.HasSuffix(value, "\"") {
		return "", fmt.Errorf("must be a quoted string")
	}
	return strconv.Unquote(value)
}

func parseStringArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("must be an array of quoted strings")
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return nil, nil
	}
	var parts []string
	start, quoted, escaped := 0, false, false
	for i, runeValue := range body {
		switch {
		case escaped:
			escaped = false
		case runeValue == '\\' && quoted:
			escaped = true
		case runeValue == '"':
			quoted = !quoted
		case runeValue == ',' && !quoted:
			parts = append(parts, body[start:i])
			start = i + 1
		}
	}
	if quoted || escaped {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	parts = append(parts, body[start:])
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		parsed, err := parseString(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		parsed = strings.TrimSpace(parsed)
		if parsed == "" {
			return nil, fmt.Errorf("scope entries must be non-empty")
		}
		result = append(result, parsed)
	}
	return result, nil
}

func stripComment(line string) string {
	quoted, escaped := false, false
	for i, runeValue := range line {
		switch {
		case escaped:
			escaped = false
		case runeValue == '\\' && quoted:
			escaped = true
		case runeValue == '"':
			quoted = !quoted
		case runeValue == '#' && !quoted:
			return line[:i]
		}
	}
	return line
}

func validateScope(scope string) error {
	if scope == "*" {
		return nil
	}
	owner, repo, ok := splitScope(scope)
	if !ok || (owner == "*" && repo == "*") {
		return fmt.Errorf("must be *, owner/*, */repo, or owner/repo")
	}
	return nil
}

func splitScope(scope string) (string, string, bool) {
	if scope != strings.TrimSpace(scope) || strings.Count(scope, "/") != 1 {
		return "", "", false
	}
	owner, repo, ok := strings.Cut(scope, "/")
	if !ok || owner == "" || repo == "" || strings.ContainsAny(owner+repo, " \t\r\n") {
		return "", "", false
	}
	return owner, repo, true
}

func validRepo(repo string) bool {
	owner, name, ok := splitScope(repo)
	return ok && owner != "*" && name != "*"
}

func scopeContains(parent, child string) bool {
	if parent == "*" {
		return true
	}
	if child == "*" {
		return false
	}
	parentOwner, parentRepo, parentOK := splitScope(parent)
	childOwner, childRepo, childOK := splitScope(child)
	if !parentOK || !childOK {
		return false
	}
	ownerCovered := parentOwner == "*" || parentOwner == childOwner
	repoCovered := parentRepo == "*" || parentRepo == childRepo
	return ownerCovered && repoCovered
}
