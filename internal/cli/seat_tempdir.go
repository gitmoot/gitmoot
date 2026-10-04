package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// A read-only seat's TMPDIR (#2314).
//
// SHORT, because a Unix socket path is limited to 108 bytes. The seat's TMPDIR
// used to be <cache root>/tmp, which on a real host is 57+ bytes before a test
// adds anything. Go's t.TempDir appends up to 64 bytes of test name plus a
// random suffix and "/001", so a test that binds <t.TempDir()>/x.sock overran
// the limit and failed for a reason that had nothing to do with the code under
// review. /tmp/gmr-<8 hex> is 17 bytes and leaves room for that whole suffix.
// A short daemon TMPDIR is honoured as the parent, so an operator can move seat
// scratch space off a small /tmp.
//
// PRIVATE, because the temp parent is shared by every process on the host.
// Only this directory is granted; the parent itself stays read-only, so a seat
// cannot plant files for another process or read another review's scratch
// files.
//
// REGISTERED UNDER THE GITMOOT HOME, because a crashed daemon cannot run its
// own cleanup. Each directory gets a marker in seatTempRegistry naming the
// process that owns it and the directory's path. The registry is never granted
// to a seat, so a seat cannot point the sweep at another path, and the sweep
// only removes a directory whose base name is the marker's own gmr-<8 hex>.
const seatTempPrefix = "gmr-"

// seatTempParentMaxLen keeps a TMPDIR-derived <parent>/gmr-<8 hex> at most 29
// bytes, which still leaves a test 78 bytes of its own below the socket limit.
const seatTempParentMaxLen = 16

// seatTempParent is the daemon's TMPDIR when that is short enough for the
// seat's sockets, and /tmp otherwise. A long daemon TMPDIR is exactly the
// defect this exists to fix, so it is never used.
func seatTempParent() string {
	parent := os.TempDir()
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent || len(parent) > seatTempParentMaxLen {
		return "/tmp"
	}
	return parent
}

var seatTempNamePattern = regexp.MustCompile(`^gmr-[0-9a-f]{8}$`)

func seatTempRegistry(home string) string {
	return filepath.Join(home, "cache", "seat-tmp")
}

// seatTempDir is one seat's temp dir together with the Gitmoot home whose
// registry records it. The zero value is "no temp dir" and removes nothing.
type seatTempDir struct {
	home string
	dir  string
}

func (s seatTempDir) remove() error {
	if s.dir == "" {
		return nil
	}
	return removeSeatTempDir(s.home, s.dir)
}

// createSeatTempDir makes a fresh seat temp dir, mode 0700 and owned by this
// process's user (the user the seat runs as), and records it in the registry
// before it exists so a crash between the two steps leaves a marker, not an
// unowned directory.
func createSeatTempDir(home string) (string, error) {
	if _, err := sweepStaleSeatTempDirs(home); err != nil {
		fmt.Fprintf(os.Stderr, "gitmoot: read-only seat temp sweep: %v\n", err)
	}
	registry := seatTempRegistry(home)
	if err := os.MkdirAll(registry, 0o700); err != nil {
		return "", fmt.Errorf("create seat temp registry %q: %w", registry, err)
	}
	parent := seatTempParent()
	for range 16 {
		var suffix [4]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("generate seat temp name: %w", err)
		}
		name := seatTempPrefix + hex.EncodeToString(suffix[:])
		marker := filepath.Join(registry, name)
		file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("register seat temp dir %q: %w", name, err)
		}
		dir := filepath.Join(parent, name)
		_, writeErr := file.WriteString(strconv.Itoa(os.Getpid()) + "\n" + dir + "\n")
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return "", errors.Join(fmt.Errorf("register seat temp dir %q: %w", name, err), os.Remove(marker))
		}
		// Mkdir, never MkdirAll: an existing name (a planted directory or
		// symlink) is refused and another name is drawn.
		if err := os.Mkdir(dir, 0o700); err != nil {
			removeErr := os.Remove(marker)
			if errors.Is(err, fs.ErrExist) && removeErr == nil {
				continue
			}
			return "", errors.Join(fmt.Errorf("create seat temp dir %q: %w", dir, err), removeErr)
		}
		if err := verifySeatTempDir(dir); err != nil {
			return "", errors.Join(err, removeSeatTempDir(home, dir))
		}
		return dir, nil
	}
	return "", fmt.Errorf("create seat temp dir under %s: no free name after 16 attempts", parent)
}

// verifySeatTempDir proves the directory is what the grant will claim: a real
// directory, owned by the seat's user, readable by nobody else.
func verifySeatTempDir(dir string) error {
	// Mkdir honours the umask, which can only narrow 0700; set it exactly.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set seat temp dir %q mode: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect seat temp dir %q: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("seat temp dir %q is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("seat temp dir %q has mode %v, want 0700", dir, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("seat temp dir %q is owned by uid %d, want %d", dir, stat.Uid, os.Geteuid())
	}
	return nil
}

// removeSeatTempDir deletes a seat temp dir and then its registry marker. The
// marker is kept when removal fails, so the next sweep retries it.
func removeSeatTempDir(home, dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	name := filepath.Base(dir)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !seatTempNamePattern.MatchString(name) {
		return fmt.Errorf("refuse to remove %q: not a seat temp dir", dir)
	}
	if err := removeTreeForcibly(dir); err != nil {
		return fmt.Errorf("remove seat temp dir %q: %w", dir, err)
	}
	if err := os.Remove(filepath.Join(seatTempRegistry(home), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove seat temp marker %q: %w", name, err)
	}
	return nil
}

// removeTreeForcibly is os.RemoveAll that also handles directories a seat made
// read-only (the go command does that to module directories), which RemoveAll
// cannot delete when the daemon is not root.
func removeTreeForcibly(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// sweepStaleSeatTempDirs removes seat temp dirs whose owning process is gone:
// a daemon or foreground dispatch that crashed before its cleanup ran. A
// directory whose owner is still alive is left alone, and an unreadable marker
// is kept, so the failure mode is a leaked directory rather than deleting a
// live seat's files.
func sweepStaleSeatTempDirs(home string) (int, error) {
	registry := seatTempRegistry(home)
	entries, err := os.ReadDir(registry)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read seat temp registry %q: %w", registry, err)
	}
	removed := 0
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !seatTempNamePattern.MatchString(name) || !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(registry, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		pidText, dir, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid <= 0 {
			errs = append(errs, fmt.Errorf("seat temp marker %q has no owner pid", name))
			continue
		}
		dir = strings.TrimSpace(dir)
		if filepath.Base(dir) != name {
			errs = append(errs, fmt.Errorf("seat temp marker %q names directory %q", name, dir))
			continue
		}
		if seatTempOwnerAlive(pid) {
			continue
		}
		if err := removeSeatTempDir(home, dir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

func seatTempOwnerAlive(pid int) bool {
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
