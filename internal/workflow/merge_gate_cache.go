package workflow

import "sync"

// workflowPresence caches the per-head result of the #596 layer-2 workflow-
// awareness lookup (does `.github/workflows/` exist at this head SHA). A head SHA
// is immutable, so a cached answer never goes stale; caching it means the merge
// gate makes at most one contents read per head across the many daemon polls that
// re-evaluate a PR stuck in the no-CI grace window. It is process-global because
// the daemon rebuilds the PolicyMergeGate value on every evaluation.
var workflowPresence = struct {
	sync.Mutex
	byHead map[string]bool
}{byHead: map[string]bool{}}

// workflowPresenceCap bounds the cache so a long-lived daemon cannot grow it
// without limit. On overflow the whole map is dropped (a subsequent lookup simply
// re-reads from GitHub — correctness is unaffected since the key is an immutable
// SHA).
const workflowPresenceCap = 4096

func workflowPresenceKey(repoFullName string, headSHA string) string {
	return repoFullName + "@" + headSHA
}

func lookupWorkflowPresence(repoFullName string, headSHA string) (present bool, cached bool) {
	workflowPresence.Lock()
	defer workflowPresence.Unlock()
	present, cached = workflowPresence.byHead[workflowPresenceKey(repoFullName, headSHA)]
	return present, cached
}

func storeWorkflowPresence(repoFullName string, headSHA string, present bool) {
	workflowPresence.Lock()
	defer workflowPresence.Unlock()
	if len(workflowPresence.byHead) >= workflowPresenceCap {
		workflowPresence.byHead = map[string]bool{}
	}
	workflowPresence.byHead[workflowPresenceKey(repoFullName, headSHA)] = present
}
