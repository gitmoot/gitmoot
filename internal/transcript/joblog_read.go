package transcript

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"syscall"
	"time"
)

// jobLogSegment is one tail segment of a job log that had a header when it
// was listed, so its place in the run is known.
type jobLogSegment struct {
	seq  int64
	file *os.File
}

// openJobLogSegments opens the tail segments of the log at path that have a
// header, oldest first. A segment that does not exist, or has no header yet,
// is not part of the log. The numbers are read without a lock, so they are a
// listing only: copyJobLogSegment re-checks a segment's number before trusting
// its bytes.
func openJobLogSegments(path string) ([]jobLogSegment, error) {
	var segments []jobLogSegment
	for _, tailPath := range JobLogTailPaths(path) {
		file, err := os.Open(tailPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			closeJobLogSegments(segments)
			return nil, err
		}
		seq, err := readJobLogSegmentSeq(file)
		if err != nil || seq == 0 {
			_ = file.Close()
			if err != nil {
				closeJobLogSegments(segments)
				return nil, err
			}
			continue
		}
		segments = append(segments, jobLogSegment{seq: seq, file: file})
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].seq < segments[j].seq })
	return segments, nil
}

func closeJobLogSegments(segments []jobLogSegment) {
	for _, segment := range segments {
		_ = segment.file.Close()
	}
}

// copyJobLogSegment copies a segment's bytes from offset to its current end,
// under flock(LOCK_SH) on it, which rotation cannot take over (rotateTail
// reuses a segment only under LOCK_EX). It reports false, and copies nothing,
// when the segment is no longer generation seq: it was reused since it was
// listed. The lock is held for the copy alone, never while the caller uses
// the bytes, so a slow consumer cannot hold up a writer; the copy is at most
// one segment, JobLogTailSegmentBytes.
func copyJobLogSegment(file *os.File, seq, offset int64) (data []byte, same bool, err error) {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		return nil, false, err
	}
	defer func() {
		if unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err == nil {
			err = unlockErr
		}
	}()
	current, err := readJobLogSegmentSeq(file)
	if err != nil || current != seq {
		return nil, false, err
	}
	if jobLogSegmentLocked != nil {
		jobLogSegmentLocked()
	}
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() <= offset {
		return nil, true, nil
	}
	data = make([]byte, info.Size()-offset)
	n, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	return data[:n], true, nil
}

// jobLogSegmentLocked, when set by a test, runs while copyJobLogSegment holds
// a segment's shared lock, so a test can race a rotation against the copy.
var jobLogSegmentLocked func()

// jobLogSnapshotAttempts bounds how often OpenJobLog relists the segments
// when a rotation reused one between listing and copying. A rotation needs
// 4 MiB of new output, so a second attempt practically always succeeds.
const jobLogSnapshotAttempts = 8

// JobLogReader reads a retained job log as the one stream it records: the
// head, then the older and the newer tail segment. It is the only reader of
// the on-disk layout; every consumer of a job log opens it through OpenJobLog.
//
// It is a SNAPSHOT taken at open. The tail segments are copied then, each
// under a shared lock and checked to be one generation (copyJobLogSegment),
// because a writer reuses their files; after that the reader holds no lock,
// so it can never block a writer. The head needs no copy: a writer only ever
// appends to it, so its first Size bytes never change.
type JobLogReader struct {
	head     *os.File
	headSize int64
	tail     []byte
	offset   int64
}

// OpenJobLog opens the log at path with its tail segments. A missing head is
// os.ErrNotExist, as for os.Open.
func OpenJobLog(path string) (*JobLogReader, error) {
	head, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	tail, err := snapshotJobLogTail(path)
	if err != nil {
		_ = head.Close()
		return nil, err
	}
	// The head's size is taken AFTER the tail: the head gets its last line,
	// the marker, before the first segment exists, so a tail that was copied
	// always follows a head that is complete.
	info, err := head.Stat()
	if err != nil {
		_ = head.Close()
		return nil, err
	}
	return &JobLogReader{head: head, headSize: info.Size(), tail: tail}, nil
}

// snapshotJobLogTail copies the tail segments oldest first. Each is checked
// to still be the generation it was listed as; if one was reused in between,
// the listing is stale and is taken again. Copying the segments one at a time
// is consistent: a rotation can only reuse the OLDER one, which leaves the
// already-copied older segment and the still-listed newer one a contiguous
// stretch of the run.
func snapshotJobLogTail(path string) ([]byte, error) {
	for range jobLogSnapshotAttempts {
		segments, err := openJobLogSegments(path)
		if err != nil {
			return nil, err
		}
		var tail []byte
		consistent := true
		for _, segment := range segments {
			data, same, err := copyJobLogSegment(segment.file, segment.seq, 0)
			if err != nil {
				closeJobLogSegments(segments)
				return nil, err
			}
			if !same {
				consistent = false
				break
			}
			tail = append(tail, data...)
		}
		closeJobLogSegments(segments)
		if consistent {
			return tail, nil
		}
	}
	return nil, fmt.Errorf("job log %s: its tail segments kept rotating while being read", path)
}

// Size is the length of the whole stream, as of OpenJobLog.
func (r *JobLogReader) Size() int64 { return r.headSize + int64(len(r.tail)) }

func (r *JobLogReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("job log: negative offset")
	}
	n := 0
	if off < r.headSize {
		want := min(int64(len(p)), r.headSize-off)
		got, err := r.head.ReadAt(p[:want], off)
		n += got
		if int64(got) < want {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		p, off = p[got:], r.headSize
	}
	if len(p) == 0 {
		return n, nil
	}
	tailOff := off - r.headSize
	if tailOff >= int64(len(r.tail)) {
		return n, io.EOF
	}
	copied := copy(p, r.tail[tailOff:])
	n += copied
	if copied < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *JobLogReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.offset)
	r.offset += int64(n)
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

func (r *JobLogReader) Close() error {
	return r.head.Close()
}

// JobLogInfo is a job log's progress evidence: its total size and its latest
// write, over the head and both tail segments.
type JobLogInfo struct {
	Size    int64
	ModTime time.Time
}

// StatJobLog reports a job log's progress evidence. Once the head is full only
// the tail segments change, so the head alone would read as a silent run. A
// missing head is os.ErrNotExist; a missing segment is no evidence.
func StatJobLog(path string) (JobLogInfo, error) {
	head, err := os.Stat(path)
	if err != nil {
		return JobLogInfo{}, err
	}
	if head.IsDir() {
		return JobLogInfo{}, errors.New("job log is a directory")
	}
	info := JobLogInfo{Size: head.Size(), ModTime: head.ModTime()}
	for _, tailPath := range JobLogTailPaths(path) {
		tail, err := os.Stat(tailPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return JobLogInfo{}, err
		}
		info.Size += tail.Size()
		if tail.ModTime().After(info.ModTime) {
			info.ModTime = tail.ModTime()
		}
	}
	return info, nil
}

// RemoveJobLog removes the log at path and its tail segments with remove. The
// head's error is returned; a segment that is already gone is not an error.
func RemoveJobLog(path string, remove func(string) error) error {
	err := remove(path)
	for _, tailPath := range JobLogTailPaths(path) {
		if tailErr := remove(tailPath); tailErr != nil && !errors.Is(tailErr, os.ErrNotExist) && err == nil {
			err = tailErr
		}
	}
	return err
}

// jobLogTailStarted reports whether the log at path has gone past its head.
func jobLogTailStarted(path string) bool {
	for _, tailPath := range JobLogTailPaths(path) {
		if info, err := os.Stat(tailPath); err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// jobLogTailCursor is a follower's place in the tail: a segment number and an
// offset in it.
type jobLogTailCursor struct {
	seq    int64
	offset int64
}

// followJobLogTail feeds buffer everything the tail holds past cursor, in
// order, and reports whether it fed anything. A segment the cursor was in that
// has been rotated away is skipped to the oldest newer one: its unread lines
// are gone from disk.
//
// Each step copies one segment under its shared lock (copyJobLogSegment),
// checked to be the generation the cursor names, and feeds the bytes only
// after the lock is released: a follower whose consumer stalls holds no lock,
// and bytes of two generations of one file are never joined.
func followJobLogTail(path string, cursor *jobLogTailCursor, buffer *lineBuffer) (bool, error) {
	segments, err := openJobLogSegments(path)
	if err != nil {
		return false, err
	}
	defer closeJobLogSegments(segments)
	fed := false
	for _, segment := range segments {
		if segment.seq < cursor.seq {
			continue
		}
		offset := cursor.offset
		if segment.seq > cursor.seq {
			offset = 0
		}
		data, same, err := copyJobLogSegment(segment.file, segment.seq, offset)
		if err != nil {
			return fed, err
		}
		if !same {
			// Reused since it was listed; the next pass lists it afresh.
			return fed, nil
		}
		if segment.seq > cursor.seq {
			cursor.seq, cursor.offset = segment.seq, 0
			buffer.Reset()
		}
		if len(data) == 0 {
			continue
		}
		cursor.offset += int64(len(data))
		fed = true
		if err := buffer.Write(data); err != nil {
			return fed, err
		}
	}
	return fed, nil
}
