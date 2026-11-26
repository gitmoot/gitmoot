package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

func TestIsReadOnlyDelegationWorktree(t *testing.T) {
	base := JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1"}
	if !isReadOnlyDelegationWorktree("ask", base) {
		t.Fatal("ask delegation worktree child not detected")
	}
	if !isReadOnlyDelegationWorktree("review", base) {
		t.Fatal("review delegation worktree child not detected")
	}
	if isReadOnlyDelegationWorktree("implement", base) {
		t.Fatal("implement child must not be treated as a read-only worktree")
	}
	if isReadOnlyDelegationWorktree("ask", JobPayload{WorktreePath: "/wt/d1"}) {
		t.Fatal("non-delegation job with no marker must not match")
	}
	if isReadOnlyDelegationWorktree("ask", JobPayload{DelegationID: "d1"}) {
		t.Fatal("delegation child without a worktree path must not match")
	}

	// #739: a TOP-LEVEL read-only (ask) worktree carries the explicit
	// ReadOnlyWorktree marker and NO DelegationID — it must match so the terminal
	// cleanup disposes it (it would otherwise be orphaned by the DelegationID gate).
	topLevel := JobPayload{WorktreePath: "/wt/seat", ReadOnlyWorktree: true}
	if !isReadOnlyDelegationWorktree("ask", topLevel) {
		t.Fatal("top-level marked read-only worktree (no delegation id) not detected (#739)")
	}
	// The marker without a worktree path is still not disposable (nothing to remove).
	if isReadOnlyDelegationWorktree("ask", JobPayload{ReadOnlyWorktree: true}) {
		t.Fatal("marker without a worktree path must not match")
	}
	// An implement/task worktree (Branch set, marker false) must NEVER match, even
	// under a DelegationID — it is torn down through the merge gate, not here.
	if isReadOnlyDelegationWorktree("implement", JobPayload{DelegationID: "d1", WorktreePath: "/wt/d1", Branch: "gitmoot-delegation-d1"}) {
		t.Fatal("implement delegation worktree must not match the read-only predicate")
	}
}

func TestCleanupDisposesTopLevelReadOnlyWorktree(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	if err := world.engine.cleanupReadOnlyDelegationWorktree(context.Background(), world.jobID, "ask", world.payload); err != nil {
		t.Fatal(err)
	}
	world.assertRemoved(t)
	if data, err := os.ReadFile(world.preserved); err != nil || string(data) != "producer result\n" {
		t.Fatalf("published result did not survive disposal: %q %v", data, err)
	}
}

func TestReadOnlyPrecleanupFailureRedactsDurableError(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
		return errors.New("hostile helper failed with token=" + secret)
	}
	decision, err := world.engine.InspectProducerCleanup(ctx, world.jobID, CleanupPolicy{Mode: "ordinary", BudgetGate: CleanupBudgetGateEnforced})
	if err != nil || !decision.Eligible {
		t.Fatalf("valid resource did not reach preservation: %+v %v", decision, err)
	}
	result, err := world.engine.ApplyProducerCleanup(ctx, world.jobID, decision.ResourceID, decision.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err == nil || result.State != "retryable" {
		t.Fatalf("failed preservation was not retryable: %+v %v", result, err)
	}
	world.assertPreserved(t)
	obligation, err := world.store.GetCleanupObligation(ctx, decision.ResourceID)
	if err != nil || obligation.State != db.CleanupObligationRetryable || obligation.AttemptCount != 1 {
		t.Fatalf("failed preservation was not retained durably: %+v %v", obligation, err)
	}
	if strings.Contains(obligation.LastError, secret) || !strings.Contains(obligation.LastError, "[REDACTED]") {
		t.Fatalf("durable preservation diagnostic exposed a credential: %q", obligation.LastError)
	}
}

// #2491 (phobos-assistant#318): the read-only diff snapshot timed out on the
// sshfs seat. Its failure was recorded on the job, but returning it failed the
// terminal advance, so the review's verdict never reached its task and the
// advance retried forever. A deferred preservation must keep the worktree with
// a retryable obligation and let the advance continue; a real publication
// failure joined beside it still fails.
func TestDeferredPreservationKeepsWorktreeWithoutFailingAdvance(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
		return fmt.Errorf("%w: read-only worktree diff not captured: signal: killed", ErrPreservationDeferred)
	}
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload); err != nil {
		t.Fatalf("deferred preservation failed the advance: %v", err)
	}
	world.assertPreserved(t)
	resource := db.CleanupObligationResourceID(world.jobID, world.payload.WorktreePath)
	obligation, err := world.store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationRetryable {
		t.Fatalf("kept worktree has no retryable cleanup obligation: %+v %v", obligation, err)
	}
	events, err := world.store.ListJobEvents(ctx, world.jobID)
	if err != nil {
		t.Fatal(err)
	}
	skipped := false
	for _, event := range events {
		skipped = skipped || (event.Kind == "delegation_worktree_cleanup_skipped" && strings.Contains(event.Message, "evidence not captured"))
	}
	if !skipped {
		t.Fatalf("kept worktree recorded no reason: %+v", events)
	}

	world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
		return errors.Join(fmt.Errorf("%w: diff", ErrPreservationDeferred), errors.New("pipeline artifact publish failed"))
	}
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload); err == nil {
		t.Fatal("a real publication failure beside a deferred one did not fail")
	}
	world.assertPreserved(t)
}

func TestReclaimReadOnlyWorktreeRejectsUnownedPath(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	unowned := t.TempDir()
	sentinel := filepath.Join(unowned, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("not a producer resource"), 0600); err != nil {
		t.Fatal(err)
	}
	payload := world.payload
	payload.WorktreePath = unowned
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.jobID, encoded); err != nil {
		t.Fatal(err)
	}
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.jobID)
	if err == nil || removed {
		t.Fatalf("unowned path was admitted: removed=%v err=%v", removed, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "not a producer resource" {
		t.Fatalf("unowned contents changed: %q %v", data, err)
	}
	world.assertPreserved(t)
}

func TestReadOnlyCleanupFailureIsReclaimable(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	manager := &failingReadOnlyCleanupManager{Client: world.manager, failure: errors.New("transient removal failure")}
	world.engine.DelegationWorktrees = manager
	if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "ask", world.payload); err == nil {
		t.Fatal("removal failure was lost")
	}
	world.assertPreserved(t)
	resource := db.CleanupObligationResourceID(world.jobID, world.path)
	obligation, err := world.store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationRetryable {
		t.Fatalf("failed removal is not reclaimable: %+v %v", obligation, err)
	}
	manager.failure = nil
	next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	world.engine.Now = func() time.Time { return next.Add(time.Second) }
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.jobID)
	if err != nil || !removed {
		t.Fatalf("retry did not reclaim the resource: removed=%v err=%v", removed, err)
	}
	world.assertRemoved(t)
	if data, err := os.ReadFile(world.preserved); err != nil || string(data) != "producer result\n" {
		t.Fatalf("retry lost the preserved result: %q %v", data, err)
	}
}

type failingReadOnlyCleanupManager struct {
	gitutil.Client
	failure error
}

func (m *failingReadOnlyCleanupManager) RemoveWorktreeForce(ctx context.Context, path string) error {
	if m.failure != nil {
		return m.failure
	}
	return m.Client.RemoveWorktreeForce(ctx, path)
}

// MoveWorktree fails like removal does: since #2524 cleanup moves the worktree
// aside under the fence, and falls back to removing it in place when the move
// fails, so both must fail for the cleanup itself to fail.
func (m *failingReadOnlyCleanupManager) MoveWorktree(ctx context.Context, from, to string) error {
	if m.failure != nil {
		return m.failure
	}
	return m.Client.MoveWorktree(ctx, from, to)
}

// TestRecordReadOnlyWorktreeReclaimOnAbort pins the #739-review leak fix: a
// read-only ask aborted (cancel/kill/supersede) before it ever runs must mark its
// dispatch-allocated worktree for daemon reclaim, but only when the worktree is
// still on disk (so an already-disposed job is not turned into a permanent,
// never-reconciled reclaim candidate) and only for genuine read-only worktrees.
func TestRecordReadOnlyWorktreeReclaimOnAbort(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)

	// A queued read-only ask carrying a live dispatch worktree is marked for reclaim.
	wt := t.TempDir()
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "local-ask-live", Type: "ask", State: string(JobQueued)},
		JobPayload{Repo: "owner/repo", WorktreePath: wt, ReadOnlyWorktree: true})
	if got := countJobEvents(t, store, "local-ask-live", "delegation_worktree_cleanup_skipped"); got != 1 {
		t.Fatalf("aborted read-only ask with a live worktree must be marked for reclaim, got %d", got)
	}

	// A worktree already gone from disk must NOT be marked (else it becomes a
	// permanent candidate the reclaim pass can never reconcile).
	gone := filepath.Join(t.TempDir(), "already-removed")
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "local-ask-gone", Type: "ask", State: string(JobCancelled)},
		JobPayload{Repo: "owner/repo", WorktreePath: gone, ReadOnlyWorktree: true})
	if got := countJobEvents(t, store, "local-ask-gone", "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("already-disposed worktree must not be marked for reclaim, got %d", got)
	}

	// An implement leg (Branch set, no read-only marker) is left to its own merge-gate
	// cleanup and must not be touched by the read-only abort helper.
	recordReadOnlyWorktreeReclaimOnAbort(ctx, store,
		db.Job{ID: "impl-leg", Type: "implement", State: string(JobQueued)},
		JobPayload{DelegationID: "d1", WorktreePath: t.TempDir(), Branch: "gitmoot-delegation-d1"})
	if got := countJobEvents(t, store, "impl-leg", "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("implement worktree must not be marked by the read-only abort helper, got %d", got)
	}
}

func TestReadOnlyWorktreeContextNote(t *testing.T) {
	// Blank base checkout → "" so ask-path / test engines that never set
	// Engine.DelegationCheckout produce byte-identical prompts (#654).
	if got := readOnlyWorktreeContextNote(""); got != "" {
		t.Fatalf("readOnlyWorktreeContextNote(\"\") = %q, want empty", got)
	}
	if got := readOnlyWorktreeContextNote("   "); got != "" {
		t.Fatalf("readOnlyWorktreeContextNote(blank) = %q, want empty", got)
	}

	base := "/root/gitmoot"
	note := readOnlyWorktreeContextNote(base)
	if note == "" {
		t.Fatal("readOnlyWorktreeContextNote with a base checkout returned empty")
	}
	if !strings.Contains(note, base) {
		t.Fatalf("note must contain the base checkout path %q: %q", base, note)
	}
	for _, want := range []string{"COMMITTED TIP", "gitignored", "read-only"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note must mention %q: %q", want, note)
		}
	}
	// Deterministic: identical input → byte-identical output, so a re-dispatch or
	// retry recomputes the same payload for the idempotent-enqueue equality check.
	if again := readOnlyWorktreeContextNote(base); again != note {
		t.Fatalf("note is non-deterministic: %q != %q", again, note)
	}
	// The exported wrapper the daemon's top-level pool-isolation path uses (#696)
	// MUST return byte-identical text so top-level auto-isolated read-only jobs and
	// read-only delegation fan-out share one committed-tip note.
	if got := ReadOnlyWorktreeContextNote(base); got != note {
		t.Fatalf("ReadOnlyWorktreeContextNote diverged from readOnlyWorktreeContextNote: %q != %q", got, note)
	}
	if got := ReadOnlyWorktreeContextNote(""); got != "" {
		t.Fatalf("ReadOnlyWorktreeContextNote(\"\") = %q, want empty (byte-identical no-note path)", got)
	}
}

// #2517: a worktree whose evidence capture failed (it timed out on the sshfs
// seat) was retried one minute later, forever; when the capture can never
// finish in time that is every sweep. It now backs off with how long its
// cleanup has been pending: a quarter of that, between 1 minute and 6 hours.
func TestDeferredPreservationBacksOffWithPendingAge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending time.Duration
		want    time.Duration
	}{
		{"new cleanup", time.Second, time.Minute},
		{"pending 4 hours", 4 * time.Hour, time.Hour},
		{"pending 5 days", 5 * 24 * time.Hour, 6 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newProducerCleanupFixture(t, true)
			ctx := context.Background()
			if _, err := world.store.EnsureCleanupObligation(ctx, world.jobID, world.payload.WorktreePath, time.Now().UTC().Add(-tc.pending)); err != nil {
				t.Fatal(err)
			}
			world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
				return fmt.Errorf("%w: read-only worktree diff not captured: signal: killed", ErrPreservationDeferred)
			}
			before := time.Now().UTC()
			if err := world.engine.cleanupReadOnlyDelegationWorktree(ctx, world.jobID, "review", world.payload); err != nil {
				t.Fatalf("deferred preservation failed the advance: %v", err)
			}
			obligation, err := world.store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(world.jobID, world.payload.WorktreePath))
			if err != nil {
				t.Fatal(err)
			}
			next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt)
			if err != nil {
				t.Fatal(err)
			}
			if got := next.Sub(before); got < tc.want-5*time.Second || got > tc.want+5*time.Second {
				t.Fatalf("retry %s after a failed capture, want about %s", got.Round(time.Second), tc.want)
			}
		})
	}
}
