// Package toolchain materialises an immutable, daemon-owned COPY of the
// operator-pinned Go toolchain for read-only seats to use.
//
// WHY A COPY RATHER THAN A GRANT ON THE OPERATOR'S TREE (#1878). Granting a
// read-only seat a Landlock rule over the operator's installation was attempted
// across three review rounds and produced escape-class defects each time: a
// member symlink that needed no privilege to redirect a grant outside
// containment, and a TOCTOU between classifying a path and installing a rule for
// it. A TOCTOU on a path the daemon does not own has no Landlock rule that
// closes it, only rules that narrow it. Copying the tree the daemon DOES own
// removes the outside root entirely, so there is nothing left to contain.
//
// The residual risk class changes rather than disappearing: what is left is
// LIFECYCLE, whose worst case is a stale toolchain or wasted disk, never an
// escape. That trade is the whole reason this package exists, and it is a trade:
// this package IS the new lifecycle surface, and it carries a copier, a publish
// protocol, a retention rule and a collector.
//
// SCOPE. Only the OPERATOR-PINNED toolchain is staged. Toolchains under system
// package roots (/opt, /usr/local, /nix/store, /snap) are untouched here and
// keep their pre-existing location-only grant in internal/sandbox: that
// behaviour predates #1878, was never the requirement, and there is no evidence
// about packaged layouts to justify changing it.
package toolchain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// DirName is the daemon-owned subtree holding staged copies. It sits directly
// under the gitmoot home, a sibling of the other daemon-owned trees, and NEVER
// under a seat's cache root: a seat's cache root is its WRITE grant, so a copy
// placed there would let the seat rewrite its own go binary and reproduce the
// defect this package exists to remove.
const DirName = "toolchains"

// MinFreeBytes is the floor below which staging REFUSES rather than starting a
// copy it might not finish.
//
// It is a stated number rather than a fraction of whatever the disk happens to
// be, because a proportional floor silently approves a copy on a nearly full
// volume. One staged toolchain measured 269 MB across 15,022 files, and the box
// this was written for sat at 92 percent used with roughly 37 GiB free while
// declining about half a gibibyte every four minutes under a live writer. 4 GiB
// leaves room for a copy, its temporary twin during publish, and the writers
// that are not this one.
const MinFreeBytes = 4 << 30

// Identity names a staged copy by CONTENT, never by path or version string.
//
// A repin can land IN PLACE at the same directory name, and a patched rebuild
// can carry the same VERSION. Keyed on either of those, a stale copy serves an
// OLD COMPILER to every later job while every check passes: the wrong-answer
// failure mode rather than the error one, which is the expensive kind because
// nothing looks broken. The fingerprint makes staleness answer itself, because a
// repin resolves to a different Identity and the previous copy simply stops
// being referenced. Hashing the compiler costs about 0.01s, so it is
// unconditional.
type Identity struct {
	Version     string
	Fingerprint string
}

// String is the on-disk directory name for an identity.
func (i Identity) String() string {
	return i.Version + "-" + i.Fingerprint
}

var (
	// ErrNotPinned reports a toolchain that is not an operator-pinned tree this
	// package stages. It is not a failure: callers fall back to leaving the
	// sandbox's pre-existing behaviour in place.
	ErrNotPinned = errors.New("toolchain is not an operator-pinned installation")
	// ErrSymlink reports a symlink inside the source tree. It is LOUD and fatal
	// rather than skipped: following one would copy, or later grant, something
	// outside the tree that was validated, which is the back door through which
	// the outside-root problem returns.
	ErrSymlink = errors.New("toolchain source contains a symlink")
	// ErrLowDisk reports that staging refused before starting.
	ErrLowDisk = errors.New("insufficient free space to stage a toolchain")
)

// requiredMembers must exist in a source tree for it to be a stageable
// installation. bin holds the compiler, so the signature already implies it.
var requiredMembers = []string{"bin"}

// optionalMembers are copied when present and SKIPPED ONLY when absent. Any
// other error on an optional member is fatal, because "could not read it" and
// "it is not there" are different facts and only the second is benign.
var optionalMembers = []string{"pkg", "src", "lib", "api", "misc", "test", "doc"}

// rootFiles are the files go reads from the installation root.
//
// go.env is here because of a measurement rather than for completeness: with it
// unreadable and GOTOOLCHAIN unset, builds still pass but `go env GOTOOLCHAIN`
// reports EMPTY instead of its configured default, and an empty GOTOOLCHAIN
// invites the toolchain auto-download a sandboxed seat cannot complete.
var rootFiles = []string{"VERSION", "go.env"}

// stagers serialises materialisation per PUBLISHED PATH so concurrent jobs on
// one version do not race to publish the same tree. The loser waits for the
// winner rather than copying in parallel.
//
// KEYED ON THE PUBLISHED PATH, NOT ON THE IDENTITY ALONE. An identity-keyed memo
// returns the FIRST caller's path to every later caller, so a second gitmoot
// home is served a tree that does not live under it. Caught by a test whose
// staged path pointed into another test's home directory.
var stagers sync.Map // published path -> *stageOnce

type stageOnce struct {
	once sync.Once
	path string
	err  error
}

// Root returns the daemon-owned directory holding staged copies.
func Root(gitmootHome string) string {
	return filepath.Join(gitmootHome, DirName)
}

// Identify reads a candidate installation and returns its content identity.
//
// It reads the VERSION file and fingerprints the compiler binary. It does NOT
// copy anything and does NOT decide placement.
func Identify(source string) (Identity, error) {
	source = strings.TrimSpace(source)
	if source == "" || !filepath.IsAbs(source) {
		return Identity{}, fmt.Errorf("%w: source %q must be an absolute path", ErrNotPinned, source)
	}
	goBinary := filepath.Join(source, "bin", "go")
	info, err := os.Lstat(goBinary)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNotPinned, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return Identity{}, fmt.Errorf("%w: bin/go is a symlink", ErrSymlink)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Identity{}, fmt.Errorf("%w: bin/go is not an executable regular file", ErrNotPinned)
	}
	version, err := readVersion(filepath.Join(source, "VERSION"))
	if err != nil {
		return Identity{}, err
	}
	fingerprint, err := fingerprintFile(goBinary)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Version: version, Fingerprint: fingerprint}, nil
}

// readVersion returns the Go version string, refusing anything that is not a
// regular file naming a Go release.
//
// O_NOFOLLOW so a VERSION symlink cannot point the check at an unrelated file,
// and O_NONBLOCK because opening a FIFO with no writer would otherwise BLOCK
// staging: an availability defect in the launch path rather than a leak. Type is
// proven from the descriptor BEFORE any content is read.
func readVersion(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("%w: VERSION: %v", ErrNotPinned, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: VERSION is not a regular file", ErrNotPinned)
	}
	buffer := make([]byte, 64)
	read, err := file.Read(buffer)
	if err != nil && read == 0 {
		return "", fmt.Errorf("%w: VERSION: %v", ErrNotPinned, err)
	}
	line := strings.TrimSpace(strings.SplitN(string(buffer[:read]), "\n", 2)[0])
	if !strings.HasPrefix(line, "go") {
		return "", fmt.Errorf("%w: VERSION %q does not name a Go release", ErrNotPinned, line)
	}
	return line, nil
}

func fingerprintFile(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("%w: fingerprint: %v", ErrNotPinned, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("%w: fingerprint: %v", ErrNotPinned, err)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))[:16], nil
}

// Stage materialises source into the daemon-owned root and returns the published
// path. It is idempotent per published path and safe under concurrent callers.
//
// PUBLISH PROTOCOL: copy into a temporary directory named by the identity, fsync
// the tree, then atomic-rename into place. A partial tree is therefore never
// visible at the published name, and an interrupted stage leaves only a
// temporary directory for Collect to remove. Source and destination live under
// the same filesystem, so the rename is atomic rather than a cross-device copy.
//
// THE SOURCE IS VALIDATED BEFORE THE MEMO IS CONSULTED. A refusal is a fact
// about the source the caller named, so it must not be answered from a cache
// entry another source populated: two trees can share an identity (which
// fingerprints bin/go and VERSION) while differing elsewhere, and serving the
// clean one's answer to a symlink-poisoned one would skip the check that exists
// to keep the outside-root problem shut. Validation is stat-only; the copy is
// what the memo is worth caching.
func Stage(gitmootHome, source string) (string, error) {
	identity, err := Identify(source)
	if err != nil {
		return "", err
	}
	if err := validateSource(source); err != nil {
		return "", err
	}
	root := Root(gitmootHome)
	published := filepath.Join(root, identity.String())

	entry, _ := stagers.LoadOrStore(published, &stageOnce{})
	staging := entry.(*stageOnce)
	staging.once.Do(func() {
		staging.path, staging.err = stage(root, published, source)
	})
	if staging.err != nil {
		// A failed stage must not be cached forever: the next caller retries.
		stagers.Delete(published)
		return "", staging.err
	}
	return staging.path, nil
}

// validateSource proves the source tree is copyable BEFORE anything is copied or
// served from cache: no symlinks and no irregular files among the members this
// package would copy.
//
// Absence of an optional member is benign and skips. Any other error is
// operational and refuses, because "could not read it" and "it is not there" are
// different facts and only the second is a layout choice.
func validateSource(source string) error {
	for _, member := range requiredMembers {
		if err := validateTree(filepath.Join(source, member)); err != nil {
			return fmt.Errorf("required member %s: %w", member, err)
		}
	}
	for _, member := range optionalMembers {
		path := filepath.Join(source, member)
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("optional member %s: %w", member, err)
		}
		if err := validateTree(path); err != nil {
			return fmt.Errorf("optional member %s: %w", member, err)
		}
	}
	for _, name := range rootFiles {
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("root file %s: %w", name, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("root file %s: %w: %s", name, ErrSymlink, path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("root file %s: %w: not a regular file", name, ErrNotPinned)
		}
	}
	return nil
}

// validateTree walks a member WITHOUT following links, refusing a symlink or an
// irregular file anywhere inside it.
func validateTree(path string) error {
	return filepath.WalkDir(path, func(entry string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s", ErrSymlink, entry)
		case dirEntry.IsDir() || info.Mode().IsRegular():
			return nil
		default:
			return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, entry)
		}
	})
}

func stage(root, published, source string) (string, error) {
	if info, err := os.Stat(published); err == nil && info.IsDir() {
		return published, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := checkFreeSpace(root); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(root, ".staging-"+filepath.Base(published)+"-")
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(temp)
		}
	}()
	if err := copyInstallation(source, temp); err != nil {
		return "", err
	}
	if err := syncTree(temp); err != nil {
		return "", err
	}
	if err := os.Rename(temp, published); err != nil {
		// A concurrent stager may have published first, which is a success.
		if info, statErr := os.Stat(published); statErr == nil && info.IsDir() {
			success = true
			return published, nil
		}
		return "", err
	}
	success = true
	return published, nil
}

// checkFreeSpace refuses UP FRONT rather than failing part-way through a copy,
// and names the shortfall so the failure is actionable instead of mysterious.
func checkFreeSpace(path string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return err
	}
	free := stat.Bavail * uint64(stat.Bsize)
	if free < MinFreeBytes {
		return fmt.Errorf("%w: %d bytes free at %s, floor is %d", ErrLowDisk, free, path, uint64(MinFreeBytes))
	}
	return nil
}

// copyInstallation copies the required and optional members plus the root files.
//
// Symlinks anywhere in the copied set are FATAL. Optional members are skipped
// only when ABSENT; any other error fails the stage, because permission denied,
// too many open files, an I/O error and a concurrent removal are operational
// failures rather than a layout that lacks a directory.
func copyInstallation(source, destination string) error {
	for _, member := range requiredMembers {
		if err := copyTree(filepath.Join(source, member), filepath.Join(destination, member)); err != nil {
			return fmt.Errorf("required member %s: %w", member, err)
		}
	}
	for _, member := range optionalMembers {
		from := filepath.Join(source, member)
		if _, err := os.Lstat(from); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("optional member %s: %w", member, err)
		}
		if err := copyTree(from, filepath.Join(destination, member)); err != nil {
			return fmt.Errorf("optional member %s: %w", member, err)
		}
	}
	for _, name := range rootFiles {
		from := filepath.Join(source, name)
		if _, err := os.Lstat(from); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("root file %s: %w", name, err)
		}
		if err := copyFile(from, filepath.Join(destination, name)); err != nil {
			return fmt.Errorf("root file %s: %w", name, err)
		}
	}
	return nil
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s", ErrSymlink, path)
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode().IsRegular():
			return copyFile(path, target)
		default:
			// Devices, sockets and FIFOs are not part of a Go installation and
			// are refused rather than silently skipped.
			return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, path)
		}
	})
}

// copyFile preserves the executable bit and STRIPS setuid and setgid, so a
// staged tree can never carry privilege the source happened to have.
func copyFile(from, to string) error {
	source, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, from)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	mode := info.Mode().Perm() & 0o755
	destination, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		return err
	}
	return destination.Close()
}

// syncTree fsyncs directories so the rename publishes a durable tree rather than
// a name whose contents may not have reached disk.
func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		handle, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		syncErr := handle.Sync()
		closeErr := handle.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
}

// Collect deletes staged copies not named by keep, and removes any leftover
// temporary directories from interrupted stages.
//
// Retention is by CURRENT PIN rather than by age or count: a copy is kept
// because something resolves to it, not because it is recent.
func Collect(gitmootHome string, keep []Identity) error {
	root := Root(gitmootHome)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	live := make(map[string]struct{}, len(keep))
	for _, identity := range keep {
		live[identity.String()] = struct{}{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	var failures []string
	for _, name := range names {
		if _, wanted := live[name]; wanted {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("collect staged toolchains: %s", strings.Join(failures, "; "))
	}
	return nil
}
