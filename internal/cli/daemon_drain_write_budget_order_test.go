package cli

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestShutdownDrainOutlastsEveryDurableWriteBudget pins the RELATIVE ORDER of
// two constants in two packages, which is the whole point (#1911, #2028).
//
// THE DEFECT CLASS, third instance. #1836 was "two constants in two packages
// whose relative order nothing enforced": a caller bounded a wake-outbox write
// below the store's own busy wait. #1911's second half was the same shape one
// level up: a budget sized for one lock acquisition, used by writes that take
// the lock twice. This is that shape again, across a package boundary.
//
// A wake bookkeeping write DETACHES the caller's cancellation on purpose,
// because it records a delivery that already happened. So on shutdown the drain
// bound, not the caller, decides whether that write survives. If the drain were
// shorter than the write's own floor, the daemon would exit with the write in
// flight, the row would stay `attempted`, and the age-out sweep would later
// relabel it `delivery_unknown` - the exact symptom #1911 and #1958 are about,
// produced by the fix for them.
//
// THE ORDER IS ALREADY CORRECT AND THIS TEST DOES NOT CHANGE IT. #2018 raised
// the drain to 900s, so the 33s write floor sits inside it with 867s of
// headroom. This exists because I measured the mismatch at 18-vs-15 against a
// tree that was one merge stale, which is exactly how an unenforced relation
// gets lost: the fetch had already happened and the decisive line was still
// read from the wrong ref.
//
// WHY THE 900s CANNOT BE DERIVED FROM ANYTHING IN internal/db, recorded so the
// next reader with a tidier instinct does not re-propose what I proposed and
// gm-staged refused. It is a MEASUREMENT, not a round number: p90 of
// daemon-dispatched in-flight duration over 14 days, median 8s, p90 893s, p95
// 1383s, peak concurrency 3. `db.DurableWriteBudgetFor(2)` plus a margin is
// about 36s, which is 25x smaller, so deriving one from the other would
// reintroduce the defect #2018 fixed: a drain that returns while real work is
// still running. The general rule this is an instance of: single-sourcing a
// constant is the right instinct, and it is WRONG when the two values are not
// the same quantity. A shared constant is a fix only when the thing shared is
// one fact. Here one value answers how long a MODEL RUN may take and the other
// how long a LOCK ACQUISITION may take.
//
// The drain must also stay UNDER systemd's TimeoutStopSec, currently 930s. 900s
// was chosen against it with the 5s post-cancel grace reserved OUT of the
// budget rather than added to it, so the worst case is 905s. That upper bound
// is NOT asserted here because it lives in a unit file this test cannot read;
// naming it is the most a Go test can honestly do, and it is the reason raising
// the drain is today's hazard where lowering it was yesterday's.
//
// This test lives in internal/cli, though its subject is an internal/db budget,
// because `daemonShutdownDrainTimeout` is UNEXPORTED: an internal/db test
// cannot read it at all. The location is forced, not preferred.
//
// The VALUES are deliberately not asserted. Pinning 15s or 33s or 900s would
// need editing whenever someone tunes a timeout, which is how a relation drifts
// while both halves keep passing their own tests. The ORDER is the invariant.
func TestShutdownDrainOutlastsEveryDurableWriteBudget(t *testing.T) {
	// The largest acquisition count any wake bookkeeping write declares
	// (internal/db: a transaction's first contended write plus its COMMIT).
	const maxWakeWriteAcquisitions = 2
	floor := db.DurableWriteBudgetFor(maxWakeWriteAcquisitions)
	if daemonShutdownDrainTimeout <= floor {
		t.Fatalf("daemonShutdownDrainTimeout = %s, want more than the largest durable write budget %s: "+
			"a detached bookkeeping write would outlive the drain and be abandoned with the daemon",
			daemonShutdownDrainTimeout, floor)
	}
}
