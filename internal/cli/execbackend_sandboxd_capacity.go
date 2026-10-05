package cli

import (
	"context"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// sandboxdCapacityCacheTTL bounds how stale a capacity report admission may
// use. Enrolling a worker shows up within one TTL with no config change; the
// guarded INSERT and sandboxd's own 409 keep a stale report from over-admitting.
const sandboxdCapacityCacheTTL = 10 * time.Second

type sandboxdCapacityCacheEntry struct {
	capacity e2b.Capacity
	readAt   time.Time
}

// sandboxdCapacityCache is process-wide, keyed by gateway base URL plus an API
// key digest. Only successful reports are cached: an error is re-read on the
// next provision so a recovered gateway is seen at once.
type sandboxdCapacityCache struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]sandboxdCapacityCacheEntry
}

var sandboxdCapacityReports = &sandboxdCapacityCache{now: time.Now, entries: map[string]sandboxdCapacityCacheEntry{}}

// get returns the cached report for key, reading it with read when absent or
// older than the TTL. Concurrent misses may each read; the last one wins.
func (c *sandboxdCapacityCache) get(ctx context.Context, key string, read func(context.Context) (e2b.Capacity, error)) (e2b.Capacity, error) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	now := c.now()
	c.mu.Unlock()
	if ok && now.Sub(entry.readAt) < sandboxdCapacityCacheTTL {
		return entry.capacity, nil
	}
	capacity, err := read(ctx)
	if err != nil {
		return e2b.Capacity{}, err
	}
	c.mu.Lock()
	c.entries[key] = sandboxdCapacityCacheEntry{capacity: capacity, readAt: c.now()}
	c.mu.Unlock()
	return capacity, nil
}

// invalidate drops key's report; sandboxd's create 409 proves it stale.
func (c *sandboxdCapacityCache) invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// sandboxdCapacitySource is one sandboxd ledger's admission input: the
// gateway's capacity report for the template its jobs create, bounded by the
// optional operator ceiling.
type sandboxdCapacitySource struct {
	cache    *sandboxdCapacityCache
	key      string
	read     func(context.Context) (e2b.Capacity, error)
	ceiling  int
	template string
}

// policy reads (or reuses) the report and turns it into this provision's
// admission policy, or a refusal that writes no row.
func (s *sandboxdCapacitySource) policy(ctx context.Context) (db.ExecBackendCostCap, error) {
	capacity, err := s.cache.get(ctx, s.key, s.read)
	return execBackendSandboxdCap(s.ceiling, capacity, err, s.template)
}

// invalidate drops the cached report after sandboxd refused a create.
func (s *sandboxdCapacitySource) invalidate() { s.cache.invalidate(s.key) }
