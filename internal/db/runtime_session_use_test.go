package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestLatestSuccessfulRuntimeSessionUseReadsResolvedRuntimeEvent(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const pin = "550e8400-e29b-41d4-a716-446655440031"
	if err := store.UpsertAgent(ctx, Agent{Name: "worker", Runtime: "codex", RuntimeRef: pin}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "resolved-override", Agent: "worker", Type: "review", State: "succeeded", Payload: `{}`,
	}, JobEvent{Kind: "runtime_override", Message: "job runs on runtime claude (agent default codex); session lock runtime:claude:" + pin}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	usedAt, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "claude", pin)
	if err != nil {
		t.Fatalf("LatestSuccessfulRuntimeSessionUse: %v", err)
	}
	if !found || usedAt.IsZero() || time.Since(usedAt) > time.Minute {
		t.Fatalf("use = %v, found=%v; want a recent use from the claude override event", usedAt, found)
	}

	if _, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "claude", "different-pin"); err != nil || found {
		t.Fatalf("different pin found=%v err=%v, want no use", found, err)
	}
}

func TestLatestSuccessfulRuntimeSessionUseRequiresSucceededJob(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const pin = "550e8400-e29b-41d4-a716-446655440032"
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "failed-use", Agent: "worker", Type: "review", State: "failed", Payload: `{}`,
	}, JobEvent{Kind: "effective_runtime", Message: "job runs on runtime codex (agent default codex); session lock runtime:codex:" + pin}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	if _, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "codex", pin); err != nil || found {
		t.Fatalf("failed job found=%v err=%v, want no successful use", found, err)
	}
}

func TestLatestSuccessfulRuntimeSessionUseRejectsDifferentRuntimeWithSameRef(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const pin = "550e8400-e29b-41d4-a716-446655440033"
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "wrong-runtime", Agent: "worker", Type: "review", State: "succeeded", Payload: `{}`,
	}, JobEvent{Kind: "runtime_override", Message: "job runs on runtime claude (agent default codex); session lock runtime:claude:" + pin}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	if _, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "codex", pin); err != nil || found {
		t.Fatalf("different-runtime session found=%v err=%v, want no codex use", found, err)
	}
}

func TestLatestSuccessfulRuntimeSessionUseIgnoresFailedAttemptBeforeSuccessfulRetry(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const (
		failedPin     = "550e8400-e29b-41d4-a716-446655440034"
		successfulPin = "550e8400-e29b-41d4-a716-446655440035"
	)
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "retried-job", Agent: "worker", Type: "review", State: "failed", Payload: `{}`,
	}, JobEvent{Kind: "effective_runtime", Message: "job runs on runtime codex (agent default codex); session lock runtime:codex:" + failedPin}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	transitioned, err := store.TransitionJobStateWithEvent(ctx, "retried-job", "failed", "succeeded", JobEvent{
		Kind: "effective_runtime", Message: "job runs on runtime codex (agent default codex); session lock runtime:codex:" + successfulPin,
	})
	if err != nil || !transitioned {
		t.Fatalf("TransitionJobStateWithEvent transitioned=%v err=%v", transitioned, err)
	}

	if _, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "codex", failedPin); err != nil || found {
		t.Fatalf("failed-attempt session found=%v err=%v, want no successful use", found, err)
	}
	if usedAt, found, err := store.LatestSuccessfulRuntimeSessionUse(ctx, "worker", "codex", successfulPin); err != nil || !found || usedAt.IsZero() {
		t.Fatalf("successful-retry session use=%v found=%v err=%v", usedAt, found, err)
	}
}
