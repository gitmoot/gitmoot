package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
func RuntimeRoot(gitmootHome string) string {
	return filepath.Join(Root(gitmootHome), RuntimeDirName)
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
// EVERY read and write goes through an os.Root, so a symlink or ".." inside the
// source cannot address anything outside the boundary, and no name derived from
// the source can address anything outside the daemon's own directory.
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
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %q is not a regular file", ErrRuntimeNotStageable, resolved)
	}

	boundary, relative, packaged := runtimeBoundary(resolved)
	fingerprint, err := fingerprintRuntime(boundary, relative, packaged)
	if err != nil {
		return "", err
	}

	root := RuntimeRoot(gitmootHome)
	published := filepath.Join(root, name+"-"+fingerprint)
	entry, _ := runtimeStagers.LoadOrStore(published, &sync.Mutex{})
	lock := entry.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if err := publishRuntime(root, name+"-"+fingerprint, name, boundary, relative, packaged, fingerprint); err != nil {
		return "", err
	}
	return filepath.Join(published, ".bin", name), nil
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
	publishedName := name + "-unavailable-v1"
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

// stagedTreeMember decides whether a package-tree member is part of the runtime
// or part of the operator's private state.
//
// A PACKAGE MARKER MUST NOT LAUNDER A SECRET (#1921 contract item 4). Round 3's
// probe planted node_modules/operator-profile/{package.json,bin/kimi,config.toml}
// and read the mode-0600 config. Deleting the recursive GRANT does not close that
// by itself: copying the same file into an engine tree the seat may read exposes
// exactly the same bytes, one indirection later.
//
// So a member that denies group AND other read - the shape of every credential
// and private config measured here, against 0644 package payloads - is neither
// hashed nor copied. The resolved ENTRYPOINT is always included, because a
// runtime installed mode 0700 must still run. Excluding too much costs a visible
// launch failure, never a silent exposure, which is the direction to fail in.
func stagedTreeMember(name string, relative string, perm fs.FileMode) bool {
	if name == relative {
		return true
	}
	return perm&0o044 != 0
}

// fingerprintRuntime identifies the boundary by CONTENT so a runtime upgrade
// publishes a new tree instead of silently reusing the previous one.
//
// A single file hashes its own bytes. A package tree hashes every INCLUDED
// regular file's path and contents: package trees here are small (codex ships
// under 40 MiB), unlike the 269 MiB Go toolchain whose full hash the Go path
// deliberately avoids.
func fingerprintRuntime(boundary string, relative string, packaged bool) (string, error) {
	root, err := os.OpenRoot(boundary)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	defer root.Close()

	digest := sha256.New()
	if !packaged {
		if err := hashRuntimeFile(root, relative, digest); err != nil {
			return "", err
		}
		return hex.EncodeToString(digest.Sum(nil))[:16], nil
	}
	// ONE PRIVACY RULE, APPLIED IN BOTH PLACES. The first version of the
	// ancestor rule lived only in the copier, so any tree containing a private
	// subtree produced a fingerprint over files the copy would never take - and
	// mechanism 3's own digest comparison then failed the stage. That mismatch
	// was the bug's tell: if the name and the bytes are computed by different
	// rules, one of them is describing a tree that does not exist.
	private := map[string]bool{}
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if private[filepath.Dir(name)] && name != relative {
			if entry.IsDir() {
				private[name] = true
			}
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entry.IsDir() {
			if info.Mode().Perm()&0o044 == 0 {
				private[name] = true
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			// A symlink inside the package is neither hashed nor copied. Copying
			// it could point outside the boundary, and following it would import
			// exactly the escape this whole change removes.
			return nil
		}
		// Hashed exactly when copied, so identity describes the tree the seat
		// actually gets rather than one that includes files it never receives.
		if !stagedTreeMember(name, relative, info.Mode().Perm()) {
			return nil
		}
		return hashRuntimeFile(root, name, digest)
	}); err != nil {
		return "", fmt.Errorf("%w: fingerprint %q: %v", ErrRuntimeNotStageable, boundary, err)
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

func hashRuntimeFile(root *os.Root, name string, digest io.Writer) error {
	handle, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	defer handle.Close()
	// The NAME is hashed with the bytes so two trees differing only in layout
	// cannot collide.
	if _, err := io.WriteString(digest, name+"\x00"); err != nil {
		return err
	}
	if _, err := io.Copy(digest, handle); err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	return nil
}

// publishRuntime copies the boundary into the daemon root and publishes it by
// rename, reusing an existing copy only when the published name already exists.
//
// Identity is by content, so an existing name is a tree whose fingerprint already
// matched; a partially copied tree never acquires the published name, because the
// rename is the last step.
func publishRuntime(root string, name string, runtimeName string, boundary string, relative string, packaged bool, fingerprint string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	destinationRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()

	if info, err := destinationRoot.Lstat(name); err == nil {
		// Lstat, never Stat: a symlink planted at the published name must not be
		// mistaken for a staged copy. An existing content-addressed directory is
		// reusable only while both its copied artifact and shim remain intact.
		if !info.IsDir() {
			return fmt.Errorf("%w: published name %q is not a directory", ErrRuntimeNotStageable, name)
		}
		if err := validatePublishedRuntime(destinationRoot, name, runtimeName, relative, fingerprint, packaged); err != nil {
			return err
		}
		return nil
	}

	if err := checkFreeSpace(root); err != nil {
		return err
	}

	staging := name + ".staging-" + randomSuffix()
	if err := destinationRoot.Mkdir(staging, 0o700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(filepath.Join(root, staging))
		}
	}()

	sourceRoot, err := os.OpenRoot(boundary)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	defer sourceRoot.Close()

	// THE DIGEST COMES FROM THE COPY PATH, and the published name is only
	// claimed if it matches the fingerprint the source produced (bridge F3).
	// Staging reads the source TWICE - once to fingerprint, once to copy - so
	// without this the name can describe a tree that was never published.
	var copied string
	if packaged {
		copied, err = copyRuntimeTree(sourceRoot, destinationRoot, staging, relative)
		if err != nil {
			return err
		}
	} else {
		digest := sha256.New()
		if err := copyRuntimeFile(sourceRoot, relative, destinationRoot, filepath.Join(staging, relative), relative, digest); err != nil {
			return err
		}
		copied = hex.EncodeToString(digest.Sum(nil))[:16]
	}
	if copied != fingerprint {
		return fmt.Errorf("%w: staged content digest %q does not match the fingerprint %q the source produced; the source changed between the fingerprint and the copy", ErrRuntimeNotStageable, copied, fingerprint)
	}

	// RECORD THE ENTRYPOINT DIGEST INSIDE THE ENGINE TREE so reuse can compare
	// like with like for a package boundary, whose full tree is too expensive to
	// re-hash on every launch.
	entrypointDigest := sha256.New()
	entrypointHandle, err := destinationRoot.OpenFile(filepath.Join(staging, relative), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%w: reopen staged entrypoint: %v", ErrRuntimeNotStageable, err)
	}
	if _, err := io.WriteString(entrypointDigest, relative+"\x00"); err != nil {
		_ = entrypointHandle.Close()
		return err
	}
	if _, err := io.Copy(entrypointDigest, entrypointHandle); err != nil {
		_ = entrypointHandle.Close()
		return fmt.Errorf("%w: rehash staged entrypoint: %v", ErrRuntimeNotStageable, err)
	}
	if err := entrypointHandle.Close(); err != nil {
		return err
	}
	digestFile, err := destinationRoot.OpenFile(filepath.Join(staging, entrypointDigestName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(digestFile, hex.EncodeToString(entrypointDigest.Sum(nil))[:16]+"\n"); err != nil {
		_ = digestFile.Close()
		return err
	}
	if err := digestFile.Close(); err != nil {
		return err
	}

	// The caller executes the runtime by its configured name, but resolved
	// artifacts do not necessarily carry that name: claude's target is a
	// version number and codex's is codex.js. Publish a fingerprint-local shim,
	// not a stable global one, so a concurrent upgrade cannot retarget an
	// already-running seat to a tree its Landlock rules never granted.
	shimDir := filepath.Join(staging, ".bin")
	if err := destinationRoot.Mkdir(shimDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	shimTarget, err := filepath.Rel(".bin", relative)
	if err != nil || strings.HasPrefix(shimTarget, ".."+string(filepath.Separator)+"..") {
		return fmt.Errorf("%w: executable %q cannot be addressed from its staged shim", ErrRuntimeNotStageable, relative)
	}
	if err := destinationRoot.Symlink(shimTarget, filepath.Join(shimDir, runtimeName)); err != nil {
		return fmt.Errorf("%w: create runtime shim: %v", ErrRuntimeNotStageable, err)
	}

	if err := destinationRoot.Rename(staging, name); err != nil {
		if _, statErr := destinationRoot.Lstat(name); statErr == nil {
			// A concurrent publisher won. Its tree proved the same fingerprint,
			// so the loser discards its own copy rather than failing the launch.
			return nil
		}
		return err
	}
	cleanup = false
	return nil
}

// validatePublishedRuntime re-proves an existing published tree before it is
// reused. It verifies CONTENT, not merely shape (bridge F3): a tree whose
// entrypoint bytes changed after publication no longer matches the name it was
// published under, and reusing it would serve a copy nothing ever fingerprinted.
func validatePublishedRuntime(root *os.Root, published string, runtimeName string, relative string, fingerprint string, packaged bool) error {
	artifact := filepath.Join(published, relative)
	info, err := root.Lstat(artifact)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: published runtime artifact %q is missing or not regular", ErrRuntimeNotStageable, artifact)
	}
	shim := filepath.Join(published, ".bin", runtimeName)
	info, err = root.Lstat(shim)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%w: published runtime shim %q is missing or not a symlink", ErrRuntimeNotStageable, shim)
	}
	target, err := root.Readlink(shim)
	if err != nil {
		return fmt.Errorf("%w: read published runtime shim: %v", ErrRuntimeNotStageable, err)
	}
	want, err := filepath.Rel(".bin", relative)
	if err != nil || target != want {
		return fmt.Errorf("%w: published runtime shim %q targets %q, want %q", ErrRuntimeNotStageable, shim, target, want)
	}
	// A SINGLE-FILE RUNTIME IS RE-HASHED IN FULL. A package tree is not, because
	// re-hashing every member on every launch costs more than the copy; its
	// ENTRYPOINT is the file that runs, so that is the one re-proven. A torn
	// non-entrypoint member surfaces as a visible launch failure, never as a
	// silently wrong binary - the same trade the Go toolchain path documents.
	entrypointDigest := sha256.New()
	handle, err := root.OpenFile(artifact, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%w: reopen published artifact %q: %v", ErrRuntimeNotStageable, artifact, err)
	}
	defer handle.Close()
	if _, err := io.WriteString(entrypointDigest, relative+"\x00"); err != nil {
		return err
	}
	if _, err := io.Copy(entrypointDigest, handle); err != nil {
		return fmt.Errorf("%w: reread published artifact %q: %v", ErrRuntimeNotStageable, artifact, err)
	}
	got := hex.EncodeToString(entrypointDigest.Sum(nil))[:16]
	if !packaged && got != fingerprint {
		return fmt.Errorf("%w: published artifact %q hashes to %q, not the %q it was published under", ErrRuntimeNotStageable, artifact, got, fingerprint)
	}
	if packaged {
		// For a tree, the entrypoint digest is recorded beside the copy at
		// publish time so reuse can compare like with like.
		expected, readErr := publishedEntrypointDigest(root, published)
		if readErr != nil {
			return readErr
		}
		if expected != got {
			return fmt.Errorf("%w: published entrypoint %q hashes to %q, recorded %q", ErrRuntimeNotStageable, artifact, got, expected)
		}
	}
	return nil
}

// entrypointDigestName holds the entrypoint digest recorded at publish time. It
// lives inside the engine-owned tree, which no seat can write.
const entrypointDigestName = ".entrypoint-digest"

func publishedEntrypointDigest(root *os.Root, published string) (string, error) {
	handle, err := root.OpenFile(filepath.Join(published, entrypointDigestName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("%w: published tree %q records no entrypoint digest", ErrRuntimeNotStageable, published)
	}
	defer handle.Close()
	recorded, err := io.ReadAll(io.LimitReader(handle, 64))
	if err != nil {
		return "", fmt.Errorf("%w: read recorded entrypoint digest: %v", ErrRuntimeNotStageable, err)
	}
	return strings.TrimSpace(string(recorded)), nil
}

// copyRuntimeTree copies a package boundary and returns the digest of what it
// ACTUALLY wrote, so the caller can prove the published name describes the
// published bytes (#1921 bridge F3).
//
// EVERY DECISION COMES FROM AN OPEN DESCRIPTOR, NOT FROM READDIR (bridge F2).
// The previous form took entry.Info() from the directory listing and then opened
// the member by NAME, so the name could be replaced between the two - and an
// in-root symlink pointing at a private sibling was followed, because os.Root
// refuses escapes OUT of the root, never redirection WITHIN it. Opening first
// and testing the descriptor removes the window rather than narrowing it.
//
// ANCESTOR PRIVACY DECIDES A SUBTREE (bridge F1). A leaf's own permission bits
// cannot tell you it lives in credentials/: a 0700 directory holding a 0644
// token was copied because only the token was consulted. A directory that denies
// group AND other read marks its whole subtree operator-private and is skipped.
func copyRuntimeTree(sourceRoot *os.Root, destinationRoot *os.Root, destination string, relative string) (string, error) {
	digest := sha256.New()
	private := map[string]bool{}
	err := fs.WalkDir(sourceRoot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		// A member under a private ancestor is the operator's, whatever its own
		// mode says. The entrypoint is the sole exemption: a runtime installed
		// mode 0700 must still run, and then only that file is taken.
		if private[filepath.Dir(name)] && name != relative {
			if entry.IsDir() {
				private[name] = true
			}
			return nil
		}
		target := filepath.Join(destination, name)
		if entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			if info.Mode().Perm()&0o044 == 0 {
				private[name] = true
				// The directory itself is still created only if something
				// inside it is later taken; skipping it entirely keeps the
				// staged tree free of empty private shells.
				return nil
			}
			if err := destinationRoot.Mkdir(target, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			return nil
		}
		// Sockets, devices and symlinks never reach copyRuntimeFile: it opens
		// with O_NOFOLLOW and refuses anything that is not a regular file on the
		// descriptor it holds.
		return copyRuntimeFile(sourceRoot, name, destinationRoot, target, relative, digest)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// copyRuntimeFile opens the source member, decides from the OPEN descriptor, and
// hashes the same bytes it writes.
//
// O_NOFOLLOW is the load-bearing flag: it makes a symlink at this name an error
// rather than a redirection, which is what closes bridge F2's substitution
// window. A non-regular file is skipped rather than failing the whole stage,
// because a package tree may legitimately contain a socket or a fifo.
func copyRuntimeFile(sourceRoot *os.Root, name string, destinationRoot *os.Root, target string, relative string, digest io.Writer) error {
	// TWO LAYERS, deliberately, because each catches what the other cannot.
	// Lstat identifies a symlink WITHOUT following it, so an in-root link to a
	// private sibling and a link that escapes the root are both skipped cleanly
	// rather than surfacing as an error. O_NOFOLLOW below is the AUTHORITATIVE
	// check: if the name is swapped after this Lstat, the open fails instead of
	// following the substitute (bridge F2).
	if info, lstatErr := sourceRoot.Lstat(name); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	handle, err := sourceRoot.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			// The name became a symlink between the Lstat and the open. Skipped,
			// not followed: the substitution window is closed, not merely small.
			return nil
		}
		return fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeNotStageable, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	// THE PRIVACY TEST NOW READS THE DESCRIPTOR, so it describes the file being
	// copied rather than the name that was listed earlier.
	if !stagedTreeMember(name, relative, info.Mode().Perm()) {
		return nil
	}
	// EXECUTE BITS ARE PRESERVED because the copy must be runnable; write bits
	// are not, so the staged tree is read-only to the seat that uses it.
	mode := fs.FileMode(0o500)
	if info.Mode().Perm()&0o111 == 0 {
		mode = 0o400
	}
	if parent := filepath.Dir(target); parent != "." {
		if err := destinationRoot.MkdirAll(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	written, err := destinationRoot.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer written.Close()
	// The NAME is hashed with the bytes, and the digest is taken from the copy
	// path itself, so the fingerprint describes what was written.
	if _, err := io.WriteString(digest, name+"\x00"); err != nil {
		return err
	}
	if _, err := io.Copy(io.MultiWriter(written, digest), handle); err != nil {
		return err
	}
	return written.Close()
}
