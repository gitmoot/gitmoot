package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// vagueImplementResult is a deliberately deficient implement result: it claims
// "implemented" but lists no changes_made and no tests_run, so it fails both
// implement checks. It parses cleanly (valid contract), so only the #526 audit
// catches it — the perfect deterministic, no-LLM fixture.
const vagueImplementResult = `{"gitmoot_result":{"decision":"implemented","summary":"did the work","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func shellAgent() runtime.Agent {
	return runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "implementer"}
}

func TestMailboxRunResultChecksOffIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Off (the zero value): no audit runs at all.
	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksOff}
	adapter := &fakeDelivery{outputs: []string{vagueImplementResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-off", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-off", shellAgent(), adapter)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("decision = %q, want implemented", result.Decision)
	}
	// No result_checks_failed event.
	events, err := store.ListJobEvents(ctx, "job-off")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, e := range events {
		if e.Kind == ResultChecksFailedEventKind {
			t.Fatalf("off mode must record no %s event; events=%+v", ResultChecksFailedEventKind, events)
		}
	}
	// No payload field and no feed-forward rows.
	job, err := store.GetJob(ctx, "job-off")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if len(payload.ResultChecks) != 0 {
		t.Fatalf("off mode must set no payload.ResultChecks, got %+v", payload.ResultChecks)
	}
	rows, err := store.ListResultCheckFailures(ctx, "job-off")
	if err != nil {
		t.Fatalf("ListResultCheckFailures: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("off mode must write no feed-forward rows, got %+v", rows)
	}
}

func TestMailboxRunResultChecksWarnSurfacesButSucceeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksWarn}
	adapter := &fakeDelivery{outputs: []string{vagueImplementResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-warn", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-warn", shellAgent(), adapter)
	if err != nil {
		t.Fatalf("Run must NOT error in warn mode: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("decision = %q, want implemented (warn does not change the outcome)", result.Decision)
	}
	// Job still succeeded on its own decision.
	job, err := store.GetJob(ctx, "job-warn")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded (warn does not fail the job)", job.State)
	}
	// The failed checks are visible as a job event.
	events, err := store.ListJobEvents(ctx, "job-warn")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == ResultChecksFailedEventKind {
			found = true
		}
	}
	if !found {
		t.Fatalf("warn mode must record a %s event; events=%+v", ResultChecksFailedEventKind, events)
	}
	// The failed checks are on the job-detail payload (dashboard / job show).
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if len(payload.ResultChecks) != 2 {
		t.Fatalf("warn payload.ResultChecks = %+v, want the two failed implement checks", payload.ResultChecks)
	}
	for _, c := range payload.ResultChecks {
		if c.Pass || c.Explanation == "" {
			t.Fatalf("payload check should be a failure with an explanation: %+v", c)
		}
	}
	// The feed-forward stub persisted the failures for SkillOpt.
	rows, err := store.ListResultCheckFailures(ctx, "job-warn")
	if err != nil {
		t.Fatalf("ListResultCheckFailures: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("warn mode must write 2 feed-forward rows, got %+v", rows)
	}
	if rows[0].Action != "implement" || rows[0].CheckID == "" {
		t.Fatalf("feed-forward row missing context: %+v", rows[0])
	}
}

func TestMailboxRunResultChecksBlockFailsLikeContractViolation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksBlock}
	adapter := &fakeDelivery{outputs: []string{vagueImplementResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-block", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_, err := mailbox.Run(ctx, "job-block", shellAgent(), adapter)
	if err == nil {
		t.Fatal("block mode must return an error when checks fail")
	}
	var checksErr *ResultChecksError
	if !errors.As(err, &checksErr) {
		t.Fatalf("expected a *ResultChecksError, got %T: %v", err, err)
	}
	if len(checksErr.Failed) != 2 {
		t.Fatalf("block error should carry the 2 failed checks, got %+v", checksErr.Failed)
	}
	// The job is terminally failed (the contract-violation path), NOT succeeded.
	job, err := store.GetJob(ctx, "job-block")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed (block maps to the contract-violation path)", job.State)
	}
	// The failing checks are still surfaced on the payload for the operator.
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if len(payload.ResultChecks) != 2 {
		t.Fatalf("block payload must carry the failed checks, got %+v", payload.ResultChecks)
	}
	events, err := store.ListJobEvents(ctx, "job-block")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var sawChecks, sawFailed bool
	for _, e := range events {
		switch e.Kind {
		case ResultChecksFailedEventKind:
			sawChecks = true
		case string(JobFailed):
			sawFailed = true
		}
	}
	if !sawChecks || !sawFailed {
		t.Fatalf("block mode must record both a %s and a failed event; events=%+v", ResultChecksFailedEventKind, events)
	}
}

func TestMailboxRunResultChecksWarnCleanResultIsQuiet(t *testing.T) {
	// A COMPLETE implement result under warn mode records nothing — proving the
	// audit only fires on genuine failures, not on every job.
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksWarn}
	clean := `{"gitmoot_result":{"decision":"implemented","summary":"implemented X","findings":[],"changes_made":["added foo.go"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	adapter := &fakeDelivery{outputs: []string{clean}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-clean", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-clean", shellAgent(), adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "job-clean")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, e := range events {
		if e.Kind == ResultChecksFailedEventKind {
			t.Fatalf("a clean result must record no %s event; events=%+v", ResultChecksFailedEventKind, events)
		}
	}
	rows, err := store.ListResultCheckFailures(ctx, "job-clean")
	if err != nil {
		t.Fatalf("ListResultCheckFailures: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a clean result must write no feed-forward rows, got %+v", rows)
	}
}
