package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const (
	// readOnlyWorktreeVerifyWorkers reads tracked files in parallel: on the
	// sshfs worktree mount each file costs a network round trip, and 1,325
	// files took 27.6s read one at a time but 3.5s with 64 in parallel (#2517).
	readOnlyWorktreeVerifyWorkers = 32
	// readOnlyWorktreeVerifyMaxFileBytes leaves larger files to git's own
	// content check instead of reading them here.
	readOnlyWorktreeVerifyMaxFileBytes = 64 << 20
	readOnlyWorktreeIndexListMaxBytes  = 64 << 20
)

// readOnlyWorktreeVerifySlots bounds file system operations in flight for
// content verification across ALL captures in the process (#2518 review). A
// read stuck on a hung mount outlives its capture (it cannot be cancelled), so
// without a shared bound every later timed-out capture would leave another
// batch of blocked goroutines and open files behind. A worker takes a slot
// before touching the file system and gives up when its capture ends while
// waiting, so at most this many reads (and open files) are ever outstanding.
var readOnlyWorktreeVerifySlots = make(chan struct{}, readOnlyWorktreeVerifyWorkers)

// readOnlyWorktreeIndexEntry is one `git ls-files -s -t` record.
type readOnlyWorktreeIndexEntry struct {
	tag, mode, oid, stage, path string
}

// invalidateUnverifiedIndexEntries makes the capture's content detection
// independent of stat data (#2517). The capture runs git with
// core.checkStat=minimal so it does not re-read every file on the sshfs mount,
// but then git trusts whole-second mtime and size, and an edit that keeps both
// (same-size rewrite in the same second, or with its mtime restored) would go
// unreported. So every tracked file is first compared by CONTENT with its index
// blob, in parallel; each entry not proven identical gets its cached stat data
// cleared in the capture's private index copy (same mode and blob), which
// forces git to compare that file's content. Verified entries are left alone:
// whatever git decides for them is right, because they equal the index.
//
// Entries this does not handle (skip-worktree, unmerged, gitlinks) are left to
// git exactly as before. Any doubt (unreadable, oversized, type or mode change,
// hash mismatch, which includes files git normalises through attributes)
// counts as unverified: that only costs a git content check, never a missed
// edit.
func (s *readOnlyWorktreeGitSandbox) invalidateUnverifiedIndexEntries(ctx context.Context) error {
	listing := &boundedCountingBuffer{limit: readOnlyWorktreeIndexListMaxBytes}
	var stderr boundedCountingBuffer
	stderr.limit = 4096
	args := []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "ls-files", "-s", "-t", "-z"}
	if err := runReadOnlyWorktreeGit(ctx, s.runner, s.env, s.worktree, listing, &stderr, args...); err != nil {
		return fmt.Errorf("git ls-files in %s: %w: %s", s.worktree, err, strings.TrimSpace(stderr.String()))
	}
	if listing.dropped != 0 {
		return fmt.Errorf("git ls-files in %s: index listing exceeds %d bytes", s.worktree, readOnlyWorktreeIndexListMaxBytes)
	}
	entries, err := parseReadOnlyWorktreeIndexEntries(listing.String())
	if err != nil {
		return fmt.Errorf("git ls-files in %s: %w", s.worktree, err)
	}
	root, err := os.OpenRoot(s.worktree)
	if err != nil {
		return fmt.Errorf("open worktree for content verification: %w", err)
	}
	defer root.Close()
	unverified, err := unverifiedReadOnlyWorktreeEntries(ctx, root, entries)
	if err != nil {
		return err
	}
	const perCall = 100
	for start := 0; start < len(unverified); start += perCall {
		batch := unverified[start:min(start+perCall, len(unverified))]
		update := []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "update-index"}
		for _, entry := range batch {
			update = append(update, "--cacheinfo", entry.mode, entry.oid, entry.path)
		}
		var out, errOut boundedCountingBuffer
		out.limit, errOut.limit = 4096, 4096
		if err := runReadOnlyWorktreeGit(ctx, s.runner, s.env, s.worktree, &out, &errOut, update...); err != nil {
			return fmt.Errorf("git update-index in %s: %w: %s", s.worktree, err, strings.TrimSpace(errOut.String()))
		}
	}
	return nil
}

func parseReadOnlyWorktreeIndexEntries(listing string) ([]readOnlyWorktreeIndexEntry, error) {
	var entries []readOnlyWorktreeIndexEntry
	for _, record := range strings.Split(listing, "\x00") {
		if record == "" {
			continue
		}
		meta, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || path == "" {
			return nil, fmt.Errorf("unparseable index record %q", record)
		}
		entries = append(entries, readOnlyWorktreeIndexEntry{tag: fields[0], mode: fields[1], oid: fields[2], stage: fields[3], path: path})
	}
	return entries, nil
}

// unverifiedReadOnlyWorktreeEntries returns the ordinary tracked entries whose
// worktree content was not proven identical to their index blob. It returns as
// soon as ctx ends (#2518 review): a read already in progress on a stalled
// network mount cannot be force-cancelled, so, like the index copy, this stops
// waiting for it instead of holding the capture (and the daemon tick that runs
// it) past its deadline. Workers still running then exit on their own: they
// stop taking work, stop hashing between chunks, and write only to state this
// call no longer reads.
func unverifiedReadOnlyWorktreeEntries(ctx context.Context, root *os.Root, entries []readOnlyWorktreeIndexEntry) ([]readOnlyWorktreeIndexEntry, error) {
	// Read once here: workers may outlive this call after a deadline.
	verify := verifyReadOnlyWorktreeEntry
	work := make(chan readOnlyWorktreeIndexEntry)
	var (
		mu         sync.Mutex
		unverified []readOnlyWorktreeIndexEntry
		wg         sync.WaitGroup
	)
	for range readOnlyWorktreeVerifyWorkers {
		wg.Go(func() {
			for entry := range work {
				select {
				case readOnlyWorktreeVerifySlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				matched := verify(ctx, root, entry)
				<-readOnlyWorktreeVerifySlots
				if !matched {
					mu.Lock()
					unverified = append(unverified, entry)
					mu.Unlock()
				}
			}
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(work)
		for _, entry := range entries {
			if entry.tag != "H" || entry.stage != "0" || (entry.mode != "100644" && entry.mode != "100755" && entry.mode != "120000") {
				continue
			}
			select {
			case work <- entry:
			case <-ctx.Done():
				return
			}
		}
	}()
	finished := make(chan struct{})
	go func() {
		<-done
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
	}
	// Checked after both: an entry the feeder dropped on cancellation was never
	// verified, so a cancelled run must not report success.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("verify worktree content: %w", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]readOnlyWorktreeIndexEntry(nil), unverified...), nil
}

// verifyReadOnlyWorktreeEntry is readOnlyWorktreeEntryMatches; a var only so a
// test can stand in for a read that hangs on a stalled mount.
var verifyReadOnlyWorktreeEntry = readOnlyWorktreeEntryMatches

// readOnlyWorktreeEntryMatches reports whether the worktree holds exactly the
// entry's blob with the entry's file type and executable bit, as git compares
// them with core.filemode on. Reads stay inside the worktree (os.Root), never
// follow a final symlink, and never open anything but a regular file.
func readOnlyWorktreeEntryMatches(ctx context.Context, root *os.Root, entry readOnlyWorktreeIndexEntry) bool {
	if ctx.Err() != nil {
		return false
	}
	info, err := root.Lstat(entry.path)
	if err != nil {
		return false
	}
	var content io.Reader
	var size int64
	switch entry.mode {
	case "120000":
		if info.Mode()&os.ModeSymlink == 0 {
			return false
		}
		target, err := root.Readlink(entry.path)
		if err != nil {
			return false
		}
		content, size = strings.NewReader(target), int64(len(target))
	default:
		if !info.Mode().IsRegular() || (info.Mode().Perm()&0o111 != 0) != (entry.mode == "100755") ||
			info.Size() > readOnlyWorktreeVerifyMaxFileBytes {
			return false
		}
		file, err := root.OpenFile(entry.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || opened.Size() != info.Size() {
			return false
		}
		content, size = file, info.Size()
	}
	var hasher hash.Hash
	switch len(entry.oid) {
	case sha1.Size * 2:
		hasher = sha1.New()
	case sha256.Size * 2:
		hasher = sha256.New()
	default:
		return false
	}
	hasher.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	copied, err := io.Copy(hasher, contextReader{ctx: ctx, r: io.LimitReader(content, size+1)})
	if err != nil || copied != size {
		return false
	}
	want, err := hex.DecodeString(entry.oid)
	return err == nil && bytes.Equal(hasher.Sum(nil), want)
}

// contextReader stops a long hash between reads once ctx ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
