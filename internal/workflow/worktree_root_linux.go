//go:build linux

package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/gitmoot/gitmoot/internal/config"
)

// mountInfoPath is a variable so tests can supply a synthetic mount table.
var mountInfoPath = "/proc/self/mountinfo"

type mountEntry struct {
	id         uint64
	mountPoint string
	fsType     string
	source     string
}

// WorktreeStorageDevice measures a path (or its nearest existing ancestor)
// through a pinned descriptor. network is true only for the configured sshfs
// mount, never for a local mount. No pathname lookup of cfg.Root is performed.
// The caller must omit this path, not delete its device from other paths.
func WorktreeStorageDevice(path string, cfg config.WorktreeRoot) (device uint64, network bool, err error) {
	var fd int
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fd, err = unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) || p == filepath.Dir(p) {
			return 0, false, fmt.Errorf("resolve device for %s: %w", path, err)
		}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, false, err
	}
	return uint64(st.Dev), worktreeNetworkFD(fd, cfg), nil
}

// The descriptor pins both the inode and mount while the local mount table is
// read. Unmount/replacement cannot retarget it to the directory underneath or
// recycle its mount ID. Config is a caller-owned snapshot, not reloaded here.
func worktreeNetworkFD(fd int, cfg config.WorktreeRoot) bool {
	if !cfg.Configured() || !cfg.HasMount() || cfg.MountFSType != "fuse.sshfs" {
		return false
	}
	id, err := mountIDAt(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC)
	if err != nil {
		return false
	}
	entries, err := readMountInfo(mountInfoPath)
	if err != nil {
		return false
	}
	var top *mountEntry
	for i := range entries {
		if entries[i].mountPoint == cfg.MountPoint {
			top = &entries[i]
		}
	}
	return top != nil && top.id == id && top.fsType == "fuse.sshfs" && top.source == cfg.MountSource
}

// verifyWorktreeMount proves cfg.MountPoint is currently mounted with the configured source and
// filesystem type, and that cfg.Root lives on that very mount (same mount ID). It returns the
// mount ID. The topmost mount at the mount point wins, as it is the one path lookups reach.
func verifyWorktreeMount(cfg config.WorktreeRoot) (uint64, error) {
	entries, err := readMountInfo(mountInfoPath)
	if err != nil {
		return 0, err
	}
	var top *mountEntry
	for i := range entries {
		if entries[i].mountPoint == cfg.MountPoint {
			top = &entries[i]
		}
	}
	if top == nil {
		return 0, fmt.Errorf("%s is not mounted", cfg.MountPoint)
	}
	if top.fsType != cfg.MountFSType {
		return 0, fmt.Errorf("%s is a %s mount, want %s", cfg.MountPoint, top.fsType, cfg.MountFSType)
	}
	if top.source != cfg.MountSource {
		return 0, fmt.Errorf("%s is mounted from %q, want %q", cfg.MountPoint, top.source, cfg.MountSource)
	}
	id, err := mountIDAt(unix.AT_FDCWD, cfg.Root, 0)
	if err != nil {
		return 0, fmt.Errorf("root %s: %w", cfg.Root, err)
	}
	if id != top.id {
		return 0, fmt.Errorf("root %s is on mount %d, not on %s (mount %d)", cfg.Root, id, cfg.MountPoint, top.id)
	}
	return top.id, nil
}

func mountIDAt(dirfd int, path string, flags int) (uint64, error) {
	var st unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_MNT_ID, &st); err != nil {
		return 0, err
	}
	if st.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("kernel did not report a mount id")
	}
	return st.Mnt_id, nil
}

// mkdirBelowWorktreeRoot creates rel below cfg.Root one component at a time, each relative to a
// directory handle on the verified mount; it never creates cfg.Root itself.
func mkdirBelowWorktreeRoot(cfg config.WorktreeRoot, rel string) error {
	var want uint64
	if cfg.HasMount() {
		id, err := verifyWorktreeMount(cfg)
		if err != nil {
			return err
		}
		want = id
	}
	fd, err := unix.Open(cfg.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open root %s: %w", cfg.Root, err)
	}
	defer func() { _ = unix.Close(fd) }()
	check := func(at string) error {
		if !cfg.HasMount() {
			return nil
		}
		id, err := mountIDAt(fd, "", unix.AT_EMPTY_PATH)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if id != want {
			return fmt.Errorf("%s left the configured mount (mount %d, want %d)", at, id, want)
		}
		return nil
	}
	if err := check(cfg.Root); err != nil {
		return err
	}
	at := cfg.Root
	for _, name := range strings.Split(filepath.ToSlash(rel), "/") {
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			return fmt.Errorf("refusing %q below %s", rel, cfg.Root)
		}
		at = filepath.Join(at, name)
		if err := unix.Mkdirat(fd, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("mkdir %s: %w", at, err)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", at, err)
		}
		_ = unix.Close(fd)
		fd = next
		if err := check(at); err != nil {
			return err
		}
	}
	return nil
}

func readMountInfo(path string) ([]mountEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	var entries []mountEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 5 || sep < 0 || sep+2 >= len(fields) {
			return nil, fmt.Errorf("malformed mount table line %q", line)
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed mount id in %q", line)
		}
		entries = append(entries, mountEntry{
			id:         id,
			mountPoint: unescapeMountField(fields[4]),
			fsType:     fields[sep+1],
			source:     unescapeMountField(fields[sep+2]),
		})
	}
	return entries, nil
}

// unescapeMountField decodes the kernel's octal escapes (\040 space, \011 tab, \012 newline,
// \134 backslash) in mountinfo fields.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
