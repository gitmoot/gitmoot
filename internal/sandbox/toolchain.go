package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// toolchainPackageRoots are the immutable/package install roots the pre-#1839
// policy already trusted. Kept as an ANCHOR rather than as the whole rule: on
// its own it missed the pinned toolchain, but dropping it would let a PLANTED
// tree under any writable non-home directory qualify on shape alone. A
// variable so a test can point it at a fixture, since a fixture cannot live
// under a real /opt.
var toolchainPackageRoots = []string{"/opt", "/usr/local", "/nix/store", "/snap"}

// daemonBuildGoroot returns the toolchain this binary was built against. It is
// what makes an operator-pinned install outside every package root grantable -
// the #1839 case - without trusting a path the confined code could itself have
// created. A variable so a test can supply a fixture root.
var daemonBuildGoroot = func() string {
	// runtime.GOROOT() returns the GOROOT ENVIRONMENT VARIABLE when one is
	// set, and only otherwise the build-time value - which is why it is
	// deprecated. MEASURED on one binary built once, varying only the
	// environment: with no GOROOT it reports the pinned toolchain; with
	// GOROOT=/tmp/evil-goroot it reports /tmp/evil-goroot. The SAME compiled
	// binary names an attacker-supplied anchor, so an anchor taken from it
	// straight would have precisely the environment-controlled property the
	// anchor exists to avoid.
	//
	// An environment-supplied GOROOT therefore FAILS CLOSED: the anchor is
	// withheld, the grant falls back to the package roots, and a seat that
	// consequently cannot reach its toolchain gets a named diagnostic rather
	// than silence. An EMPTY value is treated as absent, matching the runtime.
	if value, ok := os.LookupEnv("GOROOT"); ok && strings.TrimSpace(value) != "" {
		return ""
	}
	return runtime.GOROOT()
}

// goToolchainHomeRoots are directories that must never be granted as a
// toolchain however they are shaped: somebody's home. A variable so a test can
// point it at a fixture, because a fixture cannot live under the real /home.
var goToolchainHomeRoots = []string{"/root", "/home"}

// optionalGoToolchainRoot returns the GOROOT that owns executable, or "" when
// executable is not a Go toolchain the seat may read.
//
// #1839: the original form allowlisted four LOCATIONS - /opt, /usr/local,
// /nix/store, /snap - so a toolchain installed anywhere else was silently not
// granted. This box pins Go at /root/.local/toolchains/go1.26.4, which matches
// none of them, so every read-only review seat got EACCES on execve of the
// pinned `go` and could not run the repository gate at all.
//
// Three independent conditions, and the third is the load-bearing one:
//
//   - SHAPED LIKE A REAL GOROOT: bin/go is a regular file AND EXECUTABLE (a
//     readable file named go is not a toolchain, and the executable bit is
//     what the grant exists to serve), src/runtime exists, and VERSION parses
//     as a Go release. Measured on this box: both real toolchains carry a
//     parseable VERSION and a real GOPATH (/root/go) does not.
//   - NOT A HOME: /root, a direct child of /home, or the caller's own
//     os.UserHomeDir. Unconditional, and checked before the shape, because a
//     home made to look like a GOROOT is the adversarial case rather than an
//     accident.
//   - ANCHORED to a root the confined code cannot write: see
//     anchoredToolchainRoot. An intermediate form of this fix trusted any root
//     whose basename began with "go", which would have granted a shaped
//     /home/gordon or /root/gopath, and shape alone would have granted a tree
//     planted under /tmp.
//
// Deliberately NOT DONE: resolving the toolchain by executing `go env GOROOT`.
// That is more accurate and would also follow a GOTOOLCHAIN switch, but it
// runs a PATH-resolved, user-controlled binary with daemon privilege BEFORE
// confinement - trading a read grant for arbitrary execution. Every check here
// is a filesystem read.
//
// The grant is READ-ONLY and is the root itself, never its parent.
func GoToolchainRoot(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	binDir := filepath.Dir(filepath.Clean(executable))
	if base := filepath.Base(binDir); base != "bin" && base != "sbin" {
		return ""
	}
	root := filepath.Dir(binDir)
	if isHomeRoot(root) {
		return ""
	}
	info, err := os.Stat(filepath.Join(root, "bin", "go"))
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "src", "runtime")); err != nil {
		return ""
	}
	if GoToolchainRelease(root) == "" {
		return ""
	}
	if !anchoredToolchainRoot(root) {
		return ""
	}
	return root
}

// anchoredToolchainRoot reports whether root descends from an anchor that the
// confined code cannot have created: the toolchain the daemon itself was built
// against - the same authority that installed the running binary - or a
// package install root.
func anchoredToolchainRoot(root string) bool {
	anchors := append([]string{}, toolchainPackageRoots...)
	if own := strings.TrimSpace(daemonBuildGoroot()); own != "" {
		anchors = append(anchors, own)
	}
	for _, anchor := range anchors {
		anchor = filepath.Clean(anchor)
		if anchor == "" || anchor == "." || anchor == string(filepath.Separator) {
			continue
		}
		if root == anchor {
			return true
		}
		rel, err := filepath.Rel(anchor, root)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// isHomeRoot reports whether root is a home directory that must never be
// granted as a toolchain.
func isHomeRoot(root string) bool {
	root = filepath.Clean(root)
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == root {
		return true
	}
	for _, homeRoot := range goToolchainHomeRoots {
		homeRoot = filepath.Clean(homeRoot)
		if root == homeRoot || filepath.Dir(root) == homeRoot {
			return true
		}
	}
	return false
}

// goToolchainVersion returns the release a GOROOT declares in its VERSION file
// ("go1.26.4"), or "" when the file is absent or does not name a Go release. A
// real toolchain always ships it; a planted tree does not get it for free.
func GoToolchainRelease(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if !strings.HasPrefix(line, "go") {
		return ""
	}
	rest := line[len("go"):]
	if rest == "" || rest[0] < '0' || rest[0] > '9' {
		return ""
	}
	return line
}

// ResolveGoToolchainFromPATH returns the toolchain root and release that a
// process would reach through PATH, or "" for both when PATH's `go` is not a
// grantable toolchain.
//
// It resolves the SAME way the confined child will, so a grant and the
// execution it exists to serve cannot disagree. It never executes the binary:
// see GoToolchainRoot for why running `go env GOROOT` before confinement was
// rejected.
func ResolveGoToolchainFromPATH() (root string, release string) {
	binary, err := exec.LookPath("go")
	if err != nil {
		return "", ""
	}
	root = GoToolchainRoot(binary)
	if root == "" {
		return "", ""
	}
	return root, GoToolchainRelease(root)
}

// GoToolchainSatisfies reports whether a toolchain release ("go1.26.4") meets a
// module's `go` directive ("1.26"), plus the parsed pair for a diagnostic.
//
// #1839 P2-4: a seat can be granted a PERFECTLY VALID toolchain that cannot
// build the repository - measured on this box, /usr/bin/go resolves to
// go1.22.2 while go.mod requires 1.26, and GOTOOLCHAIN=auto cannot rescue it
// ("download go1.26 for linux/amd64: toolchain not available"). Granting reads
// for that toolchain is correct and still leaves the seat unable to run the
// gate, so the mismatch has to be NAMEABLE rather than surfacing as a silent
// downgrade to a static-only review.
func GoToolchainSatisfies(release string, directive string) (ok bool, have [2]int, want [2]int) {
	have, haveOK := parseGoMinor(strings.TrimPrefix(strings.TrimSpace(release), "go"))
	want, wantOK := parseGoMinor(strings.TrimSpace(directive))
	if !haveOK || !wantOK {
		// Unparseable on either side is not a mismatch claim: an unknown
		// version must not manufacture a diagnostic about a working seat.
		return true, have, want
	}
	if have[0] != want[0] {
		return have[0] > want[0], have, want
	}
	return have[1] >= want[1], have, want
}

// parseGoMinor parses the major and minor of a Go version ("1.26", "1.26.4").
func parseGoMinor(value string) ([2]int, bool) {
	fields := strings.SplitN(value, ".", 3)
	if len(fields) < 2 {
		return [2]int{}, false
	}
	var out [2]int
	for i := 0; i < 2; i++ {
		n := 0
		if fields[i] == "" {
			return [2]int{}, false
		}
		for _, c := range fields[i] {
			if c < '0' || c > '9' {
				return [2]int{}, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// GoDirectiveForModule returns the `go` directive of the module rooted at dir
// ("1.26"), or "" when there is no go.mod or it names no directive.
func GoDirectiveForModule(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "go ")
		if !ok {
			continue
		}
		return strings.TrimSpace(rest)
	}
	return ""
}
