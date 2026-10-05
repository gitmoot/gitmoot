//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	runtimeProbeEnv        = "GITMOOT_TEST_RUNTIME_PROBE_PATHS"
	runtimeProbeUnmountEnv = "GITMOOT_TEST_RUNTIME_PROBE_UNMOUNT"
)

// TestContainerRuntimeProbeHelper is not a test. The E2E tests below run this
// test binary INSIDE a sandbox-exec child. It first tries to unmount each path
// in runtimeProbeUnmountEnv, then connects to each Unix socket path in
// runtimeProbeEnv, and reports every outcome on stdout. It only connects and
// closes: it never sends a byte, so a real runtime socket sees no API request.
func TestContainerRuntimeProbeHelper(t *testing.T) {
	paths := os.Getenv(runtimeProbeEnv)
	if paths == "" {
		return
	}
	if unmounts := os.Getenv(runtimeProbeUnmountEnv); unmounts != "" {
		for _, path := range strings.Split(unmounts, "\n") {
			fmt.Printf("probe unmount %s: %v\n", path, syscall.Unmount(path, syscall.MNT_DETACH))
		}
	}
	for _, path := range strings.Split(paths, "\n") {
		conn, err := net.DialTimeout("unix", path, 5*time.Second)
		if err != nil {
			fmt.Printf("probe connect %s: refused: %v\n", path, err)
			continue
		}
		conn.Close()
		fmt.Printf("probe connect %s: connected\n", path)
	}
}

// requirePrivateMountNamespace skips unless this process can create a mount
// namespace for a child and mount inside it, which the test-owned runtime
// fixture needs. Both fail without CAP_SYS_ADMIN, and the mount also fails
// inside an enclosing Landlock domain.
func requirePrivateMountNamespace(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skipf("mount(8) unavailable: %v", err)
	}
	command := exec.Command("mount", "--make-rprivate", "/")
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if output, err := command.CombinedOutput(); err != nil {
		t.Logf("cannot mount in a private mount namespace: %v: %s", err, strings.TrimSpace(string(output)))
		t.Skip("test-owned container runtime fixture needs CAP_SYS_ADMIN outside any Landlock domain; run as root")
	}
}

func listenTestRuntimeSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
}

func shellQuote(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func probeTestBinary(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func probeConnected(output, path string) bool {
	return strings.Contains(output, "probe connect "+path+": connected")
}

func probeRefused(output, path string) bool {
	return strings.Contains(output, "probe connect "+path+": refused")
}

// TestSandboxExecReadOnlySeatHidesContainerRuntimeE2E proves the hiding
// mechanism against TEST-OWNED sockets, never the host runtime. The fixture runs
// in its own private mount namespace with empty tmpfs mounts over /run (and
// over /var/snap and /home when the test does not need them), and bind-mounts
// one test-owned listening socket at runtime endpoints a read-only seat must
// hide: dockerd's API socket and embedded containerd, containerd's API and
// shim sockets, cri-dockerd, k0s, rootless podman, microk8s, and Colima in a
// user's home. Both seats then run inside that namespace, so the writable seat
// afterwards also proves the read-only seat's mounts stayed in the seat's own
// namespace.
func TestSandboxExecReadOnlySeatHidesContainerRuntimeE2E(t *testing.T) {
	requireLandlockABI(t)
	requirePrivateMountNamespace(t)
	gitmoot := buildGitmootBinary(t)
	probe := probeTestBinary(t)

	base := t.TempDir()
	workdir := filepath.Join(base, "checkout")
	cacheDir := filepath.Join(base, "cache")
	for _, dir := range []string{workdir, cacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(workdir, "source.txt")
	if err := os.WriteFile(source, []byte("checkout-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(base, "runtime.sock")
	listenTestRuntimeSocket(t, socket)

	readArgs := []string{"--read", workdir}
	needed := []string{gitmoot, probe, base}
	if resolved, err := exec.LookPath("go"); err == nil {
		if root, ok := toolchainRootForTest(resolved); ok {
			readArgs = append(readArgs, "--read", root)
			needed = append(needed, root)
		}
	}

	// Each fixture endpoint: the socket path the seat probes, and the cover the
	// read-only seat must lay over it (/dev/null on a socket file, an empty
	// tmpfs on a runtime directory).
	type endpoint struct{ target, cover string }
	endpoints := []endpoint{
		{"/run/docker.sock", "/run/docker.sock"},
		{"/run/containerd/containerd.sock", "/run/containerd"},
		{"/run/containerd/s/0123456789abcdef", "/run/containerd"},
		{"/run/docker/containerd/containerd.sock", "/run/docker"},
		{"/run/cri-dockerd.sock", "/run/cri-dockerd.sock"},
		{"/run/k0s/containerd.sock", "/run/k0s"},
		{"/run/user/4242/podman/podman.sock", "/run/user/4242/podman"},
	}
	roots := []string{"/run"}
	// /var/snap and /home are replaced only when nothing the seats run lives
	// there, so the fixture never hides the toolchain or the test binaries.
	for _, extra := range []struct {
		root string
		endpoint
	}{
		{"/var/snap", endpoint{"/var/snap/microk8s/common/run/containerd.sock", "/var/snap/microk8s/common/run"}},
		{"/home", endpoint{"/home/gitmoot-fixture/.colima/default/docker.sock", "/home/gitmoot-fixture/.colima"}},
	} {
		if info, err := os.Stat(extra.root); err != nil || !info.IsDir() {
			t.Logf("fixture skips %s: %s is not a directory here", extra.target, extra.root)
			continue
		}
		if slices.ContainsFunc(needed, func(path string) bool { return strings.HasPrefix(path, extra.root+"/") }) {
			t.Logf("fixture skips %s: the test needs files under %s", extra.target, extra.root)
			continue
		}
		roots = append(roots, extra.root)
		endpoints = append(endpoints, extra.endpoint)
	}
	if resolved, err := filepath.EvalSymlinks("/var/run"); err == nil && resolved == "/run" {
		endpoints = append(endpoints, endpoint{"/var/run/docker.sock", "/run/docker.sock"})
	}
	var targets, covers []string
	setup := []string{"set -eu", "mount --make-rprivate /"}
	for _, root := range roots {
		setup = append(setup, "mount -t tmpfs -o mode=0755 gitmoot-test "+shellQuote(root))
	}
	for _, e := range endpoints {
		targets = append(targets, e.target)
		if !slices.Contains(covers, e.cover) {
			covers = append(covers, e.cover)
		}
		if strings.HasPrefix(e.target, "/var/run/") {
			continue
		}
		setup = append(setup,
			"mkdir -p "+shellQuote(filepath.Dir(e.target)),
			": > "+shellQuote(e.target),
			`mount --bind "$0" `+shellQuote(e.target))
	}

	t.Logf("fixture endpoints: %s", strings.Join(targets, ", "))
	seat := func(readOnly bool) string {
		args := []string{gitmoot, "sandbox-exec"}
		if readOnly {
			args = append(args, "--read-only-workdir")
		}
		args = append(args, readArgs...)
		args = append(args, "--read-file", probe, "--write", cacheDir, "--", "/bin/sh", "-c",
			`cat "$0" && echo && go version && exec "$1" -test.run '^TestContainerRuntimeProbeHelper$'`, source, probe)
		return shellQuote(args...)
	}
	script := strings.Join(setup, "\n") + `
echo '== read-only'
` + seat(true) + ` 2>&1 || echo "read-only seat exit $?"
echo '== writable'
GITMOOT_TEST_RUNTIME_PROBE_UNMOUNT= ` + seat(false) + ` 2>&1 || echo "writable seat exit $?"
`
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", script, socket)
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	command.Dir = workdir
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + cacheDir,
		"GOTOOLCHAIN=local",
		"GOCACHE=" + filepath.Join(cacheDir, "go-build"),
		"TMPDIR=" + cacheDir,
		runtimeProbeEnv + "=" + strings.Join(targets, "\n"),
		runtimeProbeUnmountEnv + "=" + strings.Join(covers, "\n"),
	}
	combined, err := command.CombinedOutput()
	output := string(combined)
	if err != nil {
		t.Fatalf("runtime fixture namespace failed: %v\n%s", err, output)
	}
	readOnly, writable, ok := strings.Cut(output, "== writable")
	if !ok {
		t.Fatalf("fixture output lacks the writable seat section:\n%s", output)
	}

	if strings.Contains(readOnly, "read-only seat exit") {
		t.Fatalf("read-only seat failed to run:\n%s", readOnly)
	}
	if !strings.Contains(readOnly, "checkout-readable") {
		t.Fatalf("read-only seat could not read its checkout:\n%s", readOnly)
	}
	if !strings.Contains(readOnly, "go version go") {
		t.Fatalf("read-only seat could not run go:\n%s", readOnly)
	}
	for _, target := range targets {
		if probeConnected(readOnly, target) || !probeRefused(readOnly, target) {
			t.Errorf("read-only seat reached the container runtime socket %s, or never probed it:\n%s", target, readOnly)
		}
	}
	// The seat must not be able to remove a cover from inside: Landlock denies
	// mount topology changes to a sandboxed process.
	for _, cover := range covers {
		if !strings.Contains(readOnly, "probe unmount "+cover+": operation not permitted") {
			t.Errorf("read-only seat unmount of %s was not denied:\n%s", cover, readOnly)
		}
	}

	if strings.Contains(writable, "writable seat exit") {
		t.Fatalf("writable seat failed to run:\n%s", writable)
	}
	for _, target := range targets {
		if !probeConnected(writable, target) {
			t.Errorf("writable seat could not connect to %s; seats that write code must keep runtime access, and the read-only seat's mounts must not leak:\n%s", target, writable)
		}
	}
}

// hostContainerRuntimeSockets lists the host's existing runtime sockets for
// the read-only check below, independently of the production list.
func hostContainerRuntimeSockets(t *testing.T) []string {
	t.Helper()
	patterns := []string{
		"/run/docker.sock",
		"/var/run/docker.sock",
		"/run/containerd/containerd.sock",
		"/run/containerd/containerd.sock.ttrpc",
		"/run/containerd/s/*",
		"/run/docker/containerd/containerd.sock",
		"/run/docker/containerd/containerd.sock.ttrpc",
		"/run/docker/metrics.sock",
		"/run/podman/podman.sock",
		"/run/crio/crio.sock",
		"/run/buildkit/buildkitd.sock",
		"/run/cri-dockerd.sock",
		"/run/k0s/containerd.sock",
		"/run/k3s/containerd/containerd.sock",
		"/var/snap/microk8s/common/run/containerd.sock",
		"/run/user/*/docker.sock",
		"/run/user/*/podman/podman.sock",
		"/root/.docker/run/docker.sock",
		"/home/*/.docker/run/docker.sock",
		"/home/*/.colima/*/docker.sock",
		"/home/*/.rd/docker.sock",
	}
	var sockets []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Type() == fs.ModeSocket {
				sockets = append(sockets, match)
			}
		}
	}
	return sockets
}

// TestSandboxExecReadOnlySeatRefusesHostContainerRuntimeE2E checks the real
// host runtime from a real read-only seat. It is read-only by construction: the
// probe connects and closes without sending anything, and `docker version`
// only reads. Both must fail inside the seat.
func TestSandboxExecReadOnlySeatRefusesHostContainerRuntimeE2E(t *testing.T) {
	requireLandlockABI(t)
	sockets := hostContainerRuntimeSockets(t)
	if len(sockets) == 0 {
		t.Skip("no host container runtime socket exists")
	}
	gitmoot := buildGitmootBinary(t)
	requireReadOnlySeatLaunchable(t, gitmoot)
	probe := probeTestBinary(t)
	workdir := t.TempDir()
	home := t.TempDir()

	probes := append([]string{}, sockets...)
	// The host mount namespace stays reachable only through another process's
	// /proc/<pid>/root, which Landlock's ptrace scoping denies.
	probes = append(probes, filepath.Join("/proc/1/root", sockets[0]))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, gitmoot, "sandbox-exec", "--read-only-workdir",
		"--read", workdir, "--read-file", probe, "--write", home, "--",
		probe, "-test.run", "^TestContainerRuntimeProbeHelper$")
	command.Dir = workdir
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, runtimeProbeEnv + "=" + strings.Join(probes, "\n")}
	combined, err := command.CombinedOutput()
	output := string(combined)
	if err != nil {
		t.Fatalf("read-only seat probe failed to run: %v\n%s", err, output)
	}
	for _, path := range probes {
		if probeConnected(output, path) || !probeRefused(output, path) {
			t.Errorf("read-only seat reached the host container runtime at %s, or never probed it:\n%s", path, output)
		}
	}

	docker, err := exec.LookPath("docker")
	if err != nil {
		return
	}
	// `docker version` reads only. It must reach the CLI and fail on the
	// connection, not on a sandbox launch or file-grant error.
	dockerCommand := exec.CommandContext(ctx, gitmoot, "sandbox-exec", "--read-only-workdir",
		"--read", workdir, "--write", home, "--", docker, "version", "--format", "{{.Server.Version}}")
	dockerCommand.Dir = workdir
	dockerCommand.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	dockerOutput, err := dockerCommand.CombinedOutput()
	if err == nil {
		t.Fatalf("docker version reached the daemon inside a read-only seat:\n%s", dockerOutput)
	}
	if !strings.Contains(string(dockerOutput), "Cannot connect to the Docker daemon") {
		t.Fatalf("docker version failed inside a read-only seat, but not on the daemon connection: %v\n%s", err, dockerOutput)
	}
}

const runtimeHideHomesEnv = "GITMOOT_TEST_RUNTIME_HIDE_HOMES"

// userRuntimeFixture is a fake /home holding one user, mallory, whose home has
// every shape a runtime path can take. Final-component symlinks, each the
// runtime path itself: a loop (.lima) and a link to /etc (.colima). Symlinks
// partway along, to directories outside the home: a dotfiles-managed .config
// with gh auth and no colima in it, and .docker, whose target holds a "run"
// entry (so .docker/run exists behind the link) and no "desktop". And an
// ordinary runtime directory (.rd). Paths are symlink-resolved.
type userRuntimeFixture struct {
	homes, home, outside string
}

func newUserRuntimeFixture(t *testing.T) userRuntimeFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := userRuntimeFixture{
		homes:   filepath.Join(base, "home"),
		home:    filepath.Join(base, "home", "mallory"),
		outside: filepath.Join(base, "outside"),
	}
	for _, dir := range []string{filepath.Join(f.home, ".rd"), filepath.Join(f.outside, "dotfiles", "config", "gh")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		filepath.Join(f.home, ".rd", "docker.sock"),
		filepath.Join(f.outside, "run"),
		filepath.Join(f.outside, "dotfiles", "config", "gh", "hosts.yml"),
	} {
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		".lima":   filepath.Join(f.home, ".lima"),
		".colima": "/etc",
		".config": filepath.Join(f.outside, "dotfiles", "config"),
		".docker": f.outside,
	} {
		if err := os.Symlink(target, filepath.Join(f.home, link)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f userRuntimeFixture) candidates() runtimeCandidates {
	var candidates runtimeCandidates
	for _, rel := range containerRuntimeHomePaths {
		candidates.perUser = append(candidates.perUser, perUserRuntimePath{root: filepath.Join(globEscape(f.homes), "*"), rel: rel})
	}
	return candidates
}

// TestDiscoverContainerRuntimeNeverFollowsUserSymlinks: a user's symlinks must
// neither stop discovery (a loop used to refuse every read-only seat on the
// host) nor send a cover to their target (a link to /etc used to cover /etc).
// A link that is the runtime path itself is covered where it is. A link partway
// along (.config, .docker) is never covered, since that would hide the user's
// whole directory: the path is skipped, and reported only when something
// exists behind it (.docker/run). An ordinary runtime directory is still found.
func TestDiscoverContainerRuntimeNeverFollowsUserSymlinks(t *testing.T) {
	f := newUserRuntimeFixture(t)
	var logs []string
	paths, err := discoverContainerRuntime(f.candidates(), func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("user symlinks refused discovery: %v", err)
	}
	want := []runtimePath{
		{path: filepath.Join(f.home, ".rd"), dir: true, userRoot: f.home},
		{path: filepath.Join(f.home, ".colima"), userRoot: f.home},
		{path: filepath.Join(f.home, ".lima"), userRoot: f.home},
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("discovered %+v, want %+v", paths, want)
	}
	wantLogs := []string{fmt.Sprintf("not covering container runtime path %s: %s: %v",
		filepath.Join(f.home, ".docker/run"), filepath.Join(f.home, ".docker"), errUserRuntimeViaSymlink)}
	if !slices.Equal(logs, wantLogs) {
		t.Fatalf("discovery logged %q, want %q", logs, wantLogs)
	}
}

// TestDiscoverContainerRuntimeSkipsUserPathsItCannotInspect: a home the daemon
// may not enter is reported once and left uncovered, never a refusal. The seat
// runs as the daemon's user, so it cannot enter that home either.
func TestDiscoverContainerRuntimeSkipsUserPathsItCannotInspect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is never denied a directory, so there is nothing to skip")
	}
	f := newUserRuntimeFixture(t)
	if err := os.Chmod(f.home, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.home, 0o755) })
	var logs []string
	paths, err := discoverContainerRuntime(f.candidates(), func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("an unreadable home refused discovery: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("discovered %+v inside a home the daemon cannot enter", paths)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], f.home) || !strings.Contains(logs[0], "permission denied") {
		t.Fatalf("logs = %q, want one permission-denied line naming %s", logs, f.home)
	}
}

// TestDiscoverContainerRuntimeRefusesUnresolvableSystemPath: a root-owned
// system path that cannot be resolved still fails closed. Only root can make
// one, so it is not a lever for other users.
func TestDiscoverContainerRuntimeRefusesUnresolvableSystemPath(t *testing.T) {
	run := t.TempDir()
	loop := filepath.Join(run, "docker.sock")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	candidates := runtimeCandidates{system: []string{loop}}
	paths, err := discoverContainerRuntime(candidates, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "resolve container runtime path "+loop) {
		t.Fatalf("discoverContainerRuntime = %+v, %v; want a resolve error for %s", paths, err, loop)
	}
	if err := hideContainerRuntime(candidates, t.Logf); err == nil {
		t.Fatal("hideContainerRuntime accepted an unresolvable system runtime path")
	}
}

// TestContainerRuntimeHideHelper is not a test.
// TestHideContainerRuntimeCoversUserPathsInPlaceE2E runs this test binary as a
// child that hides the fixture's runtime paths on its own OS thread and then,
// from inside that mount namespace, reports what each path looks like.
func TestContainerRuntimeHideHelper(t *testing.T) {
	homes := os.Getenv(runtimeHideHomesEnv)
	if homes == "" {
		return
	}
	f := userRuntimeFixture{homes: homes, home: filepath.Join(homes, "mallory"), outside: filepath.Join(filepath.Dir(homes), "outside")}
	runtime.LockOSThread()
	err := hideContainerRuntime(f.candidates(), func(format string, args ...any) {
		fmt.Printf("hide log: "+format+"\n", args...)
	})
	fmt.Printf("hide: %v\n", err)
	report := func(name string, err error) {
		fmt.Printf("%s: %v\n", name, err)
	}
	_, err = os.Stat("/etc/passwd")
	report("stat /etc/passwd", err)
	_, err = os.Stat(filepath.Join(f.outside, "run"))
	report("stat outside/run", err)
	for _, rel := range []string{".colima/passwd", ".lima/default", ".config/gh/hosts.yml", ".docker/run", ".rd/docker.sock"} {
		_, err = os.Stat(filepath.Join(f.home, rel))
		report("stat home/"+rel, err)
	}
	entries, err := os.ReadDir(filepath.Join(f.home, ".rd"))
	fmt.Printf("readdir home/.rd: %d %v\n", len(entries), err)
}

// TestHideContainerRuntimeCoversUserPathsInPlaceE2E proves the covers land on
// the user's own paths, in a child's private mount namespace: a link loop no
// longer refuses, a final-component link to /etc is covered where it is while
// /etc stays intact, an ordinary runtime directory is covered with an empty
// tmpfs, and a link partway along a runtime path (a dotfiles-managed .config,
// .docker) is not covered at all: gh auth under .config stays visible.
func TestHideContainerRuntimeCoversUserPathsInPlaceE2E(t *testing.T) {
	requirePrivateMountNamespace(t)
	f := newUserRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, probeTestBinary(t), "-test.run", "^TestContainerRuntimeHideHelper$")
	command.Env = append(os.Environ(), runtimeHideHomesEnv+"="+f.homes)
	combined, err := command.CombinedOutput()
	output := string(combined)
	if err != nil {
		t.Fatalf("hide helper failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"hide: <nil>",
		"stat /etc/passwd: <nil>",
		"stat outside/run: <nil>",
		"stat home/.colima/passwd: stat " + filepath.Join(f.home, ".colima/passwd") + ": not a directory",
		"stat home/.lima/default: stat " + filepath.Join(f.home, ".lima/default") + ": not a directory",
		"stat home/.config/gh/hosts.yml: <nil>",
		"stat home/.docker/run: <nil>",
		"stat home/.rd/docker.sock: stat " + filepath.Join(f.home, ".rd/docker.sock") + ": no such file or directory",
		"readdir home/.rd: 0 <nil>",
		"hide log: not covering container runtime path " + filepath.Join(f.home, ".docker/run") + ": " +
			filepath.Join(f.home, ".docker") + ": " + errUserRuntimeViaSymlink.Error(),
	} {
		if !strings.Contains(output, want+"\n") {
			t.Errorf("hide helper output lacks %q:\n%s", want, output)
		}
	}
	if count := strings.Count(output, "hide log:"); count != 1 {
		t.Errorf("hide helper logged %d skipped paths, want only .docker/run:\n%s", count, output)
	}
}
