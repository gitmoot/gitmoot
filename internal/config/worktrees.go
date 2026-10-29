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
type WorktreeRoot struct {
	Root        string
	MountPoint  string
	MountSource string
	MountFSType string
}

// Configured reports whether [worktrees] supplied a root.
func (w WorktreeRoot) Configured() bool { return w.Root != "" }

// HasMount reports whether the root must be proven to sit on a specific mount.
func (w WorktreeRoot) HasMount() bool { return w.MountPoint != "" }

var worktreeRootKeys = []string{"root", "mount_point", "mount_source", "mount_fstype"}

// LoadWorktreeRoot parses [worktrees]. A missing config file or section is the default layout.
func LoadWorktreeRoot(paths Paths) (WorktreeRoot, error) {
	values, err := loadSingleSectionValues(paths, "worktrees")
	if err != nil || values == nil {
		return WorktreeRoot{}, err
	}
	return buildWorktreeRoot(values)
}

// loadSingleSectionValues returns the raw key = value pairs of a top-level
// [name] section that may appear at most once, or nil when the file or the
// section is absent. Duplicate sections or keys and malformed headers that
// target name are errors; values are returned unparsed.
func loadSingleSectionValues(paths Paths, name string) (map[string]string, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var values map[string]string
	inSection := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, name) {
				return nil, fmt.Errorf("parse %s section: missing closing ]", name)
			}
			inSection = strings.TrimSpace(section) == name
			if inSection {
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

func buildWorktreeRoot(values map[string]string) (WorktreeRoot, error) {
	for key := range values {
		known := false
		for _, k := range worktreeRootKeys {
			known = known || k == key
		}
		if !known {
			return WorktreeRoot{}, fmt.Errorf("[worktrees]: unknown key %q", key)
		}
	}
	text := func(key string) (string, error) {
		raw, ok := values[key]
		if !ok {
			return "", nil
		}
		s, err := strconv.Unquote(raw)
		if err != nil || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("[worktrees]: %s must be a non-empty quoted string", key)
		}
		return strings.TrimSpace(s), nil
	}
	absolute := func(key, s string) (string, error) {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s || s == "/" {
			return "", fmt.Errorf("[worktrees]: %s %q must be a clean absolute path other than /", key, s)
		}
		return s, nil
	}
	var w WorktreeRoot
	var err error
	if w.Root, err = text("root"); err != nil {
		return WorktreeRoot{}, err
	}
	if w.Root == "" {
		return WorktreeRoot{}, fmt.Errorf("[worktrees]: root is required")
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
		return WorktreeRoot{}, fmt.Errorf("[worktrees]: mount_point, mount_source and mount_fstype go together (all three or none)")
	}
	if w.MountPoint != "" {
		if w.MountPoint, err = absolute("mount_point", w.MountPoint); err != nil {
			return WorktreeRoot{}, err
		}
		if w.Root == w.MountPoint {
			return WorktreeRoot{}, fmt.Errorf("[worktrees]: root must be a directory inside mount_point, not the mount point itself")
		}
		if rel, err := filepath.Rel(w.MountPoint, w.Root); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return WorktreeRoot{}, fmt.Errorf("[worktrees]: root %q is not inside mount_point %q", w.Root, w.MountPoint)
		}
	}
	return w, nil
}
