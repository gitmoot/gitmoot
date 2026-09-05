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
		readable, toolchainFiles, held, err := readableRoots(readPaths, executable)
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
		// Toolchain root files (VERSION, go.env) are validated by the same
		// helper as caller-declared files, so nothing is granted that does not
		// exist and is not a regular file.
		files, err := readableFiles(append(append([]string{}, readFiles...), toolchainFiles...))
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
func readableRoots(paths []string, executable string) ([]string, []string, []*os.File, error) {
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
			return nil, nil, nil, err
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
			return nil, nil, nil, err
		}
	}
	if err := addExecutableReadRoots(add, executable); err != nil {
		closeHeld()
		return nil, nil, nil, err
	}
	// The toolchain grant is OPTIONAL and FAILS CLOSED. A host with no `go`, or
	// with one whose installation cannot be validated, simply gets no grant:
	// refusing the grant is the safe outcome, whereas returning an error here
	// would refuse to launch every sandbox on such a host. The visible
	// consequence of a refusal is the seat's own exit 126, which is exactly the
	// pre-#1878 behaviour rather than a new failure.
	var extraFiles []string
	if goExecutable, err := execLookPath("go"); err == nil {
		if grant := grantableToolchain(goExecutable); grant != nil {
			held = append(held, grant.handles...)
			for _, dir := range grant.dirs {
				if err := add(dir, true); err != nil {
					closeHeld()
					return nil, nil, nil, err
				}
			}
			extraFiles = append(extraFiles, grant.files...)
		}
	}
	return roots, extraFiles, held, nil
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
// WHAT IS GRANTED IS NARROWER THAN WHAT IS VALIDATED, and that is what closes
// round 2's second P1. Positive identity proved a Go installation was PRESENT
// in a directory; it never proved the directory held nothing else, so a root
// carrying both the signature and a credential - ~/.ssh/bin/go beside
// ~/.ssh/id_ed25519 - was granted WHOLESALE. Only the installation's own
// members are granted now: the member directories plus the two root files go
// reads. A sibling secret is never named, so it is never granted.
//
// MEASURED, with controls in both directions: granting only members leaves
// `go version`, `go build ./...` and `go test ./...` all at rc=0 inside a seat
// domain, while listing the installation root is rc=2 and reading an unnamed
// root file is rc=1. Landlock also accepts rules for paths UNDER a descriptor's
// magic symlink, so the narrow paths stay descriptor-derived; in a child
// process the granted children were readable, the root unlistable and a sibling
// secret denied.
//
// WHAT THE SIGNATURE DOES AND DOES NOT COVER, because claiming more than a
// check delivers is how #1839 was closed the first time. It covers ACCIDENTAL
// satisfaction: a credential store, cache or data directory does not happen to
// contain an executable bin/go beside a VERSION file reading "go...". It does
// NOT cover an attacker who can already WRITE into the operator's home - such
// an attacker can fabricate the signature anywhere. With the narrowed grant the
// residual exposure of such a directory is exactly its Go-layout members and
// the two named root files, and nothing else it holds.
//
// FAILS CLOSED throughout: any resolution, open, identity or validation error
// yields no grant. Deliberately not an error - a host whose toolchain cannot be
// validated must still launch sandboxes, and the visible consequence of refusal
// is the seat's own exit 126, the pre-#1878 behaviour rather than a new failure.
func grantableToolchain(executable string) *toolchainGrant {
	resolved, err := filepath.EvalSymlinks(strings.TrimSpace(executable))
	if err != nil {
		return nil
	}
	binDir := filepath.Dir(resolved)
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return nil
	}
	handle, err := os.Open(filepath.Dir(binDir))
	if err != nil {
		return nil
	}
	grant := describeToolchain(handle)
	if grant == nil {
		_ = handle.Close()
		return nil
	}
	return grant
}

// describeToolchain classifies and enumerates a candidate installation using
// ONLY the open descriptor it is given.
//
// THE NAME IS NEVER TRUSTED AGAIN AFTER THE OPEN, which is round 2's first P1.
// Previously EvalSymlinks produced a string, os.Open RE-WALKED that string, and
// containment was then checked on the string while the grant used the handle -
// so an exchange between the two calls left a home-descendant name classified
// while a different directory was opened and granted. Descriptor pinning
// defeated swaps after the open; it did not bind the DECISION to the opened
// inode. Here the classification path comes from readlink on the descriptor's
// own /proc/self/fd entry, an unlinked directory is refused by its " (deleted)"
// suffix, and os.SameFile proves that path still names the very inode held
// before the path is used for anything.
func describeToolchain(handle *os.File) *toolchainGrant {
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		return nil
	}
	base := procFdPath(handle)
	classified, err := os.Readlink(base)
	// THE " (deleted)" REFUSAL AND THE IDENTITY CHECK BELOW ARE DEFENCE IN
	// DEPTH, AND THEIR MUTANTS SURVIVE BY EQUIVALENCE - stated here rather than
	// claimed as covered. Measured: unlinking a directory takes its contents
	// with it, so readlink reports " (deleted)" AND goInstallationAt then fails
	// to stat bin/go through the descriptor. For every input a deterministic
	// test can construct, these two refuse exactly what the signature check
	// refuses one step later. What they add is the RACING case - a path swapped
	// between readlink and use - which cannot be constructed without
	// concurrency, so no test here pins them. They are kept because a future
	// reordering that moved the signature check earlier, or relaxed it, would
	// otherwise lose the property silently.
	if err != nil || !filepath.IsAbs(classified) || strings.HasSuffix(classified, " (deleted)") {
		return nil
	}
	named, err := os.Lstat(classified)
	if err != nil || !os.SameFile(info, named) {
		return nil
	}
	if !systemToolchainRoot(classified) && !operatorToolchainRoot(classified, operatorHomeDir()) {
		return nil
	}
	if !goInstallationAt(handle) {
		return nil
	}
	grant := &toolchainGrant{handles: []*os.File{handle}}
	// EVERY MEMBER IS ITS OWN DESCRIPTOR. Granting filepath.Join(base, member)
	// would pin the ROOT inode and then resolve "bin", "pkg" and the rest BY
	// NAME one level down, re-creating the same open-time race the root fix just
	// closed: a swap of <root>/bin between enumeration and RestrictPaths would
	// grant a directory that was never validated. Opening each member relative
	// to the held root and installing ITS descriptor path removes that.
	for _, member := range goInstallationMembers {
		entry, err := os.OpenFile(filepath.Join(base, member), os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			// ABSENT MEMBERS SKIP RATHER THAN REFUSE: a trimmed or future
			// installation without misc, test or doc must still work.
			continue
		}
		if info, statErr := entry.Stat(); statErr != nil || !info.IsDir() {
			_ = entry.Close()
			continue
		}
		grant.handles = append(grant.handles, entry)
		grant.dirs = append(grant.dirs, procFdPath(entry))
	}
	for _, member := range goInstallationRootFiles {
		entry, err := os.OpenFile(filepath.Join(base, member), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		if info, statErr := entry.Stat(); statErr != nil || !info.Mode().IsRegular() {
			_ = entry.Close()
			continue
		}
		grant.handles = append(grant.handles, entry)
		grant.files = append(grant.files, procFdPath(entry))
	}
	// A LOUD REFUSAL RATHER THAN AN EMPTY GRANT. The member list is coupled to
	// the Go distribution layout, so a future rename would otherwise install
	// nothing, look like a working sandbox, and fail at exec with the same
	// exit 126 this issue is about.
	//
	// MEASURED UNREACHABLE TODAY, and its mutant therefore survives: the
	// signature requires an executable bin/go, so "bin" always resolves as a
	// member and this set is never empty. It is a tripwire for a future change
	// that drops or loosens the bin/go requirement, not a live branch, and it is
	// documented as such rather than counted as tested.
	if len(grant.dirs) == 0 {
		grant.close()
		return nil
	}
	return grant
}

// toolchainGrant is the set of descriptors whose inodes were classified,
// together with the narrow paths installed for them. Every descriptor must
// outlive rule installation, because each installed path names one of them.
type toolchainGrant struct {
	handles []*os.File
	dirs    []string
	files   []string
}

func (g *toolchainGrant) close() {
	for _, handle := range g.handles {
		_ = handle.Close()
	}
}

// goInstallationMembers are the directories a Go installation exposes that a
// build or test run needs. Enumerated positively so the installation root
// itself is never granted; a member that is absent is simply skipped.
var goInstallationMembers = []string{"bin", "pkg", "src", "lib", "api", "misc", "test", "doc"}

// goInstallationRootFiles are the files go reads from the installation ROOT.
//
// go.env is here because of a measurement, not for completeness: with it
// unreadable and GOTOOLCHAIN unset, build and test still pass but
// `go env GOTOOLCHAIN` returns EMPTY rather than its configured default, and an
// empty GOTOOLCHAIN is what invites the toolchain auto-download a sandboxed
// seat cannot complete.
var goInstallationRootFiles = []string{"VERSION", "go.env"}

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
// SYMLINKS ARE RESOLVED BEFORE CONTAINMENT, WHICH HAS A CONSEQUENCE THE
// PREVIOUS COMMENT DENIED. That comment claimed an operator who symlinks
// ~/.local to a data volume still gets a working seat. That is FALSE: such a
// path RESOLVES OUTSIDE the home directory and is therefore refused here, and a
// test now pins the refusal. A symlink that stays inside home - a
// current -> go1.26.4 pointer, say - is still accepted, because its resolved
// path is a home descendant. Accepting a toolchain whose real location is
// outside home would mean classifying a path outside the very boundary this arm
// exists to enforce, so the refusal is the policy and the old comment was the
// error.
//
// A ROOT-VALUED HOME IS REJECTED. With home "/" every absolute path is a
// descendant and the boundary means nothing: /var/secrets was measured
// grantable under the previous shape (#1878 round 2, P1-3), whose tests SKIPPED
// that case rather than asserting it.
func operatorToolchainRoot(root, home string) bool {
	home = strings.TrimSpace(home)
	if home == "" || home == "/" || !filepath.IsAbs(home) || !filepath.IsAbs(root) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(root))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
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
	// O_NOFOLLOW so a VERSION symlink cannot point the check at an unrelated
	// regular file, and O_NONBLOCK because opening a FIFO with no writer would
	// otherwise BLOCK SANDBOX STARTUP - an availability defect in the launch
	// path, not a leak. Type is proven from the descriptor BEFORE any content is
	// read, which is what the previous version claimed and did not do.
	version, err := os.OpenFile(filepath.Join(base, "VERSION"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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
