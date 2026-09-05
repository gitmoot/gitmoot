// Package toolchain materialises an immutable, daemon-owned COPY of the
// operator-pinned Go toolchain for read-only seats to use.
//
// WHY A COPY RATHER THAN A GRANT ON THE OPERATOR'S TREE (#1878). Granting a
// read-only seat a Landlock rule over the operator's installation was attempted
// and reviewed three times, and every round produced an escape-class defect: a
// home-relative root accepted without proof, a classify-then-install race, and a
// member symlink followed out of the validated tree with no privilege required.
// A TOCTOU against a path the daemon does not own can be narrowed but never
// closed. Copying a tree the daemon owns removes the outside root.
//
// WHY EVERY PATH OPERATION IS DESCRIPTOR-ANCHORED. The first copy implementation
// reintroduced the same class it was meant to remove. It validated the source by
// walking it and then re-walked BY PATHNAME to copy, so an intermediate
// directory could be swapped between the two resolutions; and it embedded the
// VERSION string into the published path, so a hostile VERSION of "go/../.."
// published outside the daemon root entirely. Both were reproduced at the
// reviewed head.
//
// WHAT os.Root PROVIDES IS CONTAINMENT. Source reads and destination writes go
// through a root opened once, which refuses any name that escapes it, so neither
// a hostile VERSION nor a swapped component can address anything outside the
// tree. That is what closed the traversal and the planted-symlink escapes.
//
// WHAT CLOSES THE SWAP IS THE DIGEST, NOT AN ABSENCE OF RESOLUTIONS, and an
// earlier version of this comment overstated it. It claimed "there is no longer
// a second pathname resolution to lose". A reviewer disproved that: Stage still
// walks the shared source TWICE, once to fingerprint and once to copy, and a
// swap between the two passes IS copied. What stops it mattering is that the
// identity is re-proven against the COPY afterwards, so a source mutated
// mid-flight yields a refused publish rather than a poisoned tree. The
// guarantee is two resolutions reconciled by a content digest, FAILING CLOSED
// on disagreement. The duller sentence is the true one.
//
// Measured on this box with go1.26.4, including a positive control:
//
//	os.OpenRoot                      available
//	Open("../outside/SECRET")        refused, "path escapes from parent"
//	Open("link/SECRET") via symlink  refused, "path escapes from parent"
//	Mkdir("go/../../escaped")        refused
//	CONTROL Create("ok.txt")         succeeded, so it is not refusing everything
package toolchain

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// DirName is the daemon-owned directory holding staged copies, under the gitmoot
// home. A seat never has a write grant here.
const DirName = "toolchains"

// MinFreeBytes is the floor below which staging REFUSES rather than starting a
// copy it cannot finish.
//
// STATED AS A NUMBER IN THE CODE on purpose: inheriting whatever the disk happens
// to be that minute is not a policy. A toolchain is about 269 MiB, so this leaves
// room for the copy plus the temporary tree plus headroom, and the refusal names
// the shortfall so an operator can act on it.
const MinFreeBytes = 4 << 30

// maxVersionLength bounds the untrusted VERSION string before it is used in any
// path component.
const maxVersionLength = 64

var (
	// ErrNotPinned reports a source that is not an operator-pinned Go
	// installation. It is not a failure: callers leave the sandbox's
	// pre-existing behaviour in place.
	ErrNotPinned = errors.New("not a pinned go installation")
	// ErrSymlink reports a symlink where this package requires a real file or
	// directory. It is separate from ErrNotPinned because it is the signature of
	// an escape attempt rather than an unusual layout.
	ErrSymlink = errors.New("symlink refused")
	// ErrLowDisk reports refusal to start a copy that would risk filling the
	// filesystem.
	ErrLowDisk = errors.New("insufficient free space to stage a toolchain")
	// ErrUntrustedVersion reports a VERSION string that cannot be used as a path
	// component.
	ErrUntrustedVersion = errors.New("VERSION is not usable as a path component")
)

// requiredMembers must exist. bin holds the compiler, so the signature implies it.
var requiredMembers = []string{"bin"}

// optionalMembers are copied when present and SKIPPED ONLY when absent. Any
// other error is fatal, because "could not read it" and "it is not there" are
// different facts and only the second is a layout choice.
var optionalMembers = []string{"pkg", "src", "lib", "api", "misc", "test", "doc"}

// rootFiles are the files go reads from the installation root. go.env carries
// GOTOOLCHAIN, whose absence invites the auto-download a sandboxed seat cannot
// complete.
var rootFiles = []string{"VERSION", "go.env"}

// stagers serialises materialisation per PUBLISHED PATH so concurrent jobs on one
// version do not race to publish the same tree.
//
// KEYED ON THE PUBLISHED PATH, NOT THE IDENTITY ALONE. An identity-keyed memo
// returns the first caller's path to every later caller, so a second gitmoot home
// is served a tree that does not live under it. Caught by a test whose staged
// path pointed into another test's home directory.
// SERIALISES THE WORK, IT DOES NOT CACHE THE ANSWER. A sync.Once here would run
// verification exactly once per process, so a copy corrupted after its first use
// would be served for the rest of a long-lived daemon's life. That is the same
// mistake as caching a validation result: an answer that must be re-proven cannot
// be memoised. Every call re-verifies for 0.06s, which is 60x cheaper than the
// per-file fsync this durability mechanism replaced.
var stagers sync.Map // published path -> *sync.Mutex

// Identity is the content identity of a Go installation.
//
// THE FINGERPRINT COVERS EVERY EXECUTABLE THAT WAS COPIED, not just bin/go. An
// earlier version hashed VERSION plus bin/go, which omitted
// pkg/tool/linux_amd64/compile, the ACTUAL compiler. Reproduced: two sources
// agreeing on VERSION and bin/go but differing in their compiler shared a cache
// entry, and the second was served the first's compiler. Measured cost of the
// correction: the exec set is 54 files and 82.1 MiB, hashing in 0.06s against
// 0.01s for bin/go alone, so covering it costs 0.05s.
//
// STATED LIMIT, because it is a real gap rather than an oversight: a torn or
// substituted NON-executable member (src, doc, api, test, misc) is not covered,
// because hashing all 221.6 MiB on every seat launch would reintroduce the 127x
// regression per-file fsync caused. Its consequence is a visibly broken toolchain
// and a build error, NOT a silently wrong compiler.
type Identity struct {
	Version     string
	Fingerprint string
}

func (i Identity) String() string {
	return i.Version + "-" + i.Fingerprint
}

// Root returns the daemon-owned directory holding staged copies.
func Root(gitmootHome string) string {
	return filepath.Join(gitmootHome, DirName)
}

// safeVersion proves an untrusted VERSION string is usable as ONE path component.
//
// TWO ARMS, DELIBERATELY ASYMMETRIC. This charset check is defence in depth; the
// ENFORCING arm is that every publish goes through an os.Root, which refuses an
// escaping name even if this function is wrong. Belt and braces, in that order.
func safeVersion(version string) (string, error) {
	version = strings.TrimSpace(version)
	switch {
	case version == "":
		return "", fmt.Errorf("%w: empty", ErrUntrustedVersion)
	case len(version) > maxVersionLength:
		return "", fmt.Errorf("%w: longer than %d bytes", ErrUntrustedVersion, maxVersionLength)
	case !strings.HasPrefix(version, "go"):
		return "", fmt.Errorf("%w: %q does not name a Go release", ErrNotPinned, version)
	case strings.Contains(version, ".."):
		// Contained anyway, because the published component always carries a
		// "-<fingerprint>" suffix and so can never BE "..". Refused regardless:
		// it costs nothing and removes a class of reasoning from the boundary.
		return "", fmt.Errorf("%w: %q contains a parent reference", ErrUntrustedVersion, version)
	}
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '+':
		default:
			return "", fmt.Errorf("%w: %q contains %q", ErrUntrustedVersion, version, r)
		}
	}
	return version, nil
}

// Identify opens source as a root and returns its content identity WITHOUT
// copying anything.
func Identify(source string) (Identity, error) {
	source = strings.TrimSpace(source)
	if source == "" || !filepath.IsAbs(source) {
		return Identity{}, fmt.Errorf("%w: source %q must be an absolute path", ErrNotPinned, source)
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrNotPinned, err)
	}
	defer root.Close()
	return identifyRoot(root)
}

// identifyRoot derives the identity from an already-open root, so the descriptor
// that proves the installation is the one that reads it.
func identifyRoot(root *os.Root) (Identity, error) {
	if err := requireExecutableGo(root); err != nil {
		return Identity{}, err
	}
	rawVersion, err := readVersion(root)
	if err != nil {
		return Identity{}, err
	}
	version, err := safeVersion(rawVersion)
	if err != nil {
		return Identity{}, err
	}
	fingerprint, err := fingerprintExecutables(root)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Version: version, Fingerprint: fingerprint}, nil
}

// EVERY O_NOFOLLOW IN THIS PACKAGE IS UNKILLABLE BY A DETERMINISTIC TEST, AND
// THIS IS THE AUTHORITATIVE LIST. A reviewer found one such survivor and I had
// disclosed a different one; enumerating the whole class rather than patching the
// site it was found at turned up six, so the honest count is stated here once
// instead of claimed in aggregate anywhere else:
//
//	stage.go requireExecutableGo   O_NOFOLLOW on bin/go      SURVIVES
//	stage.go readVersion           O_NOFOLLOW on VERSION     SURVIVES
//	stage.go fingerprintExecutables O_NOFOLLOW on the digest read  SURVIVES
//	stage.go walkAnchored          O_NOFOLLOW on the walked name   SURVIVES
//	stage.go copyOneFile           O_NOFOLLOW on the copied file   SURVIVES
//	stage.go copyOneFile           O_EXCL on the destination create SURVIVES
//
// THE SAME CAUSE AT ALL FIVE O_NOFOLLOW SITES: refuseSymlinkedName's Lstat fires
// first and refuses the name, so removing the flag changes no observable
// behaviour and no test can see it. O_EXCL survives for a different reason - the
// destination is always a freshly created temp directory, so a collision cannot
// occur; it asserts an invariant rather than defending an attack.
//
// THEY ARE KEPT RATHER THAN DELETED, and that is the opposite call from the two
// guards deleted elsewhere in this file, so the difference matters: those were
// duplicate CHECKS with no distinct effect, whereas each O_NOFOLLOW is the only
// thing covering the Lstat-to-open window documented below. Deleting them would
// widen a residual that is real but not deterministically constructible. A guard
// that only matters inside a race cannot be killed by a deterministic test, and
// claiming coverage for one would be a false evidence claim.
//
// refuseSymlinkedName rejects a name whose FINAL COMPONENT is a symlink.
//
// MEASURED BEHAVIOUR OF os.Root, and it invalidated an assumption this package
// started with: os.Root performs its OWN component-by-component resolution, so
// O_NOFOLLOW applies to the resolved target rather than to the name given. A
// symlinked VERSION was therefore followed and yielded a version from another
// file, and a symlinked top-level member was copied rather than refused. Root's
// resolution refuses only links that ESCAPE; links that stay inside are followed
// by design. Lstat does not follow the final component, so it is what refuses.
//
// STATED RESIDUAL: this is an Lstat followed by an open, so a name swapped
// between the two is resolved by Root rather than by us. The consequence is
// BOUNDED by Root itself - the swap can only point somewhere inside the same
// source installation, so the worst case is copying a duplicate of a file that
// was already going to be copied. It is not an escape, and no rule can close a
// window on a tree the daemon does not own.
func refuseSymlinkedName(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	return nil
}

// requireExecutableGo proves bin/go is a real executable regular file, from a
// descriptor rather than from a name.
func requireExecutableGo(root *os.Root) error {
	// NO Lstat guard here: a symlinked bin/go is refused by the fingerprint walk,
	// which visits bin/go through walkAnchored and applies refuseSymlinkedName. A
	// duplicate CHECK here was deleted as redundant. The O_NOFOLLOW below is KEPT
	// and is a known survivor - see the enumerated list at refuseSymlinkedName.
	handle, err := root.OpenFile("bin/go", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("%w: bin/go is a symlink", ErrSymlink)
		}
		return fmt.Errorf("%w: bin/go: %v", ErrNotPinned, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("%w: bin/go: %v", ErrNotPinned, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: bin/go is not an executable regular file", ErrNotPinned)
	}
	return nil
}

// readVersion returns the raw first line of VERSION.
//
// O_NONBLOCK because opening a FIFO with no writer would otherwise BLOCK seat
// launch: an availability defect rather than a leak. The type is proven from the
// descriptor BEFORE any content is read.
//
// This O_NOFOLLOW is a known survivor, one of six enumerated at
// refuseSymlinkedName. O_NONBLOCK by contrast IS killed, by
// TestReadVersionRefusesAFifoWithoutBlocking.
func readVersion(root *os.Root) (string, error) {
	if err := refuseSymlinkedName(root, "VERSION"); err != nil {
		return "", err
	}
	handle, err := root.OpenFile("VERSION", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", fmt.Errorf("%w: VERSION is a symlink", ErrSymlink)
		}
		return "", fmt.Errorf("%w: VERSION: %v", ErrNotPinned, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: VERSION is not a regular file", ErrNotPinned)
	}
	buffer := make([]byte, maxVersionLength+1)
	read, err := handle.Read(buffer)
	if err != nil && read == 0 {
		return "", fmt.Errorf("%w: VERSION: %v", ErrNotPinned, err)
	}
	return strings.SplitN(string(buffer[:read]), "\n", 2)[0], nil
}

// fingerprintExecutables digests every regular file carrying an exec bit, in
// sorted order, path and content.
func fingerprintExecutables(root *os.Root) (string, error) {
	var names []string
	for _, member := range requiredMembers {
		found, err := collectExecutables(root, member)
		if err != nil {
			return "", fmt.Errorf("required member %s: %w", member, err)
		}
		names = append(names, found...)
	}
	for _, member := range optionalMembers {
		found, err := collectExecutables(root, member)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("optional member %s: %w", member, err)
		}
		names = append(names, found...)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("%w: no executables found to fingerprint", ErrNotPinned)
	}
	sort.Strings(names)

	digest := sha256.New()
	for _, name := range names {
		handle, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return "", fmt.Errorf("%w: fingerprint %s: %v", ErrNotPinned, name, err)
		}
		fmt.Fprintf(digest, "%s\n", name)
		_, copyErr := io.Copy(digest, handle)
		closeErr := handle.Close()
		if copyErr != nil {
			return "", fmt.Errorf("%w: fingerprint %s: %v", ErrNotPinned, name, copyErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil))[:16], nil
}

// collectExecutables walks a member anchored to root and returns the root-relative
// names of executable regular files.
func collectExecutables(root *os.Root, member string) ([]string, error) {
	var found []string
	err := walkAnchored(root, member, func(name string, info fs.FileInfo) error {
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			found = append(found, name)
		}
		return nil
	})
	return found, err
}

// walkAnchored walks name anchored to root, refusing symlinks and irregular files,
// and calls visit for every entry.
//
// CLASSIFY WHAT YOU HOLD. Each entry's kind comes from its DIRECTORY ENTRY, which
// is a property of the directory content rather than a separate path resolution,
// and every open is O_NOFOLLOW against the same root descriptor. So there is no
// interval in which a swapped component could redirect the operation, which is
// the race the previous pathname-based walk had.
func walkAnchored(root *os.Root, name string, visit func(string, fs.FileInfo) error) error {
	// The directory-entry check below covers entries found by ReadDir, but NOT
	// the name this walk was entered with. A symlinked top-level member was
	// copied until this existed.
	if err := refuseSymlinkedName(root, name); err != nil {
		return err
	}
	handle, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		return err
	}
	info, statErr := handle.Stat()
	if statErr != nil {
		handle.Close()
		return statErr
	}
	if !info.IsDir() {
		defer handle.Close()
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, name)
		}
		return visit(name, info)
	}
	entries, readErr := handle.ReadDir(-1)
	handle.Close()
	if readErr != nil {
		return readErr
	}
	if err := visit(name, info); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		// No directory-entry symlink check here: the recursive call's own
		// refuseSymlinkedName covers it. An earlier version had both, and a
		// mutant deleting this one survived every test, which is what a
		// redundant guard looks like. Deleted rather than given a test that
		// could only ever pass.
		if err := walkAnchored(root, path.Join(name, entry.Name()), visit); err != nil {
			return err
		}
	}
	return nil
}

// Stage materialises source into the daemon-owned root and returns the published
// path. It is idempotent per published path and safe under concurrent callers.
func Stage(gitmootHome, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" || !filepath.IsAbs(source) {
		return "", fmt.Errorf("%w: source %q must be an absolute path", ErrNotPinned, source)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotPinned, err)
	}
	defer sourceRoot.Close()

	identity, err := identifyRoot(sourceRoot)
	if err != nil {
		return "", err
	}
	root := Root(gitmootHome)
	published := filepath.Join(root, identity.String())

	entry, _ := stagers.LoadOrStore(published, &sync.Mutex{})
	lock := entry.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	return stage(root, identity, sourceRoot)
}

// stage publishes the source tree under the daemon root, reusing an existing copy
// ONLY when that copy still proves its identity.
//
// DURABILITY IS BY VERIFICATION, NOT fsync, AND THAT IS A MEASURED CHOICE. An
// earlier form fsynced every copied file: measured on the real 269 MiB toolchain,
// 15022 files, 136.18s with per-file fsync against 1.07s without, a 127x cost on
// the seat launch path that showed up as a 0.413s e2e test becoming 289.18s.
// fsync existed because os.Rename publishes a complete NAME while the data
// underneath need not be durable, and a published tree is otherwise reused
// forever, so a crash could serve truncated files as if correct. Re-verifying the
// digest on reuse closes exactly that for 0.06s, and is strictly stronger: it
// catches a corrupt copy whatever produced it, not only a crash.
func stage(root string, identity Identity, sourceRoot *os.Root) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// EVERY DESTINATION OPERATION GOES THROUGH THIS ROOT, so no name derived from
	// the untrusted VERSION can address anything outside it.
	destinationRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer destinationRoot.Close()

	name := identity.String()
	// Lstat, NEVER Stat: a symlink planted at the published name must not be
	// mistaken for a valid staged copy. Reproduced at the reviewed head, where
	// os.Stat accepted such a link and the seat's read grant followed it.
	if info, statErr := destinationRoot.Lstat(name); statErr == nil {
		if info.IsDir() && verifyPublished(destinationRoot, name, identity) == nil {
			return filepath.Join(root, name), nil
		}
		aside := ".stale-" + randomSuffix()
		if renameErr := destinationRoot.Rename(name, aside); renameErr != nil {
			return "", fmt.Errorf("replace unusable staged copy %s: %w", name, renameErr)
		}
		defer func() { _ = destinationRoot.RemoveAll(aside) }()
	}

	if err := checkFreeSpace(root); err != nil {
		return "", err
	}
	temp := ".staging-" + randomSuffix()
	if err := destinationRoot.Mkdir(temp, 0o755); err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = destinationRoot.RemoveAll(temp)
		}
	}()

	if err := copyInstallation(sourceRoot, destinationRoot, temp); err != nil {
		return "", err
	}
	// THE IDENTITY IS RE-PROVEN AGAINST THE COPY, not against the source. The
	// source could have changed while it was being read; the copy is ours and
	// immutable from here. A mismatch means the tree we hold is not the tree we
	// named, so it is discarded rather than published under a name it fails.
	if err := verifyPublished(destinationRoot, temp, identity); err != nil {
		return "", fmt.Errorf("staged copy does not match its source identity: %w", err)
	}
	if err := syncTree(filepath.Join(root, temp)); err != nil {
		return "", err
	}
	if err := destinationRoot.Rename(temp, name); err != nil {
		// A concurrent stager may have published first, which is a success only
		// if what it published proves its identity.
		if verifyPublished(destinationRoot, name, identity) == nil {
			success = true
			return filepath.Join(root, name), nil
		}
		return "", err
	}
	success = true
	return filepath.Join(root, name), nil
}

// verifyPublished re-proves that a staged tree is the installation its directory
// name claims, through the daemon root descriptor.
func verifyPublished(destinationRoot *os.Root, name string, identity Identity) error {
	nested, err := destinationRoot.OpenRoot(name)
	if err != nil {
		return err
	}
	defer nested.Close()
	found, err := identifyRoot(nested)
	if err != nil {
		return err
	}
	if found != identity {
		return fmt.Errorf("%w: staged copy %s is %v, want %v", ErrNotPinned, name, found, identity)
	}
	return nil
}

func randomSuffix() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("pid%d", os.Getpid())
	}
	return fmt.Sprintf("%x", buffer[:])
}

// checkFreeSpace refuses UP FRONT rather than failing part-way through a copy, and
// names the shortfall so the failure is actionable instead of mysterious.
func checkFreeSpace(target string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(target, &stat); err != nil {
		return err
	}
	return enoughFree(stat.Bavail*uint64(stat.Bsize), MinFreeBytes, target)
}

// enoughFree is the floor comparison, extracted as a PURE predicate so a test can
// force BOTH arms.
//
// The test guarding this before only asserted inside `if err != nil`, so on any
// host with more than the floor free - which is every CI runner, and this box has
// sat at 95 percent used all hour - the refusal branch never ran, and the test
// would have passed even if checkFreeSpace always returned nil. That is a vacuous
// row, and a reviewer found it in my own test. Taking the floor as an argument
// makes both outcomes reachable without needing a full disk.
func enoughFree(free, floor uint64, target string) error {
	if free < floor {
		return fmt.Errorf("%w: %d bytes free at %s, floor is %d", ErrLowDisk, free, target, floor)
	}
	return nil
}

// copyInstallation copies the required and optional members plus the root files,
// every read anchored to sourceRoot and every write anchored to destinationRoot.
func copyInstallation(sourceRoot, destinationRoot *os.Root, destination string) error {
	for _, member := range requiredMembers {
		if err := copyMember(sourceRoot, destinationRoot, destination, member); err != nil {
			return fmt.Errorf("required member %s: %w", member, err)
		}
	}
	for _, member := range optionalMembers {
		if err := copyMember(sourceRoot, destinationRoot, destination, member); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("optional member %s: %w", member, err)
		}
	}
	for _, name := range rootFiles {
		if err := copyOneFile(sourceRoot, destinationRoot, name, path.Join(destination, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("root file %s: %w", name, err)
		}
	}
	return nil
}

func copyMember(sourceRoot, destinationRoot *os.Root, destination, member string) error {
	return walkAnchored(sourceRoot, member, func(name string, info fs.FileInfo) error {
		target := path.Join(destination, name)
		if info.IsDir() {
			return destinationRoot.MkdirAll(target, 0o755)
		}
		return copyOneFile(sourceRoot, destinationRoot, name, target)
	})
}

// copyOneFile copies one regular file, PRESERVING the executable bit and STRIPPING
// setuid and setgid, so a staged tree can never carry privilege the source
// happened to have.
//
// NO PER-FILE fsync: measured at 136.18s against 1.07s on 15022 files. Data
// integrity comes from re-verifying the digest on reuse (see stage).
func copyOneFile(sourceRoot, destinationRoot *os.Root, name, target string) error {
	if err := refuseSymlinkedName(sourceRoot, name); err != nil {
		return err
	}
	handle, err := sourceRoot.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("%w: %s", ErrSymlink, name)
		}
		return err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, name)
	}
	if parent := path.Dir(target); parent != "." {
		if err := destinationRoot.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	// Perm() masks to 0o777 and therefore cannot represent setuid or setgid at
	// all, which is what makes the strip structural rather than a subtraction.
	mode := info.Mode().Perm() & 0o755
	destination, err := destinationRoot.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, handle); err != nil {
		destination.Close()
		return err
	}
	return destination.Close()
}

// syncTree fsyncs DIRECTORIES so the rename publishes a durable name. There are a
// few hundred directories rather than 15022 files, so the cost is in the noise
// beside the copy itself.
func syncTree(root string) error {
	return filepath.WalkDir(root, func(target string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		handle, openErr := os.Open(target)
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

// Collect removes staged copies that no live pin references, plus leftovers from
// interrupted stages.
//
// RETENTION IS BY CURRENT PIN, not by age or count: a version in use must never be
// collected however old it is, and a version nothing references is dead however
// new.
func Collect(gitmootHome string, keep []Identity) error {
	root := Root(gitmootHome)
	destinationRoot, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer destinationRoot.Close()

	handle, err := destinationRoot.Open(".")
	if err != nil {
		return err
	}
	entries, err := handle.ReadDir(-1)
	handle.Close()
	if err != nil {
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
		if err := destinationRoot.RemoveAll(name); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("collect staged toolchains: %s", strings.Join(failures, "; "))
	}
	return nil
}
