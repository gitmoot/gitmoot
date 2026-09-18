package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestSalvagePreservesFindingsFromADeadReview is the #2224 reproduction. A
// review that wrote findings to the append-only log and THEN died must not
// leave the same durable artefact as one that never started.
//
// It drives the REAL shell adapter with a real non-zero exit, so the failure
// arrives through the production delivery-error path rather than a stub.
//
// MUTATION: remove the salvagePartialReviewFindings call on the delivery-error
// arm in mailbox.go and this test goes red — payload.Result stays the bare
// failure with zero findings.
func TestSalvagePreservesFindingsFromADeadReview(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	worktree := t.TempDir()

	// Two complete findings and a TRUNCATED third line, which is the normal shape
	// of a log whose writer was killed mid-write. The truncated line must be
	// counted and skipped, never cause the salvage to fail.
	log := filepath.Join(worktree, ".gitmoot-findings.jsonl")
	body := `{"severity":"P2","title":"unchecked error on the import path","file":"internal/db/store.go","line":41}
{"severity":"P3","title":"stale comment names a deleted helper","file":"internal/cli/agent.go","line":12}
{"severity":"P1","title":"truncated before the writ`
	if err := os.WriteFile(log, []byte(body+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(findings log) returned error: %v", err)
	}

	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo "reviewer died before its envelope" >&2; exit 9`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-salvage", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		PullRequest: 4242, HeadSHA: strings.Repeat("a", 40), WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-salvage", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}

	stored, err := store.GetJob(ctx, "job-salvage")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed: salvage must not rescue the JOB", stored.State)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Result == nil {
		t.Fatal("payload.Result = nil, want the salvaged partial")
	}
	if len(payload.Result.Findings) != 2 {
		t.Fatalf("salvaged findings = %d, want 2 complete ones (the truncated third is skipped)", len(payload.Result.Findings))
	}
	// THE DECISION MUST STAY "failed". A salvaged partial that presented itself as
	// a verdict could satisfy a merge gate or suppress a re-review, which is worse
	// than losing the findings.
	if payload.Result.Decision != "failed" {
		t.Fatalf("decision = %q, want %q: a partial is not a verdict", payload.Result.Decision, "failed")
	}
	// And it must SAY it is partial. An unmarked partial indistinguishable from a
	// complete verdict is the defect this fix exists to avoid reproducing.
	if !strings.Contains(payload.Result.Summary, "PARTIAL") {
		t.Fatalf("summary = %q, want it to mark the result partial", payload.Result.Summary)
	}
	if !strings.Contains(payload.Result.Summary, "satisfies no merge gate") {
		t.Fatalf("summary = %q, want it to state it satisfies no merge gate", payload.Result.Summary)
	}
	// The unusable line is reported rather than hidden.
	if !strings.Contains(payload.Result.Summary, "unusable") {
		t.Fatalf("summary = %q, want the truncated line counted", payload.Result.Summary)
	}

	events, err := store.ListJobEvents(ctx, "job-salvage")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var salvaged bool
	for _, event := range events {
		if event.Kind == "review_findings_salvaged" {
			salvaged = true
		}
		if event.Kind == "review_findings_salvage_failed" {
			t.Fatalf("salvage reported a failure: %s", event.Message)
		}
	}
	if !salvaged {
		t.Fatal("no review_findings_salvaged event: the salvage must leave a trace")
	}
}

// TestSalvageIsANoOpWithoutALog pins that the salvage adds nothing when the
// reviewer never wrote a log. Every reviewer template predates this contract,
// so the absent-log case is the COMMON one and it must be silent rather than an
// error — otherwise the fix turns every ordinary delivery failure into two.
func TestSalvageIsANoOpWithoutALog(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	worktree := t.TempDir() // deliberately empty: no findings log

	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo "no log written" >&2; exit 3`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-nolog", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		PullRequest: 77, HeadSHA: strings.Repeat("b", 40), WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-nolog", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}

	stored, err := store.GetJob(ctx, "job-nolog")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Result != nil && len(payload.Result.Findings) != 0 {
		t.Fatalf("findings = %d, want none: there was no log to salvage", len(payload.Result.Findings))
	}
	events, err := store.ListJobEvents(ctx, "job-nolog")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == "review_findings_salvaged" || event.Kind == "review_findings_salvage_failed" {
			t.Fatalf("absent log produced a salvage event %q: %s", event.Kind, event.Message)
		}
	}
}

// TestSalvageWritesTheLedgerOnlyAfterTheJobIsFailed is the round-1 regression
// for #2225's P1.
//
// The ledger writer stops a failed review's QUOTED-ONLY findings from becoming
// merge obligations, and that guard is conditioned on the job reading
// JobFailed. The writer re-fetches the job itself, so the guard is silently
// disabled if the hook runs before the terminal transition - which is what the
// first version did. A quoted-only finding recovered from a crashed reviewer
// would then have become a P1/P2 the merge gate demands an answer to.
//
// This wires the hook the PR's other tests leave nil - the P3 coverage gap from
// the same review - and asserts the STATE THE WRITER WOULD OBSERVE.
//
// MUTATION: move recordSalvagedFindingsToLedger back above m.fail() in
// mailbox.go and this goes red with observed state "running".
func TestSalvageWritesTheLedgerOnlyAfterTheJobIsFailed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	worktree := t.TempDir()

	// A QUOTED-ONLY finding: severity and title, no file, no line. This is the
	// shape the guard exists for and the shape a killed writer leaves.
	log := filepath.Join(worktree, ".gitmoot-findings.jsonl")
	if err := os.WriteFile(log, []byte(`{"severity":"P1","title":"unverified text from a crashed reviewer"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(findings log) returned error: %v", err)
	}

	var observed []string
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	mailbox.RecordReviewFindings = func(ctx context.Context, jobID string) error {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		observed = append(observed, job.State)
		return nil
	}

	agent := shellCrashAgent(`echo "died mid-review" >&2; exit 7`)
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-ledger-order", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		PullRequest: 2225, HeadSHA: strings.Repeat("d", 40), WorktreePath: worktree,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-ledger-order", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}

	if len(observed) != 1 {
		t.Fatalf("ledger writer invocations = %d, want 1", len(observed))
	}
	if observed[0] != string(JobFailed) {
		t.Fatalf("job state observed BY THE LEDGER WRITER = %q, want %q: the quoted-only guard is conditioned on this and is silently disabled otherwise",
			observed[0], string(JobFailed))
	}
}

// TestSalvageSkipsTheLedgerWhenNothingWasSalvaged pins that an ordinary delivery
// failure - no findings log, the common case today - performs no ledger write at
// all. Without this, every crashed job would touch the ledger for nothing.
func TestSalvageSkipsTheLedgerWhenNothingWasSalvaged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	calls := 0
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	mailbox.RecordReviewFindings = func(context.Context, string) error { calls++; return nil }

	agent := shellCrashAgent(`exit 4`)
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-no-ledger", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		PullRequest: 9, HeadSHA: strings.Repeat("e", 40), WorktreePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-no-ledger", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}
	if calls != 0 {
		t.Fatalf("ledger writer invocations = %d, want 0: nothing was salvaged", calls)
	}
}

// TestSalvageSkipsTheLedgerWhenTheJobIsNotFailed is the round-2 regression for
// the race the second review found.
//
// m.fail is a CAS from JobRunning with no lease, and CancelJob or the session
// reaper can win it. If the ledger write proceeded anyway, the writer would
// observe a non-failed job and its quoted-only guard would not apply — the same
// bypass round 1 closed, reappearing probabilistically.
//
// The guarantee is made LOCAL: recordSalvagedFindingsToLedger re-reads the job
// itself and refuses to write unless it is terminally failed.
//
// MUTATION: remove the state re-check in recordSalvagedFindingsToLedger and this
// goes red with a ledger call for a cancelled job.
func TestSalvageSkipsTheLedgerWhenTheJobIsNotFailed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	calls := 0
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	mailbox.RecordReviewFindings = func(context.Context, string) error { calls++; return nil }

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-raced", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
		PullRequest: 11, HeadSHA: strings.Repeat("f", 40), WorktreePath: t.TempDir(),
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	// The job is queued, never running and never failed: exactly what the writer
	// must refuse to act on.
	mailbox.recordSalvagedFindingsToLedger(ctx, "job-raced")

	if calls != 0 {
		t.Fatalf("ledger writer invocations = %d, want 0: the job is not terminally failed, so the quoted-only guard would not apply", calls)
	}
	events, err := store.ListJobEvents(ctx, "job-raced")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var skipped bool
	for _, event := range events {
		if event.Kind == "review_findings_salvage_failed" && strings.Contains(event.Message, "ledger write was skipped") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatal("no event recorded the skipped ledger write: a silent skip is the defect this feature keeps reproducing")
	}
}
