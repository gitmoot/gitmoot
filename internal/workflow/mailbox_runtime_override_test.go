package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestMailboxRunSkipsRefreshedRefPersistForRuntimeOverride pins the #531
// session-safety invariant at the engine chokepoint: a job running under a
// per-job runtime override must NEVER write a refreshed session ref back onto
// the agent row — the stored ref belongs to the agent's DEFAULT runtime, and
// persisting an override-runtime ref there would corrupt the agent's session
// identity. Without the guard this test goes red: the fake delivery returns a
// RefreshedRuntimeRef and the stored codex agent would be re-pinned to a
// claude UUID.
func TestMailboxRunSkipsRefreshedRefPersistForRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	defaultRef := "codex-default-session"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: defaultRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	// The EFFECTIVE agent an override job runs as: override runtime + per-job ref.
	effective := runtime.Agent{Name: "shipper", Runtime: runtime.ClaudeRuntime, RuntimeRef: freshRef, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{"550e8400-e29b-41d4-a716-446655440099"},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                 "job-override",
		Agent:              "shipper",
		Action:             "ask",
		Repo:               "jerryfane/gitmoot",
		RuntimeOverride:    runtime.ClaudeRuntime,
		RuntimeOverrideRef: freshRef,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-override", effective, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stored, err := store.GetAgent(ctx, "shipper")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.Runtime != runtime.CodexRuntime || stored.RuntimeRef != defaultRef {
		t.Fatalf("override job polluted the agent's default-runtime session: runtime=%q ref=%q", stored.Runtime, stored.RuntimeRef)
	}
	events, err := store.ListJobEvents(ctx, "job-override")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasEvent(events, "session_refresh_retry") {
		t.Fatalf("override job must not emit session_refresh_retry (no re-pin), events=%+v", events)
	}
}

// TestMailboxEnqueuePersistsRuntimeOverride: the payload round-trips the
// override so a background daemon job honors it identically to foreground.
func TestMailboxEnqueuePersistsRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                 "job-override",
		Agent:              "shipper",
		Action:             "ask",
		Repo:               "jerryfane/gitmoot",
		RuntimeOverride:    runtime.ShellRuntime,
		RuntimeOverrideRef: "printf ok",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.RuntimeOverride != runtime.ShellRuntime || payload.RuntimeOverrideRef != "printf ok" {
		t.Fatalf("payload override = %q/%q", payload.RuntimeOverride, payload.RuntimeOverrideRef)
	}
}

// TestMailboxEnqueueRejectsInvalidRuntimeOverride: an invalid override fails
// at the enqueue chokepoint — BEFORE a job row exists — with an actionable
// error, for every producer.
func TestMailboxEnqueueRejectsInvalidRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}

	for name, request := range map[string]JobRequest{
		"unknown runtime":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"},
		"missing ref":          {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ClaudeRuntime},
		"ref without override": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverrideRef: "x"},
		"shell fresh ref":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: freshRef},
		// "last" names no concrete session, so its lock key could never serialize
		// with the concrete session it would actually resume.
		"last ref on a resumable runtime": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ClaudeRuntime, RuntimeOverrideRef: runtime.LastRef},
	} {
		if _, err := mailbox.Enqueue(ctx, request); err == nil {
			t.Fatalf("%s: Enqueue accepted an invalid runtime override", name)
		}
		if _, err := store.GetJob(ctx, "job-bad"); err == nil {
			t.Fatalf("%s: an invalid override must not enqueue a job", name)
		}
	}

	// The unknown-runtime error enumerates the supported registry.
	_, err = mailbox.Enqueue(ctx, JobRequest{ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"})
	if err == nil {
		t.Fatal("Enqueue accepted an unknown override runtime")
	}
	for _, supported := range runtime.SupportedRuntimes() {
		if !strings.Contains(err.Error(), supported) {
			t.Fatalf("error %q must enumerate supported runtime %q", err.Error(), supported)
		}
	}
}
