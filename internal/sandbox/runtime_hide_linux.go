//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// containerRuntimePaths are host container-runtime sockets and the directories
// that hold per-daemon, per-plugin, and per-container runtime sockets. Landlock's
// filesystem rules do not govern connect(2) on a Unix socket, so a read-only
// seat could otherwise drive the runtime that owns them.
//
// An existing socket (or other file) is covered with /dev/null, where a connect
// reaches a character device and is refused. An existing directory is covered
// with an empty read-only tmpfs: containerd's per-container shim sockets under
// /run/containerd/s cannot be named in advance, and dockerd keeps its embedded
// containerd, metrics, libnetwork, and plugin sockets under /run/docker.
var containerRuntimePaths = []string{
	"/run/docker.sock",
	"/var/run/docker.sock",
	"/run/docker",
	"/var/run/docker",
	"/run/containerd",
	"/var/run/containerd",
	"/run/podman/podman.sock",
	"/var/run/podman/podman.sock",
	"/run/buildkit/buildkitd.sock",
	"/run/crio/crio.sock",
	"/run/k3s/containerd",
	"/run/user/*/docker.sock",
	"/run/user/*/podman/podman.sock",
	"/run/user/*/dockerd-rootless",
	"/run/user/*/containerd-rootless",
}

type runtimePath struct {
	path string
	dir  bool
}

// hideContainerRuntime moves the calling OS thread into a private mount
// namespace and covers every existing host container-runtime path. The caller
// MUST hold the goroutine on its OS thread until execve: the namespace belongs
// to this thread only, and execve hands it to the runtime.
//
// It fails closed. When a runtime path exists and the namespace cannot be
// created (no CAP_SYS_ADMIN; a user namespace is deliberately not used), the
// seat must not start. When no runtime path exists there is nothing to hide.
func hideContainerRuntime() error {
	paths, err := discoverContainerRuntime(containerRuntimePaths)
	if err != nil {
		return err
	}
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		if len(paths) == 0 && errors.Is(err, syscall.EPERM) {
			return nil
		}
		names := make([]string, 0, len(paths))
		for _, p := range paths {
			names = append(names, p.path)
		}
		return fmt.Errorf("cannot create a private mount namespace (requires CAP_SYS_ADMIN) to hide the host container runtime at %s: %w",
			strings.Join(names, ", "), err)
	}
	// Without this, the mounts below would propagate back into a shared host
	// mount tree and hide the runtime from the whole host.
	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the seat mount tree private: %w", err)
	}
	for _, p := range paths {
		if p.dir {
			flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
			if err := syscall.Mount("tmpfs", p.path, "tmpfs", flags, "mode=0"); err != nil {
				return fmt.Errorf("hide container runtime directory %s: %w", p.path, err)
			}
			continue
		}
		if err := syscall.Mount("/dev/null", p.path, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("hide container runtime socket %s: %w", p.path, err)
		}
	}
	return nil
}

// discoverContainerRuntime resolves the runtime path patterns against the host.
// Paths are symlink-resolved and deduplicated, so the /var/run -> /run alias is
// covered once. Directories come first, and a path inside a covered directory
// is dropped because that directory's mount already hides it.
func discoverContainerRuntime(patterns []string) ([]runtimePath, error) {
	var found []runtimePath
	seen := make(map[string]bool)
	for _, pattern := range patterns {
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
