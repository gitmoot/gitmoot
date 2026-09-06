package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

	if err := publishRuntime(root, name+"-"+fingerprint, name, boundary, relative, packaged); err != nil {
		return "", err
	}
	return filepath.Join(published, ".bin", name), nil
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
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			// A symlink inside the package is neither hashed nor copied. Copying
			// it could point outside the boundary, and following it would import
			// exactly the escape this whole change removes.
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
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
func publishRuntime(root string, name string, runtimeName string, boundary string, relative string, packaged bool) error {
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
		if err := validatePublishedRuntime(destinationRoot, name, runtimeName, relative); err != nil {
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

	if packaged {
		if err := copyRuntimeTree(sourceRoot, destinationRoot, staging, relative); err != nil {
			return err
		}
	} else if err := copyRuntimeFile(sourceRoot, relative, destinationRoot, filepath.Join(staging, relative)); err != nil {
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

func validatePublishedRuntime(root *os.Root, published string, runtimeName string, relative string) error {
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
	return nil
}

func copyRuntimeTree(sourceRoot *os.Root, destinationRoot *os.Root, destination string, relative string) error {
	return fs.WalkDir(sourceRoot.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		target := filepath.Join(destination, name)
		switch {
		case entry.IsDir():
			if err := destinationRoot.Mkdir(target, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			return nil
		case entry.Type().IsRegular():
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			// Owner-private members are the operator's, not the runtime's, so
			// they are excluded from the copy exactly as they are from the
			// fingerprint. Copying one would re-expose it inside a tree the
			// seat is granted.
			if !stagedTreeMember(name, relative, info.Mode().Perm()) {
				return nil
			}
			return copyRuntimeFile(sourceRoot, name, destinationRoot, target)
		default:
			// Symlinks, sockets and devices are SKIPPED, not followed: a link
			// inside an operator-owned tree is the escape this change exists to
			// remove. A runtime that genuinely needs one fails to launch and says
			// so, which is the safe direction.
			return nil
		}
	})
}

func copyRuntimeFile(sourceRoot *os.Root, name string, destinationRoot *os.Root, target string) error {
	handle, err := sourceRoot.Open(name)
	if err != nil {
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
	// EXECUTE BITS ARE PRESERVED because the copy must be runnable; write bits
	// are not, so the staged tree is read-only to the seat that uses it.
	mode := fs.FileMode(0o500)
	if info.Mode().Perm()&0o111 == 0 {
		mode = 0o400
	}
	written, err := destinationRoot.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer written.Close()
	if _, err := io.Copy(written, handle); err != nil {
		return err
	}
	return written.Close()
}
