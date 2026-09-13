package db

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ROUND-3 P3. One live wait per (role, subject) is enforced by a partial unique
// index, and callers dedup by reading the waiting rows first. Two concurrent
// requesters for the same role and subject can both read empty and both insert:
// the loser surfaced the raw SQLite constraint error, which reads to the caller
// as a bug rather than as the attach the dedup would have produced.
//
// Both callers must come back holding the SAME live wait.
func TestSubscribeAwaitedFactAttachesInsteadOfRacingTheLiveSubjectIndex(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	key, err := ReviewRequestSubjectKey("owner/repo", 12, "0bd967c5ba8e506607bd3a9999a94a4db5b881b4", "code")
	if err != nil {
		t.Fatal(err)
	}
	subscription := AwaitedFactSubscription{
		WaiterRole:  "joltra",
		SubjectKind: AwaitedFactSubjectReviewVerdict,
		SubjectKey:  key,
		Deadline:    time.Now().UTC().Add(time.Hour),
	}

	var wg sync.WaitGroup
	ids := make([]int64, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fact, _, err := store.SubscribeAwaitedFact(ctx, subscription)
			ids[i], errs[i] = fact.ID, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("subscriber %d failed instead of attaching: %v", i, err)
		}
	}
	if ids[0] == 0 || ids[0] != ids[1] {
		t.Fatalf("subscribers hold different waits: %d and %d, want one live wait attached twice", ids[0], ids[1])
	}
}

// #2176: attaching must never SHORTEN the live wait. A joiner asking for a
// minute could expire a winner that asked for an hour.
func TestSubscribeAwaitedFactAttachKeepsTheLongerDeadline(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	key, err := ReviewRequestSubjectKey("owner/repo", 12, "a151df794c3434b2bb113713f85f34d19b3cd0d3", "code")
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().UTC().Add(time.Hour)
	first, _, err := store.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{WaiterRole: "joltra", SubjectKind: AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: long})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{WaiterRole: "joltra", SubjectKind: AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: time.Now().UTC().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second subscribe made a new wait %d, want the live one %d", second.ID, first.ID)
	}
	var deadline string
	if err := store.db.QueryRowContext(ctx, "SELECT deadline FROM awaited_facts WHERE id = ?", first.ID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(long.Add(-time.Second)) {
		t.Fatalf("deadline shortened to %s, want the winner's %s: a joiner must not expire an existing wait early", got, long)
	}
}

// #2176 F5. The deadline must also EXTEND, not merely refuse to shrink. The
// reviewer proved this direction had zero protection: mutating the CASE to
// "keep the existing deadline always" passed the entire suite, and that is the
// behaviour a joiner asking for LONGER actually needs.
func TestSubscribeAwaitedFactAttachExtendsAShorterDeadline(t *testing.T) {
	store := openAwaitedFactTestStore(t)
	ctx := context.Background()
	key, err := ReviewRequestSubjectKey("owner/repo", 12, "6adc4db105adc8214cd3de09325f8f7d2289cea8", "code")
	if err != nil {
		t.Fatal(err)
	}
	short := time.Now().UTC().Add(time.Minute)
	first, _, err := store.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{WaiterRole: "joltra", SubjectKind: AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: short})
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().UTC().Add(2 * time.Hour)
	if _, _, err := store.SubscribeAwaitedFact(ctx, AwaitedFactSubscription{WaiterRole: "joltra", SubjectKind: AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: long}); err != nil {
		t.Fatal(err)
	}
	var deadline string
	if err := store.db.QueryRowContext(ctx, "SELECT deadline FROM awaited_facts WHERE id = ?", first.ID).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	got, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(long.Add(-time.Second)) {
		t.Fatalf("deadline = %s, want it extended to %s: a joiner asking for longer is expired at the first waiter's much earlier deadline", got, long)
	}
}
