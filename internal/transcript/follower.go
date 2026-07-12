package transcript

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const MaxLogicalLineBytes = 1024 * 1024

// FollowOptions controls polling and supplies the job-settlement predicate.
type FollowOptions struct {
	PollInterval time.Duration
	Settled      func(context.Context) (bool, error)
}

// Follow reads path from offset zero with tail -F-style create retry, emits only
// complete logical lines while the job is live, and flushes the final partial
// line after the job settles and the file has been drained to EOF.
func Follow(ctx context.Context, path string, opts FollowOptions, onLine func(string) error) error {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 100 * time.Millisecond
	}
	if opts.Settled == nil {
		return errors.New("transcript follower requires a settlement predicate")
	}

	var file *os.File
	for file == nil {
		opened, err := os.Open(path)
		if err == nil {
			file = opened
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("open log: %w", err)
		}
		settled, settleErr := opts.Settled(ctx)
		if settleErr != nil {
			return fmt.Errorf("poll job state: %w", settleErr)
		}
		if settled {
			return fmt.Errorf("open log: %w", err)
		}
		if err := waitPoll(ctx, opts.PollInterval); err != nil {
			return err
		}
	}
	defer func() { _ = file.Close() }()

	buffer := newLineBuffer(MaxLogicalLineBytes, onLine)
	chunk := make([]byte, 32*1024)
	for {
		n, err := file.Read(chunk)
		if n > 0 {
			if feedErr := buffer.Write(chunk[:n]); feedErr != nil {
				return feedErr
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read log: %w", err)
		}
		if n > 0 {
			continue
		}
		changed, changeErr := followFileChanged(file, path)
		if changeErr != nil {
			return changeErr
		}
		if changed {
			reopened, openErr := os.Open(path)
			if openErr != nil {
				if !errors.Is(openErr, os.ErrNotExist) {
					return fmt.Errorf("reopen log: %w", openErr)
				}
			} else {
				_ = file.Close()
				file = reopened
				buffer.Reset()
				continue
			}
		}

		settled, settleErr := opts.Settled(ctx)
		if settleErr != nil {
			return fmt.Errorf("poll job state: %w", settleErr)
		}
		if settled {
			// The state transition is the stop signal. Drain once more to the
			// current EOF before flushing a final unterminated logical line.
			for {
				n, drainErr := file.Read(chunk)
				if n > 0 {
					if feedErr := buffer.Write(chunk[:n]); feedErr != nil {
						return feedErr
					}
				}
				if drainErr != nil && !errors.Is(drainErr, io.EOF) {
					return fmt.Errorf("read log: %w", drainErr)
				}
				if n == 0 {
					return buffer.Flush()
				}
			}
		}
		if err := waitPoll(ctx, opts.PollInterval); err != nil {
			return err
		}
	}
}

// followFileChanged detects both path replacement (the path now names a
// different file) and in-place truncation behind the current read offset.
func followFileChanged(file *os.File, path string) (bool, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat log: %w", err)
	}
	openInfo, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat open log: %w", err)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, fmt.Errorf("read log offset: %w", err)
	}
	return !os.SameFile(openInfo, pathInfo) || pathInfo.Size() < offset, nil
}

func waitPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type lineBuffer struct {
	limit int
	line  []byte
	emit  func(string) error
	drop  bool
}

func newLineBuffer(limit int, emit func(string) error) *lineBuffer {
	return &lineBuffer{limit: limit, emit: emit}
}

func (b *lineBuffer) Write(data []byte) error {
	for _, ch := range data {
		if ch == '\n' {
			if err := b.emit(string(b.line)); err != nil {
				return err
			}
			b.line = b.line[:0]
			b.drop = false
			continue
		}
		if len(b.line) < b.limit {
			b.line = append(b.line, ch)
		} else {
			b.drop = true
		}
	}
	return nil
}

func (b *lineBuffer) Flush() error {
	if len(b.line) == 0 && !b.drop {
		return nil
	}
	err := b.emit(string(b.line))
	b.line = b.line[:0]
	b.drop = false
	return err
}

func (b *lineBuffer) Reset() {
	b.line = b.line[:0]
	b.drop = false
}
