//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
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
	return execSandbox(readPaths, readFiles, writePaths, argv, false)
}

// ExecReadOnlyWorkdir applies the same strict ruleset as Exec without granting
// the current working directory implicit write access.
func ExecReadOnlyWorkdir(readPaths, readFiles, writePaths []string, argv []string) error {
	return execSandbox(readPaths, readFiles, writePaths, argv, true)
}

func execSandbox(readPaths, readFiles, writePaths []string, argv []string, readOnlyWorkdir bool) error {
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
	if len(readPaths) == 0 && len(readFiles) == 0 {
		// Preserve the original write-confinement contract for existing produce
		// stages: the filesystem is readable while writes remain allowlisted.
		rules = append(rules, landlock.RODirs("/"))
	} else {
		readable, held, err := readableRoots(readPaths, executable)
		if err != nil {
			return err
		}
		// HELD UNTIL RestrictPaths RETURNS. The toolchain rule is added for a
		// /proc/self/fd path, so closing the descriptor before installation
		// would leave the rule pointing at nothing. Closed immediately after,
		// because the kernel keeps the rule against the inode once installed -
		// pinned by TestToolchainGrantSurvivesDescriptorCloseE2E.
		defer func() {
			for _, handle := range held {
				_ = handle.Close()
			}
		}()
		rules = append(rules, landlock.RODirs(readable...))
		files, err := readableFiles(readFiles)
		if err != nil {
			return err
		}
		if len(files) > 0 {
			rules = append(rules, landlock.ROFiles(files...))
		}
		rules = append(rules, landlock.ROFiles(runtimeHostReadFiles...).IgnoreIfMissing())
	}
	if len(writable) > 0 {
		// WithRefer permits rename/link operations only when both the source and
		// destination are covered by the writable rules. It does not widen the
		// allowed roots, and keeps atomic output replacement usable.
		rules = append(rules, landlock.RWDirs(writable...).WithRefer())
	}
	rules = append(rules, landlock.RWFiles(
		"/dev/null",
		"/dev/zero",
		"/dev/urandom",
		"/dev/tty",
	).IgnoreIfMissing())

	// Deliberately strict: no BestEffort downgrade. If V3 or any requested rule
	// cannot be installed, the runtime must not start.
	if err := landlock.V3.RestrictPaths(rules...); err != nil {
		return fmt.Errorf("apply strict Landlock ruleset: %w", err)
	}
	return syscall.Exec(executable, argv, os.Environ())
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

// readableRoots returns the explicit read-only inputs plus the fixed host roots
// needed to execute a runtime. Writable roots are intentionally absent: their
// stronger RWDirs rules already include read rights. Existing stages with no
// reads declaration bypass this helper and retain the historical RO `/` rule.
//
// The returned files are OPEN DESCRIPTORS the caller MUST hold until
// RestrictPaths has returned: the toolchain grant is installed through
// /proc/self/fd/<n> so the rule binds the inode that was validated rather than
// a name that can be repointed afterwards (#1878 round 2, P1-1). Closing them
// early would invalidate the path the rule is being added for.
func readableRoots(paths []string, executable string) ([]string, []*os.File, error) {
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
	var held []*os.File
	closeHeld := func() {
		for _, f := range held {
			_ = f.Close()
		}
	}
	for _, candidate := range paths {
		if err := add(candidate, true); err != nil {
			closeHeld()
			return nil, nil, err
		}
	}
	for _, candidate := range []string{
		"/bin", "/sbin", "/lib", "/lib64", "/dev",
		"/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/usr/libexec", "/usr/share",
		"/usr/local/bin", "/usr/local/sbin", "/usr/local/lib", "/usr/local/lib64", "/usr/local/share",
		"/etc/ssl/certs", "/etc/pki",
		// procfs read is a runtime BOOTSTRAP requirement, not a convenience: the
		// Bun-based Claude/Kimi binaries abort with an opaque crash without it,
		// and codex's managed bwrap fails reading /proc/sys/kernel/overflowuid.
		// The legacy no-reads mode always had it via RODirs("/"), so strict read
		// mode was the regression rather than this grant being a widening.
		//
		// It is now also load-bearing for the toolchain grant, which is installed
		// as /proc/self/fd/<n> so the rule binds a validated inode (#1878 P1-1).
		//
		// EXPOSURE, stated as narrowly as it was measured. One subcase is proven:
		// /proc/<other-pid>/environ stays denied to a sandboxed process because
		// Landlock's ptrace domain check gates it (measured — own environ
		// readable, the live daemon's denied, while an unsandboxed root read of
		// that same path succeeds). That is NOT a general claim: /proc/<pid>/cmdline,
		// /proc/net/* and /proc/sys/* are gated by ordinary DAC and hidepid, which
		// this rule neither tightens nor loosens. Narrowing the grant to /proc/self
		// plus specific files is a live follow-up, untested here because Landlock
		// resolves paths at rule-add time while nested runtimes fork new pids.
		"/proc",
	} {
		if err := add(candidate, false); err != nil {
			closeHeld()
			return nil, nil, err
		}
	}
	if err := addExecutableReadRoots(add, executable); err != nil {
		closeHeld()
		return nil, nil, err
	}
	// The toolchain grant is OPTIONAL and FAILS CLOSED. A host with no `go`, or
	// with one whose installation cannot be validated, simply gets no grant:
	// refusing the grant is the safe outcome, whereas returning an error here
	// would refuse to launch every sandbox on such a host. The visible
	// consequence of a refusal is the seat's own exit 126, which is exactly the
	// pre-#1878 behaviour rather than a new failure.
	if goExecutable, err := execLookPath("go"); err == nil {
		if handle := grantableToolchainRootHandle(goExecutable); handle != nil {
			held = append(held, handle)
			if err := add(procFdPath(handle), true); err != nil {
				closeHeld()
				return nil, nil, err
			}
		}
	}
	return roots, held, nil
}

// procFdPath names an open descriptor's magic-symlink path. A Landlock rule
// added for this path binds the INODE the descriptor holds, which is what makes
// the grant immune to a later rename or symlink swap of the original name.
//
// MEASURED, because the library offers no descriptor-based rule API - only
// path-keyed RODirs/ROFiles/RWDirs/RWFiles/PathAccess: in a child process a
// directory was opened, RODirs installed for its /proc/self/fd path, and the
// ORIGINAL NAME THEN RENAMED AWAY; the directory remained readable through the
// moved name. The rule follows the object, not the string.
func procFdPath(handle *os.File) string {
	return "/proc/self/fd/" + strconv.Itoa(int(handle.Fd()))
}

func addExecutableReadRoots(add func(string, bool) error, executable string) error {
	if err := add(filepath.Dir(executable), true); err != nil {
		return err
	}
	resolvedExecutable := executable
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		resolvedExecutable = resolved
	}
	executableDir := filepath.Dir(resolvedExecutable)
	if err := add(executableDir, true); err != nil {
		return err
	}
	if base := filepath.Base(executableDir); base == "bin" || base == "sbin" {
		installRoot := filepath.Dir(executableDir)
		if installRoot != "/" && installRoot != "/usr" {
			return add(installRoot, true)
		}
	}
	return nil
}

// grantableToolchainRootHandle returns an OPEN DESCRIPTOR for the Go
// installation selected by PATH, or nil when no grant may be installed.
//
// A descriptor rather than a path because the caller installs the rule through
// /proc/self/fd/<n>: the object validated here is then the object granted, so a
// symlink swap or rename between validation and RestrictPaths cannot redirect
// it (#1878 round 2, P1-1 - the reviewer's construction repointed a PATH
// component after the lexical checks had passed).
//
// TWO ARMS, deliberately asymmetric:
//
//   - A SYSTEM package root (/opt, /usr/local, /nix/store, /snap) is eligible
//     on location alone. That is pre-existing #1711 behaviour, left unchanged:
//     requiring the identity proof below of a CI runner's toolchain could
//     refuse a valid one and break review seats on the runner, which is the
//     reject-valid-input direction, and that regression is not this change's.
//   - A root under the OPERATOR'S HOME must PROVE it is a Go installation. This
//     arm is what #1878 added and where all three round-2 P1s were.
//
// The home arm's earlier shape - a depth heuristic plus a deny-list of
// credential directory names - is DELETED rather than extended, and the
// omission of .kimi-code is not being patched: it is the evidence the shape was
// wrong. That list omitted .kimi-code while this repo stores a credential at
// ~/.kimi-code/credentials/kimi-code.json, making that directory grantable, and
// a host census also found .npm and .local/share whose sensitivity nobody has
// modelled. No artifact in this tree owns the set of credential-store roots -
// they are spread across internal/cli's seat state policy, per-runtime staging
// and test fixtures - so a list here would have been a second uncoordinated
// copy of a list with no first copy, drifting silently in the dangerous
// direction. A positive identity needs no list, so the ownership question
// disappears.
//
// WHAT THE SIGNATURE DOES AND DOES NOT COVER, because claiming more than a
// check delivers is how #1839 was closed the first time. It covers ACCIDENTAL
// satisfaction: a credential store, cache or data directory does not happen to
// contain an executable bin/go beside a VERSION file reading "go...". It does
// NOT cover an attacker who can already WRITE into the operator's home - such
// an attacker can fabricate the signature anywhere, and could equally replace
// the real toolchain binary or the daemon's own config. The signature raises
// the bar from "any directory shaped like a path" to "a directory that proves
// it holds a Go installation"; it is not an authenticity proof.
//
// FAILS CLOSED throughout: any resolution, open or validation error yields no
// grant. Deliberately not an error - a host whose toolchain cannot be validated
// must still launch sandboxes, and the visible consequence of refusal is the
// seat's own exit 126, which is the pre-#1878 behaviour rather than a new
// failure mode.
func grantableToolchainRootHandle(executable string) *os.File {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(executable))
	if err != nil {
		return nil
	}
	binDir := filepath.Dir(resolved)
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return nil
	}
	root := filepath.Dir(binDir)
	handle, err := os.Open(root)
	if err != nil {
		return nil
	}
	if systemToolchainRoot(root) || operatorToolchainRoot(root, operatorHomeDir(), handle) {
		return handle
	}
	_ = handle.Close()
	return nil
}

// systemToolchainRoot reports whether root lives under a system package root.
// Unchanged from #1711 apart from being named.
func systemToolchainRoot(root string) bool {
	for _, allowed := range []string{"/opt", "/usr/local", "/nix/store", "/snap"} {
		rel, err := filepath.Rel(allowed, root)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// operatorToolchainRoot reports whether root is a strict descendant of the
// operator's home that PROVES it is a Go installation.
//
// SYMLINKED INTERMEDIATES ARE ACCEPTED, and that is a decision rather than an
// oversight. The resolved path is used, so an operator who symlinks ~/.local to
// a data volume or keeps a current -> go1.26.4 pointer still gets a working
// seat. Requiring realpath == lexical would refuse those layouts, and it buys
// nothing here: the caller installs the rule through the descriptor validated
// below, so the inode is already pinned against a later swap. Refusing them
// would be the reject-valid-input direction for no security gain.
//
// A ROOT-VALUED HOME IS REJECTED. With home "/" every absolute path is a
// descendant and the boundary means nothing: /var/secrets was measured
// grantable under the previous shape (#1878 round 2, P1-3), whose tests SKIPPED
// that case rather than asserting it.
func operatorToolchainRoot(root, home string, handle *os.File) bool {
	home = strings.TrimSpace(home)
	if home == "" || home == "/" || !filepath.IsAbs(home) || !filepath.IsAbs(root) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(root))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return goInstallationAt(handle)
}

// goInstallationAt proves, THROUGH an open directory descriptor, that the
// directory is a Go installation: an executable bin/go beside a VERSION file
// whose contents begin with "go".
//
// This is the entire security boundary for the home arm, so it is a positive
// content proof rather than a name check. Measured on this host: the pinned
// root's VERSION reads "go1.26.4", while ~/.kimi-code/credentials, ~/.npm,
// ~/.local/share, ~/.codex, ~/.claude, ~/.gitmoot and ~/.ssh contain no VERSION
// file at all. Reads are relative to the validated descriptor via
// /proc/self/fd/<n>, so they describe the inode that will be granted.
//
// BOTH halves are required and a test mutates each: bin/go alone is satisfied
// by any directory holding a copy of the binary, and VERSION alone by any
// directory holding a file that starts with "go".
func goInstallationAt(handle *os.File) bool {
	if handle == nil {
		return false
	}
	base := procFdPath(handle)
	goBinary, err := os.Stat(filepath.Join(base, "bin", "go"))
	if err != nil || !goBinary.Mode().IsRegular() || goBinary.Mode().Perm()&0o111 == 0 {
		return false
	}
	version, err := os.Open(filepath.Join(base, "VERSION"))
	if err != nil {
		return false
	}
	defer version.Close()
	info, err := version.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(version, prefix); err != nil {
		return false
	}
	return string(prefix) == "go"
}

// operatorHomeDir resolves the INVOKING OPERATOR's home from the passwd
// database, deliberately NOT from $HOME.
//
// MEASURED, and the whole reason this helper exists: a read-only seat rewrites
// HOME to its own throwaway cache root, and sandbox-exec inherits that env, so
// os.UserHomeDir() inside this process returns the SEAT's home. With HOME
// pointed at a scratch directory, os.UserHomeDir() returned that scratch path
// while user.Current().HomeDir still returned the operator's real home. A depth
// rule written against os.UserHomeDir() would therefore never match the
// operator's toolchain and this fix would be INERT in exactly the process it
// exists to serve.
//
// Called before RestrictPaths installs any rule, so reading passwd needs no
// grant of its own.
func operatorHomeDir() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(current.HomeDir)
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
