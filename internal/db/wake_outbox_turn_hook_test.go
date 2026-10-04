package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func insertTurnHookTestNote(t *testing.T, store *Store, target, body string) int64 {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), WorkflowNote{
		WorkflowID: "turn-hook/claim", Author: "operator", Body: body, AddressedTarget: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	return note.ID
}

func pendingTurnHookIDs(t *testing.T, store *Store, role string) []int64 {
	t.Helper()
	projection, err := store.ListWakeOutboxObligationsForRole(context.Background(), role, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.AgedAttempted)+len(projection.Blocked) != 0 {
		t.Fatalf("role %s projection has non-pending obligations: %+v", role, projection)
	}
	ids := make([]int64, 0, len(projection.Pending))
	for _, row := range projection.Pending {
		if row.MessageID == 0 || row.TargetRole != role {
			t.Fatalf("pending row %d is not %s's inbox mail: %+v", row.ID, role, row)
		}
		ids = append(ids, row.ID)
	}
	return ids
}

// Two hooks firing together (a PostToolUse racing a Stop, or two sessions bound
// to one role) must split the mail, never both show the same item. Each claim
// runs on its own pooled SQLite connection, as separate hook processes do.
func TestClaimWakeOutboxForTurnHookClaimsEachRowOnceAcrossProcesses(t *testing.T) {
	first, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	for range 6 {
		insertTurnHookTestNote(t, first, "deimos", "work")
	}
	ids := pendingTurnHookIDs(t, first, "deimos")
	if len(ids) != 6 {
		t.Fatalf("pending ids = %v, want six", ids)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	results := make([][]int64, 4)
	var wg sync.WaitGroup
	for index := range results {
		store := first
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := store.ClaimWakeOutboxForTurnHook(context.Background(), "deimos", ids, "turn-hook:claude:PostToolUse", now)
			if err != nil {
				t.Error(err)
			}
			results[index] = claimed
		}()
	}
	wg.Wait()
	seen := map[int64]int{}
	for _, claimed := range results {
		for _, id := range claimed {
			seen[id]++
		}
	}
	if len(seen) != len(ids) {
		t.Fatalf("claimed %v across both hooks, want all of %v", results, ids)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("row %d claimed %d times", id, count)
		}
	}
	if again, err := first.ClaimWakeOutboxForTurnHook(context.Background(), "deimos", ids, "turn-hook:claude:Stop", now); err != nil || len(again) != 0 {
		t.Fatalf("re-claim = %v, %v; want nothing", again, err)
	}
	delivered, err := first.ListWakeOutbox(context.Background(), WakeOutboxStateDelivered)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range delivered {
		if entry.LastError != "turn-hook:claude:PostToolUse" || entry.FinishedAt == "" {
			t.Fatalf("delivered row lacks its turn-hook receipt: %+v", entry)
		}
	}
}

func TestClaimWakeOutboxForTurnHookNeverTakesOtherRolesOrAttemptedRows(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	insertTurnHookTestNote(t, store, "deimos", "mine")
	insertTurnHookTestNote(t, store, "deimos", "already with the daemon")
	insertTurnHookTestNote(t, store, "jarvis", "theirs")
	mine := pendingTurnHookIDs(t, store, "deimos")
	theirs := pendingTurnHookIDs(t, store, "jarvis")
	if len(mine) != 2 || len(theirs) != 1 {
		t.Fatalf("mine=%v theirs=%v", mine, theirs)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	// The daemon's claim is an attempt whose receipt may be unknown; a hook
	// must not turn that uncertain row into a second showing.
	if ok, err := store.ClaimWakeOutbox(ctx, mine[1], nil, now); err != nil || !ok {
		t.Fatalf("daemon claim = %v, %v", ok, err)
	}
	all := append(append([]int64{}, mine...), theirs...)
	claimed, err := store.ClaimWakeOutboxForTurnHook(ctx, "deimos", all, "turn-hook:codex:UserPromptSubmit", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0] != mine[0] {
		t.Fatalf("claimed = %v, want only %d", claimed, mine[0])
	}
	if left := pendingTurnHookIDs(t, store, "jarvis"); len(left) != 1 || left[0] != theirs[0] {
		t.Fatalf("jarvis pending = %v, want untouched %v", left, theirs)
	}
	if _, err := store.ClaimWakeOutboxForTurnHook(ctx, "deimos", mine, "daemon", now); err == nil {
		t.Fatal("claim accepted a receipt without the turn-hook marker")
	}
}

func TestHasPendingInboxWakeOutboxTracksUnclaimedMail(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if waiting, err := store.HasPendingInboxWakeOutbox(ctx); err != nil || waiting {
		t.Fatalf("empty store waiting = %v, %v", waiting, err)
	}
	insertTurnHookTestNote(t, store, "deimos", "mail")
	if waiting, err := store.HasPendingInboxWakeOutbox(ctx); err != nil || !waiting {
		t.Fatalf("waiting after mail = %v, %v", waiting, err)
	}
	ids := pendingTurnHookIDs(t, store, "deimos")
	if _, err := store.ClaimWakeOutboxForTurnHook(ctx, "deimos", ids, "turn-hook:claude:Stop", time.Now()); err != nil {
		t.Fatal(err)
	}
	if waiting, err := store.HasPendingInboxWakeOutbox(ctx); err != nil || waiting {
		t.Fatalf("waiting after claim = %v, %v", waiting, err)
	}
}
