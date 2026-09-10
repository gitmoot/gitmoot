package toolchain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// SelectInstallation picks the installation a seat should be staged from.
//
// `required` is the module's go directive, e.g. "1.26" or "go1.26"; an empty
// requirement means any installation will do and the newest is taken.
//
// AMONG SATISFYING CANDIDATES THE LOWEST IS CHOSEN. This is a POLICY CHOICE,
// not a reproduction of Go's rule, and the difference is worth stating because
// an earlier draft of this comment claimed the latter and was wrong.
//
// Go does not rank installations at all. Under GOTOOLCHAIN=auto it uses the
// toolchain it was invoked as whenever that satisfies the module, and
// otherwise fetches the minimum the directive names. Measured on the #2143
// host: invoked as go1.26.4 against this repository's `go 1.26`, it builds
// with 1.26.4 and performs no switch; invoked as go1.22.2 it tries to download
// go1.26 and fails offline. So "what Go would do" is a function of which
// binary you happen to run, which is precisely the input this selection exists
// to stop trusting.
//
// Choosing the lowest satisfying release is therefore this package's own rule,
// picked for determinism: the staged toolchain depends only on the module's
// requirement and the set installed, so a seat does not silently move to a
// newer compiler the moment an unrelated toolchain appears on the host. A
// review is exactly where that drift is least welcome.
//
// ORDER ON PATH IS NOT AUTHORITY. Every candidate is considered, so a launcher
// that happens to be first no longer decides. Candidates whose VERSION cannot
// be read are skipped rather than failing the selection, because an unreadable
// tree is one this package could not have staged anyway; the reason is
// preserved and reported if NOTHING satisfies, so the failure still names what
// was rejected and why.
//
// WITH NO REQUIREMENT THE NEWEST IS TAKEN, not the lowest. The lowest rule
// exists to avoid drifting ABOVE what a module asked for; with nothing asked,
// "lowest" would mean staging the oldest Go on the host, which is the original
// defect wearing different clothes.
func SelectInstallation(candidates []GoInstallation, required string) (GoInstallation, error) {
	want, err := parseGoVersion(required)
	if err != nil {
		return GoInstallation{}, err
	}
	preferNewest := len(want) == 0

	var (
		best      GoInstallation
		bestParts []int
		found     bool
	)
	for _, candidate := range candidates {
		parts, err := parseGoVersion(candidate.Version)
		if err != nil {
			continue
		}
		if compareGoVersion(parts, want) < 0 {
			continue
		}
		if !found {
			best, bestParts, found = candidate, parts, true
			continue
		}
		order := compareGoVersion(parts, bestParts)
		if (preferNewest && order > 0) || (!preferNewest && order < 0) {
			best, bestParts = candidate, parts
		}
	}
	if !found {
		return GoInstallation{}, fmt.Errorf("%w %s: %s",
			ErrNoSatisfyingToolchain, describeRequirement(required), describeCandidates(candidates))
	}
	return best, nil
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

// parseGoVersion turns "go1.26.4", "1.26" or "" into comparable components.
//
// Release suffixes are truncated rather than ordered: "go1.26rc1" parses as
// 1.26, which treats a release candidate as its release. That is deliberate and
// narrow - ordering prereleases correctly would matter only on a host whose
// only satisfying toolchain is an rc, and treating it as satisfying is the
// answer that lets that host work rather than refusing it.
func parseGoVersion(version string) ([]int, error) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "go")
	if version == "" {
		return nil, nil
	}
	parts := strings.Split(version, ".")
	parsed := make([]int, 0, len(parts))
	for _, part := range parts {
		digits := part
		for index, r := range part {
			if r < '0' || r > '9' {
				digits = part[:index]
				break
			}
		}
		if digits == "" {
			if len(parsed) == 0 {
				return nil, fmt.Errorf("%w: %q is not a Go version", ErrNotPinned, version)
			}
			break
		}
		value, err := strconv.Atoi(digits)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a Go version", ErrNotPinned, version)
		}
		parsed = append(parsed, value)
		if digits != part {
			break
		}
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("%w: %q is not a Go version", ErrNotPinned, version)
	}
	return parsed, nil
}

// compareGoVersion orders two parsed versions, treating absent components as
// zero so that 1.26 and 1.26.0 compare equal.
func compareGoVersion(left, right []int) int {
	for index := 0; index < len(left) || index < len(right); index++ {
		var leftPart, rightPart int
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		if leftPart != rightPart {
			if leftPart < rightPart {
				return -1
			}
			return 1
		}
	}
	return 0
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

// ModuleGoDirective reports the `go` directive of the module rooted at dir, or
// "" when there is no go.mod to read.
//
// Parsed directly rather than through golang.org/x/mod, because this runs in
// the daemon on a path the operator controls and the directive is one token on
// one line. An absent or unreadable go.mod is NOT an error: a seat may be
// reviewing a repository that is not a Go module at all, and that must select
// the newest installation rather than refuse.
//
// The `toolchain` line is deliberately ignored. It names a toolchain to
// DOWNLOAD, and a seat runs with GOTOOLCHAIN=local precisely so that no
// download happens mid-review; honouring it would either force a download or
// produce a requirement no local installation can satisfy.
func ModuleGoDirective(dir string) string {
	contents, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "go ") {
			continue
		}
		directive := strings.TrimSpace(strings.TrimPrefix(line, "go "))
		if index := strings.IndexAny(directive, " \t/"); index >= 0 {
			directive = directive[:index]
		}
		if _, err := parseGoVersion(directive); err != nil {
			return ""
		}
		return directive
	}
	return ""
}
