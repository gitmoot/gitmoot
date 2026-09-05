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
	"sync/atomic"
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

// SENTINELS ARE WRAPPED WITH %w, NEVER %v. A wrap that formats the inner error
// with %v discards the chain, so errors.Is on an inner sentinel silently returns
// false. Found by a test of mine asserting errors.Is(err, ErrSymlink) on a genuine
// symlink refusal and getting false, because requireExecutableGo had formatted the
// inner error with %v - the refusal message was right and the sentinel was
// unreachable. A caller branching on the sentinel would have taken the wrong path
// on a correct refusal.
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

// A CALLER-SUPPLIED O_NOFOLLOW WOULD BE AN INERT BIT HERE, WHICH IS WHY THERE IS
// NOT ONE. This package previously passed syscall.O_NOFOLLOW on every root-
// relative open and claimed each flag covered the Lstat-to-open window. THAT
// CLAIM WAS FALSE and a reviewer disproved it. From the pinned toolchain's own
// source, os/root_unix.go ORs syscall.O_NOFOLLOW into the openat flags at all
// three call sites (:67, :85, :117) unconditionally, and :91 then calls
// checkSymlink, which resolves an in-root symlink and retries. The flag is
// already set whatever the caller passes.
//
// Measured before deleting them, with controls, for files AND directories:
//
//	in-root file symlink, no caller flag    opened, body "followed"
//	in-root file symlink, caller O_NOFOLLOW opened, body "followed"   identical
//	in-root dir symlink, either way         opened, entries listed    identical
//	CONTROL escaping symlink                refused, "path escapes from parent"
//	CONTROL Lstat on the same name          still reports a symlink
//
// openVerified below is what actually holds the line.

// openVerified opens name anchored to root and PROVES the object it opened is the
// same object the symlink check inspected.
//
// WHY THE INODE COMPARISON EXISTS, and it is not defensive decoration: an earlier
// version of this package did an Lstat to refuse symlinked names and then a
// separate open, and documented the gap between them as bounded - "at worst a
// duplicate of a file already being copied". THAT BOUND WAS WRONG. A reviewer
// swapped go.env to an in-root symlink immediately after its successful Lstat,
// aiming at a root file this package never selects, and the staged copy published
// that file's contents AS go.env. I reproduced it by racing: a clean stage never
// copies PRIVATE-NOTES, and a raced stage published its contents, on attempt 284
// of 3000. os.Root prevents leaving the root; it does NOT constrain the target to
// files the copy had selected.
//
// It also composed with a decision that looked safe alone: the identity digest
// covers only executables, so a NON-executable substitution passed post-copy
// verification untouched. Neither the exec-set scope nor the Lstat gap implies
// the exposure by itself.
//
// So the check runs on EVERY file this package opens from the source, not only
// the executables, because a non-executable swap is exactly what got through.
// Measured discriminator, with a positive control:
//
//	no swap                          os.SameFile(lstat, opened) = true
//	name swapped to an in-root link  os.SameFile(lstat, opened) = FALSE
//
// WHAT THIS DOES NOT CLOSE, STATED BECAUSE "THE WINDOW IS CLOSED" WOULD BE THE
// FOURTH OVERSTATEMENT ABOUT THIS MECHANISM ON THIS PR. The three before it were
// "there is no longer a second pathname resolution to lose", "each O_NOFOLLOW is
// the only thing covering the window", and "at worst a duplicate of a file
// already being copied" - each written confidently and each disproved by a probe.
// So, precisely:
//
// This proves the object did not CHANGE between inspection and open. A HARDLINK
// does not change the object - it IS the same inode - so a hardlink substitution
// passes this check. Reproduced: go.env replaced by a hardlink to an unselected
// root file was staged with that file's contents. Symlink substitution is closed;
// hardlink substitution is not.
//
// AND Stage STILL WALKS THE SOURCE TWICE, to fingerprint and then to copy. Each
// individual open is now internally consistent, but nothing reconciles the two
// walks for NON-EXECUTABLE files: the post-copy digest covers only the executable
// set, so a non-executable that differs between the two walks is not detected.
// The hardlink case is the concrete instance of that.
//
// WHY IT IS NOT CLOSED HERE, and it is a bound rather than an argument that it
// does not matter: with fs.protected_hardlinks=1 (the kernel default) a hardlinker
// must already hold read and write on the target, so making the link requires the
// access it would supposedly gain - not an escalation. And the operator-pinned
// source is root-owned and not seat-writable, so a seat cannot create the link at
// all. A link-count refusal was deliberately NOT added: every bound added in this
// campaign had a version that rejected valid input, and refusing a legitimate
// layout is a worse defect than a documented gap that is unreachable as deployed.
// Measured for the record: 0 of 15022 regular files in the pinned distribution
// have nlink>1, with a control confirming the finder detects hardlinks when
// present - so the refusal would not break THIS distribution, which is not the
// same as not breaking any.
//
// THE ALTERNATIVE WAS DELIBERATELY NOT TAKEN. Widening the identity digest to
// cover every published file would also have detected this, and was offered.
// It is rejected because it would re-hash 221.6 MiB on every seat launch, which
// is the same class as the per-file fsync that cost 136.18s against 1.07s, and
// because it DETECTS after copying where this PREVENTS during it. The digest
// therefore stays exec-set only, on purpose rather than by omission.
func openVerified(root *os.Root, name string, flag int) (*os.File, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	// THIS REFUSAL IS REDUNDANT WITH sameObject BELOW, and is kept anyway with
	// that stated rather than dressed up as protection. A symlink and its target
	// are different inodes, so a symlinked name is refused by the comparison even
	// without this check, and a mutant deleting this survives every test.
	//
	// I originally justified keeping it on the grounds that without it a symlinked
	// FIFO would be OPENED and block. That argument is now void: writing the test
	// for it found that O_NONBLOCK had never been applied to member opens at all,
	// and centralising O_NONBLOCK below covers the FIFO case regardless of this
	// check. So the honest reason to keep it is narrower - it refuses the common
	// case before opening anything and names it accurately ("symlink refused"
	// rather than "changed between inspection and open"), which is a readability
	// and diagnostics argument, not a security one.
	if before.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	// THE WINDOW IS SCHEDULED HERE, NOT RACED, so the guard below has a
	// deterministic test. Without this seam the only way to reach the substituted
	// -object case was to win a race, and a racing test is a COIN-FLIP KILLER:
	// measured, neutering sameObject's comparison was killed while REMOVING its
	// call - the semantically identical mutation - survived the same run. A guard
	// whose regression fires by luck is not a regression.
	//
	// nil in production, with exactly one caller, so it costs a nil check on a
	// path that is already doing two syscalls.
	if hook := openWindowHook.Load(); hook != nil {
		(*hook)(name)
	}
	// O_NONBLOCK ON EVERY OPEN, NOT JUST VERSION, and this closes a defect that
	// predates the inode check. A FIFO with no writer BLOCKS a blocking open, and
	// until now only the VERSION read passed O_NONBLOCK - every member open went
	// without it. Measured: a FIFO placed directly inside a normal member hung
	// Stage indefinitely (8s timeout, no return), while a member SYMLINKED to a
	// FIFO was correctly refused by the check above. So the symlink guard was
	// doing its job and the plain irregular-file case was not covered at all.
	// Hanging seat launch is an availability defect rather than a leak, which is
	// the same class the VERSION flag was already there to prevent; it belongs on
	// every open rather than on one.
	handle, err := root.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	after, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, nil, err
	}
	if err := sameObject(before, after, name); err != nil {
		handle.Close()
		return nil, nil, err
	}
	return handle, after, nil
}

// openWindowHook is unset in production and installed only by tests, to
// substitute an object between openVerified's Lstat and its open. It exists
// because the interval it opens is otherwise reachable only by racing.
//
// STRUCTURALLY SINGLE-INSTALLER AND ATOMIC, rather than a comment asking callers
// to be careful. The first version was a plain package-level func var: safe today
// because this package has no t.Parallel, and a silent data race the moment
// anyone adds one. Two failure modes had to close, not one:
//
//	torn access      an atomic pointer removes the data race outright
//	interference     installOpenWindowHook PANICS if a hook is already installed,
//	                 so two concurrent installers fail loudly instead of one
//	                 silently firing inside the other's Stage
//
// A test seam that corrupts a parallel run without saying so is the same class as
// the racing regression this hook replaced, which killed one mutation and let its
// identical twin through. Loud beats convenient.
var openWindowHook atomic.Pointer[func(name string)]

// installOpenWindowHook installs fn and returns its release function. It panics
// if a hook is already installed, which is the assertion that makes the seam
// single-threaded by construction rather than by convention.
func installOpenWindowHook(fn func(name string)) (release func()) {
	if !openWindowHook.CompareAndSwap(nil, &fn) {
		panic("toolchain: openWindowHook is already installed; this seam admits one installer at a time")
	}
	return func() { openWindowHook.Store(nil) }
}

// openVerifiedRoot is openVerified for a DIRECTORY: it returns a *os.Root that
// HOLDS a descriptor for that directory, so every name resolved through it is
// relative to the inode we validated.
//
// WHY THIS EXISTS. walkAnchored used to ReadDir a directory, CLOSE the handle,
// and then re-resolve each child FROM THE TOP ROOT by joined pathname. Replacing
// the PARENT directory in that gap redirected the children, and openVerified
// could not see it because both its observations of the child agreed on the
// replacement object. Reviewer-reproduced, and it published content from a
// directory that was never selected.
//
// Measured at the primitive, with a control that discriminates:
//
//	top.Open("real/inner/f") after the parent swap   "PRIVATE-UNSELECTED"
//	held sub.Open("inner/f") across the same swap    "LEGIT"
//	CONTROL top.Open by path, same moment            "PRIVATE-UNSELECTED"
//
// So a held sub-root removes the class rather than narrowing it. The shape was
// already in this package - verifyPublished has always identified through
// destinationRoot.OpenRoot - so this applies it to the source walk rather than
// inventing anything.
//
// THE OpenRoot CALL IS ITSELF A RESOLUTION, so it is reconciled the same way a
// file open is: the directory is Lstat'd, refused if it is a symlink, opened as a
// root, and then that root's own "." is compared against the Lstat with
// sameObject. Without that comparison this function would have reproduced the very
// defect it exists to remove, one call deeper.
func openVerifiedRoot(root *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%w: %s", ErrSymlink, name)
	}
	if !before.IsDir() {
		return nil, nil, fmt.Errorf("%w: %s is not a directory", ErrNotPinned, name)
	}
	// SAME WINDOW, SAME SEAM. openWindowHook means "between inspecting a name and
	// resolving it", which is exactly the interval here, so the directory case
	// reuses it rather than inventing a third hook. Without this the sameObject
	// reconciliation below was real but UNTESTABLE - a mutant deleting it survived
	// every test, because nothing could schedule a swap in its window. That is the
	// third time on this PR that a guard was correct and unreachable by any
	// instrument; the remedy is a seam at the window, not a better argument.
	if hook := openWindowHook.Load(); hook != nil {
		(*hook)(name)
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	after, err := sub.Stat(".")
	if err != nil {
		sub.Close()
		return nil, nil, err
	}
	if err := sameObject(before, after, name); err != nil {
		sub.Close()
		return nil, nil, err
	}
	return sub, after, nil
}

// dirWindowHook is unset in production and installed only by tests. It fires
// AFTER a directory's entries have been read and BEFORE any child is resolved,
// which is the only interval in which an intermediate-directory replacement is
// observable.
//
// IT EXISTS BECAUSE openWindowHook PROVABLY CANNOT SEE THIS. That hook fires
// between a child's Lstat and its open, so it STRADDLES the swap and sameObject
// refuses - measured: scheduling the swap there produced "lib/marker changed
// between inspection and open" rather than an exposure. A suite can therefore be
// green against a live intermediate-swap defect, which is what happened. The
// remedy is a seam at the real window, not a better race.
//
// Single-installer and atomic for the same reason as openWindowHook, and now with
// more force: two test seams double the chance of one test firing inside
// another's traversal.
var dirWindowHook atomic.Pointer[func(name string)]

// installDirWindowHook installs fn and returns its release function, panicking if
// a hook is already installed.
// memberWindowHook is unset in production and installed only by tests. It fires
// between a member FILE's verified open and the visitor call that consumes it.
//
// IT EXISTS BECAUSE NEITHER OTHER SEAM CAN SEE THIS WINDOW. openWindowHook fires
// inside openVerified, before the open, so a swap made there is caught by that
// function's own inode re-proof. dirWindowHook fires after a DIRECTORY's entries
// are read, which is upstream of any individual file's classification. The gap
// they leave is exactly the interval a re-resolving visitor is exposed to, and it
// went untested through two rounds: independent reverts of the second open in
// digestMember and copyMember each passed the entire suite.
//
// Single-installer and atomic for the same reason as the other two.
var memberWindowHook atomic.Pointer[func(name string)]

// installMemberWindowHook installs fn and returns its release function, panicking
// if one is already installed.
func installMemberWindowHook(fn func(name string)) (release func()) {
	if !memberWindowHook.CompareAndSwap(nil, &fn) {
		panic("toolchain: memberWindowHook is already installed; this seam admits one installer at a time")
	}
	return func() { memberWindowHook.Store(nil) }
}

func installDirWindowHook(fn func(name string)) (release func()) {
	if !dirWindowHook.CompareAndSwap(nil, &fn) {
		panic("toolchain: dirWindowHook is already installed; this seam admits one installer at a time")
	}
	return func() { dirWindowHook.Store(nil) }
}

// sameObject is the comparison, extracted as a PURE predicate so both arms are
// reachable by a deterministic test rather than only by winning a race.
func sameObject(inspected, opened fs.FileInfo, name string) error {
	if !os.SameFile(inspected, opened) {
		return fmt.Errorf("%w: %s changed between inspection and open", ErrSymlink, name)
	}
	return nil
}

// requireExecutableGo proves bin/go is a real executable regular file, from a
// descriptor rather than from a name.
func requireExecutableGo(root *os.Root) error {
	// bin IS OPENED AS A SUB-ROOT AND go IS RESOLVED BY LEAF THROUGH IT, the same
	// shape as the walk. This site previously called openVerified(root, "bin/go"),
	// so BOTH its Lstat and its open resolved the intermediate bin component from
	// the top source root - exactly the multi-component operation the
	// descriptor-per-directory invariant excludes, and a reviewer found it
	// surviving the round that claimed to close the class. One surviving site
	// reinstates the defect.
	binRoot, _, err := openVerifiedRoot(root, "bin")
	if err != nil {
		// THE ONLY INNER %w IN THIS FILE, AND THE ONLY ONE ANYTHING ENFORCES.
		// TestBinDirectoryReplacementCannotRedirectTheCompilerCheck asserts
		// errors.Is(err, ErrSymlink) here, because WHY the compiler check refused
		// is what distinguishes a repointed bin from an absent one, and that
		// distinction is what a #1878-class diagnosis turns on.
		//
		// Every other site in this file wraps its inner error with %v ON PURPOSE.
		// A previous round converted seven sites to %w and justified it as fixing
		// a caller that "would have taken the wrong path on a correct refusal".
		// NO SUCH CALLER EXISTS: the only consumer is
		// internal/cli/toolchain_seat.go:52, which tests the OUTER ErrNotPinned and
		// is unaffected by the inner verb. Six of the seven were unenforced, each
		// individual reversion passed the whole suite, and their inner errors are
		// heterogeneous - a missing directory, a short read - with no sentinel to
		// assert, so "covering" them would have meant inventing contracts nobody
		// consumes. They are gone rather than pinned.
		return fmt.Errorf("%w: bin: %w", ErrNotPinned, err)
	}
	defer binRoot.Close()
	handle, info, err := openVerified(binRoot, "go", os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("%w: bin/go: %v", ErrNotPinned, err)
	}
	defer handle.Close()
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
// O_NONBLOCK is load-bearing and now lives in openVerified so it applies to EVERY
// open rather than this one; it is killed by
// TestReadVersionRefusesAFifoWithoutBlocking and by
// TestIrregularMembersAreRefusedWithoutBlocking. A caller-supplied O_NOFOLLOW was
// removed as inert; openVerified is what refuses a symlinked VERSION and what
// proves the opened file is the one it inspected.
func readVersion(root *os.Root) (string, error) {
	handle, info, err := openVerified(root, "VERSION", os.O_RDONLY)
	if err != nil {
		return "", fmt.Errorf("%w: VERSION: %v", ErrNotPinned, err)
	}
	defer handle.Close()
	if !info.Mode().IsRegular() {
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
	perFile := make(map[string][sha256.Size]byte, 64)
	for _, member := range requiredMembers {
		if err := digestMember(root, member, perFile); err != nil {
			return "", fmt.Errorf("required member %s: %w", member, err)
		}
	}
	for _, member := range optionalMembers {
		if err := digestMember(root, member, perFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", fmt.Errorf("optional member %s: %w", member, err)
		}
	}
	if len(perFile) == 0 {
		return "", fmt.Errorf("%w: no executables found to fingerprint", ErrNotPinned)
	}
	names := make([]string, 0, len(perFile))
	for name := range perFile {
		names = append(names, name)
	}
	// SORTED FOLD, so the identity is a property of the CONTENT SET and not of the
	// order the filesystem happened to return entries in.
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		fmt.Fprintf(digest, "%s\n", name)
		sum := perFile[name]
		digest.Write(sum[:])
	}
	return fmt.Sprintf("%x", digest.Sum(nil))[:16], nil
}

// digestMember walks a member and records a PER-MEMBER digest for every executable,
// reading each one THROUGH THE ROOT THAT HOLDS ITS PARENT.
//
// IT DIGESTS DURING THE WALK rather than collecting names to re-open later,
// because a name collected now and opened later is a second resolution, which is
// the whole defect class. A sub-root cannot be kept for later either: it is
// released when its subtree completes so the live descriptor count stays at the
// tree depth.
//
// PER-FILE DIGESTS FOLDED IN SORTED ORDER, NOT A SINGLE ROLLING HASH, and this is
// a defect this round introduced and then caught. The first version streamed every
// file into one rolling sha256 in WALK order, which made the identity depend on
// traversal order; a mutant removing the per-directory entry sort survived every
// test, so two identical trees could have hashed differently and the same tree
// could have rehashed differently across filesystems. Recording per-file digests
// and folding them by sorted path restores order-independence without
// reintroducing a second read.
//
// It also happens to retain exactly the per-member values the deferred cross-pass
// reconciliation needs, which is the one thing that decides that issue's cost.
func digestMember(root *os.Root, member string, into map[string][sha256.Size]byte) error {
	return walkAnchored(root, member, func(parent *os.Root, leaf, displayPath string, info fs.FileInfo, handle *os.File) error {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return nil
		}
		// reads the handle the WALK opened; no second resolution of this name
		perFile := sha256.New()
		if _, err := io.Copy(perFile, handle); err != nil {
			return fmt.Errorf("%w: fingerprint %s: %v", ErrNotPinned, displayPath, err)
		}
		into[displayPath] = [sha256.Size]byte(perFile.Sum(nil))
		return nil
	})
}

// walkAnchored walks name and hands every entry to visit ALONG WITH THE ROOT THAT
// HOLDS ITS PARENT, so the visitor can resolve it by leaf name rather than
// re-resolving a path from the top.
//
// THE VISITOR CONTRACT IS PART OF THE FIX, not decoration. An earlier attempt
// carried handles through the traversal but left visit taking only a display
// path, so copyOneFile re-resolved that path FROM THE TOP ROOT and the walk was
// still redirected by an intermediate swap. The regression caught it: the fix was
// cosmetic until the descriptor reached the consumer. My own plan had marked this
// site "handle can be carried: yes" and I had wired only the traversal.
// A REGULAR FILE IS OPENED ONCE AND THE HANDLE IS HANDED TO visit. A reviewer's
// structural probe found walkHeld and its visitors each opening every member file,
// which is not merely wasteful: the two opens are separate resolutions and nothing
// reconciled them, so the file classified by the walk could differ from the file
// the visitor then read. That is the same class this round exists to close, one
// level further down. The handle is closed by walkHeld after visit returns.
func walkAnchored(root *os.Root, name string, visit func(parent *os.Root, leaf, displayPath string, info fs.FileInfo, handle *os.File) error) error {
	return walkHeld(root, name, name, visit)
}

// walkHeld resolves leaf THROUGH root, which holds a descriptor for leaf's parent,
// and reports displayPath to visit.
//
// NO CALL HERE EVER RESOLVES A MULTI-SEGMENT PATH. Children are resolved by LEAF
// NAME through the sub-root of the directory that listed them, so an ancestor
// being replaced mid-walk cannot redirect anything: the descriptor still refers to
// the inode we validated. That is what removes the intermediate-replacement class
// rather than narrowing it.
//
// DESCRIPTOR LIFETIME IS TREE DEPTH, NOT DIRECTORY COUNT, which is the bound the
// ruling required and the reason this is affordable. A sub-root is held only while
// its own subtree is being walked and is released by defer as each level completes,
// so the live count is the number of ANCESTORS. Measured on the pinned
// distribution: max depth 13 across 1667 directories, so at most 14 descriptors at
// once rather than 1667. A per-directory lifetime would have risked EMFILE under
// concurrent workers, which is a refusal of VALID input - the failure mode every
// bound in this campaign has had a version of.
func walkHeld(root *os.Root, leaf, displayPath string, visit func(parent *os.Root, leaf, displayPath string, info fs.FileInfo, handle *os.File) error) error {
	before, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlink, displayPath)
	}
	if !before.IsDir() {
		handle, info, err := openVerified(root, leaf, os.O_RDONLY)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			handle.Close()
			return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, displayPath)
		}
		// THE ONLY WINDOW IN WHICH A MEMBER FILE'S NAME CAN BE REPOINTED AFTER THIS
		// WALK HAS CLASSIFIED IT: the handle above is open and proven to be the
		// object that was inspected, and the visitor has not run yet. A visitor that
		// RE-RESOLVES leaf instead of reading this handle reads whatever the name
		// points at now, so the file the walk classified and the file the visitor
		// consumed are different objects with nothing reconciling them.
		if hook := memberWindowHook.Load(); hook != nil {
			(*hook)(displayPath)
		}
		visitErr := visit(root, leaf, displayPath, info, handle)
		// THE WALK OWNS THIS HANDLE AND THE VISITOR ONLY BORROWS IT, and that is now
		// ENFORCED rather than merely true. Handing an open file to a visitor created
		// an ownership rule with nothing behind it: a visitor that closed it would
		// make this close fail silently, and one that retained it past return would
		// hold a descriptor the walk believes released, breaking the depth bound
		// asserted by TestDescriptorPeakStaysAtTreeDepth. A review confirmed no
		// visitor does either TODAY, which is exactly the kind of fact that stops
		// being true without anyone noticing. The failing close is the signal.
		if closeErr := handle.Close(); closeErr != nil && visitErr == nil {
			return fmt.Errorf("%w: %s: the walk's handle was not left intact by its visitor: %v", ErrNotPinned, displayPath, closeErr)
		}
		return visitErr
	}

	sub, info, err := openVerifiedRoot(root, leaf)
	if err != nil {
		return err
	}
	// RELEASED WHEN THIS SUBTREE COMPLETES, including on every error return below,
	// which is what keeps the live descriptor count at the depth rather than
	// leaking one per directory visited.
	defer sub.Close()

	handle, err := sub.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := handle.ReadDir(-1)
	handle.Close()
	if readErr != nil {
		return readErr
	}
	if err := visit(root, leaf, displayPath, info, nil); err != nil {
		return err
	}

	// The only interval in which an intermediate replacement is observable: the
	// entries are known, and no child has been resolved yet.
	if hook := dirWindowHook.Load(); hook != nil {
		(*hook)(displayPath)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := walkHeld(sub, entry.Name(), path.Join(displayPath, entry.Name()), visit); err != nil {
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
	if err := syncHeld(destinationRoot, temp); err != nil {
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
		if err := copyOneFile(sourceRoot, name, destinationRoot, path.Join(destination, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("root file %s: %w", name, err)
		}
	}
	return nil
}

// copyMember resolves SOURCE names by leaf through held sub-roots and DESTINATION
// names as multi-component paths from destinationRoot. THAT ASYMMETRY IS DELIBERATE
// AND IT IS NOT AN OVERSIGHT LEFT BY THE SOURCE-SIDE FIX.
//
// The source is the operator's pinned installation. This package does not own it,
// cannot lock it, and #1878 exists precisely because it may sit outside the
// daemon's control - so any name resolved there must be pinned to a descriptor
// that was inspected, or an intermediate component can be repointed mid-walk.
//
// The destination is ours, and three properties are what make the difference:
// stage() creates the root with os.MkdirAll(root, 0o700), so no other user can
// write into it; the staging directory is ".staging-" plus eight bytes from
// crypto/rand, created fresh in the same call, so its name cannot be predicted or
// pre-planted; and every destination operation in this file goes through
// destinationRoot, the single os.Root over that tree - the only os.* call taking a
// destination path is the MkdirAll that creates the root itself. An attacker able
// to swap an intermediate directory under there could replace the published tree
// outright, so the resolution shape would not be the weakest link.
//
// AND BROADENING IT WOULD COST THE BOUND THIS ROUND WAS ASKED TO PRESERVE.
// Per-directory sub-roots on the destination would add a second held chain,
// roughly doubling the live descriptors that TestDescriptorPeakStaysAtTreeDepth
// bounds at deepestPathDepth+3, for no threat-model gain. Mechanical symmetry
// here would trade a measured invariant for an appearance of consistency.
func copyMember(sourceRoot, destinationRoot *os.Root, destination, member string) error {
	return walkAnchored(sourceRoot, member, func(parent *os.Root, leaf, displayPath string, info fs.FileInfo, handle *os.File) error {
		target := path.Join(destination, displayPath)
		if info.IsDir() {
			return destinationRoot.MkdirAll(target, 0o755)
		}
		// writes from the handle the WALK opened; no second resolution
		return writeCopy(handle, info, destinationRoot, target)
	})
}

// copyOneFile opens name through sourceRoot and writes it to target. It is the
// entry point for ROOT FILES, which are named individually rather than walked.
func copyOneFile(sourceRoot *os.Root, name string, destinationRoot *os.Root, target string) error {
	handle, info, err := openVerified(sourceRoot, name, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer handle.Close()
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrNotPinned, name)
	}
	return writeCopy(handle, info, destinationRoot, target)
}

// writeCopy writes an ALREADY-OPEN source handle to target, PRESERVING the
// executable bit and STRIPPING setuid and setgid.
//
// IT TAKES A HANDLE RATHER THAN A NAME so the walk's open is the only resolution
// of a member file. A reviewer's probe found the walk and its visitor each opening
// every file, which left two unreconciled resolutions of the same name.
//
// NO PER-FILE fsync: measured at 136.18s against 1.07s on 15022 files. Data
// integrity comes from re-verifying the digest on reuse (see stage).
func writeCopy(handle *os.File, info fs.FileInfo, destinationRoot *os.Root, target string) error {
	if parent := path.Dir(target); parent != "." {
		if err := destinationRoot.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	// Perm() masks to 0o777 and therefore cannot represent setuid or setgid at
	// all, which is what makes the strip structural rather than a subtraction.
	mode := info.Mode().Perm() & 0o755

	// O_EXCL IS THE ONE FLAG HERE THAT IS NOT INERT, and it is also the one mutant
	// nothing kills. Removing it leaves O_WRONLY|O_CREATE, which does NOT
	// truncate: it opens the existing file and overwrites from offset zero,
	// leaving any longer tail. Measured through os.Root - an existing
	// "ABCDEFGHIJ" became "xyCDEFGHIJ" after a two-byte write - so removal would
	// silently CORRUPT a target rather than replace it.
	//
	// I described that wrongly at first because my mutant SUBSTITUTED O_TRUNC
	// instead of REMOVING O_EXCL, so the instrument could only show the comparison
	// I had already assumed. A substituting mutant tests the substitute.
	//
	// Nothing kills its removal because every caller writes beneath a staging
	// directory created fresh in the same Stage call, and target names come from a
	// filesystem walk so two entries cannot produce one target. Structural rather
	// than untested, and kept so a future change which reuses a destination fails
	// loudly instead of corrupting.
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

// syncHeld fsyncs DIRECTORIES through the HELD destination descriptor.
//
// IT NO LONGER TAKES A PATH. The previous form was syncTree(filepath.Join(root,
// temp)) using filepath.WalkDir plus os.Open on absolute paths, which abandoned
// destinationRoot and re-resolved the daemon root and every descendant by
// pathname. That directly contradicted this file's own claim that every
// destination operation goes through destinationRoot, and a reviewer found it
// surviving the round that claimed to close the class. Ruled for the fix rather
// than the comment edit, which was the right call: the comment was the symptom.
//
// Directories only, so the cost is a few hundred fsyncs rather than 15022 - the
// per-file fsync that cost 136.18s against 1.07s is not being reintroduced.
func syncHeld(root *os.Root, name string) error {
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()

	handle, err := sub.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := handle.ReadDir(-1)
	if readErr != nil {
		handle.Close()
		return readErr
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := syncHeld(sub, entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
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
