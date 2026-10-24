package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// In-memory databases cannot be shared across processes. A process-wide fence
// also covers separate Store handles opening the same named memory database.
var memoryJobLifecycleFence sync.RWMutex

// ErrCleanupLifecycleBusy means an enclosing lifecycle operation still owns the
// shared fence. Automatic cleanup must defer, not attempt a lock upgrade.
var ErrCleanupLifecycleBusy = errors.New("cleanup cannot upgrade a lifecycle mutation fence")

type jobLifecycleFenceKey struct{}

type jobLifecycleLease struct {
	store     *Store
	exclusive bool
	refs      atomic.Int32
	unlock    func()
}

func (l *jobLifecycleLease) retain() bool {
	for n := l.refs.Load(); n > 0; n = l.refs.Load() {
		if l.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
	return false
}

func (l *jobLifecycleLease) release() {
	if l.refs.Add(-1) == 0 {
		l.unlock()
	}
}

// AcquireJobLifecycle fences a complete read/prepare/write lifecycle operation
// against destructive cleanup. Pass the returned context to nested store calls;
// each acquisition must be released exactly once. Nested operations retain the
// lease, so a child cannot accidentally outlive the kernel lock protecting it.
func (s *Store) AcquireJobLifecycle(ctx context.Context) (context.Context, func(), error) {
	return s.acquireJobLifecycle(ctx, false)
}

// AcquireCleanupLifecycle excludes lifecycle mutations across processes until
// cleanup has finished reconciling filesystem effects and durable receipts.
// Acquire it before checkout/resource locks, never by upgrading a shared lease.
func (s *Store) AcquireCleanupLifecycle(ctx context.Context) (context.Context, func(), error) {
	return s.acquireJobLifecycle(ctx, true)
}

func (s *Store) acquireJobLifecycle(ctx context.Context, exclusive bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if lease, ok := ctx.Value(jobLifecycleFenceKey{}).(*jobLifecycleLease); ok && lease.store == s {
		if exclusive && !lease.exclusive && lease.refs.Load() > 0 {
			return ctx, nil, ErrCleanupLifecycleBusy
		}
		if lease.retain() {
			return ctx, lease.release, nil
		}
	}

	path, memory, err := s.jobLifecycleIdentity(ctx)
	if err != nil {
		return ctx, nil, err
	}

	var tryLock func() (bool, error)
	var unlock func()
	var file *os.File
	if memory {
		tryLock = func() (bool, error) { return memoryJobLifecycleFence.TryRLock(), nil }
		unlock = memoryJobLifecycleFence.RUnlock
		if exclusive {
			tryLock = func() (bool, error) { return memoryJobLifecycleFence.TryLock(), nil }
			unlock = memoryJobLifecycleFence.Unlock
		}
	} else {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return ctx, nil, fmt.Errorf("open cleanup lifecycle fence: %w", err)
		}
		mode := syscall.LOCK_SH | syscall.LOCK_NB
		if exclusive {
			mode = syscall.LOCK_EX | syscall.LOCK_NB
		}
		tryLock = func() (bool, error) {
			err := syscall.Flock(int(file.Fd()), mode)
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				return false, nil
			}
			return err == nil, err
		}
		// Never unlink the sidecar: another process may already have it open.
		unlock = func() { _ = file.Close() }
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			if file != nil {
				_ = file.Close()
			}
			return ctx, nil, err
		}
		locked, err := tryLock()
		if err != nil {
			if file != nil {
				_ = file.Close()
			}
			return ctx, nil, fmt.Errorf("acquire cleanup lifecycle fence: %w", err)
		}
		if locked {
			lease := &jobLifecycleLease{store: s, exclusive: exclusive, unlock: unlock}
			lease.refs.Store(1)
			return context.WithValue(ctx, jobLifecycleFenceKey{}, lease), lease.release, nil
		}
		if timer == nil {
			timer = time.NewTimer(10 * time.Millisecond)
		} else {
			timer.Reset(10 * time.Millisecond)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Store) jobLifecycleIdentity(ctx context.Context) (string, bool, error) {
	s.lifecycleIdentityMu.Lock()
	defer s.lifecycleIdentityMu.Unlock()
	if s.lifecycleFencePath != "" || s.lifecycleMemory {
		return s.lifecycleFencePath, s.lifecycleMemory, nil
	}
	path := s.path
	if path == "" {
		// Migration callers may construct a Store around an already-open DB.
		// Ask SQLite for that connection's identity rather than invent a path.
		if err := s.db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
			return "", false, fmt.Errorf("cleanup lifecycle database identity: %w", err)
		}
	}
	memory := path == "" || path == ":memory:"
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil {
			return "", false, fmt.Errorf("cleanup lifecycle database URI: %w", err)
		}
		path = u.Path
		if u.Opaque != "" {
			path = u.Opaque
		}
		memory = path == ":memory:" || u.Query().Get("mode") == "memory"
	}
	if memory {
		s.lifecycleMemory = true
		return "", true, nil
	}
	// A Store's database identity is fixed for its lifetime. Resolve aliases
	// once; do not perform filesystem walks on every job mutation.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, fmt.Errorf("cleanup lifecycle database identity: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", false, err
	}
	s.lifecycleFencePath = resolved + ".cleanup-lifecycle.lock"
	return s.lifecycleFencePath, false, nil
}
