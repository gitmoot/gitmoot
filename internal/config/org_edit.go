package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OrgSeatConfigEdit retains the exact pre-edit bytes so a caller coordinating
// config with another owned store can compensate without reformatting or
// reconstructing the prior role.
type OrgSeatConfigEdit struct {
	path     string
	original []byte
	// written is the exact file this edit installed; Restore rolls back only
	// while the config still holds it.
	written []byte
	changed bool
}

func (e OrgSeatConfigEdit) Changed() bool { return e.changed }

// ErrOrgSeatConfigChangedSinceEdit reports that Restore did not roll back
// because the config was edited after this edit (by the daemon's automatic
// seat rebind, another command, or by hand): those edits win, and the caller
// must reconcile the role by hand.
var ErrOrgSeatConfigChangedSinceEdit = errors.New("config changed since this edit; not restored")

// Restore rolls the config back to its bytes from before the edit, under the
// config edit lock, but only if the file is still exactly what this edit
// wrote (#2510). Blindly writing the old bytes would silently revert any edit
// made in between, such as a seat rebind.
func (e OrgSeatConfigEdit) Restore() error {
	if !e.changed {
		return nil
	}
	release, err := lockConfigEdits(Paths{ConfigFile: e.path})
	if err != nil {
		return err
	}
	defer release()
	return e.restoreLocked()
}

// restoreLocked is Restore for a caller that already holds the lock.
func (e OrgSeatConfigEdit) restoreLocked() error {
	current, err := os.ReadFile(e.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, e.written) {
		return ErrOrgSeatConfigChangedSinceEdit
	}
	return writeConfigAtomic(e.path, e.original)
}

// UpsertOrgSeatRole adds a role section, fills an empty pane binding, or
// replaces rebindFromPane with the desired binding. The guard and edit use the
// same file snapshot, so a concurrent config update cannot pass validation
// against stale bytes. Other existing role fields are never rewritten.
func UpsertOrgSeatRole(paths Paths, desired OrgRole, rebindFromPane string) (OrgSeatConfigEdit, bool, error) {
	// #2502: serialised with the daemon's automatic seat rebind.
	release, err := lockConfigEdits(paths)
	if err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	defer release()
	desired.Name = strings.ToLower(strings.TrimSpace(desired.Name))
	desired.Pane = strings.TrimSpace(desired.Pane)
	if desired.Name == "" {
		return OrgSeatConfigEdit{}, false, fmt.Errorf("org seat role name is required")
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	cfg, err := parseOrgContent(original)
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
	if err := writeOrgConfigBytes(paths, []byte(updated), &edit); err != nil {
		return OrgSeatConfigEdit{}, false, err
	}
	return edit, !exists, nil
}

// ErrOrgSeatBindingChanged reports that a conditional rebind found the role
// missing, its binding no longer the expected one, or the config file edited
// since it was read. Nothing was written.
var ErrOrgSeatBindingChanged = errors.New("org seat binding changed; rebind not applied")

// RebindOrgSeatPane replaces the pane binding of an EXISTING role only while it
// still reads exactly from (#2502). Unlike UpsertOrgSeatRole it never creates a
// role and never fills an empty binding, so a background writer cannot undo an
// operator's removal or unbind.
//
// It holds the config edit lock, so no other gitmoot command interleaves. A
// hand edit in an editor takes no lock, so on Linux the new file is installed
// with an atomic exchange and the file it replaced is compared with the one
// the guard read: if anything changed in between, the exchange is reversed,
// putting the operator's file back. On other platforms the comparison runs
// just before the rename, leaving a sub-millisecond window. Validation after
// installing reverses the change on any parse failure.
func RebindOrgSeatPane(paths Paths, name, from, to string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if name == "" || from == "" || to == "" || from == to {
		return fmt.Errorf("org seat rebind needs a role and two different bindings")
	}
	release, err := lockConfigEdits(paths)
	if err != nil {
		return err
	}
	defer release()
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	cfg, err := parseOrgContent(original)
	if err != nil {
		return err
	}
	current, exists := cfg.Role(name)
	if !exists || strings.TrimSpace(current.Pane) != from {
		return fmt.Errorf("%w: role %q", ErrOrgSeatBindingChanged, name)
	}
	updated, err := setOrgRolePane(string(original), name, to)
	if err != nil {
		return err
	}
	if !canExchangeFiles {
		latest, err := os.ReadFile(paths.ConfigFile)
		if err != nil {
			return err
		}
		if !bytes.Equal(latest, original) {
			return fmt.Errorf("%w: config file edited during the rebind", ErrOrgSeatBindingChanged)
		}
		return writeOrgConfigBytes(paths, []byte(updated), &OrgSeatConfigEdit{path: paths.ConfigFile, original: original, changed: true})
	}
	staged, err := stageConfigFile(paths.ConfigFile, []byte(updated))
	if err != nil {
		return err
	}
	keepStaged := false
	defer func() {
		if !keepStaged {
			_ = os.Remove(staged)
		}
	}()
	beforeConfigExchange()
	if err := exchangeFiles(staged, paths.ConfigFile); err != nil {
		return err
	}
	afterConfigExchange()
	// undo puts the replaced file back, but only while the config still holds
	// the file this rebind installed: if a newer edit already replaced it,
	// that newer edit wins and stays (the replaced file it superseded is
	// dropped). A further unlocked write between this check and the swap is
	// the remaining window; gitmoot's own writers are excluded by the lock.
	undo := func() error {
		current, err := os.ReadFile(paths.ConfigFile)
		if err != nil || !bytes.Equal(current, []byte(updated)) {
			return err
		}
		return exchangeFiles(staged, paths.ConfigFile)
	}
	// staged now holds exactly the file this exchange replaced.
	replaced, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(replaced, original) {
		if undoErr := undo(); undoErr != nil {
			keepStaged = true
			return fmt.Errorf("config file edited during the rebind and restoring it failed (the operator's file is kept at %s): %w", staged, undoErr)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: config file edited during the rebind", ErrOrgSeatBindingChanged)
	}
	if err := validateConfigFile(paths); err != nil {
		if undoErr := undo(); undoErr != nil {
			keepStaged = true
			return fmt.Errorf("rebound config is invalid (%v) and restoring it failed (the previous file is kept at %s): %w", err, staged, undoErr)
		}
		return err
	}
	return nil
}

// beforeConfigExchange and afterConfigExchange run just before and just after
// a conditional rebind installs its file; tests use them to land concurrent
// hand edits in those windows.
var (
	beforeConfigExchange = func() {}
	afterConfigExchange  = func() {}
)

// stageConfigFile writes contents to a synced 0600 temporary file beside path.
func stageConfigFile(path string, contents []byte) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_, err = tmp.Write(contents)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// RemoveOrgSeatRole removes exactly one role table and returns the removed role.
// The real org parser validates parent references after the edit.
func RemoveOrgSeatRole(paths Paths, name string) (OrgSeatConfigEdit, OrgRole, error) {
	// #2502: serialised with the daemon's automatic seat rebind.
	release, err := lockConfigEdits(paths)
	if err != nil {
		return OrgSeatConfigEdit{}, OrgRole{}, err
	}
	defer release()
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
	if err := writeOrgConfigBytes(paths, []byte(strings.Join(lines, "\n")), &edit); err != nil {
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

// writeOrgConfigBytes runs under the caller's config edit lock and records
// the installed bytes on edit, so a later Restore can tell whether anything
// changed the file since.
func writeOrgConfigBytes(paths Paths, updated []byte, edit *OrgSeatConfigEdit) error {
	edit.written = updated
	if err := writeConfigAtomic(paths.ConfigFile, updated); err != nil {
		return err
	}
	if err := validateConfigFile(paths); err != nil {
		if restoreErr := edit.restoreLocked(); restoreErr != nil {
			return fmt.Errorf("config invalid after org seat edit AND restore failed (file left broken: %v): %w", restoreErr, err)
		}
		return fmt.Errorf("config invalid after org seat edit (reverted): %w", err)
	}
	return nil
}

// AppendOrgStarterConfig appends the starter org registry while holding the
// config edit lock (#2510), and only if the config still has no org roles
// when the lock is held, so a concurrent edit is neither lost nor doubled.
// It returns whether it appended.
func AppendOrgStarterConfig(paths Paths, starter string) (bool, error) {
	release, err := lockConfigEdits(paths)
	if err != nil {
		return false, err
	}
	defer release()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return false, err
	}
	cfg, err := parseOrgContent(content)
	if err != nil {
		return false, err
	}
	if cfg.Enabled() {
		return false, nil
	}
	return true, writeConfigAtomic(paths.ConfigFile, append(content, []byte(starter)...))
}
