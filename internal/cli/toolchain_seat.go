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
// SCOPE IS THE OPERATOR-PINNED TOOLCHAIN ONLY. A toolchain under a system
// package root keeps its pre-existing location-only grant inside
// internal/sandbox and is deliberately untouched here: that behaviour predates
// #1878, was never the requirement, and there is no evidence about packaged
// layouts that would justify changing it. A previous attempt changed that arm
// without such evidence and regressed all four prefixes.
//
// FAILURE IS A DIAGNOSTIC, NOT AN ERROR. If `go` cannot be found, is not a
// pinned installation, or cannot be staged, the seat is left exactly as it was
// before this change: no grant, and a visible exit 126 if it tries to build.
// Refusing to launch the sandbox instead would turn a missing convenience into
// an outage.
func stageSeatToolchain(paths config.Paths) (string, []string, string) {
	resolved, err := exec.LookPath("go")
	if err != nil {
		return "", nil, ""
	}
	source, ok := pinnedToolchainRoot(resolved)
	if !ok {
		// A system-prefix or otherwise unpinned toolchain: not this path's
		// business, and silence rather than a diagnostic because it is the
		// normal case on a packaged host.
		return "", nil, ""
	}
	staged, err := toolchain.Stage(paths.Home, source)
	if err != nil {
		if errors.Is(err, toolchain.ErrNotPinned) {
			return "", nil, ""
		}
		return "", nil, fmt.Sprintf("staged toolchain unavailable, seat has no Go toolchain: %v", err)
	}
	return staged, []string{
		"GOROOT=" + staged,
		"PATH=" + seatPath(staged),
		// The staged copy is the only toolchain the seat can BUILD with — it is
		// first on PATH and the only Go tree under a read grant — so pin the
		// selector too: an empty GOTOOLCHAIN invites the auto-download a
		// sandboxed seat cannot complete.
		"GOTOOLCHAIN=local",
	}, ""
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
// PATH IS NOT THE CONTAINMENT BOUNDARY, so widening it grants nothing: the
// Landlock ruleset decides what a seat may read or execute, and a PATH entry
// with no read grant behind it is simply an exec that fails. The pin is not
// weakened either — the staged bin is first, so `go` resolves inside the copy
// the daemon owns even when the operator's own installation is still on the
// inherited PATH, and GOROOT plus GOTOOLCHAIN=local hold independently of
// lookup order.
//
// An empty inherited PATH keeps the previous system defaults rather than
// shipping a seat with only one directory on PATH.
func seatPath(staged string) string {
	stagedBin := filepath.Join(staged, "bin")
	inherited := strings.TrimSpace(os.Getenv("PATH"))
	if inherited == "" {
		return stagedBin + ":/usr/local/bin:/usr/bin:/bin"
	}
	return stagedBin + string(os.PathListSeparator) + inherited
}

// pinnedToolchainRoot reports the installation root of an OPERATOR-PINNED
// toolchain, and refuses a system package root.
//
// The four system prefixes are named here only to EXCLUDE them, so that the
// pre-existing grant in internal/sandbox remains the single owner of that case
// and this path cannot silently start competing with it.
func pinnedToolchainRoot(goExecutable string) (string, bool) {
	binDir := filepath.Dir(filepath.Clean(goExecutable))
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return "", false
	}
	root := filepath.Dir(binDir)
	for _, systemPrefix := range []string{"/opt", "/usr/local", "/nix/store", "/snap"} {
		relative, err := filepath.Rel(systemPrefix, root)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", false
		}
	}
	return root, true
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
