package toolchain

import (
	"errors"
	"fmt"
	"go/version"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Installation selection for a seat's Go toolchain (#2143).
//
// THE DEFECT THIS EXISTS TO REMOVE. Seat staging resolved its source with
// exec.LookPath("go") and then took the installation that owned that binary.
// That answers "who owns the first go on PATH", which is a different question
// from "which toolchain would actually build this repository". On a host where
// PATH leads with a distro launcher, the two differ every time GOTOOLCHAIN
// resolves upward, which is the normal case: measured on the host that produced
// #2143, PATH led to go1.22.2 while the repository's go.mod says go 1.26.
//
// It produced TWO failures, and the second is why a narrow fix was refused.
// First, the distro tree is refused outright: golang-1.22-go ships
// pkg/include as a symlink into /usr/share, and the member walk refuses a
// symlink at any component, so staging failed and the seat's toolchain was
// published unavailable. Second, and underneath it, even a tree that copies
// cleanly is the WRONG VERSION - a seat pinned to the launcher gets
// GOROOT=<staged 1.22> and GOTOOLCHAIN=local, and then:
//
//	go: go.mod requires go >= 1.26 (running go 1.22.2; GOTOOLCHAIN=local)
//
// So repairing only the symlink moves the failure rather than removing it, and
// moves it into a message that reads as a repository problem rather than a
// staging one.
//
// WHY NOT ASK `go` WHICH TOOLCHAIN IT WOULD USE. Because it cannot answer
// offline. Measured on the same host, with every candidate already in the
// module cache:
//
//	$ GOTOOLCHAIN=auto go env GOROOT
//	go: downloading go1.26 (linux/amd64)
//	go: download go1.26 for linux/amd64: toolchain not available
//
// Resolving a go.mod directive like "1.26" to a concrete release is a proxy
// lookup, so a seat that asked would depend on the network at staging time.
// Selection here is therefore done from LOCALLY PRESENT installations and the
// requirement is compared directly.
//
// GOTOOLCHAIN=local FOR THE SEAT IS DELIBERATE AND STAYS. A seat that
// downloads a toolchain mid-review is worse than one that fails: the download
// is unsandboxed network use during a graded run. The bug was never that the
// seat pins a toolchain, only WHICH one it pins.

// ErrNoSatisfyingToolchain reports that no installed Go satisfies the
// workspace's requirement.
//
// Distinct from ErrNotPinned, which says a particular tree is not a usable Go
// installation. Wrapping that one here produced the operator-facing string
// "not a pinned go installation: no installed Go satisfies go1.26", which
// names the wrong problem first: every candidate may be perfectly pinned and
// simply too old.
var ErrNoSatisfyingToolchain = errors.New("no installed Go satisfies the workspace")

// GoInstallation is a Go installation root and the release it contains.
type GoInstallation struct {
	// Root is the installation directory, the parent of bin/.
	Root string
	// Version is the contents of the tree's VERSION file, e.g. "go1.22.2".
	Version string
}

// InstallationRoot reports the installation root that owns a go executable.
//
// A PATH entry is frequently a link into the real installation, so the caller
// is expected to have resolved symlinks on the EXECUTABLE first: on the #2143
// host /usr/bin/go points at ../lib/go-1.22/bin/go, and classifying the raw
// path yields "/usr" and would stage the entire system tree.
func InstallationRoot(goExecutable string) (string, bool) {
	binDir := filepath.Dir(filepath.Clean(goExecutable))
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return "", false
	}
	return filepath.Dir(binDir), true
}

// InstallationVersion reads an installation's VERSION file through the same
// contained reader staging uses, so a tree that cannot be read here cannot be
// silently staged later either.
func InstallationVersion(root string) (string, error) {
	handle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	version, err := readVersion(handle)
	if err != nil {
		return "", err
	}
	return safeVersion(version)
}

// SelectInstallations returns every installation that satisfies a workspace's
// Go requirement, in the order a seat should try to stage them.
//
// `required` is the module's go directive, e.g. "1.26" or "go1.26"; an empty
// requirement means any installation will do and the newest is tried first.
//
// AMONG SATISFYING CANDIDATES THE LOWEST IS TRIED FIRST. This is a POLICY
// CHOICE, not a reproduction of Go's rule. Go does not rank installations: it
// keeps the invoked toolchain when that satisfies the module and otherwise
// selects a newer one according to GOTOOLCHAIN. Ranking the installed set here
// makes review seats deterministic without trusting PATH order.
//
// Selection does not establish that a tree is stageable. The caller must try
// every returned installation in order: a structurally invalid lower release
// must not mask a usable higher release.
func SelectInstallations(candidates []GoInstallation, required string) ([]GoInstallation, error) {
	required = strings.TrimSpace(required)
	preferNewest := required == ""
	var want string
	if !preferNewest {
		var err error
		want, err = normalizedGoVersion(required)
		if err != nil {
			return nil, err
		}
	}

	type rankedInstallation struct {
		installation GoInstallation
		version      string
	}
	ranked := make([]rankedInstallation, 0, len(candidates))
	for _, candidate := range candidates {
		candidateVersion, err := normalizedGoVersion(candidate.Version)
		if err != nil {
			continue
		}
		if !preferNewest && version.Compare(candidateVersion, want) < 0 {
			continue
		}
		ranked = append(ranked, rankedInstallation{
			installation: candidate,
			version:      candidateVersion,
		})
	}
	if len(ranked) == 0 {
		return nil, fmt.Errorf("%w %s: %s",
			ErrNoSatisfyingToolchain, describeRequirement(required), describeCandidates(candidates))
	}
	slices.SortStableFunc(ranked, func(left, right rankedInstallation) int {
		order := version.Compare(left.version, right.version)
		if order == 0 {
			return strings.Compare(left.installation.Root, right.installation.Root)
		}
		if preferNewest {
			return -order
		}
		return order
	})

	installations := make([]GoInstallation, len(ranked))
	for index := range ranked {
		installations[index] = ranked[index].installation
	}
	return installations, nil
}

func normalizedGoVersion(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "go") {
		value = "go" + value
	}
	if !version.IsValid(value) {
		return "", fmt.Errorf("%w: %q is not a Go version", ErrNotPinned, strings.TrimPrefix(value, "go"))
	}
	return value, nil
}

func describeRequirement(required string) string {
	if strings.TrimSpace(required) == "" {
		return "any version"
	}
	return "go" + strings.TrimPrefix(strings.TrimSpace(required), "go")
}

func describeCandidates(candidates []GoInstallation) string {
	if len(candidates) == 0 {
		return "no Go installation was found"
	}
	described := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		version := candidate.Version
		if strings.TrimSpace(version) == "" {
			version = "unreadable version"
		}
		described = append(described, fmt.Sprintf("%s (%s)", candidate.Root, version))
	}
	return "found " + strings.Join(described, ", ")
}

// InstallationsOnPath lists every Go installation reachable through a PATH
// value, newest-first order NOT assumed and duplicates removed.
//
// EVERY ENTRY IS CONSIDERED, WHICH IS THE POINT. exec.LookPath stops at the
// first match, and that single behaviour is what made a host with a suitable
// toolchain installed stage an unsuitable one instead: on the #2143 host the
// distro launcher precedes the pinned 1.26 for any process whose PATH is not
// the daemon's.
//
// Symlinks are resolved on the EXECUTABLE, not on the directory, matching what
// staging does: /usr/bin/go points into ../lib/go-1.22/bin/go, and classifying
// the unresolved path would nominate /usr as an installation root.
//
// A candidate whose VERSION cannot be read is returned with an empty Version
// rather than dropped, so the selection error can name it. Reporting a tree
// that exists but could not be read is more useful than reporting that nothing
// was found.
func InstallationsOnPath(pathEnv string) []GoInstallation {
	seen := make(map[string]bool)
	installations := make([]GoInstallation, 0, 4)
	for _, entry := range filepath.SplitList(pathEnv) {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		candidate := filepath.Join(entry, "go")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			candidate = resolved
		}
		root, ok := InstallationRoot(candidate)
		if !ok || seen[root] {
			continue
		}
		seen[root] = true
		version, err := InstallationVersion(root)
		if err != nil {
			version = ""
		}
		installations = append(installations, GoInstallation{Root: root, Version: version})
	}
	return installations
}

const maxGoModBytes int64 = 1 << 20

// ModuleGoDirective reports the `go` directive of the module rooted at dir, or
// "" when there is no safe, valid go.mod to read.
func ModuleGoDirective(dir string) string {
	directive, _ := goDirectiveAt(dir, "go.mod")
	return directive
}

// WorkspaceGoRequirement resolves the minimum Go version that commands started
// in dir must satisfy and the GOWORK value that pins those commands to the same
// workspace decision. GOWORK=off disables workspace discovery; an explicit
// absolute GOWORK names that file; an unset value searches dir and its parents.
//
// Returning an explicit file or "off" removes the staging-to-execution race in
// which a parent go.work appears after selection and silently changes which
// toolchain the pinned seat needs.
func WorkspaceGoRequirement(dir, gowork string) (version, effectiveGOWORK string) {
	module := ModuleGoDirective(dir)
	gowork = strings.TrimSpace(gowork)
	if gowork == "off" {
		return module, "off"
	}
	if gowork != "" {
		if !filepath.IsAbs(gowork) {
			// The go command rejects a relative GOWORK. Preserve it so the seat
			// reports that real configuration error rather than changing it.
			return module, gowork
		}
		workspace, _ := goDirectiveAt(filepath.Dir(gowork), filepath.Base(gowork))
		return laterGoDirective(module, workspace), gowork
	}

	current, err := filepath.Abs(dir)
	if err != nil {
		return module, "off"
	}
	for {
		workspace, present := goDirectiveAt(current, "go.work")
		if present {
			return laterGoDirective(module, workspace), filepath.Join(current, "go.work")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return module, "off"
		}
		current = parent
	}
}

func laterGoDirective(first, second string) string {
	if first == "" {
		return second
	}
	if second == "" {
		return first
	}
	left, leftErr := normalizedGoVersion(first)
	right, rightErr := normalizedGoVersion(second)
	if leftErr != nil || rightErr != nil || version.Compare(left, right) >= 0 {
		return first
	}
	return second
}

// goDirectiveAt safely reads a go.mod or go.work directive. The checkout is not
// sandboxed when this runs. OpenRoot keeps resolution beneath dir; refusing
// links and non-regular files avoids blocking on devices or pipes; the size
// checks bound both ordinary reads and a file that grows after its initial stat.
//
// The `toolchain` line is deliberately ignored. It names a toolchain to
// download, and a seat runs with GOTOOLCHAIN=local precisely so that no
// download happens mid-review.
//
// This is intentionally not a complete parser. Go rejects block comments, so
// an invalid file that hides `go X` inside one may produce an unavailable-
// toolchain diagnostic instead of a syntax diagnostic; no valid workspace can
// be mis-selected by that divergence. present distinguishes an absent go.work
// (continue searching parents) from a present but invalid one (Go would stop).
func goDirectiveAt(dir, name string) (directive string, present bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", false
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if err != nil {
		return "", false
	}
	if !info.Mode().IsRegular() || info.Size() > maxGoModBytes {
		return "", true
	}
	file, err := root.Open(name)
	if err != nil {
		return "", true
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGoModBytes {
		return "", true
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxGoModBytes+1))
	if err != nil || int64(len(contents)) > maxGoModBytes {
		return "", true
	}

	for _, line := range strings.Split(string(contents), "\n") {
		line, _, _ = strings.Cut(line, "//")
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "go" {
			continue
		}
		if len(fields) != 2 {
			return "", true
		}
		if _, err := normalizedGoVersion(fields[1]); err != nil {
			return "", true
		}
		return fields[1], true
	}
	return "", true
}
