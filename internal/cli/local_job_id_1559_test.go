package cli

import (
	"sync"
	"testing"
)

// #1559: `local-<action>-<agent>-<hex nanos>` can collide for the same agent and
// action inside one instant, and the insert then fails with
// SQLITE_CONSTRAINT_PRIMARYKEY, so the job is silently not written while the
// caller holds an id it believes exists. Observed with four reviews claimed in
// one second under `[parallel_sessions]`.
//
// A PER-PROCESS COUNTER RATHER THAN RANDOMNESS, so a same-instant collision is
// UNREACHABLE within a process rather than merely unlikely. Randomness would
// have made the defect rarer, which is the failure mode this campaign keeps
// finding: made invisible instead of impossible.
//
// The loop is not a probabilistic race hunt. It calls the minter directly and
// concurrently, so a duplicate is a deterministic property of the id shape
// rather than something the test has to be lucky to see: without the counter,
// any two calls that read the same UnixNano produce byte-identical ids.
func TestLocalAgentJobIDIsUniquePerProcess(t *testing.T) {
	const workers = 16
	const perWorker = 200

	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*perWorker)
	duplicates := []string{}

	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			for range perWorker {
				id := localAgentJobID("review", "g7-review")
				mu.Lock()
				if _, dup := seen[id]; dup {
					duplicates = append(duplicates, id)
				}
				seen[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	start.Done()
	wg.Wait()

	if len(duplicates) > 0 {
		t.Fatalf("%d duplicate dispatch ids minted for one agent (%v...); a colliding id means the job is silently not written while the caller holds an id it believes exists", len(duplicates), duplicates[:1])
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("minted %d distinct ids, want %d", len(seen), workers*perWorker)
	}
}

// The id must stay parseable in the established shape: consumers and operator
// queries key on the `local-<action>-<agent>-` prefix, so adding entropy must
// extend the id rather than restructure it.
func TestLocalAgentJobIDKeepsItsPrefix(t *testing.T) {
	id := localAgentJobID("review", "g7-review")
	const want = "local-review-g7-review-"
	if len(id) <= len(want) || id[:len(want)] != want {
		t.Fatalf("id = %q, want the %q prefix preserved", id, want)
	}
}
