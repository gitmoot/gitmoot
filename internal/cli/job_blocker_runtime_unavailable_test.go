package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1821, #1823. A capability refusal is the one operational blocker whose
// condition does NOT clear on its own, so it must terminate the job BLOCKED
// rather than be re-queued into the same wall.
//
// Measured over this store's history before writing any of this: 29 refusal
// deaths, ZERO recorded blocked, and 25 of the 29 belonged to an agent hit more
// than once - gm-review-opus eleven times.

func TestClassifyRuntimeUnavailableRecognisesTheRenderingsThatActuallyOccur(t *testing.T) {
	now := time.Now().UTC()
	// Every string here was taken from a real job_events row on this host.
	for _, text := range []string{
		"delivery failed: exit status 126",
		`delivery failed: exec: "claude": executable file not found in $PATH`,
		`sandbox-exec: resolve sandbox target "kimi": exec: "kimi": executable file not found in $PATH`,
		"gitmoot: runtime unavailable: no daemon-staged artifact exists: exit status 126",
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, now)
		if !ok {
			t.Fatalf("a real capability refusal was not classified at all: %q", text)
		}
		if got.Class != blockerClassRuntimeUnavailable {
			t.Fatalf("capability refusal %q classified as %q, want %q", text, got.Class, blockerClassRuntimeUnavailable)
		}
		// The absent RetryAt is what routes this to a terminal block instead of a
		// re-queue, so it is part of the contract, not an incidental zero.
		if !got.RetryAt.IsZero() {
			t.Fatalf("capability refusal %q carries RetryAt %s; a retry instant is a promise the engine does not keep", text, got.RetryAt)
		}
		if strings.TrimSpace(got.SuggestedAction) == "" {
			t.Fatalf("capability refusal %q has no suggested action; an operator has to know what to do with a blocked job", text)
		}
	}
}

// These four close the gap review found in this change (#2022 P2): a refusal
// that happens AFTER execLookPath resolved the target. The sandbox ends in a
// bare syscall.Exec, so the errno reaches the delivery seam unwrapped.
func TestClassifyRuntimeUnavailableCoversPostResolutionRefusals(t *testing.T) {
	now := time.Now().UTC()
	for _, text := range []string{
		"delivery failed: exec format error",
		"delivery failed: apply strict Landlock ruleset: invalid argument",
		"delivery failed: Landlock ABI v2 is unavailable; v3 or newer is required",
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, now)
		if !ok || got.Class != blockerClassRuntimeUnavailable {
			t.Fatalf("a post-resolution capability refusal was classified as %q (ok=%v): %q", got.Class, ok, text)
		}
	}
}

// TestRuntimeUnavailableRefusesAmbiguousErrnoText pins a DELIBERATE gap.
//
// EACCES and ENOENT reach this seam from a genuine capability refusal AND from
// an agent's own file operations, a denied cache directory, a missing
// repository path. This predicate decides a TERMINAL state, so a signature
// right only some of the time is worse than a miss: a miss keeps today's
// behaviour, a false positive blocks work that would have succeeded.
//
// This test exists so that closing "the rest of the gap" means arguing with a
// test rather than editing a comment.
func TestRuntimeUnavailableRefusesAmbiguousErrnoText(t *testing.T) {
	now := time.Now().UTC()
	for _, text := range []string{
		"delivery failed: permission denied",
		"delivery failed: open /tmp/gitmoot-go-build-cache: permission denied",
		"delivery failed: no such file or directory",
		"delivery failed: stat /root/repo/missing.go: no such file or directory",
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, now)
		if ok && got.Class == blockerClassRuntimeUnavailable {
			t.Fatalf("ambiguous errno text was read as a capability refusal, which would BLOCK work that may succeed on retry: %q", text)
		}
	}
}

// The specific classes must not be able to steal each other's text. Ordering is
// a property here, not an implementation detail: an auth or quota failure that
// happens to mention an exit code keeps its own, more actionable class.
func TestClassifyRuntimeUnavailableDoesNotStealAuthOrQuota(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		text string
		want blockerClass
	}{
		{"quota wins", "Claude AI usage limit reached, try again in 3 hours (exit status 126)", blockerClassRuntimeQuota},
		{"auth wins", "invalid API key, authentication_error (exit status 126)", blockerClassRuntimeAuth},
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(tc.text)}, now)
		if !ok || got.Class != tc.want {
			t.Fatalf("%s: classified %q as %q (ok=%v), want %q", tc.name, tc.text, got.Class, ok, tc.want)
		}
	}
}

// A bare "126" must never trigger this. It appears in SHAs, job ids and token
// counts, and this predicate decides a terminal state.
func TestClassifyRuntimeUnavailableRefusesIncidentalNumbers(t *testing.T) {
	now := time.Now().UTC()
	for _, text := range []string{
		"delivery failed: job local-review-gm-review-opus-18d126ab produced no result",
		"delivery failed: 126 findings exceeded the cap",
		"delivery failed: contract parse error at offset 126",
	} {
		got, ok := classifyOperationalBlocker(workflow.DeliveryError{Err: errors.New(text)}, now)
		if ok && got.Class == blockerClassRuntimeUnavailable {
			t.Fatalf("an incidental number was read as a capability refusal: %q", text)
		}
	}
}

// The DeliveryError gate is load-bearing for this class exactly as it is for
// auth and quota: an agent-authored summary that DISCUSSES an exit-126 wall is
// a product output, not a capability refusal, and must not block the job.
func TestClassifyRuntimeUnavailableRequiresTheDeliverySeam(t *testing.T) {
	now := time.Now().UTC()
	text := "review summary: the implementer's runtime exits with exit status 126 on this host"
	if got, ok := classifyOperationalBlocker(errors.New(text), now); ok {
		t.Fatalf("agent-authored text was classified as %q; only the delivery seam may produce this class", got.Class)
	}
}

// The end of the contract: a running job whose delivery hit a capability refusal
// is BLOCKED, carries its reason, and is NOT queued for another identical run.
func TestCapabilityRefusalBlocksInsteadOfRequeueing(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", "shell", "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "cap-job", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	if _, err := store.TransitionJobState(ctx, "cap-job", string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
		t.Fatalf("claim job: %v", err)
	}

	worker := jobWorker{Store: store}
	took, err := worker.deferOperationalBlockerPreTerminal(ctx,
		"cap-job", workflow.DeliveryError{Err: errors.New(`exec: "claude": executable file not found in $PATH`)})
	if err != nil {
		t.Fatalf("pre-terminal seam returned an error: %v", err)
	}
	if !took {
		t.Fatal("the seam declined a capability refusal, so Mailbox.Run would fail the job and invite the identical re-dispatch")
	}

	job, err := store.GetJob(ctx, "cap-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != string(workflow.JobBlocked) {
		t.Fatalf("job state is %q, want %q: `failed` is what produced eleven re-dispatches into one wall", job.State, workflow.JobBlocked)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.BlockerClass != string(blockerClassRuntimeUnavailable) {
		t.Fatalf("blocker_class is %q, want %q", payload.BlockerClass, blockerClassRuntimeUnavailable)
	}
	if strings.TrimSpace(payload.BlockerSuggestedAction) == "" {
		t.Fatal("a blocked job carries no suggested action; nothing tells an operator what to fix")
	}
	if strings.TrimSpace(payload.BlockerRetryAt) != "" {
		t.Fatalf("blocked job carries retry_at %q; nothing is going to retry it", payload.BlockerRetryAt)
	}
	if payload.BlockerAttempts != 0 {
		t.Fatalf("blocked job counted %d retry attempts against a budget it never uses", payload.BlockerAttempts)
	}

	events, err := store.ListJobEvents(ctx, "cap-job")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var haveBlocked, haveDeferred bool
	for _, e := range events {
		switch e.Kind {
		case blockerBlockedEventKind:
			haveBlocked = true
		case blockerDeferredEventKind:
			haveDeferred = true
		}
	}
	if !haveBlocked {
		t.Fatalf("no %s event: the blocked job would be unexplained on the stuck surface", blockerBlockedEventKind)
	}
	if haveDeferred {
		t.Fatal("a capability refusal recorded a DEFERRAL; that is the re-dispatch this slice removes")
	}
}

// The should-succeed arm, which every guard in this campaign is required to
// have: a self-clearing blocker still defers. A guard that blocks valid work is
// its own defect.
func TestSelfClearingBlockerStillDefers(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", "shell", "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "quota-job", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	if _, err := store.TransitionJobState(ctx, "quota-job", string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	worker := jobWorker{Store: store}
	took, err := worker.deferOperationalBlockerPreTerminal(ctx,
		"quota-job", workflow.DeliveryError{Err: errors.New("Claude AI usage limit reached, try again in 3 hours")})
	if err != nil {
		t.Fatalf("pre-terminal seam returned an error: %v", err)
	}
	if !took {
		t.Fatal("a quota blocker was not deferred; this slice must not change the self-clearing path")
	}
	job, err := store.GetJob(ctx, "quota-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("quota blocker left the job %q, want %q: the deferral path is unchanged by this slice", job.State, workflow.JobQueued)
	}
}

// A stored result means the agent ANSWERED, so the runtime plainly executed
// whatever the error text says. That is a product outcome and must keep the
// terminal failure path.
func TestAnsweredJobIsNeverBlockedOnRuntimeText(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", "shell", "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "answered-job", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	job, err := store.GetJob(ctx, "answered-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	payload.Result = &workflow.AgentResult{Decision: "failed", Summary: "could not finish"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded := string(raw)
	if err := store.UpdateJobPayload(ctx, "answered-job", encoded); err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if _, err := store.TransitionJobState(ctx, "answered-job", string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	worker := jobWorker{Store: store}
	took, err := worker.deferOperationalBlockerPreTerminal(ctx,
		"answered-job", workflow.DeliveryError{Err: errors.New("exit status 126")})
	if err != nil {
		t.Fatalf("pre-terminal seam returned an error: %v", err)
	}
	if took {
		t.Fatal("a job that produced a result was blocked on runtime text; a product outcome must stay terminal")
	}
	after, err := store.GetJob(ctx, "answered-job")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if after.State != string(workflow.JobRunning) {
		t.Fatalf("the seam moved an answered job to %q; it must leave the terminal path alone", after.State)
	}
}

var _ = db.JobEvent{}
