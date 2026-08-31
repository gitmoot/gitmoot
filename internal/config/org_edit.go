package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// OrgSeatConfigEdit retains the exact pre-edit bytes so a caller coordinating
// config with another owned store can compensate without reformatting or
// reconstructing the prior role.
type OrgSeatConfigEdit struct {
	path     string
	original []byte
	changed  bool
}

func (e OrgSeatConfigEdit) Changed() bool { return e.changed }

// Restore atomically restores the exact config bytes from before the edit.
func (e OrgSeatConfigEdit) Restore() error {
	if !e.changed {
		return nil
	}
	return writeConfigAtomic(e.path, e.original)
}

// UpsertOrgSeatRole adds a role section, fills an empty pane binding, or
// replaces rebindFromPane with the desired binding. Matching the previous
// binding prevents a concurrent config update from being overwritten. Other
// existing role fields are never rewritten.
func UpsertOrgSeatRole(paths Paths, desired OrgRole, rebindFromPane string) (OrgSeatConfigEdit, bool, error) {
	desired.Name = strings.ToLower(strings.TrimSpace(desired.Name))
	desired.Pane = strings.TrimSpace(desired.Pane)
	if desired.Name == "" {
		return OrgSeatConfigEdit{}, false, fmt.Errorf("org seat role name is required")
	}
	cfg, err := LoadOrg(paths)
	if err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	current, exists := cfg.Role(desired.Name)
	if exists {
		currentPane := strings.TrimSpace(current.Pane)
		if currentPane != "" {
			if desired.Pane == "" || currentPane == desired.Pane {
				return OrgSeatConfigEdit{path: paths.ConfigFile}, false, nil
			}
			if strings.TrimSpace(rebindFromPane) != currentPane {
				return OrgSeatConfigEdit{}, false, fmt.Errorf(
					"org role %q already binds pane %q, not expected pane %q",
					desired.Name, current.Pane, rebindFromPane,
				)
			}
		}
		if desired.Pane == "" {
			return OrgSeatConfigEdit{path: paths.ConfigFile}, false, nil
		}
	}

	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	updated := string(original)
	if exists {
		updated, err = setOrgRolePane(updated, desired.Name, desired.Pane)
	} else {
		updated = appendOrgRoleSection(updated, desired)
	}
	if err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	edit := OrgSeatConfigEdit{path: paths.ConfigFile, original: original, changed: true}
	if err := writeOrgConfigBytes(paths, []byte(updated), edit); err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	return edit, !exists, nil
}

// RemoveOrgSeatRole removes exactly one role table and returns the removed role.
// The real org parser validates parent references after the edit.
func RemoveOrgSeatRole(paths Paths, name string) (OrgSeatConfigEdit, OrgRole, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	cfg, err := LoadOrg(paths)
	if err != nil {
		return OrgSeatConfigEdit{}, OrgRole{}, err
	}
	role, ok := cfg.Role(name)
	if !ok {
		return OrgSeatConfigEdit{}, OrgRole{}, fmt.Errorf("org role %q is not declared", name)
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return OrgSeatConfigEdit{}, OrgRole{}, err
	}
	lines := strings.Split(string(original), "\n")
	start, end, ok, err := orgRoleSectionBounds(lines, name)
	if err != nil {
		return OrgSeatConfigEdit{}, OrgRole{}, err
	}
	if !ok {
		return OrgSeatConfigEdit{}, OrgRole{}, fmt.Errorf("org role %q section not found", name)
	}
	lines = append(lines[:start], lines[end:]...)
	edit := OrgSeatConfigEdit{path: paths.ConfigFile, original: original, changed: true}
	if err := writeOrgConfigBytes(paths, []byte(strings.Join(lines, "\n")), edit); err != nil {
		return OrgSeatConfigEdit{}, OrgRole{}, err
	}
	return edit, role, nil
}

func setOrgRolePane(contents, name, pane string) (string, error) {
	lines := strings.Split(contents, "\n")
	start, end, ok, err := orgRoleSectionBounds(lines, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("org role %q section not found", name)
	}
	paneLine := "pane = " + strconv.Quote(pane)
	for i := start + 1; i < end; i++ {
		line := strings.TrimSpace(stripOrgConfigComment(lines[i]))
		key, _, hasValue := strings.Cut(line, "=")
		if hasValue && strings.TrimSpace(key) == "pane" {
			lines[i] = paneLine
			return strings.Join(lines, "\n"), nil
		}
	}
	insert := end
	for insert > start+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	lines = append(lines, "")
	copy(lines[insert+1:], lines[insert:])
	lines[insert] = paneLine
	return strings.Join(lines, "\n"), nil
}

func appendOrgRoleSection(contents string, role OrgRole) string {
	var section strings.Builder
	fmt.Fprintf(&section, "[org.roles.%s]\n", strconv.Quote(role.Name))
	if role.Parent != "" {
		fmt.Fprintf(&section, "parent = %s\n", strconv.Quote(role.Parent))
	}
	fmt.Fprintf(&section, "scope = %s\n", StringListScalar(role.Scope).toml())
	if role.MergeRule != "" {
		fmt.Fprintf(&section, "merge_rule = %s\n", strconv.Quote(role.MergeRule))
	}
	if role.Pane != "" {
		fmt.Fprintf(&section, "pane = %s\n", strconv.Quote(role.Pane))
	}
	if contents != "" && !strings.HasSuffix(contents, "\n") {
		contents += "\n"
	}
	if contents != "" && !strings.HasSuffix(contents, "\n\n") {
		contents += "\n"
	}
	return contents + section.String()
}

func orgRoleSectionBounds(lines []string, want string) (start, end int, found bool, err error) {
	start = -1
	for i, raw := range lines {
		line := strings.TrimSpace(stripOrgConfigComment(raw))
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		_, role, ok, parseErr := parseOrgSection(strings.TrimSpace(line[1 : len(line)-1]))
		if parseErr != nil {
			return 0, 0, false, parseErr
		}
		if start >= 0 {
			return start, i, true, nil
		}
		if ok && role == want {
			start = i
		}
	}
	if start >= 0 {
		return start, len(lines), true, nil
	}
	return 0, 0, false, nil
}

func writeOrgConfigBytes(paths Paths, updated []byte, edit OrgSeatConfigEdit) error {
	if err := writeConfigAtomic(paths.ConfigFile, updated); err != nil {
		return err
	}
	if err := validateConfigFile(paths); err != nil {
		if restoreErr := edit.Restore(); restoreErr != nil {
			return fmt.Errorf("config invalid after org seat edit AND restore failed (file left broken: %v): %w", restoreErr, err)
		}
		return fmt.Errorf("config invalid after org seat edit (reverted): %w", err)
	}
	return nil
}
