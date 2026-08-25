package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestLatestSuccessfulRuntimeRefUseReadsResolvedRuntimeEvent(t *testing.T) {
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

	usedAt, found, err := store.LatestSuccessfulRuntimeRefUse(ctx, "worker", pin)
	if err != nil {
		t.Fatalf("LatestSuccessfulRuntimeRefUse: %v", err)
	}
	if !found || usedAt.IsZero() || time.Since(usedAt) > time.Minute {
		t.Fatalf("use = %v, found=%v; want a recent use from the claude override event", usedAt, found)
	}

	if _, found, err := store.LatestSuccessfulRuntimeRefUse(ctx, "worker", "different-pin"); err != nil || found {
		t.Fatalf("different pin found=%v err=%v, want no use", found, err)
	}
}

func TestLatestSuccessfulRuntimeRefUseRequiresSucceededJob(t *testing.T) {
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

	if _, found, err := store.LatestSuccessfulRuntimeRefUse(ctx, "worker", pin); err != nil || found {
		t.Fatalf("failed job found=%v err=%v, want no successful use", found, err)
	}
}
