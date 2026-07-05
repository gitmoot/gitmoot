package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// baseline reads the stored cumulative counters for a session key directly, so a
// test can assert the persisted baseline after each delta call.
func baseline(t *testing.T, store *Store, key string) (int, int) {
	t.Helper()
	var in, out int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT input_cum, output_cum FROM runtime_session_usage WHERE session_key = ?`, key).Scan(&in, &out); err != nil {
		t.Fatalf("read baseline for %q returned error: %v", key, err)
	}
	return in, out
}

// TestRecordRuntimeSessionUsageDelta pins the #661 per-session delta contract: the
// first delivery on a fresh session key returns the full cumulative (baseline 0),
// each subsequent delivery returns only the increase since the last cumulative, and
// the stored baseline advances to the newest cumulative after every call.
func TestRecordRuntimeSessionUsageDelta(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	const key = "codex:019f3041-cfed-7e82-8766-b5ca75cf92da"

	// First delivery: no prior row, so the whole cumulative is this job's delta and
	// becomes the baseline.
	dIn, dOut, err := store.RecordRuntimeSessionUsageDelta(ctx, key, 16504, 20)
	if err != nil {
		t.Fatalf("first delta returned error: %v", err)
	}
	if dIn != 16504 || dOut != 20 {
		t.Fatalf("first delta = (%d, %d), want (16504, 20) — full cumulative on a fresh key", dIn, dOut)
	}
	if in, out := baseline(t, store, key); in != 16504 || out != 20 {
		t.Fatalf("baseline after first = (%d, %d), want (16504, 20)", in, out)
	}

	// Second delivery on the same session: cumulative advanced, so only the increase
	// is this job's usage — NOT the whole session history.
	dIn, dOut, err = store.RecordRuntimeSessionUsageDelta(ctx, key, 85681, 40)
	if err != nil {
		t.Fatalf("second delta returned error: %v", err)
	}
	if dIn != 85681-16504 || dOut != 40-20 {
		t.Fatalf("second delta = (%d, %d), want (%d, %d)", dIn, dOut, 85681-16504, 40-20)
	}
	if in, out := baseline(t, store, key); in != 85681 || out != 40 {
		t.Fatalf("baseline after second = (%d, %d), want (85681, 40)", in, out)
	}

	// Third delivery: same shape, live-probed cumulative.
	dIn, dOut, err = store.RecordRuntimeSessionUsageDelta(ctx, key, 103779, 45)
	if err != nil {
		t.Fatalf("third delta returned error: %v", err)
	}
	if dIn != 103779-85681 || dOut != 45-40 {
		t.Fatalf("third delta = (%d, %d), want (%d, %d)", dIn, dOut, 103779-85681, 45-40)
	}
}

// TestRecordRuntimeSessionUsageDeltaBackwardsResync pins the reset/rollover
// behavior: codex session compaction can make the cumulative counter go backwards.
// The crossing job's delta clamps to 0 (a safe under-count, never a spurious huge
// delta) AND the baseline resyncs to the new lower cumulative so subsequent deltas
// are measured from there.
func TestRecordRuntimeSessionUsageDeltaBackwardsResync(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	const key = "codex:thread-reset"

	if _, _, err := store.RecordRuntimeSessionUsageDelta(ctx, key, 100000, 500); err != nil {
		t.Fatalf("seed delta returned error: %v", err)
	}

	// Counter went backwards (compaction/rollover): delta clamps to 0.
	dIn, dOut, err := store.RecordRuntimeSessionUsageDelta(ctx, key, 300, 5)
	if err != nil {
		t.Fatalf("backwards delta returned error: %v", err)
	}
	if dIn != 0 || dOut != 0 {
		t.Fatalf("backwards delta = (%d, %d), want (0, 0) — clamped", dIn, dOut)
	}
	// Baseline resynced to the new lower cumulative.
	if in, out := baseline(t, store, key); in != 300 || out != 5 {
		t.Fatalf("baseline after backwards = (%d, %d), want (300, 5) — resynced", in, out)
	}

	// A subsequent forward step is measured from the resynced baseline.
	dIn, dOut, err = store.RecordRuntimeSessionUsageDelta(ctx, key, 800, 12)
	if err != nil {
		t.Fatalf("post-resync delta returned error: %v", err)
	}
	if dIn != 500 || dOut != 7 {
		t.Fatalf("post-resync delta = (%d, %d), want (500, 7)", dIn, dOut)
	}
}

// TestRecordRuntimeSessionUsageDeltaNegativeClamp pins that a malformed negative
// cumulative report is clamped to 0 before it can corrupt the baseline.
func TestRecordRuntimeSessionUsageDeltaNegativeClamp(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	dIn, dOut, err := store.RecordRuntimeSessionUsageDelta(ctx, "codex:neg", -5, -7)
	if err != nil {
		t.Fatalf("negative delta returned error: %v", err)
	}
	if dIn != 0 || dOut != 0 {
		t.Fatalf("negative delta = (%d, %d), want (0, 0)", dIn, dOut)
	}
	if in, out := baseline(t, store, "codex:neg"); in != 0 || out != 0 {
		t.Fatalf("baseline after negative = (%d, %d), want (0, 0)", in, out)
	}
}

// TestRecordRuntimeSessionUsageDeltaConcurrent proves the SELECT+UPSERT pair is
// atomic under concurrent callers. N goroutines hammer ONE session key with the
// SAME cumulative C. If each call is atomic, exactly one caller (whichever commits
// first) sees baseline 0 and returns the full delta C; every later caller reads the
// committed baseline C and returns 0. So — independent of interleaving order — the
// deltas sum to exactly C and exactly one is non-zero. A non-atomic implementation
// (SELECT and UPSERT in separate statements) lets two callers both read baseline 0
// before either writes, so both return C: the sum exceeds C / more than one delta is
// non-zero. Runs under `go test -race`, which additionally flags any data race in
// the method itself. Same-value C makes the assertion order-independent — distinct
// increasing cumulatives would make the expected sum depend on the nondeterministic
// arrival order (a backwards arrival clamps to 0 and resyncs the baseline).
func TestRecordRuntimeSessionUsageDeltaConcurrent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	const key = "codex:race"
	const cumIn, cumOut = 123456, 789
	const n = 64

	var mu sync.Mutex
	var sumIn, sumOut, nonZero int
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			dIn, dOut, err := store.RecordRuntimeSessionUsageDelta(ctx, key, cumIn, cumOut)
			if err != nil {
				t.Errorf("goroutine %d delta returned error: %v", i, err)
				return
			}
			if dIn < 0 || dOut < 0 {
				t.Errorf("goroutine %d delta = (%d, %d), want both >= 0", i, dIn, dOut)
			}
			mu.Lock()
			sumIn += dIn
			sumOut += dOut
			if dIn != 0 || dOut != 0 {
				nonZero++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Atomic: exactly one caller claimed the whole cumulative, the rest saw 0.
	if sumIn != cumIn || sumOut != cumOut {
		t.Fatalf("sum of deltas = (%d, %d), want (%d, %d) — read/write interleaved and double-counted", sumIn, sumOut, cumIn, cumOut)
	}
	if nonZero != 1 {
		t.Fatalf("non-zero deltas = %d, want exactly 1 — a stale-baseline read let multiple callers claim the cumulative", nonZero)
	}
	if in, out := baseline(t, store, key); in != cumIn || out != cumOut {
		t.Fatalf("final baseline = (%d, %d), want (%d, %d)", in, out, cumIn, cumOut)
	}
}
