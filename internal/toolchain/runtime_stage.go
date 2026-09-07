package toolchain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// RuntimeDirName holds staged runtime copies beside the staged Go toolchains.
const RuntimeDirName = "runtimes"

// ErrRuntimeNotStageable reports a runtime whose boundary cannot be resolved or
// copied. It is separate from ErrNotPinned because the Go path's "not a pinned
// installation" is a normal, silent case, whereas a runtime that cannot be staged
// is an availability failure a seat must be told about.
var ErrRuntimeNotStageable = errors.New("runtime cannot be staged")

// runtimeStagers serialises materialisation per published path. It caches no
// answer: every reuse revalidates the copied entrypoint and its shim, so a
// damaged command is not served for the daemon's lifetime.
var runtimeStagers sync.Map // published path -> *sync.Mutex

// RuntimeRoot returns the daemon-owned directory holding staged runtime copies.
//
// IT IS A SIBLING OF THE TOOLCHAIN ROOT, NOT A CHILD OF IT, and that placement
// is load-bearing. Collect() removes every entry under Root() that is not a
// PINNED TOOLCHAIN IDENTITY, so a runtimes/ directory living there was deleted
// out from under staging: measured as "mkdirat codex-….staging-…: no such file
// or directory" and a deterministic failure of
// TestTrackedPoolIsolationHonorsSamePassRuntimeSibling - 3/3 fail with the
// nested placement, 3/3 pass at base. Runtime copies are not toolchain versions
// and must not be judged by a toolchain retention list.
func RuntimeRoot(gitmootHome string) string {
	return filepath.Join(gitmootHome, RuntimeDirName)
}

// StageRuntime materialises a daemon-owned copy of ONE runtime's executable and,
// when the runtime is packaged, the package boundary it cannot run without. It
// returns the absolute path of the staged executable.
//
// WHY THIS EXISTS (#1918 / #1921 review, ruling 122157). Three rounds tried to
// make a recursive Landlock grant over the OPERATOR's install root safe by
// inspecting that root, and each round was broken by measurement: a credential
// filename list can never be complete, and an Lstat scan is a time-of-check
// decision guarding a recursive grant, so a credential created afterwards lands
// inside an already-granted tree. Markers prove SHAPE, never IMMUTABILITY.
//
// So the grant is not narrowed, it is REMOVED: the engine copies what the runtime
// needs into a tree it owns, and grants that instead. A tree the daemon created
// has no operator-writable descendants to appear later, which is why this closes
// the class rather than the two instances.
//
// THE BOUNDARY IS THE RESOLVED ARTIFACT, NEVER A PROFILE. For a self-contained
// executable the boundary is the file itself; the operator's profile directory
// around it is not copied and not granted, which is precisely the kimi case that
// exposed ~/.kimi-code/credentials. For a node-packaged runtime the boundary is
// the package directory, because codex resolves to
// <node>/lib/node_modules/@openai/codex/bin/codex.js and cannot run without the
// files beside its bin dir.
//
// EVERY read goes through openat2 with RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH, so
// no component of any source path can be a symlink and nothing resolves outside
// the boundary. The digest and the copy read the SAME descriptors, so the
// published name describes the published bytes by construction.
func StageRuntime(gitmootHome string, name string, executable string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q is not usable as one path component", ErrRuntimeNotStageable, name)
	}
	executable = strings.TrimSpace(executable)
	if executable == "" || !filepath.IsAbs(executable) {
		return "", fmt.Errorf("%w: executable %q must be an absolute path", ErrRuntimeNotStageable, executable)
	}
	// The RESOLVED target is what runs, and it is what must be copied: a PATH
	// entry is frequently a symlink into a versioned directory (claude ships
	// ~/.local/bin/claude pointing at ~/.local/share/claude/versions/<v>), so
	// copying the link would stage something unrunnable.
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %v", ErrRuntimeNotStageable, executable, err)
	}
	boundary, relative, packaged := runtimeBoundary(resolved)
	source, err := openStageSource(boundary, relative, packaged)
	if err != nil {
		return "", err
	}
	defer source.close()

	fingerprint, err := source.digest()
	if err != nil {
		return "", err
	}

	// THE INTERPRETER IS PART OF THE IDENTITY. A script runtime that cannot run
	// without node is a different artifact when node changes, so the staged
	// interpreter's own published directory folds into this fingerprint - a
	// stale launcher can never point at a retired interpreter tree.
	interpreter, err := source.stageInterpreter(gitmootHome)
	if err != nil {
		return "", err
	}
	if interpreter != "" {
		fingerprint = shortDigest(fingerprint + "\x00" + interpreter)
	}

	root := RuntimeRoot(gitmootHome)
	publishedName := name + "-" + fingerprint
	published := filepath.Join(root, publishedName)
	launcher := filepath.Join(published, launcherDirName, name)
	entry, _ := runtimeStagers.LoadOrStore(published, &sync.Mutex{})
	lock := entry.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if _, err := os.Lstat(published); err == nil {
		if err := validatePublished(published, name, relative, fingerprint); err != nil {
			return "", err
		}
		return launcher, nil
	}
	if err := source.publish(root, publishedName, name, relative, interpreter, fingerprint); err != nil {
		return "", err
	}
	return launcher, nil
}

// RuntimeInterpreter reports the interpreter a staged entrypoint needs, and the
// resolved absolute path to stage for it.
//
// WHY THIS EXISTS, and it is the hole a launch probe cannot see from outside
// Landlock: codex's resolved artifact is
// <node>/lib/node_modules/@openai/codex/bin/codex.js, whose first line is
// "#!/usr/bin/env node". Staging that file copies the SCRIPT and nothing that
// can run it. The interpreter on this host resolves to
// /root/.nvm/versions/node/<v>/bin/node - inside the operator's HOME, which is
// exactly the root this whole change refuses to grant. So the seat would either
// read an ungranted operator path or fail to exec at all: the #1918 symptom
// wearing the #1921 exposure.
//
// It returns ok=false for a binary (claude and kimi are ELF here) and for a
// shebang it cannot resolve, because copying less can only fail closed.
func RuntimeInterpreter(executable string) (name string, resolved string, ok bool) {
	handle, err := os.Open(executable)
	if err != nil {
		return "", "", false
	}
	defer handle.Close()
	header := make([]byte, 256)
	read, err := handle.Read(header)
	if err != nil && read == 0 {
		return "", "", false
	}
	header = header[:read]
	if !strings.HasPrefix(string(header), "#!") {
		return "", "", false
	}
	line := string(header[2:])
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", "", false
	}
	candidate := fields[0]
	// "#!/usr/bin/env node" names the LOOKUP TOOL, not the interpreter. The
	// argument is what has to exist on the seat's PATH, so that is what is
	// staged; /usr/bin/env itself is a system binary already inside the fixed
	// read set.
	if filepath.Base(candidate) == "env" {
		if len(fields) < 2 {
			// "#!/usr/bin/env" with no argument names no interpreter at all, so
			// there is nothing to stage and nothing that could run this file.
			return "", "", true
		}
		candidate = fields[1]
	}
	target, err := exec.LookPath(candidate)
	if err != nil {
		// REQUIRED BUT UNRESOLVABLE, and the distinction is the whole of bridge
		// F4. Returning "not needed" here would stage a script that cannot run
		// and let the seat discover it as an opaque exec failure - or worse,
		// resolve the interpreter from the inherited PATH later. The empty path
		// makes StageRuntime refuse, and the caller republishes the runtime as
		// the exit-126 command.
		return filepath.Base(candidate), "", true
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", "", false
	}
	base := filepath.Base(target)
	if resolvedTarget, linkErr := filepath.EvalSymlinks(target); linkErr == nil {
		target = resolvedTarget
	}
	// The NAME is the one the shebang will look up, never the resolved file's
	// name: a versioned target (node -> node22) must still answer to "node".
	if filepath.Base(candidate) != "" && !strings.ContainsAny(candidate, `/\`) {
		base = candidate
	}
	// A SYSTEM INTERPRETER IS ALREADY REACHABLE AND MUST NOT BE COPIED. The
	// sandbox grants the fixed system roots unconditionally, so /bin/sh needs no
	// staging - and staging it would put an engine copy of the shell ahead of
	// the real one on the seat's PATH for no gain. Measured: without this,
	// every "#!/bin/sh" runtime fixture staged /bin/sh as a runtime command.
	//
	// Only an interpreter OUTSIDE those roots is the codex case - node in the
	// operator's home - which is the one that must be copied.
	if systemInterpreterRoots(target) {
		return "", "", false
	}
	return base, target, true
}

// systemInterpreterRoots reports whether an interpreter already lives under a
// root the sandbox grants unconditionally. The list mirrors internal/sandbox's
// fixed read set; a path not on it is treated as operator-owned, which is the
// direction that fails closed (an unnecessary copy, never a missing grant).
func systemInterpreterRoots(target string) bool {
	for _, root := range []string{"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/libexec/", "/usr/lib/", "/usr/lib64/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(target, root) {
			return true
		}
	}
	return false
}

// UnavailableRuntimeSuffix names the published root StageUnavailableRuntime
// writes for a runtime the daemon could not stage. It is exported so a caller
// can RECOGNISE that shim in a staging result instead of re-deriving the
// literal: a dispatch that hands a seat this command has already decided the
// seat cannot execute that runtime, and #1817 needs to refuse on exactly that
// fact rather than wait for the exit-126 exec.
const UnavailableRuntimeSuffix = "-unavailable-v1"

// StageUnavailableRuntime publishes an engine-owned command that fails with an
// explicit availability error. It shadows a host runtime name when the daemon
// cannot stage that runtime, so appending the inherited PATH cannot silently
// route the seat back through an ungranted operator installation.
func StageUnavailableRuntime(gitmootHome string, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q is not usable as one path component", ErrRuntimeNotStageable, name)
	}
	root := RuntimeRoot(gitmootHome)
	publishedName := name + UnavailableRuntimeSuffix
	published := filepath.Join(root, publishedName)
	entry, _ := runtimeStagers.LoadOrStore(published, &sync.Mutex{})
	lock := entry.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	destinationRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer destinationRoot.Close()
	shim := filepath.Join(publishedName, ".bin", name)
	if info, statErr := destinationRoot.Lstat(shim); statErr == nil {
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return filepath.Join(published, ".bin", name), nil
		}
		return "", fmt.Errorf("%w: unavailable runtime command %q is not executable and regular", ErrRuntimeNotStageable, shim)
	}
	if info, statErr := destinationRoot.Lstat(publishedName); statErr == nil {
		return "", fmt.Errorf("%w: unavailable runtime root %q is incomplete (%s)", ErrRuntimeNotStageable, publishedName, info.Mode())
	}

	staging := publishedName + ".staging-" + randomSuffix()
	if err := destinationRoot.Mkdir(staging, 0o700); err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(filepath.Join(root, staging))
		}
	}()
	if err := destinationRoot.Mkdir(filepath.Join(staging, ".bin"), 0o700); err != nil {
		return "", err
	}
	command := filepath.Join(staging, ".bin", name)
	handle, err := destinationRoot.OpenFile(command, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		return "", err
	}
	const script = "#!/bin/sh\nprintf '%s\\n' 'gitmoot: runtime unavailable: no daemon-staged artifact exists' >&2\nexit 126\n"
	if _, err := io.WriteString(handle, script); err != nil {
		_ = handle.Close()
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	if err := destinationRoot.Rename(staging, publishedName); err != nil {
		if info, statErr := destinationRoot.Lstat(shim); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return filepath.Join(published, ".bin", name), nil
		}
		return "", err
	}
	cleanup = false
	return filepath.Join(published, ".bin", name), nil
}

// StagedRuntimeRoot returns the engine-owned root that must be granted for a
// shim returned by StageRuntime. The caller supplies the same resolved gitmoot
// home used for staging; paths outside its runtime root are refused.
func StagedRuntimeRoot(gitmootHome string, shim string) (string, error) {
	shim = filepath.Clean(shim)
	bin := filepath.Dir(shim)
	if filepath.Base(bin) != ".bin" {
		return "", fmt.Errorf("%w: runtime shim %q is not inside a .bin directory", ErrRuntimeNotStageable, shim)
	}
	root := filepath.Dir(bin)
	relative, err := filepath.Rel(RuntimeRoot(gitmootHome), root)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: runtime shim %q is outside engine runtime root %q", ErrRuntimeNotStageable, shim, RuntimeRoot(gitmootHome))
	}
	return root, nil
}

// runtimeBoundary decides what must be copied for a resolved executable.
//
// A node package root is identified POSITIVELY, by a regular package.json inside
// a node_modules path element. That is not a security boundary here - nothing is
// granted based on it - it only decides how much to COPY, so a wrong answer costs
// a launch failure rather than an exposure. Anything unrecognised stages as a
// single file, which is the containing case: copying less can only fail closed.
func runtimeBoundary(resolved string) (boundary string, relative string, packaged bool) {
	dir := filepath.Dir(resolved)
	if base := filepath.Base(dir); base == "bin" || base == "sbin" {
		candidate := filepath.Dir(dir)
		if isNodePackageRoot(candidate) {
			if rel, err := filepath.Rel(candidate, resolved); err == nil && !strings.HasPrefix(rel, "..") {
				return candidate, rel, true
			}
		}
	}
	return dir, filepath.Base(resolved), false
}

func isNodePackageRoot(candidate string) bool {
	candidate = filepath.Clean(candidate)
	if !hasPathElement(candidate, "node_modules") {
		return false
	}
	info, err := os.Lstat(filepath.Join(candidate, "package.json"))
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// hasPathElement matches a WHOLE path element, so /opt/node_modules_backup/x does
// not match node_modules.
func hasPathElement(path string, name string) bool {
	for _, element := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if element == name {
			return true
		}
	}
	return false
}
