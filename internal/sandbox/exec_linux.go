//go:build linux

package sandbox

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// MinimumABI is the oldest Landlock ABI that confines file truncation as well
// as the basic filesystem writes covered by ABI v1. Produce stages routinely
// replace existing output files, so accepting an older ABI would silently leave
// an important write operation outside the policy.
const MinimumABI = 3

// ReadOnlyWorkdirSupported reports whether this build can enforce the review
// seat's hard read-only checkout boundary.
func ReadOnlyWorkdirSupported() bool {
	return true
}

// Runtime bootstrap needs these host files, but not their credential-bearing
// parent directories. Missing platform-specific files are ignored below.
var runtimeHostReadFiles = []string{
	"/etc/ld.so.cache",
	"/etc/resolv.conf",
	"/etc/hosts",
	"/etc/nsswitch.conf",
	"/etc/passwd",
	"/etc/group",
	"/etc/localtime",
	"/etc/ssl/openssl.cnf",
}

// Exec applies Gitmoot's strict filesystem ruleset to the current process and
// replaces it with argv. Landlock restrictions survive execve, so the runtime
// and every descendant inherit the same filesystem confinement.
func Exec(readPaths, readFiles, writePaths []string, argv []string) error {
	return execSandbox(readPaths, readFiles, writePaths, argv, false, false)
}

// ExecReadOnlyWorkdir applies the same strict ruleset as Exec without granting
// the current working directory implicit write access. Before Landlock is
// applied it also hides the host container runtime in a private mount
// namespace (see hideContainerRuntime), and refuses to exec when it cannot.
func ExecReadOnlyWorkdir(readPaths, readFiles, writePaths []string, argv []string) error {
	return execSandbox(readPaths, readFiles, writePaths, argv, true, false)
}

// ExecNoExecutionChild is the confined half of a no-execution seat (#2364),
// run by SuperviseNoExecution as a child. Every read and write root keeps its
// read/write rights but loses LANDLOCK_ACCESS_FS_EXECUTE; Execute remains
// only on the runtime and its ELF loader, which the kernel needs to start a
// dynamic runtime. The child then installs the exec-notify seccomp filter,
// hands it to the supervisor, and execs the runtime. The supervisor allows
// that one exec and refuses every later one, so the loader grant is never a
// way to start another program.
func ExecNoExecutionChild(readPaths, readFiles, writePaths []string, argv []string) error {
	return execSandbox(readPaths, readFiles, writePaths, argv, true, true)
}

// noExecRead and noExecWrite are RODirs/RWDirs without the Execute right;
// noExecReadFile is the same for single-file rules, which may not carry
// directory rights.
var (
	noExecRead     = landlock.AccessFSSet(llsys.AccessFSReadFile | llsys.AccessFSReadDir)
	noExecReadFile = landlock.AccessFSSet(llsys.AccessFSReadFile)
	noExecWrite    = landlock.AccessFSSet(llsys.AccessFSReadFile | llsys.AccessFSReadDir |
		llsys.AccessFSWriteFile | llsys.AccessFSRemoveDir | llsys.AccessFSRemoveFile |
		llsys.AccessFSMakeChar | llsys.AccessFSMakeDir | llsys.AccessFSMakeReg |
		llsys.AccessFSMakeSock | llsys.AccessFSMakeFifo | llsys.AccessFSMakeBlock |
		llsys.AccessFSMakeSym | llsys.AccessFSTruncate)
	execOnly = landlock.AccessFSSet(llsys.AccessFSExecute | llsys.AccessFSReadFile)
)

func execSandbox(readPaths, readFiles, writePaths []string, argv []string, readOnlyWorkdir, noExecution bool) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("sandbox target command is required")
	}
	abi, err := ABI()
	if err != nil {
		return fmt.Errorf("query Landlock ABI: %w", err)
	}
	if abi < MinimumABI {
		return fmt.Errorf("Landlock ABI v%d is unavailable; v%d or newer is required", abi, MinimumABI)
	}
	executable, err := execLookPath(argv[0])
	if err != nil {
		return fmt.Errorf("resolve sandbox target %q: %w", argv[0], err)
	}

	workdir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve sandbox workdir: %w", err)
	}
	writable, err := writableRoots(writePaths, workdir, !readOnlyWorkdir)
	if err != nil {
		return err
	}

	var rules []landlock.Rule
	if noExecution && len(readPaths) == 0 && len(readFiles) == 0 {
		// A no-execution seat must name its reads; the historical RO "/" rule
		// would grant Execute on the whole filesystem.
		return errors.New("no-execution sandbox requires explicit read paths")
	}
	if len(readPaths) == 0 && len(readFiles) == 0 {
		// Preserve the original write-confinement contract for existing produce
		// stages: the filesystem is readable while writes remain allowlisted.
		rules = append(rules, landlock.RODirs("/"))
	} else {
		readable, err := readableRoots(readPaths)
		if err != nil {
			return err
		}
		// The executable itself is a FILE grant, never a recursive grant of
		// its host directory. Runtime package trees and Go roots arrive through
		// readPaths only after the daemon has staged them into an engine-owned
		// tree. This is ruling 122157's clean ownership boundary.
		executableFiles := append([]string{}, readFiles...)
		executableFiles = append(executableFiles, executable)
		if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil && resolved != executable {
			executableFiles = append(executableFiles, resolved)
		}
		files, err := readableFiles(executableFiles)
		if err != nil {
			return err
		}
		if noExecution {
			rules = append(rules, landlock.PathAccess(noExecRead, readable...))
			// Read-only files stay data. Execute is granted only to the runtime
			// binary and, for a dynamic runtime, its ELF loader: the kernel
			// requires Execute on PT_INTERP to start the runtime at all (measured:
			// without it the runtime exec fails EACCES). That grant alone would
			// be a bypass, since ld.so run directly maps and runs any readable
			// ELF. The supervisor closes it: after the one exec that starts the
			// runtime, every execve and execveat is refused, so the loader is
			// never started as a program.
			rules = append(rules, landlock.PathAccess(noExecReadFile, files...))
			runtimeExec := []string{executable}
			if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
				runtimeExec = append(runtimeExec, resolved)
			}
			interp, interpErr := elfInterpreter(runtimeExec[len(runtimeExec)-1])
			if interpErr != nil {
				return fmt.Errorf("no-execution sandbox runtime %q must be an ELF executable: %w", executable, interpErr)
			}
			if interp != "" {
				runtimeExec = append(runtimeExec, interp)
				if resolved, resolveErr := filepath.EvalSymlinks(interp); resolveErr == nil && resolved != interp {
					runtimeExec = append(runtimeExec, resolved)
				}
			}
			rules = append(rules, landlock.PathAccess(execOnly, runtimeExec...))
			rules = append(rules, landlock.PathAccess(noExecReadFile, runtimeHostReadFiles...).IgnoreIfMissing())
		} else {
			rules = append(rules, landlock.RODirs(readable...))
			rules = append(rules, landlock.ROFiles(files...))
			rules = append(rules, landlock.ROFiles(runtimeHostReadFiles...).IgnoreIfMissing())
		}
	}
	if len(writable) > 0 {
		// WithRefer permits rename/link operations only when both the source and
		// destination are covered by the writable rules. It does not widen the
		// allowed roots, and keeps atomic output replacement usable.
		if noExecution {
			rules = append(rules, landlock.PathAccess(noExecWrite, writable...).WithRefer())
		} else {
			rules = append(rules, landlock.RWDirs(writable...).WithRefer())
		}
	}
	rules = append(rules, landlock.RWFiles(
		"/dev/null",
		"/dev/zero",
		"/dev/urandom",
		"/dev/tty",
	).IgnoreIfMissing())

	if readOnlyWorkdir {
		// A mount namespace created by unshare(2) belongs to the calling OS
		// thread until execve. Never unlock: the runtime must be exec'd from the
		// thread that hides the host container runtime.
		runtime.LockOSThread()
		logf := func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "sandbox-exec: "+format+"\n", args...)
		}
		if err := hideContainerRuntime(containerRuntimeCandidates(), logf); err != nil {
			return fmt.Errorf("%s: %w", ContainerRuntimeHidingRefusal, err)
		}
	}

	// Deliberately strict: no BestEffort downgrade. If V3 or any requested rule
	// cannot be installed, the runtime must not start.
	if err := landlock.V3.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("apply strict Landlock ruleset: %w", err)
	}
	if noExecution {
		return execNoExecutionChild(executable, argv, noExecutionEnv(os.Environ()))
	}
	return syscall.Exec(executable, argv, os.Environ())
}

// noExecutionEnvBlocked are environment variables that make a runtime load
// or run code it was not built with: dynamic-loader injection, and Bun/Node
// switches that turn a compiled runtime back into a general interpreter or
// preload modules. They are dropped for a no-execution seat.
var noExecutionEnvBlocked = []string{"LD_", "BUN_BE_BUN", "BUN_OPTIONS", "BUN_INSPECT", "NODE_OPTIONS", "NODE_PATH", "BUN_CONFIG_"}

func noExecutionEnv(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		blocked := false
		for _, prefix := range noExecutionEnvBlocked {
			if name == prefix || (strings.HasSuffix(prefix, "_") && strings.HasPrefix(name, prefix)) {
				blocked = true
				break
			}
		}
		if !blocked {
			kept = append(kept, entry)
		}
	}
	return kept
}

// elfInterpreter returns the PT_INTERP path of an ELF executable, or "" for a
// static binary. The kernel opens and executes that loader on the process's
// behalf when it execs a dynamic binary, so a no-execution ruleset must grant
// it Execute or the runtime itself cannot start. A non-ELF target (a script)
// is refused: its interpreter would be a general-purpose program.
func elfInterpreter(path string) (string, error) {
	file, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(prog.Open())
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(data), "\x00"), nil
	}
	return "", nil
}

func readableFiles(paths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(paths))
	files := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("sandbox read file %q must be absolute", path)
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("sandbox read file %q: %w", path, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("sandbox read file %q is a directory", path)
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}
	return files, nil
}

// readableRoots returns explicit read-only inputs plus fixed system roots needed
// to execute a runtime. Writable roots are intentionally absent: their stronger
// RWDirs rules already include read rights. Existing stages with no reads
// declaration bypass this helper and retain the historical RO `/` rule.
//
// It deliberately knows NOTHING about the executable or Go installation.
// Runtime package trees and toolchains are copied into daemon-owned roots by the
// caller and arrive in paths. The executable itself is granted as a file by
// execSandbox. This removes all recursive host-root inference from the sandbox.
func readableRoots(paths []string) ([]string, error) {
	roots := make([]string, 0, len(paths)+12)
	seen := make(map[string]struct{}, len(paths)+12)
	add := func(candidate string, required bool) error {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return nil
		}
		if !filepath.IsAbs(candidate) {
			return fmt.Errorf("sandbox read path %q must be absolute", candidate)
		}
		candidate = filepath.Clean(candidate)
		info, err := os.Stat(candidate)
		if err != nil {
			if !required && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("sandbox read path %q: %w", candidate, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("sandbox read path %q is not a directory", candidate)
		}
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			roots = append(roots, candidate)
		}
		return nil
	}
	for _, candidate := range paths {
		if err := add(candidate, true); err != nil {
			return nil, err
		}
	}
	for _, candidate := range []string{
		"/bin", "/sbin", "/lib", "/lib64", "/dev",
		"/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/usr/libexec", "/usr/share",
		"/etc/ssl/certs", "/etc/pki",
		// procfs read is a runtime BOOTSTRAP requirement, not a convenience:
		// Bun-based Claude/Kimi binaries abort without it and codex's managed
		// bwrap reads /proc/sys/kernel/overflowuid.
		"/proc",
	} {
		if err := add(candidate, false); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

func writableRoots(paths []string, workdir string, includeImplicitRoots bool) ([]string, error) {
	candidates := append([]string{}, paths...)
	if includeImplicitRoots {
		candidates = append(candidates, workdir, os.TempDir(), "/tmp")
	}
	seen := make(map[string]struct{}, len(candidates))
	roots := make([]string, 0, len(candidates))
	for _, path := range candidates {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("sandbox write path %q must be absolute", path)
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("sandbox write path %q: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("sandbox write path %q is not a directory", path)
		}
		seen[path] = struct{}{}
		roots = append(roots, path)
	}
	return roots, nil
}
