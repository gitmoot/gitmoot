package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gitmoot/gitmoot/internal/db"
)

// WorktreeRefIndex answers the path_referenced cleanup guard: does any OTHER
// job, in any state, record a worktree_path that cleans to this path? The zero
// value is ready. It reads every job's worktree_path once, on its first
// question, and before each later answer reads only the rows inserted since.
// It is safe for concurrent use; use one index with one store.
//
// #2691: the guard used to decode every payload that mentions worktree_path
// (10,611 rows, 257 MB on the live host) for each question, up to three times
// per candidate, so a reclaim pass over N candidates made ~3N full scans.
//
// A held index may only REFUSE more than a fresh read, never less:
//   - A new row is seen: rowids only grow (production code never deletes a
//     job and SQLite serializes writers), so the rowid range read before
//     every answer returns every row inserted since the previous read.
//   - A reference that has since disappeared (a payload whose path was
//     cleared or rewritten) is kept, so it can only cause an extra refusal.
//   - An existing row does not gain a path naming another job's tree. The
//     pass's own writes touch the filesystem, the cleanup tables and the
//     candidate's own payload (collector output, skip markers), and the guard
//     excludes the candidate by id. The writers that give an existing row a
//     worktree_path (native review seats, pool isolation, manual retry) only
//     ever write that job's own seat, derived from its own id, or clear it.
type WorktreeRefIndex struct {
	mu     sync.Mutex
	built  bool
	last   int64
	byPath map[string][]string
}

// worktreeRefIndex is the engine's shared index, or a fresh one that serves a
// single inspection or cleanup.
func (e Engine) worktreeRefIndex() *WorktreeRefIndex {
	if e.WorktreeRefs != nil {
		return e.WorktreeRefs
	}
	return &WorktreeRefIndex{}
}

// referencedByOther reports whether a job other than jobID records a
// worktree_path equal to path once both are cleaned. Payloads that do not
// parse record no path, exactly as ListJobWorktreeRefs skips them.
func (x *WorktreeRefIndex) referencedByOther(ctx context.Context, store *db.Store, jobID, path string) (bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	var refs []db.JobWorktreeRef
	var last int64
	var err error
	if x.built {
		refs, last, err = store.ListJobWorktreeRefsAfter(ctx, x.last)
	} else {
		refs, last, err = store.ListJobWorktreeRefs(ctx)
	}
	if err != nil {
		return false, err
	}
	if !x.built {
		x.byPath, x.built = make(map[string][]string, len(refs)), true
	}
	x.last = last
	for _, ref := range refs {
		key := filepath.Clean(strings.TrimSpace(ref.WorktreePath))
		x.byPath[key] = append(x.byPath[key], ref.ID)
	}
	for _, id := range x.byPath[filepath.Clean(path)] {
		if id != jobID {
			return true, nil
		}
	}
	return false, nil
}
