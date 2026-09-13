package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
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
// WHICH TOOLCHAIN (#2143). The source is the installation that satisfies the
// WORKSPACE, chosen from every Go on PATH, not the installation that owns the
// first `go` PATH happens to resolve.
//
// exec.LookPath answers "who owns the first go on PATH", which is a different
// question from "which toolchain would build this repository", and on a host
// whose PATH leads with a distro launcher the two differ every time
// GOTOOLCHAIN would resolve upward. Measured on the host that produced #2143:
// PATH led to go1.22.2 while go.mod said go 1.26, so the seat staged 1.22 and
// then could not build the repository it was reviewing.
//
// An empty workspace means no module context, and selection then takes the
// newest installation rather than refusing.
func stageSeatToolchain(paths config.Paths, workspace string) (string, []string, string, error) {
	candidates := toolchain.InstallationsOnPath(os.Getenv("PATH"))
	if len(candidates) == 0 {
		// TWO DIFFERENT HOSTS, TWO DIFFERENT REPAIRS, so they must not collapse
		// into one message. "No Go at all" is reported with an empty
		// diagnostic, which the caller turns into its own absent-Go wording.
		// A `go` that EXISTS but does not sit in an installation layout is a
		// different fact and the operator can act on it, so it keeps saying so.
		//
		// Caught by TestSeatToolchainUnavailableEndsAReviewBlockedNotVerdicted:
		// the first version of this cutover skipped a non-layout `go` silently
		// while enumerating, and a host with an unstageable `go` on PATH
		// started reporting that no Go was found at all.
		if _, err := exec.LookPath("go"); err == nil {
			return unavailableSeatToolchain(paths, "resolved Go executable is not inside a bin/ or sbin/ installation; host copy is shadowed")
		}
		return unavailableSeatToolchain(paths, "")
	}
	required, effectiveGOWORK := "", "off"
	var err error
	if strings.TrimSpace(workspace) != "" {
		required, effectiveGOWORK, err = toolchain.WorkspaceGoRequirement(workspace, os.Getenv("GOWORK"))
		if err != nil {
			return unavailableSeatToolchain(paths, fmt.Sprintf("%v; host copy is shadowed", err))
		}
	}
	candidates, err = toolchain.SelectInstallations(candidates, required)
	if err != nil {
		// Reported at STAGING time, naming the requirement and every rejected
		// tree. The alternative is what this replaces: stage something too old
		// and let the seat emit "go.mod requires go >= 1.26" later, which reads
		// as a repository problem rather than a staging one.
		return unavailableSeatToolchain(paths, fmt.Sprintf("%v; host copy is shadowed", err))
	}

	var refused []string
	for _, candidate := range candidates {
		staged, stageErr := toolchain.Stage(paths.Home, candidate.Root)
		if stageErr != nil {
			refused = append(refused, fmt.Sprintf("%s (%s) could not be staged: %v",
				candidate.Root, candidate.Version, stageErr))
			continue
		}
		path, diagnostic := seatPath(staged)
		if diagnostic != "" {
			refused = append(refused, fmt.Sprintf("%s (%s) produced an unusable staged path: %s",
				candidate.Root, candidate.Version, diagnostic))
			continue
		}
		return staged, []string{
			"GOROOT=" + staged,
			"PATH=" + path,
			// The staged copy is the only toolchain the seat can BUILD with —
			// it is first on PATH and the only Go tree under a read grant — so
			// pin the selector too. An empty GOTOOLCHAIN invites an auto-download
			// a sandboxed seat cannot complete.
			"GOTOOLCHAIN=local",
			// Pin workspace discovery to the same decision used above. Otherwise
			// an ambient parent go.work can make execution require a newer Go
			// release than staging selected.
			"GOWORK=" + effectiveGOWORK,
		}, strings.Join(refused, "; "), nil
	}

	requirement := "any version"
	if strings.TrimSpace(required) != "" {
		requirement = "go" + strings.TrimPrefix(strings.TrimSpace(required), "go")
	}
	return unavailableSeatToolchain(paths, fmt.Sprintf(
		"no satisfying Go installation could be staged for %s: %s; host copy is shadowed",
		requirement, strings.Join(refused, "; ")))
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

// seatRuntimeNames are the provider CLIs every read-only seat may need as a
// sibling runtime. OMP is different: it is a routing harness capable of
// launching more agents, so it is staged only when OMP is the selected runtime.
var seatRuntimeNames = []string{runtime.ClaudeRuntime, runtime.KimiRuntime, runtime.CodexRuntime}

// stageSeatRuntimes materialises engine-owned commands for the provider runtime
// set and, when selected, OMP. Each installed runtime becomes a copied artifact;
// each missing or unstageable one becomes an explicit exit-126 command so
// inherited PATH entries cannot route around the policy.
//
// EVERY INSTALLED PROVIDER RUNTIME IS STAGED, not only the seat's own. A seat's
// prompt may legitimately invoke a sibling provider runtime, and contract item
// 5 of ruling 122157 requires the actual installed Claude, Kimi and Codex to
// LAUNCH from staged copies. Copies are content-addressed, so the second seat
// wanting the same runtime reuses the first seat's tree instead of copying again.
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
// It returns the launchers a seat may exec, THE EXEC-CLOSURE ROOTS those
// launchers additionally require, and diagnostics. The closure is separate from
// the launcher list on purpose: a launcher belongs on the seat's PATH and an
// interpreter must NOT, because a seat that can resolve `node` by name gets a
// second way to run code that no dispatch decision chose (#2141).
func stageSeatRuntimes(paths config.Paths, selectedRuntime string) ([]string, []string, []string, error) {
	names := seatRuntimeNames
	if strings.TrimSpace(selectedRuntime) == runtime.OmpRuntime {
		names = append(append([]string(nil), seatRuntimeNames...), runtime.OmpRuntime)
	}
	var staged []string
	var interpreters []string
	var diagnostics []string
	for _, name := range names {
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
			launcher, closure, stageErr := toolchain.StageRuntime(paths.Home, name, resolved)
			if stageErr == nil {
				staged = append(staged, launcher)
				// The FULL closure, not the immediate interpreter: an
				// interpreter can itself be a script needing another. StageRuntime
				// already filters out system interpreters, whose roots the
				// sandbox grants unconditionally and which have no staged root.
				interpreters = append(interpreters, closure...)
				continue
			}
			diagnostics = append(diagnostics, fmt.Sprintf("runtime %s could not be staged, so it is published unavailable rather than left host-resolvable: %v", name, stageErr))
		}
		unavailable, err := toolchain.StageUnavailableRuntime(paths.Home, name)
		if err != nil {
			return nil, nil, diagnostics, fmt.Errorf("publish unavailable runtime %s: %w", name, err)
		}
		staged = append(staged, unavailable)
	}
	return staged, interpreters, diagnostics, nil
}

// seatRuntimeUnavailable reports WHY the runtime a seat is about to execute was
// published as the exit-126 unavailable shim, or "" when it staged normally.
//
// It reads stageSeatRuntimes' OWN OUTPUT rather than re-probing the host, which
// is the whole point: #1817's first comment measured that a host-side check
// answers about the wrong side of the seat boundary. The seat resolves the
// runtime name from the staged shim directories this list becomes, so the
// published root recorded here IS what argv[0] will resolve to.
//
// SCOPED TO THE SEAT'S OWN RUNTIME, deliberately. A sibling runtime published
// unavailable is a fact about the host, and the comment on the staging call in
// readOnlyRuntimeSandboxGrants explains why that stays a log line: making a
// job's outcome depend on an unrelated runtime's disk state broke a CI event
// baseline once already. A seat whose OWN runtime is unavailable is a different
// statement - that job cannot execute one command - so it is the only case that
// refuses.
//
// The diagnostic is best-effort context, not the trigger. An absent executable
// produces no diagnostic at all (stageSeatRuntimes only records one when
// LookPath succeeded and StageRuntime then failed), so the published shim is
// the fact and the diagnostic only sharpens the message.
func seatRuntimeUnavailable(runtimeName string, stagedExecutables []string, diagnostics []string) string {
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return ""
	}
	unavailableRoot := runtimeName + toolchain.UnavailableRuntimeSuffix
	published := false
	for _, executable := range stagedExecutables {
		if filepath.Base(executable) != runtimeName {
			continue
		}
		// <runtime root>/<name><suffix>/.bin/<name>
		if filepath.Base(filepath.Dir(filepath.Dir(executable))) == unavailableRoot {
			published = true
			break
		}
	}
	if !published {
		return ""
	}
	for _, diagnostic := range diagnostics {
		if strings.HasPrefix(diagnostic, "runtime "+runtimeName+" ") {
			return diagnostic
		}
	}
	return fmt.Sprintf("runtime %s has no daemon-staged artifact, so it is published unavailable rather than left host-resolvable", runtimeName)
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
