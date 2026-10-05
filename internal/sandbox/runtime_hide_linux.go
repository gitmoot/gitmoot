//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// containerRuntimePaths are the local container-runtime endpoints a read-only
// seat must not reach: API sockets, and the directories that hold per-daemon,
// per-plugin, and per-container runtime sockets. Landlock's filesystem rules do
// not govern connect(2) on a Unix socket, so a read-only seat could otherwise
// drive the runtime that owns them. Each /run entry also covers /var/run, which
// is usually the same directory.
//
// An existing socket (or other file) is covered with /dev/null, where a connect
// reaches a character device and is refused. An existing directory is covered
// with an empty read-only tmpfs: containerd's per-container shim sockets under
// /run/containerd/s cannot be named in advance, and dockerd keeps its embedded
// containerd, metrics, libnetwork, and plugin sockets under /run/docker.
//
// This is a fixed list of local endpoints. A runtime listening on TCP is not
// covered: seats need the network.
//
// These are system paths: only root can create or replace them, so they are
// symlink-resolved, and any error resolving one refuses the seat.
var containerRuntimePaths = []string{
	"/run/docker.sock",
	"/run/docker",
	"/run/containerd",
	"/run/cri-dockerd.sock",
	"/run/podman",
	"/run/buildkit",
	"/run/crio",
	"/run/k3s/containerd",
	"/run/k0s",
	"/var/snap/microk8s/common/run",
}

// containerRuntimeUserRunPaths are rootless runtime endpoints, relative to each
// user's runtime directory /run/user/<uid>.
var containerRuntimeUserRunPaths = []string{
	"docker.sock",
	"docker",
	"podman",
	"buildkit",
	"dockerd-rootless",
	"containerd-rootless",
}

// containerRuntimeHomePaths are per-user runtime directories, relative to a
// home directory: Colima, Lima, Rancher Desktop, and Docker Desktop keep their
// docker and containerd sockets there.
var containerRuntimeHomePaths = []string{
	".colima",
	".config/colima",
	".lima",
	".rd",
	".docker/run",
	".docker/desktop",
}

// runtimeCandidates is where discovery looks for runtime endpoints.
type runtimeCandidates struct {
	// system holds glob patterns of root-owned paths.
	system []string
	// perUser holds paths below directories a user owns.
	perUser []perUserRuntimePath
}

// perUserRuntimePath is a runtime path below each directory root matches (a
// home or /run/user/<uid>). The user who owns that directory controls
// everything below it, so the daemon never follows a symlink there: see
// openUserRuntimePath.
type perUserRuntimePath struct {
	root string // glob pattern of the per-user directories
	rel  string // slash-separated path below each
}

// containerRuntimeCandidates expands the fixed lists: every /run entry also
// under /var/run, the rootless entries under each /run/user/*, and every home
// path under /root, each /home/* directory, and the launching user's own home.
func containerRuntimeCandidates() runtimeCandidates {
	var candidates runtimeCandidates
	for _, path := range containerRuntimePaths {
		candidates.system = append(candidates.system, path)
		if rest, ok := strings.CutPrefix(path, "/run/"); ok {
			candidates.system = append(candidates.system, "/var/run/"+rest)
		}
	}
	for _, root := range []string{"/run/user/*", "/var/run/user/*"} {
		for _, rel := range containerRuntimeUserRunPaths {
			candidates.perUser = append(candidates.perUser, perUserRuntimePath{root: root, rel: rel})
		}
	}
	homes := []string{"/root", "/home/*"}
	if account, err := user.Current(); err == nil && filepath.IsAbs(account.HomeDir) {
		homes = append(homes, globEscape(account.HomeDir))
	}
	for _, home := range homes {
		for _, rel := range containerRuntimeHomePaths {
			candidates.perUser = append(candidates.perUser, perUserRuntimePath{root: home, rel: rel})
		}
	}
	return candidates
}

func globEscape(path string) string {
	var escaped strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`*?[\`, r) {
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(r)
	}
	return escaped.String()
}

type runtimePath struct {
	path string
	dir  bool
	// userRoot is set for a path below a per-user directory: that directory,
	// symlink-resolved. The path is reopened from it, without following any
	// symlink, when it is covered.
	userRoot string
}

// hideContainerRuntime moves the calling OS thread into a private mount
// namespace and covers every existing host container-runtime path. The caller
// MUST hold the goroutine on its OS thread until execve: the namespace belongs
// to this thread only, and execve hands it to the runtime.
//
// When no runtime path exists there is nothing to hide, and the seat starts in
// the host mount namespace. Otherwise it fails closed: when the namespace or
// any cover cannot be set up (no CAP_SYS_ADMIN, since a user namespace is
// deliberately not used, or mounts denied by an enclosing Landlock domain), the
// seat must not start. A per-user path the daemon cannot inspect is reported
// through logf and left uncovered instead; see discoverContainerRuntime.
func hideContainerRuntime(candidates runtimeCandidates, logf func(format string, args ...any)) error {
	paths, err := discoverContainerRuntime(candidates, logf)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		names := make([]string, 0, len(paths))
		for _, p := range paths {
			names = append(names, p.path)
		}
		return fmt.Errorf("create a private mount namespace (requires CAP_SYS_ADMIN) to cover %s: %w",
			strings.Join(names, ", "), err)
	}
	// Without this, the mounts below would propagate back into a shared host
	// mount tree and hide the runtime from the whole host.
	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the seat mount tree private: %w", err)
	}
	for _, p := range paths {
		if p.userRoot != "" {
			if err := coverUserRuntimePath(p, logf); err != nil {
				return err
			}
			continue
		}
		if err := coverRuntimePath(p.path, p.path, p.dir); err != nil {
			return err
		}
	}
	return nil
}

// coverRuntimePath mounts the cover for name at target: an empty read-only
// tmpfs on a directory, /dev/null on anything else.
func coverRuntimePath(target, name string, dir bool) error {
	if dir {
		flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
		if err := syscall.Mount("tmpfs", target, "tmpfs", flags, "mode=0"); err != nil {
			return fmt.Errorf("cover runtime directory %s: %w", name, err)
		}
		return nil
	}
	if err := syscall.Mount("/dev/null", target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("cover runtime socket %s: %w", name, err)
	}
	return nil
}

// coverUserRuntimePath reopens a per-user path inside the seat's namespace (a
// descriptor opened before unshare names the host's mounts, which cannot be
// mounted over from here) and mounts the cover on that descriptor through
// /proc/self/fd. mount(2) follows a symlink in the path it is given; the
// descriptor pins the very inode inspected, so a link swapped in after
// inspection cannot redirect the cover outside the user's directory.
func coverUserRuntimePath(p runtimePath, logf func(format string, args ...any)) error {
	rel, err := filepath.Rel(p.userRoot, p.path)
	if err != nil {
		return fmt.Errorf("cover runtime path %s: %w", p.path, err)
	}
	fd, path, dir, ok, err := openUserRuntimePath(p.userRoot, rel)
	if err != nil {
		logf("not covering container runtime path %s: %v", p.path, err)
		return nil
	}
	if !ok {
		return nil
	}
	defer unix.Close(fd)
	return coverRuntimePath("/proc/self/fd/"+strconv.Itoa(fd), path, dir)
}

// errUserRuntimeViaSymlink marks a per-user runtime path that runs through a
// symlink partway along, such as ~/.config/colima under a dotfiles-managed
// ~/.config.
var errUserRuntimeViaSymlink = errors.New("runs through a symlink its owner made")

// openUserRuntimePath opens root/rel as an O_PATH descriptor without following
// any symlink below root, one component at a time. When the final component is
// a symlink it returns that link: the cover then lands on the link itself, so
// the runtime path is unreachable, and never on a target its owner chose. A
// symlink partway along is an errUserRuntimeViaSymlink error, never a cover:
// that link is an ordinary directory of the user's (~/.config, ~/.docker) and
// covering it would hide everything else in it. ok is false when the path does
// not exist. The caller closes fd.
func openUserRuntimePath(root, rel string) (fd int, path string, dir, ok bool, err error) {
	fd, err = unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return -1, "", false, false, nil
		}
		return -1, "", false, false, fmt.Errorf("open %s: %w", root, err)
	}
	path = root
	parts := strings.Split(rel, "/")
	for i, name := range parts {
		next, err := unix.Openat(fd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		path = filepath.Join(path, name)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return -1, "", false, false, nil
		}
		if err != nil {
			return -1, "", false, false, fmt.Errorf("open %s: %w", path, err)
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return -1, "", false, false, fmt.Errorf("inspect %s: %w", path, err)
		}
		last := i == len(parts)-1
		switch stat.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			if last {
				return fd, path, false, true, nil
			}
			unix.Close(fd)
			return -1, "", false, false, fmt.Errorf("%s: %w", path, errUserRuntimeViaSymlink)
		case unix.S_IFDIR:
			if last {
				return fd, path, true, true, nil
			}
		default:
			if last {
				return fd, path, false, true, nil
			}
			// A file in the middle of the path: the path cannot exist.
			unix.Close(fd)
			return -1, "", false, false, nil
		}
	}
	unix.Close(fd)
	return -1, "", false, false, fmt.Errorf("empty runtime path below %s", root)
}

// discoverContainerRuntime resolves the candidates against the host. Paths are
// deduplicated, so the /var/run -> /run alias is covered once. Directories come
// first, and a path inside a covered directory is dropped because that
// directory's mount already hides it.
//
// System paths are symlink-resolved, and any error other than not-found
// refuses: only root controls them. A per-user path is never resolved through a
// symlink its owner made, so no user can point a cover at a directory of their
// choosing (/usr, the toolchain, a checkout). When the runtime path itself is
// a symlink, the link is covered where it is. When a symlink sits partway
// along (a dotfiles-managed ~/.config or ~/.docker), nothing is covered: that
// link is an ordinary directory of the user's, and the path is reported
// through logf if something exists behind it. Following the link instead would
// need proof that its target stays inside the same user's home, for a path a
// user can retarget at will; the skip is the simpler sound choice, since a
// runtime reached that way is outside the fixed list like any other custom
// endpoint. An error inspecting a per-user path (a permission denied to the
// daemon, which the seat as the same user meets too) is reported through logf
// and the path left uncovered; it never refuses, so no user can stop every
// read-only seat on the host from starting.
func discoverContainerRuntime(candidates runtimeCandidates, logf func(format string, args ...any)) ([]runtimePath, error) {
	var found []runtimePath
	seen := make(map[string]bool)
	for _, pattern := range candidates.system {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("container runtime path pattern %q: %w", pattern, err)
		}
		for _, match := range matches {
			resolved, err := filepath.EvalSymlinks(match)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("resolve container runtime path %s: %w", match, err)
			}
			if seen[resolved] {
				continue
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return nil, fmt.Errorf("inspect container runtime path %s: %w", resolved, err)
			}
			seen[resolved] = true
			found = append(found, runtimePath{path: resolved, dir: info.IsDir()})
		}
	}
	reported := make(map[string]bool)
	for _, candidate := range candidates.perUser {
		roots, err := filepath.Glob(candidate.root)
		if err != nil {
			return nil, fmt.Errorf("container runtime path pattern %q: %w", candidate.root, err)
		}
		for _, root := range roots {
			// The per-user directory itself, like /home/<user>, sits in a
			// root-owned parent, so resolving it only follows root's links.
			resolved, err := filepath.EvalSymlinks(root)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				if !reported[root] {
					reported[root] = true
					logf("not covering container runtime paths under %s: %v", root, err)
				}
				continue
			}
			fd, path, dir, ok, err := openUserRuntimePath(resolved, candidate.rel)
			if errors.Is(err, errUserRuntimeViaSymlink) {
				// Report it only when a runtime path might exist behind the
				// link. The lookup follows the link but only reads metadata;
				// nothing is covered on its result.
				full := filepath.Join(resolved, candidate.rel)
				if _, statErr := os.Lstat(full); errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
					continue
				}
				logf("not covering container runtime path %s: %v", full, err)
				continue
			}
			if err != nil {
				// One line per directory: a daemon that may not enter another
				// user's home would otherwise report every path below it.
				if !reported[resolved] {
					reported[resolved] = true
					logf("not covering container runtime path %s: %v", filepath.Join(resolved, candidate.rel), err)
				}
				continue
			}
			if !ok {
				continue
			}
			unix.Close(fd)
			if seen[path] {
				continue
			}
			seen[path] = true
			found = append(found, runtimePath{path: path, dir: dir, userRoot: resolved})
		}
	}
	slices.SortStableFunc(found, func(a, b runtimePath) int {
		switch {
		case a.dir == b.dir:
			return 0
		case a.dir:
			return -1
		default:
			return 1
		}
	})
	paths := found[:0]
	for _, candidate := range found {
		covered := slices.ContainsFunc(paths, func(p runtimePath) bool {
			return p.dir && strings.HasPrefix(candidate.path, p.path+string(filepath.Separator))
		})
		if !covered {
			paths = append(paths, candidate)
		}
	}
	return paths, nil
}
