package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WorktreeRoot places NEW gitmoot-created worktrees outside the home (#2375). The database, WAL,
// caches, artifacts and receipts always stay in the home.
//
//	[worktrees]
//	root         = "/mnt/storagebox-workspaces/worktrees"  # absolute; must already exist
//	mount_point  = "/mnt/storagebox-workspaces"            # optional; all three mount keys or none
//	mount_source = "u666042@u666042.your-storagebox.de:fleet-workspaces"
//	mount_fstype = "fuse.sshfs"
//
// With the mount keys, every allocation, reuse and cleanup under root first proves that root lives
// on exactly that mount; otherwise the work is refused. Gitmoot never creates root itself, so an
// absent mount can never be silently replaced by a directory on the local disk.
//
// An absent section means the default <home>/worktrees. A present but invalid section is a load
// ERROR: callers refuse rather than fall back to the default.
//
// A root that is moved keeps the worktrees already created under it. List each former root as a
// retired root, with the mount keys it had (#2763):
//
//	[[worktrees.retired]]
//	root         = "/mnt/storagebox-workspaces/worktrees"
//	mount_point  = "/mnt/storagebox-workspaces"
//	mount_source = "u666042@u666042.your-storagebox.de:fleet-workspaces"
//	mount_fstype = "fuse.sshfs"
//
// A retired root is validated like root and is only ever used to recognise an EXISTING worktree
// (reopening its job, cleaning it up, doctor accounting), after the same preflight against its own
// mount keys. New worktrees are never created under it. [[worktrees.retired]] needs no [worktrees]
// section (moving back to <home>/worktrees). No two of root, <home>/worktrees (always recognised)
// and the retired roots may overlap (be equal or nested), so every existing worktree path has one
// owning root and one preflight.
type WorktreeRoot struct {
	Root        string
	MountPoint  string
	MountSource string
	MountFSType string
}

// WorktreeRoots is the loaded [worktrees] configuration: Active places new worktrees (zero for the
// default <home>/worktrees), Retired only identifies existing ones.
type WorktreeRoots struct {
	Active  WorktreeRoot
	Retired []WorktreeRoot
}

// Configured reports whether [worktrees] supplied a root.
func (w WorktreeRoot) Configured() bool { return w.Root != "" }

// HasMount reports whether the root must be proven to sit on a specific mount.
func (w WorktreeRoot) HasMount() bool { return w.MountPoint != "" }

var worktreeRootKeys = []string{"root", "mount_point", "mount_source", "mount_fstype"}

// LoadWorktreeRoots parses [worktrees] and every [[worktrees.retired]] from one read of the config
// file. A missing config file or section is the default layout.
func LoadWorktreeRoots(paths Paths) (WorktreeRoots, error) {
	content, err := readConfigContent(paths)
	if err != nil || content == "" {
		return WorktreeRoots{}, err
	}
	values, err := singleSectionValues(content, "worktrees")
	if err != nil {
		return WorktreeRoots{}, err
	}
	retired, err := arrayTableValues(content, "worktrees.retired")
	if err != nil {
		return WorktreeRoots{}, err
	}
	var roots WorktreeRoots
	type owner struct{ label, root string }
	var owners []owner
	claim := func(label, root string) error {
		for _, o := range owners {
			if worktreeRootsOverlap(o.root, root) {
				return fmt.Errorf("%s: root %q overlaps %s root %q; a worktree under both would have two owners", label, root, o.label, o.root)
			}
		}
		owners = append(owners, owner{label, root})
		return nil
	}
	if home := strings.TrimSpace(paths.Home); home != "" {
		owners = append(owners, owner{"<home>/worktrees (always recognised)", filepath.Join(filepath.Clean(home), "worktrees")})
	}
	if values != nil {
		if roots.Active, err = buildWorktreeRoot("[worktrees]", values); err != nil {
			return WorktreeRoots{}, err
		}
		if err := claim("[worktrees]", roots.Active.Root); err != nil {
			return WorktreeRoots{}, err
		}
	}
	for i, entry := range retired {
		label := fmt.Sprintf("[[worktrees.retired]] #%d", i+1)
		r, err := buildWorktreeRoot(label, entry)
		if err != nil {
			return WorktreeRoots{}, err
		}
		if err := claim(label, r.Root); err != nil {
			return WorktreeRoots{}, err
		}
		roots.Retired = append(roots.Retired, r)
	}
	return roots, nil
}

// worktreeRootsOverlap reports whether two clean absolute roots are equal or one contains the other.
func worktreeRootsOverlap(a, b string) bool {
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(b, a+sep) || strings.HasPrefix(a, b+sep)
}

// readConfigContent returns the config file's text, or "" when it does not exist.
func readConfigContent(paths Paths) (string, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(content), err
}

// loadSingleSectionValues reads the config file and returns singleSectionValues of it.
func loadSingleSectionValues(paths Paths, name string) (map[string]string, error) {
	content, err := readConfigContent(paths)
	if err != nil || content == "" {
		return nil, err
	}
	return singleSectionValues(content, name)
}

// singleSectionValues returns the raw key = value pairs of a top-level [name] table that may
// appear at most once, or nil when it is absent. Headers are read as TOML key paths, so
// [ name ], ["name"] and ['name'] all open it. Duplicate tables or keys, [[name]], and malformed
// headers that target name are errors; values are returned unparsed.
func singleSectionValues(content, name string) (map[string]string, error) {
	var values map[string]string
	inSection := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if _, ok := sectionHeader(line); ok {
			header := parseTableHeader(line)
			if header.malformedTargets(name) {
				return nil, fmt.Errorf("parse %s: malformed header %q", name, line)
			}
			inSection = header.path == name
			if inSection {
				if header.array {
					return nil, fmt.Errorf("parse %s: header %q must be written [%s]", name, line, name)
				}
				if values != nil {
					return nil, fmt.Errorf("parse %s: [%s] appears twice", name, name)
				}
				values = map[string]string{}
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("parse %s: line %q is not key = value", name, line)
		}
		key = strings.TrimSpace(key)
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("parse %s: %s set twice", name, key)
		}
		values[key] = strings.TrimSpace(value)
	}
	return values, nil
}

// arrayTableValues returns the raw key = value pairs of every [[name]] array-of-tables entry, in
// file order, or nil when there are none. Headers are read as TOML key paths, so
// [[ worktrees . retired ]] and [[worktrees."retired"]] are entries too. The table form [name]
// in any spelling, a malformed header that targets name, and duplicate keys in an entry are
// errors.
func arrayTableValues(content, name string) ([]map[string]string, error) {
	var entries []map[string]string
	var current map[string]string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if _, ok := sectionHeader(line); ok {
			header := parseTableHeader(line)
			current = nil
			switch {
			case header.malformedTargets(name):
				return nil, fmt.Errorf("parse %s: malformed header %q", name, line)
			case header.path == name && !header.array:
				return nil, fmt.Errorf("parse %s: header %q must be written [[%s]]", name, line, name)
			case header.path == name:
				current = map[string]string{}
				entries = append(entries, current)
			}
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("parse %s: line %q is not key = value", name, line)
		}
		key = strings.TrimSpace(key)
		if _, dup := current[key]; dup {
			return nil, fmt.Errorf("parse %s: %s set twice", name, key)
		}
		current[key] = strings.TrimSpace(value)
	}
	return entries, nil
}

// buildWorktreeRoot validates one root and its optional mount keys; label names the section in
// errors ([worktrees] or one [[worktrees.retired]] entry).
func buildWorktreeRoot(label string, values map[string]string) (WorktreeRoot, error) {
	for key := range values {
		known := false
		for _, k := range worktreeRootKeys {
			known = known || k == key
		}
		if !known {
			return WorktreeRoot{}, fmt.Errorf("%s: unknown key %q", label, key)
		}
	}
	text := func(key string) (string, error) {
		raw, ok := values[key]
		if !ok {
			return "", nil
		}
		s, err := strconv.Unquote(raw)
		if err != nil || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("%s: %s must be a non-empty quoted string", label, key)
		}
		return strings.TrimSpace(s), nil
	}
	absolute := func(key, s string) (string, error) {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s || s == "/" {
			return "", fmt.Errorf("%s: %s %q must be a clean absolute path other than /", label, key, s)
		}
		return s, nil
	}
	var w WorktreeRoot
	var err error
	if w.Root, err = text("root"); err != nil {
		return WorktreeRoot{}, err
	}
	if w.Root == "" {
		return WorktreeRoot{}, fmt.Errorf("%s: root is required", label)
	}
	if w.Root, err = absolute("root", w.Root); err != nil {
		return WorktreeRoot{}, err
	}
	if w.MountPoint, err = text("mount_point"); err != nil {
		return WorktreeRoot{}, err
	}
	if w.MountSource, err = text("mount_source"); err != nil {
		return WorktreeRoot{}, err
	}
	if w.MountFSType, err = text("mount_fstype"); err != nil {
		return WorktreeRoot{}, err
	}
	set := 0
	for _, s := range []string{w.MountPoint, w.MountSource, w.MountFSType} {
		if s != "" {
			set++
		}
	}
	if set != 0 && set != 3 {
		return WorktreeRoot{}, fmt.Errorf("%s: mount_point, mount_source and mount_fstype go together (all three or none)", label)
	}
	if w.MountPoint != "" {
		if w.MountPoint, err = absolute("mount_point", w.MountPoint); err != nil {
			return WorktreeRoot{}, err
		}
		if w.Root == w.MountPoint {
			return WorktreeRoot{}, fmt.Errorf("%s: root must be a directory inside mount_point, not the mount point itself", label)
		}
		if rel, err := filepath.Rel(w.MountPoint, w.Root); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return WorktreeRoot{}, fmt.Errorf("%s: root %q is not inside mount_point %q", label, w.Root, w.MountPoint)
		}
	}
	return w, nil
}
