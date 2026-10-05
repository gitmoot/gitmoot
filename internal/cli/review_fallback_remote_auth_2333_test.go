package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteAuthPool mirrors the production `code` pool shape: kimi-code has no
// credential route into a remote sandbox, the others do.
var remoteAuthPool = []string{"devin/swe-2", "kimi-code/k3", "xai-oauth/grok-4.7", "openai-codex/gpt-6-sol"}

// remoteSkippedKind is spelled out rather than taken from the constant so this
// file states the operator-visible event name it pins.
const remoteSkippedKind = "review_model_remote_skipped"

// quotaOnFirstPoolModel drives the REAL pre-terminal blocker seam for a running
// omp review on the pool head, on the given execution backend, and returns the
// stored payload and events.
func quotaOnFirstPoolModel(t *testing.T, execBackend string, pool []string) (db.Job, workflow.JobPayload, []db.JobEvent) {
	t.Helper()
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "omp-reviewer", runtime.OmpRuntime, "true", []string{"review"}, "owner/repo")
	request := poolReviewRequest("job-2333", "omp-reviewer")
	request.ReviewModelPool = pool
	request.Model = pool[0]
	request.ExecBackend = &execBackend
	enqueueDaemonWorkerJob(t, store, request)
	if _, err := store.TransitionJobState(ctx, request.ID, string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	worker := blockerE2EWorker(store, home, checkout)
	if _, err := worker.deferOperationalBlockerPreTerminal(ctx, request.ID, workflow.DeliveryError{Err: errors.New(quotaFailure)}); err != nil {
		t.Fatalf("deferOperationalBlockerPreTerminal: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, request.ID)
	events, err := store.ListJobEvents(ctx, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job, payload, events
}

func eventsOfKind(events []db.JobEvent, kind string) []db.JobEvent {
	var matched []db.JobEvent
	for _, event := range events {
		if event.Kind == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

// #2333. A remote review's fallback must never spend a sandbox on a model omp
// cannot authenticate there. Before the fix the chain went to the next pool
// entry whatever its provider, and a provider with no credential route into the
// sandbox failed runtime_auth on a freshly provisioned sandbox. The skip is
// recorded, and the same pool stays whole for a local review.
func TestRemoteReviewFallbackSkipsModelsThatCannotAuthenticateRemotely(t *testing.T) {
	t.Run("remote", func(t *testing.T) {
		job, payload, events := quotaOnFirstPoolModel(t, "remote", remoteAuthPool)
		if job.State != string(workflow.JobQueued) || payload.Model != "xai-oauth/grok-4.7" {
			t.Fatalf("remote fallback state=%s model=%q, want queued on xai-oauth/grok-4.7 (kimi-code/k3 has no remote credential route)", job.State, payload.Model)
		}
		skipped := eventsOfKind(events, remoteSkippedKind)
		if len(skipped) != 1 || !strings.Contains(skipped[0].Message, "kimi-code/k3") || strings.Contains(skipped[0].Message, "xai-oauth/grok-4.7") {
			t.Fatalf("%s events = %+v, want one naming only kimi-code/k3", remoteSkippedKind, skipped)
		}
		if len(eventsOfKind(events, reviewModelFallbackEventKind)) != 1 {
			t.Fatalf("missing %s event: %+v", reviewModelFallbackEventKind, events)
		}
	})
	t.Run("local keeps every pool entry", func(t *testing.T) {
		job, payload, events := quotaOnFirstPoolModel(t, "local", remoteAuthPool)
		if job.State != string(workflow.JobQueued) || payload.Model != "kimi-code/k3" {
			t.Fatalf("local fallback state=%s model=%q, want queued on kimi-code/k3", job.State, payload.Model)
		}
		if skipped := eventsOfKind(events, remoteSkippedKind); len(skipped) != 0 {
			t.Fatalf("local review recorded %s: %+v", remoteSkippedKind, skipped)
		}
	})
}

// When every remaining entry is remote-unusable, the remote review takes the
// ordinary timed hold instead of provisioning a sandbox for a model that cannot
// authenticate, and the skip is still visible on the job.
func TestRemoteReviewFallbackHoldsWhenOnlyUnauthenticatableModelsRemain(t *testing.T) {
	job, payload, events := quotaOnFirstPoolModel(t, "remote", []string{"devin/swe-2", "kimi-code/k3"})
	if job.State != string(workflow.JobQueued) || payload.Model != "devin/swe-2" {
		t.Fatalf("state=%s model=%q, want devin/swe-2 kept for the timed hold", job.State, payload.Model)
	}
	if len(eventsOfKind(events, reviewModelFallbackEventKind)) != 0 || len(eventsOfKind(events, blockerDeferredEventKind)) != 1 {
		t.Fatalf("events = %+v, want the timed hold and no model fallback", events)
	}
	if skipped := eventsOfKind(events, remoteSkippedKind); len(skipped) != 1 || !strings.Contains(skipped[0].Message, "kimi-code/k3") {
		t.Fatalf("%s events = %+v, want one naming kimi-code/k3", remoteSkippedKind, skipped)
	}
}
