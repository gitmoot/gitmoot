package toolchain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RUNTIME COPIES ARE COLLECTED BY DISUSE, NEVER BY A RETENTION LIST (#2382).
//
// Every staging pass publishes <name>-<fingerprint> under RuntimeRoot and
// nothing ever removed one, so each provider upgrade left a full copy behind:
// the production home held 19 omp copies (4.9 GB) from five weeks of upgrades.
// The fingerprint is a content digest, so neither the suffix nor the directory
// mtime says which copy is current.
//
// The collector therefore never decides "current". A copy is removed only when
// ALL of these hold:
//
//   - it has not been staged for at least MinIdle. Every StageRuntime call,
//     including one that reuses an existing copy, refreshes a last-used stamp
//     under the shared root lock, and the daemon stages a seat's runtimes at the
//     start of every job, so a copy any job started on within MinIdle is kept;
//   - no live process references it through its executable, working directory,
//     command line or mapped files (a long-running seat older than MinIdle); a
//     process scan that cannot be completed keeps every copy;
//   - under the EXCLUSIVE root lock its stamp is still old. Staging holds the
//     shared lock while it refreshes the stamp and validates or publishes the
//     copy, so a collection pass either finishes before a staging pass looks at
//     the copy (which then republishes it) or sees the refreshed stamp.
//
// A copy removed while still current is not an error: the next job republishes
// it from the operator's installation. Unavailable-runtime shims are a few
// bytes and are never collected.
const (
	runtimeLastUsedDir    = ".last-used"
	runtimeGCLockName     = ".gc.lock"
	runtimeTrashPrefix    = ".trash-"
	DefaultRuntimeMinIdle = 7 * 24 * time.Hour
)

// collectableRuntimeName matches a published copy, <name>-<16 hex fingerprint>.
// An in-progress <published>.staging-<suffix> tree is never a candidate.
var collectableRuntimeName = regexp.MustCompile(`^[A-Za-z0-9._-]+-[0-9a-f]{16}$`)

// lockRuntimeRoot takes the cross-process staging/collection lock on root.
// Staging takes it shared; collection takes it exclusive.
func lockRuntimeRoot(root string, exclusive bool) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime root %q: %w", root, err)
	}
	file, err := os.OpenFile(filepath.Join(root, runtimeGCLockName), os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime root lock: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err = syscall.Flock(int(file.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock runtime root: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// markRuntimeUsed refreshes the last-used stamp of one published copy. The
// stamp lives outside the published tree, whose members are digest-validated.
func markRuntimeUsed(root string, publishedName string, now time.Time) error {
	dir := filepath.Join(root, runtimeLastUsedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime last-used dir: %w", err)
	}
	stamp := filepath.Join(dir, publishedName)
	file, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create runtime last-used stamp: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runtime last-used stamp: %w", err)
	}
	if err := os.Chtimes(stamp, now, now); err != nil {
		return fmt.Errorf("refresh runtime last-used stamp: %w", err)
	}
	return nil
}

// RuntimeCopy describes one published copy in a collection report.
type RuntimeCopy struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	LastUsed time.Time `json:"last_used"`
	// Stamped is false for a copy published before last-used stamps existed;
	// its LastUsed is the directory's modification time.
	Stamped bool   `json:"stamped"`
	Bytes   int64  `json:"bytes"`
	Reason  string `json:"reason,omitempty"`
}

// RuntimeCollectOptions controls one collection pass.
type RuntimeCollectOptions struct {
	MinIdle time.Duration
	Now     time.Time
	DryRun  bool
	// ProcRoot is the procfs mount to scan; empty means /proc.
	ProcRoot string
}

// RuntimeCollectReport is the outcome of one pass. Removed lists copies that
// were removed (or, for a dry run, would be); Kept lists the others with why.
type RuntimeCollectReport struct {
	Removed      []RuntimeCopy `json:"removed"`
	Kept         []RuntimeCopy `json:"kept"`
	RemovedBytes int64         `json:"removed_bytes"`
	DryRun       bool          `json:"dry_run"`
	// Incomplete names why nothing was removed when the process scan or the
	// root could not be read completely.
	Incomplete string `json:"incomplete,omitempty"`
}

// CollectRuntimes removes published runtime copies that are idle and
// unreferenced, under the rules at the top of this file.
func CollectRuntimes(gitmootHome string, options RuntimeCollectOptions) (RuntimeCollectReport, error) {
	report := RuntimeCollectReport{DryRun: options.DryRun}
	if options.MinIdle <= 0 {
		return report, fmt.Errorf("runtime collection needs a positive minimum idle time, got %s", options.MinIdle)
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	procRoot := options.ProcRoot
	if procRoot == "" {
		procRoot = "/proc"
	}
	root := RuntimeRoot(gitmootHome)
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return report, nil
	} else if err != nil {
		return report, fmt.Errorf("stat runtime root: %w", err)
	}

	copies, err := listRuntimeCopies(root)
	if err != nil {
		return report, err
	}
	var idle []RuntimeCopy
	for _, copy := range copies {
		if now.Sub(copy.LastUsed) < options.MinIdle {
			copy.Reason = "staged within the minimum idle time"
			report.Kept = append(report.Kept, copy)
			continue
		}
		idle = append(idle, copy)
	}
	if len(idle) == 0 {
		return report, removeRuntimeTrash(root)
	}

	paths := make([]string, len(idle))
	for i, copy := range idle {
		paths[i] = copy.Path
	}
	referenced, scanErr := runtimeCopiesReferencedByProcesses(procRoot, paths)
	if scanErr != nil {
		report.Incomplete = scanErr.Error()
		for _, copy := range idle {
			copy.Reason = "process scan incomplete"
			report.Kept = append(report.Kept, copy)
		}
		return report, nil
	}
	var candidates []RuntimeCopy
	for _, copy := range idle {
		if referenced[copy.Path] {
			copy.Reason = "referenced by a live process"
			report.Kept = append(report.Kept, copy)
			continue
		}
		copy.Bytes = treeBytes(copy.Path)
		candidates = append(candidates, copy)
	}
	if options.DryRun {
		for _, copy := range candidates {
			report.Removed = append(report.Removed, copy)
			report.RemovedBytes += copy.Bytes
		}
		return report, nil
	}

	unlock, err := lockRuntimeRoot(root, true)
	if err != nil {
		return report, err
	}
	var renameErrs []error
	for _, copy := range candidates {
		current, err := runtimeLastUsed(root, copy.Name, copy.Path)
		if err != nil {
			copy.Reason = "could not re-read last-used under the lock"
			report.Kept = append(report.Kept, copy)
			continue
		}
		if now.Sub(current) < options.MinIdle {
			copy.Reason = "staged while the pass was running"
			report.Kept = append(report.Kept, copy)
			continue
		}
		trash := filepath.Join(root, runtimeTrashPrefix+copy.Name+"-"+strconv.FormatInt(now.UnixNano(), 36))
		if err := os.Rename(copy.Path, trash); err != nil {
			renameErrs = append(renameErrs, fmt.Errorf("retire %s: %w", copy.Name, err))
			copy.Reason = "could not be retired"
			report.Kept = append(report.Kept, copy)
			continue
		}
		_ = os.Remove(filepath.Join(root, runtimeLastUsedDir, copy.Name))
		report.Removed = append(report.Removed, copy)
		report.RemovedBytes += copy.Bytes
	}
	unlock()
	// The retired trees are no longer reachable under their published names, so
	// deleting them needs no lock; a failure leaves a .trash- entry that the
	// next pass removes.
	if err := removeRuntimeTrash(root); err != nil {
		renameErrs = append(renameErrs, err)
	}
	return report, errors.Join(renameErrs...)
}

func listRuntimeCopies(root string) ([]RuntimeCopy, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read runtime root: %w", err)
	}
	var copies []RuntimeCopy
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || strings.Contains(name, ".staging-") || !collectableRuntimeName.MatchString(name) {
			continue
		}
		path := filepath.Join(root, name)
		lastUsed, stamped, err := runtimeLastUsedStamp(root, name, path)
		if err != nil {
			return nil, err
		}
		copies = append(copies, RuntimeCopy{Name: name, Path: path, LastUsed: lastUsed, Stamped: stamped})
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].LastUsed.Before(copies[j].LastUsed) })
	return copies, nil
}

func runtimeLastUsed(root, name, path string) (time.Time, error) {
	lastUsed, _, err := runtimeLastUsedStamp(root, name, path)
	return lastUsed, err
}

// runtimeLastUsedStamp returns the stamp's time, or for an unstamped copy
// published before stamps existed, the copy directory's modification time.
func runtimeLastUsedStamp(root, name, path string) (time.Time, bool, error) {
	info, err := os.Stat(filepath.Join(root, runtimeLastUsedDir, name))
	if err == nil {
		return info.ModTime(), true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, fmt.Errorf("stat last-used stamp of %s: %w", name, err)
	}
	info, err = os.Stat(path)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("stat runtime copy %s: %w", name, err)
	}
	return info.ModTime(), false, nil
}

func removeRuntimeTrash(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read runtime root: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), runtimeTrashPrefix) {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				errs = append(errs, fmt.Errorf("remove retired runtime copy %s: %w", entry.Name(), err))
			}
		}
	}
	return errors.Join(errs...)
}

func treeBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// runtimeCopiesReferencedByProcesses reports which of copies a live process
// references through /proc/<pid>/{exe,cwd,cmdline,maps}. A process that exits
// mid-scan is skipped; any other read failure makes the whole scan fail, so
// the caller keeps every copy rather than guessing.
func runtimeCopiesReferencedByProcesses(procRoot string, copies []string) (map[string]bool, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	referenced := make(map[string]bool)
	match := func(path string) {
		path = strings.TrimSuffix(path, " (deleted)")
		for _, copy := range copies {
			if path == copy || strings.HasPrefix(path, copy+string(filepath.Separator)) {
				referenced[copy] = true
			}
		}
	}
	gone := func(err error) bool {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		for _, link := range []string{"exe", "cwd"} {
			target, err := os.Readlink(filepath.Join(dir, link))
			if err != nil {
				// Kernel threads have no exe or cwd, and a process may exit.
				if gone(err) {
					continue
				}
				return nil, fmt.Errorf("read %s/%s: %w", dir, link, err)
			}
			match(target)
		}
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			if gone(err) {
				continue
			}
			return nil, fmt.Errorf("read %s/cmdline: %w", dir, err)
		}
		for _, arg := range strings.Split(string(cmdline), "\x00") {
			if filepath.IsAbs(arg) {
				match(arg)
			}
		}
		maps, err := os.ReadFile(filepath.Join(dir, "maps"))
		if err != nil {
			if gone(err) {
				continue
			}
			return nil, fmt.Errorf("read %s/maps: %w", dir, err)
		}
		for _, line := range strings.Split(string(maps), "\n") {
			if i := strings.IndexByte(line, '/'); i >= 0 {
				match(line[i:])
			}
		}
	}
	return referenced, nil
}
