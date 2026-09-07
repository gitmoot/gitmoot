package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// stageSeatToolchain materialises a daemon-owned copy of the operator-pinned Go
// toolchain and returns the path to grant plus the environment that points a
// seat at it.
//
// WHY A COPY (#1878). Three review rounds tried granting a read-only seat a
// Landlock rule over the operator's own installation, and each round produced an
// escape-class defect: a member symlink that redirected a grant outside
// containment with no privilege required, and a TOCTOU between classifying a
// path and installing a rule for it. A TOCTOU on a path the daemon does not own
// cannot be closed by any rule, only narrowed. Copying a tree the daemon owns
// removes the outside root, so there is nothing left to contain.
//
// EVERY INSTALLATION PREFIX IS IN SCOPE (#1921, ruling 122157). `/opt`,
// `/usr/local`, `/nix/store`, `/snap`, and profile paths all take the same copy
// path; ownership of the operator's parent directory is irrelevant once no
// recursive rule is granted over it.
//
// FAILURE IS EXPLICIT. If `go` is absent, unpinned, or cannot be copied, an
// engine-owned exit-126 command shadows any later host PATH entry. Failure to
// publish that command aborts seat setup: falling through to the host copy would
// silently restore the grant class this cutover removes.
func stageSeatToolchain(paths config.Paths) (string, []string, string, error) {
	resolved, err := exec.LookPath("go")
	if err != nil {
		return unavailableSeatToolchain(paths, "")
	}
	// RESOLVE THE SYMLINK BEFORE CLASSIFYING. A PATH entry is frequently a link
	// into the real installation: /usr/bin/go on this host points at
	// /usr/lib/go-1.22/bin/go. Classifying the RAW path yields the install root
	// "/usr", so staging would try to copy the entire system tree instead of a
	// toolchain - StageRuntime already resolves for exactly this reason.
	if target, linkErr := filepath.EvalSymlinks(resolved); linkErr == nil {
		resolved = target
	}
	source, ok := toolchainInstallRoot(resolved)
	if !ok {
		return unavailableSeatToolchain(paths, "resolved Go executable is not inside a bin/ or sbin/ installation; host copy is shadowed")
	}
	staged, err := toolchain.Stage(paths.Home, source)
	if err != nil {
		if errors.Is(err, toolchain.ErrNotPinned) {
			return unavailableSeatToolchain(paths, "resolved Go installation is not pinned; host copy is shadowed")
		}
		return unavailableSeatToolchain(paths, fmt.Sprintf("staged toolchain unavailable; host copy is shadowed: %v", err))
	}
	path, diagnostic := seatPath(staged)
	if diagnostic != "" {
		return unavailableSeatToolchain(paths, diagnostic)
	}
	return staged, []string{
		"GOROOT=" + staged,
		"PATH=" + path,
		// The staged copy is the only toolchain the seat can BUILD with — it is
		// first on PATH and the only Go tree under a read grant — so pin the
		// selector too: an empty GOTOOLCHAIN invites the auto-download a
		// sandboxed seat cannot complete.
		"GOTOOLCHAIN=local",
	}, "", nil
}

func unavailableSeatToolchain(paths config.Paths, diagnostic string) (string, []string, string, error) {
	command, err := toolchain.StageUnavailableRuntime(paths.Home, "go")
	if err != nil {
		return "", nil, diagnostic, fmt.Errorf("publish unavailable Go command: %w", err)
	}
	root, err := toolchain.StagedRuntimeRoot(paths.Home, command)
	if err != nil {
		return "", nil, diagnostic, err
	}
	env, pathDiagnostics := withSeatRuntimePath(nil, []string{command})
	if len(pathDiagnostics) != 0 {
		return "", nil, diagnostic, fmt.Errorf("publish unavailable Go command: %s", strings.Join(pathDiagnostics, "; "))
	}
	return root, env, diagnostic, nil
}

// seatRuntimeNames are the runtimes a read-only seat may need to LAUNCH. Each is
// a runtime this engine dispatches jobs to, so the list is derived from what the
// engine can start rather than from what happens to be installed on a host.
var seatRuntimeNames = []string{"claude", "kimi", "codex"}

// stageSeatRuntimes materialises one engine-owned command for every runtime
// class the engine can dispatch. Each INSTALLED runtime becomes a copied
// artifact; each missing or unstageable one becomes an explicit exit-126 command
// so inherited PATH entries cannot route around the policy.
//
// EVERY INSTALLED RUNTIME IS STAGED, not only the seat's own. A seat's prompt
// may legitimately invoke a sibling runtime, and contract item 5 of ruling
// 122157 requires the actual installed Claude, Kimi and Codex to LAUNCH from
// staged copies - "a design that merely turns all four into MISSING is not
// accepted". Copies are content-addressed, so the second seat wanting the same
// runtime reuses the first seat's tree instead of copying again.
//
// WHY COPIES AND NOT GRANTS (#1921 review rounds 1-3, ruling 122157). Three
// rounds tried to make a recursive Landlock grant over the OPERATOR's install
// root safe by inspecting that root. Round 1 withheld the grant for a list of
// credential filenames; the list can never be complete, and kimi's config.toml
// carried api_key material that was not on it. Round 3 required positive proof of
// a package tree; both markers are PLANTABLE, and a probe built
// node_modules/operator-profile/{package.json,bin/kimi,config.toml} and read the
// mode-0600 config. That round also showed the Lstat scan is a time-of-check
// decision guarding a RECURSIVE grant, so a credential created after setup lands
// inside an already-granted tree.
//
// Markers prove SHAPE, never IMMUTABILITY, and no pre-exec check constrains
// descendants that do not exist yet. So the grant is REMOVED rather than
// narrowed: the engine copies each runtime's own artifact into a tree it created
// and grants that. A daemon-owned tree has no operator-writable descendants to
// appear later, which closes the class rather than its two known instances.
func stageSeatRuntimes(paths config.Paths) ([]string, []string, error) {
	var staged []string
	var diagnostics []string
	for _, name := range seatRuntimeNames {
		if resolved, lookupErr := exec.LookPath(name); lookupErr == nil {
			// STAGING NOW OWNS THE WHOLE ARTIFACT, INTERPRETER INCLUDED. The
			// engine reads the entrypoint's shebang from the descriptor it will
			// copy from, stages the interpreter it names, and writes a launcher
			// that execs the STAGED interpreter with the STAGED entrypoint - so
			// the kernel never resolves the original absolute shebang, which
			// may not exist under the seat's grants and, where it does, is the
			// operator's copy (#1921 panel, directive 122816).
			//
			// ANY failure here - an unresolvable interpreter, an ambiguous
			// package member, openat2 unavailable - publishes the runtime NAME
			// as the exit-126 command below rather than leaving it
			// host-resolvable. That is the whole point: an explicit
			// unavailability instead of a silent fallback.
			launcher, stageErr := toolchain.StageRuntime(paths.Home, name, resolved)
			if stageErr == nil {
				staged = append(staged, launcher)
				continue
			}
			diagnostics = append(diagnostics, fmt.Sprintf("runtime %s could not be staged, so it is published unavailable rather than left host-resolvable: %v", name, stageErr))
		}
		unavailable, err := toolchain.StageUnavailableRuntime(paths.Home, name)
		if err != nil {
			return nil, diagnostics, fmt.Errorf("publish unavailable runtime %s: %w", name, err)
		}
		staged = append(staged, unavailable)
	}
	return staged, diagnostics, nil
}

// seatRuntimePathEntries turns staged executables into PATH entries, deduplicated
// and in a stable order.
//
// An entry containing the list separator is REFUSED rather than shipped: PATH
// cannot express one, so it would split and the seat would resolve the operator's
// copy instead of the staged one, which is the exposure this change removes.
func seatRuntimePathEntries(stagedExecutables []string) ([]string, []string) {
	var entries []string
	var diagnostics []string
	seen := map[string]bool{}
	for _, executable := range stagedExecutables {
		dir := filepath.Dir(executable)
		if strings.ContainsRune(dir, os.PathListSeparator) {
			diagnostics = append(diagnostics, fmt.Sprintf("staged runtime path %q contains %q and cannot be a PATH entry", dir, string(os.PathListSeparator)))
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries = append(entries, dir)
	}
	return entries, diagnostics
}

// withSeatRuntimePath prepends fingerprint-local runtime shim directories to
// the PATH already prepared for the staged Go toolchain. If Go was not staged,
// it prepends them to the inherited PATH directly.
//
// The host PATH remains behind the shims for ordinary system commands. The
// runtime names cannot fall through to their host copies when staging succeeds,
// because every configured name has a shim in the prefix. A staging failure is
// emitted by stageSeatRuntimes; the follow-up fail-stub test pins the explicit
// exit rather than silently executing the host copy.
func withSeatRuntimePath(env []string, stagedExecutables []string) ([]string, []string) {
	entries, diagnostics := seatRuntimePathEntries(stagedExecutables)
	if len(entries) == 0 {
		return env, diagnostics
	}
	prefix := strings.Join(entries, string(os.PathListSeparator))
	for i, item := range env {
		if !strings.HasPrefix(item, "PATH=") {
			continue
		}
		current := strings.TrimPrefix(item, "PATH=")
		if current == "" {
			env[i] = "PATH=" + prefix
		} else {
			env[i] = "PATH=" + prefix + string(os.PathListSeparator) + current
		}
		return env, diagnostics
	}
	current := strings.TrimSpace(os.Getenv("PATH"))
	if current == "" {
		current = "/usr/local/bin:/usr/bin:/bin"
	}
	return append(env, "PATH="+prefix+string(os.PathListSeparator)+current), diagnostics
}

// seatPath puts the staged toolchain first and KEEPS the inherited PATH behind
// it.
//
// WHY NOT A FIXED LIST (#1918). The first form of this returned
// `<staged>/bin:/usr/local/bin:/usr/bin:/bin`, which does not extend PATH, it
// REPLACES it: grants.env is appended to os.Environ() by the subprocess
// runners and exec.Cmd dedups by key keeping the last occurrence, so that list
// became the seat's whole PATH. Runtime binaries do not live in those three
// directories — on the host that shipped it, `claude` is in /root/.local/bin
// and `kimi` in /root/.kimi-code/bin — and sandbox-exec resolves argv[0] with
// exec.LookPath BEFORE it installs any Landlock rule, so EVERY claude and kimi
// read-only seat failed to launch with "executable file not found in $PATH".
// Measured boundary: gm-review-opus was 11-for-11 in the fourteen hours before
// the deploy and 0-for-2 after it.
//
// PATH IS NOT THE CONTAINMENT BOUNDARY FOR WHAT A SEAT MAY READ — the Landlock
// ruleset decides that, and a PATH entry with no read grant behind it is simply
// an exec that fails. It IS, however, an input to the grant computation:
// sandbox-exec resolves argv[0] from PATH and grants the resolved binary's
// directory, so widening PATH can widen grants (#1921 review found kimi's
// self-contained binary promoting ~/.kimi-code, a credential store, to a
// readable root). That promotion is now withheld for credential-bearing roots
// in internal/sandbox, which is where the grant is decided; PATH stays wide so
// the binaries remain resolvable.
//
// The pin is not weakened: the staged bin is first, so `go` resolves inside the
// copy the daemon owns even when the operator's own installation is still on
// the inherited PATH, and GOROOT plus GOTOOLCHAIN=local hold independently of
// lookup order.
//
// An empty inherited PATH keeps the previous system defaults rather than
// shipping a seat with only one directory on PATH.
func seatPath(staged string) (string, string) {
	stagedBin := filepath.Join(staged, "bin")
	// A PATH entry cannot contain the list separator, so a staged path holding
	// one cannot be expressed on PATH at all: the entry would split and `go`
	// would resolve outside the staged copy. Refuse rather than ship a PATH
	// whose first entry is a fragment, and say which value did it — a home
	// containing ':' is legal on disk and the diagnostic is the only way an
	// operator learns why the seat has no staged toolchain.
	if strings.ContainsRune(stagedBin, os.PathListSeparator) {
		return "", fmt.Sprintf("staged toolchain path %q contains %q, which cannot be expressed as a PATH entry; seat has no staged Go toolchain", stagedBin, string(os.PathListSeparator))
	}
	inherited := strings.TrimSpace(os.Getenv("PATH"))
	if inherited == "" {
		return stagedBin + ":/usr/local/bin:/usr/bin:/bin", ""
	}
	return stagedBin + string(os.PathListSeparator) + inherited, ""
}

// toolchainInstallRoot reports the installation root containing a Go
// executable. Every root is staged — including /opt, /usr/local, /nix/store and
// /snap — because ruling 122157 removed the split ownership that previously
// handed those prefixes to optionalSystemToolchainRoot.
//
// THE OLD SYSTEM-PREFIX EXCLUSION WAS THE SAME DEFECT AT A DIFFERENT SITE. It
// left the operator's root in place and compensated with a recursive read grant.
// Review round 3 proved that no inspection can make such a grant safe: the
// root's shape says nothing about who can create a descendant after setup.
// Returning every installation layout here does not TRUST the root; it makes the
// daemon COPY it before the seat sees it.
func toolchainInstallRoot(goExecutable string) (string, bool) {
	binDir := filepath.Dir(filepath.Clean(goExecutable))
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return "", false
	}
	return filepath.Dir(binDir), true
}

// validateStagedToolchainPlacement is this shape's P1 expressed as code.
//
// A seat's cache root IS its write grant: readOnlyRuntimeSandboxGrants sets
// grants.writes from agent.WritablePaths, and that same root is the adapter's
// cleanupRoot. A staged copy placed anywhere beneath it would therefore be
// WRITABLE BY THE SEAT, which means the seat could rewrite its own go binary and
// this shape would have reproduced the defect it exists to remove, with extra
// steps.
//
// The check is symmetric on purpose. A copy inside a write root is the obvious
// hazard; a write root inside the copy root is the same hazard inverted, and
// both are refused. It returns an ERROR rather than a diagnostic because a
// misplaced copy is a containment failure, not a missing convenience.
func validateStagedToolchainPlacement(staged string, writes []string) error {
	staged = filepath.Clean(staged)
	for _, write := range writes {
		write = filepath.Clean(strings.TrimSpace(write))
		if write == "" {
			continue
		}
		if pathWithin(staged, write) {
			return fmt.Errorf("staged toolchain %q is inside seat-writable %q; a seat could rewrite its own toolchain", staged, write)
		}
		if pathWithin(write, staged) {
			return fmt.Errorf("seat-writable %q is inside staged toolchain %q; a seat could rewrite its own toolchain", write, staged)
		}
	}
	return nil
}

// pathWithin reports whether inner is at or below outer.
func pathWithin(inner, outer string) bool {
	relative, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
