package transcript

import (
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

// jobLogSegment is one tail segment of a job log that has a header, so its
// place in the run is known.
type jobLogSegment struct {
	seq  int64
	file *os.File
}

// openJobLogSegments opens the tail segments of the log at path that have a
// header, oldest first. A segment that does not exist, or has no header yet,
// is not part of the log.
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

// JobLogReader reads a retained job log as the one stream it records: the
// head, then the older and the newer tail segment. It is the only reader of
// the on-disk layout; every consumer of a job log opens it through OpenJobLog.
//
// The part sizes are taken at open. A log still being written can rotate a
// segment while it is read; the read then ends early at that segment rather
// than mixing two segments' bytes.
type JobLogReader struct {
	files  []*os.File
	sizes  []int64
	size   int64
	offset int64
}

// OpenJobLog opens the log at path with its tail segments. A missing head is
// os.ErrNotExist, as for os.Open.
func OpenJobLog(path string) (*JobLogReader, error) {
	head, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	segments, err := openJobLogSegments(path)
	if err != nil {
		_ = head.Close()
		return nil, err
	}
	r := &JobLogReader{files: []*os.File{head}}
	for _, segment := range segments {
		r.files = append(r.files, segment.file)
	}
	for _, file := range r.files {
		info, err := file.Stat()
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		r.sizes = append(r.sizes, info.Size())
		r.size += info.Size()
	}
	return r, nil
}

// Size is the length of the whole stream, as of OpenJobLog.
func (r *JobLogReader) Size() int64 { return r.size }

func (r *JobLogReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("job log: negative offset")
	}
	n := 0
	for i, file := range r.files {
		if len(p) == 0 {
			return n, nil
		}
		if off >= r.sizes[i] {
			off -= r.sizes[i]
			continue
		}
		want := min(int64(len(p)), r.sizes[i]-off)
		got, err := file.ReadAt(p[:want], off)
		n += got
		if int64(got) < want {
			// The part shrank under us: a rotation of a live log.
			if err == nil || errors.Is(err, io.EOF) {
				err = io.EOF
			}
			return n, err
		}
		p = p[got:]
		off = 0
	}
	if len(p) > 0 {
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
	var err error
	for _, file := range r.files {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	return err
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
		if segment.seq > cursor.seq {
			cursor.seq, cursor.offset = segment.seq, 0
			buffer.Reset()
		}
		data, err := io.ReadAll(io.NewSectionReader(segment.file, cursor.offset, 1<<62))
		if err != nil {
			return fed, err
		}
		// A segment rotated while it was read holds a newer segment's bytes
		// past the offset; leave it to the next pass, which lists it afresh.
		if seq, err := readJobLogSegmentSeq(segment.file); err != nil || seq != segment.seq {
			return fed, err
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
