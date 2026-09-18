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
