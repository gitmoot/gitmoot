//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		readable, err := readableRoots(readPaths, executable)
		if err != nil {
			return err
		}
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
func readableRoots(paths []string, executable string) ([]string, error) {
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
		"/usr/local/bin", "/usr/local/sbin", "/usr/local/lib", "/usr/local/lib64", "/usr/local/share",
		"/etc/ssl/certs", "/etc/pki",
		// procfs read is a runtime BOOTSTRAP requirement, not a convenience: the
		// Bun-based Claude/Kimi binaries abort with an opaque crash without it,
		// and codex's managed bwrap fails reading /proc/sys/kernel/overflowuid.
		// The legacy no-reads mode always had it via RODirs("/"), so strict read
		// mode was the regression rather than this grant being a widening.
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
			return nil, err
		}
	}
	if err := addExecutableReadRoots(add, executable); err != nil {
		return nil, err
	}
	if goExecutable, err := execLookPath("go"); err == nil {
		if root := optionalSystemToolchainRoot(goExecutable); root != "" {
			if err := add(root, true); err != nil {
				return nil, err
			}
		}
	}
	return roots, nil
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
		if installRoot == "/" || installRoot == "/usr" {
			return nil
		}
		// PROMOTION REQUIRES POSITIVE PROOF OF A PACKAGE TREE (#1921 review, both P1s).
		//
		// Promotion exists for one reason: a node-packaged runtime cannot run
		// without its package root. codex resolves to
		// <node>/lib/node_modules/@openai/codex/bin/codex.js and needs the files
		// beside that bin dir. Nothing else needs it — a self-contained
		// executable runs from its exec dir, which is already granted above.
		//
		// THE PREVIOUS RULE ASKED THE WRONG QUESTION AND COULD NOT BE REPAIRED BY
		// ASKING IT BETTER. It promoted any install root that did NOT contain a
		// known credential name, which failed twice for reasons that are the same
		// reason: absence is not proof.
		//
		//   - The name list can never be complete (review F1). kimi's config.toml
		//     carries api_key and OAuth material (internal/runtime/kimi.go,
		//     internal/cli/daemon_worker.go read it), and it was not on the list,
		//     so a profile holding bin/kimi plus a secret-bearing config.toml was
		//     granted whole. A kernel probe read it: CONFIG_CREDENTIAL=READABLE.
		//   - The scan can never be timely (review F2). Lstat is a time-of-check
		//     decision and the Landlock grant is recursive, so a credential
		//     CREATED AFTER setup lands inside an already-granted root. A
		//     synchronized probe created profile/credentials/late-token.json after
		//     the seat had entered and read it: LATE_CREDENTIAL=READABLE. No
		//     longer list and no repeated pre-exec scan closes a temporal gap.
		//
		// So the rule is inverted: grant only a root PROVEN to be a package tree,
		// and withhold otherwise. A mutable operator-owned profile can never
		// present that proof, which is what makes both findings unreachable rather
		// than merely unlikely — there is no credential name to enumerate and no
		// window in which to create one, because the root is never granted.
		//
		// Withholding degrades to a launch failure that names itself; granting
		// leaks an account. That asymmetry is why the unproven case loses.
		if !isPackageInstallRoot(installRoot) {
			return nil
		}
		return add(installRoot, true)
	}
	return nil
}

// isPackageInstallRoot reports whether a candidate root is a node package tree,
// the only shape promotion is for.
//
// The proof has two parts and needs both: the root must carry a regular
// `package.json`, and it must sit under a `node_modules` path segment. Either
// alone is too weak — an operator profile may hold a stray package.json, and a
// `node_modules` ancestor alone does not make an arbitrary directory a package.
//
// It fails CLOSED on every uncertainty: any Lstat error, including a permission
// error, and any non-regular package.json (a symlink or directory, which is what
// a planted marker looks like) withholds the grant.
func isPackageInstallRoot(root string) bool {
	root = filepath.Clean(root)
	if !hasPathSegment(root, "node_modules") {
		return false
	}
	info, err := os.Lstat(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// hasPathSegment reports whether name appears as a WHOLE element of path, so
// `/opt/node_modules_backup/x` does not match `node_modules`.
func hasPathSegment(path string, name string) bool {
	for _, element := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if element == name {
			return true
		}
	}
	return false
}

// optionalSystemToolchainRoot grants the Go installation selected by PATH when
// it lives under a system package root. Review agents must be able to run the
// repository's toolchain, while a user-controlled binary under /root or /home
// must not turn its credential-bearing parent into a readable subtree.
func optionalSystemToolchainRoot(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	binDir := filepath.Dir(filepath.Clean(executable))
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return ""
	}
	root := filepath.Dir(binDir)
	for _, allowed := range []string{"/opt", "/usr/local", "/nix/store", "/snap"} {
		rel, err := filepath.Rel(allowed, root)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root
		}
	}
	return ""
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
