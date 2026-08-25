package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/evidence"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

func TestIssue1616DescriptionPathLikeTokensDoNotFailObservedChanges(t *testing.T) {
	const claim = "internal/cli/daemon_worker.go:596 — … via agent.Runtime."
	observation := compareResultChanges(
		[]string{claim},
		[]string{"internal/cli/daemon_worker.go"},
	)
	if observation.Divergent {
		t.Fatalf("description tokens created a false divergence: %+v", observation)
	}
	if got, want := fmt.Sprint(observation.Changes[0].ClaimedFiles), "[internal/cli/daemon_worker.go]"; got != want {
		t.Fatalf("ClaimedFiles = %s, want %s", got, want)
	}
	check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	}), "implement-changes-observed")
	if !ok || !check.Pass {
		t.Fatalf("matching leading path binding should pass the observation check: %+v", check)
	}
}

func TestIssue1616ProseFirstTouchedFileBinds(t *testing.T) {
	const claim = "updated ordinary.go"
	observation := compareResultChanges([]string{claim}, []string{"ordinary.go"})
	if observation.Divergent {
		t.Fatalf("prose-first touched-file claim diverged: %+v", observation)
	}
	if got, want := fmt.Sprint(observation.Changes[0].ClaimedFiles), "[ordinary.go]"; got != want {
		t.Fatalf("ClaimedFiles = %s, want %s", got, want)
	}
}

func TestIssue1616UniqueBasenameFallbackOnlyBindsLeadingToken(t *testing.T) {
	const touched = "internal/workflow/result.go"

	t.Run("leading_fallback_binds", func(t *testing.T) {
		const claim = "result.go — did a thing"
		observation := compareResultChanges([]string{claim}, []string{touched})
		if observation.Divergent {
			t.Fatalf("leading unique basename should bind without divergence: %+v", observation)
		}
		change := observation.Changes[0]
		if got, want := fmt.Sprint(change.ClaimedFiles), "[result.go]"; got != want {
			t.Fatalf("ClaimedFiles = %s, want %s", got, want)
		}
		if got, want := fmt.Sprint(change.Observation), "["+touched+"]"; got != want {
			t.Fatalf("Observation = %s, want %s", got, want)
		}
	})

	t.Run("non_leading_basename_does_not_bind", func(t *testing.T) {
		const claim = "refactored the credential gate for result.go"
		observation := compareResultChanges([]string{claim}, []string{touched})
		if got, want := fmt.Sprint(observation.UnclaimedFiles), "["+touched+"]"; got != want {
			t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
		}
		if got, want := fmt.Sprint(observation.UnboundClaims), "["+claim+"]"; got != want {
			t.Fatalf("UnboundClaims = %s, want %s", got, want)
		}
		check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
			Action:      "implement",
			Result:      AgentResult{Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"targeted test"}},
			Observation: observation,
		}), "implement-changes-observed")
		if !ok || check.Pass {
			t.Fatalf("incidental mid-prose basename should leave the diff file unclaimed and fail: %+v", check)
		}
	})
}

func TestIssue1616ProseFirstOverclaimFailsAsUnbound(t *testing.T) {
	const (
		boundClaim = "updated internal/workflow/result_checks.go"
		overclaim  = "updated absent.go"
	)
	observation := compareResultChanges(
		[]string{boundClaim, overclaim},
		[]string{"internal/workflow/result_checks.go"},
	)
	if got, want := fmt.Sprint(observation.UnboundClaims), "["+overclaim+"]"; got != want {
		t.Fatalf("UnboundClaims = %s, want %s", got, want)
	}
	if got := fmt.Sprint(observation.ClaimedOnlyFiles); got != "[]" {
		t.Fatalf("unbound prose-first over-claim produced ClaimedOnlyFiles = %s", got)
	}
	if got := fmt.Sprint(observation.UnclaimedFiles); got != "[]" {
		t.Fatalf("fully claimed diff produced UnclaimedFiles = %s", got)
	}
	check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{boundClaim, overclaim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	}), "implement-changes-observed")
	if !ok || check.Pass {
		t.Fatalf("unbound prose-first over-claim should fail with a non-empty, fully claimed diff: %+v", check)
	}
}

func TestIssue1616LeadingPathContractFormsBind(t *testing.T) {
	for _, tc := range []struct {
		claim   string
		touched string
	}{
		{claim: "internal/foo/bar.go — did a thing", touched: "internal/foo/bar.go"},
		{claim: "internal/foo/bar.go:12 — did a thing", touched: "internal/foo/bar.go"},
		{claim: "internal/foo/bar.go:12:4 — did a thing", touched: "internal/foo/bar.go"},
		{claim: "Makefile — did a thing", touched: "Makefile"},
	} {
		t.Run(tc.claim, func(t *testing.T) {
			observation := compareResultChanges([]string{tc.claim}, []string{tc.touched})
			if observation.Divergent {
				t.Fatalf("contract claim diverged: %+v", observation)
			}
			if got, want := fmt.Sprint(observation.Changes[0].ClaimedFiles), "["+tc.touched+"]"; got != want {
				t.Fatalf("ClaimedFiles = %s, want %s", got, want)
			}
		})
	}
}

func TestIssue1616ClaimedFileAbsentFromDiffStillFails(t *testing.T) {
	const claim = "internal/workflow/absent.go:17 — updates the result gate"
	observation := compareResultChanges([]string{claim}, nil)
	if got, want := fmt.Sprint(observation.ClaimedOnlyFiles), "[internal/workflow/absent.go]"; got != want {
		t.Fatalf("ClaimedOnlyFiles = %s, want %s", got, want)
	}
	check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	}), "implement-changes-observed")
	if !ok || check.Pass {
		t.Fatalf("claimed file absent from diff should fail the observation check: %+v", check)
	}
	if !strings.Contains(check.Explanation, "work may be missing") {
		t.Fatalf("failure does not name the missing-work condition: %q", check.Explanation)
	}
}

func TestIssue1616UnclaimedDiffFileStillFails(t *testing.T) {
	const claim = "internal/workflow/claimed.go:9 — updates the claimed file"
	observation := compareResultChanges(
		[]string{claim},
		[]string{"internal/workflow/claimed.go", "internal/workflow/unmentioned.go"},
	)
	if got, want := fmt.Sprint(observation.UnclaimedFiles), "[internal/workflow/unmentioned.go]"; got != want {
		t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
	}
	failed := failedIDs(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("unclaimed diff file should fail the observation check; failed=%v", keys(failed))
	}
}

func TestIssue1616UnboundOverclaimFailsWithNonEmptyFullyClaimedDiff(t *testing.T) {
	const (
		boundClaim   = "internal/workflow/result_checks.go:162 — updated the result gate"
		unboundClaim = "../internal/workflow/absent.go — updated another gate"
	)
	observation := compareResultChanges(
		[]string{boundClaim, unboundClaim},
		[]string{"internal/workflow/result_checks.go"},
	)
	if got, want := fmt.Sprint(observation.UnboundClaims), "["+unboundClaim+"]"; got != want {
		t.Fatalf("UnboundClaims = %s, want %s", got, want)
	}
	check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{boundClaim, unboundClaim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	}), "implement-changes-observed")
	if !ok || check.Pass {
		t.Fatalf("an unbound over-claim should fail even when the non-empty diff is fully claimed: %+v", check)
	}
}

func TestIssue1616LeadingPathOverclaimFailsWithNonEmptyFullyClaimedDiff(t *testing.T) {
	const (
		boundClaim    = "internal/workflow/result_checks.go:162 — updated the result gate"
		overclaim     = "internal/workflow/absent.go:23 — updated another gate"
		boundDiffFile = "internal/workflow/result_checks.go"
		overclaimFile = "internal/workflow/absent.go"
	)
	observation := compareResultChanges(
		[]string{boundClaim, overclaim},
		[]string{boundDiffFile},
	)
	if got, want := fmt.Sprint(observation.ClaimedOnlyFiles), "["+overclaimFile+"]"; got != want {
		t.Fatalf("ClaimedOnlyFiles = %s, want %s", got, want)
	}
	if got := fmt.Sprint(observation.UnclaimedFiles); got != "[]" {
		t.Fatalf("fully claimed diff produced UnclaimedFiles = %s", got)
	}
	check, ok := resultCheckByID(RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{boundClaim, overclaim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	}), "implement-changes-observed")
	if !ok || check.Pass {
		t.Fatalf("a leading-path over-claim should fail with a non-empty, fully claimed diff: %+v", check)
	}
}

func TestIssue1616UnboundClaimWithEmptyDiffFails(t *testing.T) {
	const claim = "refactored the credential gate"
	observation := compareResultChanges([]string{claim}, nil)
	if got, want := fmt.Sprint(observation.UnboundClaims), "["+claim+"]"; got != want {
		t.Fatalf("UnboundClaims = %s, want %s", got, want)
	}
	if got := fmt.Sprint(observation.ClaimedOnlyFiles); got != "[]" {
		t.Fatalf("pure prose produced bogus ClaimedOnlyFiles = %s", got)
	}
	checks := RunResultChecks(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"targeted test"}},
		Observation: observation,
	})
	observed, ok := resultCheckByID(checks, "implement-changes-observed")
	if !ok || observed.Pass {
		t.Fatalf("unbound claim with empty diff should fail the observation check: %+v", observed)
	}
	if !strings.Contains(observed.Explanation, "work may be missing") {
		t.Fatalf("failure does not name the missing-work condition: %q", observed.Explanation)
	}
	for _, id := range []string{"implement-changes-listed", "implement-tests-listed"} {
		check, found := resultCheckByID(checks, id)
		if !found || !check.Pass {
			t.Fatalf("adjacent check %q should pass and leave the empty-diff guard load-bearing: %+v", id, check)
		}
	}
}

func TestCompareResultChangesFlagsClaimedFileAbsentFromDiff(t *testing.T) {
	observation := compareResultChanges(
		[]string{"internal/workflow/absent.go:1 — updated"},
		nil,
	)
	if !observation.Divergent {
		t.Fatal("claim naming a file absent from the diff was not flagged")
	}
	if got, want := fmt.Sprint(observation.ClaimedOnlyFiles), "[internal/workflow/absent.go]"; got != want {
		t.Fatalf("ClaimedOnlyFiles = %s, want %s", got, want)
	}
	if got := observation.Changes[0].Grade; got != evidence.GradeReported {
		t.Fatalf("divergent claim grade = %q, want reported", got)
	}
	failed := failedIDs(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{"internal/workflow/absent.go:1 — updated"}, TestsRun: []string{"go test ./..."}},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("claimed-only divergence was not readable by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesDoesNotRebindQualifiedClaimByBasename(t *testing.T) {
	const claim = "docs/result_checks.go:1 — updated"
	observation := compareResultChanges(
		[]string{claim},
		[]string{"internal/workflow/result_checks.go"},
	)
	if !observation.Divergent {
		t.Fatal("qualified claim was silently rebound by basename instead of flagged")
	}
	if got, want := fmt.Sprint(observation.ClaimedOnlyFiles), "[docs/result_checks.go]"; got != want {
		t.Fatalf("ClaimedOnlyFiles = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(observation.UnclaimedFiles), "[internal/workflow/result_checks.go]"; got != want {
		t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
	}
	change := observation.Changes[0]
	if got := change.Grade; got != evidence.GradeReported {
		t.Fatalf("qualified mismatched claim grade = %q, want reported", got)
	}
	if got := fmt.Sprint(change.Observation); got != "[]" {
		t.Fatalf("qualified mismatched claim observation = %s, want []", got)
	}
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{
			Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"go test ./..."},
		},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("qualified-path divergence was not readable by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesKeepsUnqualifiedBasenameBindingReported(t *testing.T) {
	const claim = "result_checks.go:1 — updated"
	observation := compareResultChanges(
		[]string{claim},
		[]string{"internal/workflow/result_checks.go"},
	)
	if observation.Divergent {
		t.Fatalf("unique unqualified filename should bind without divergence: %+v", observation)
	}
	change := observation.Changes[0]
	if got := change.Grade; got != evidence.GradeReported {
		t.Fatalf("basename-assisted claim grade = %q, want reported", got)
	}
	if got, want := fmt.Sprint(change.Observation), "[internal/workflow/result_checks.go]"; got != want {
		t.Fatalf("basename-assisted observation = %s, want %s", got, want)
	}
}

func TestRunResultChecksRejectsMismatchedObservedPathBinding(t *testing.T) {
	const claim = "docs/result_checks.go:1 — updated"
	observation := &ResultObservation{
		Source:       ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []ChangeObservation{{
			Claim:        claim,
			ClaimedFiles: []string{"docs/result_checks.go"},
			Observation:  []string{"internal/workflow/result_checks.go"},
			Grade:        evidence.GradeObserved,
		}},
	}
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{
			Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"go test ./..."},
		},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("mismatched observed binding was not rejected by the result gate; failed=%v", keys(failed))
	}
}

func TestRunResultChecksRejectsObservedBindingAbsentFromTouchedFiles(t *testing.T) {
	const claim = "docs/result_checks.go:1 — updated"
	observation := &ResultObservation{
		Source:       ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []ChangeObservation{{
			Claim:        claim,
			ClaimedFiles: []string{"docs/result_checks.go"},
			Observation:  []string{"docs/result_checks.go"},
			Grade:        evidence.GradeObserved,
		}},
	}
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{
			Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"go test ./..."},
		},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("observed binding absent from touched_files was not rejected by the result gate; failed=%v", keys(failed))
	}
}

func TestRunResultChecksRejectsReportedBindingAbsentFromTouchedFiles(t *testing.T) {
	const claim = "docs/result_checks.go:1 — updated"
	observation := &ResultObservation{
		Source:       ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []ChangeObservation{{
			Claim:        claim,
			ClaimedFiles: []string{"docs/result_checks.go"},
			Observation:  []string{"docs/result_checks.go"},
			Grade:        evidence.GradeReported,
		}},
	}
	failed := failedIDs(ResultCheckInput{
		Action: "implement",
		Result: AgentResult{
			Decision: "implemented", ChangesMade: []string{claim}, TestsRun: []string{"go test ./..."},
		},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("reported binding absent from touched_files was not rejected by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesFlagsDiffFileNoClaimMentions(t *testing.T) {
	observation := compareResultChanges(
		[]string{"internal/workflow/claimed.go:1 — updated"},
		[]string{"internal/workflow/claimed.go", "internal/workflow/unmentioned.go"},
	)
	if !observation.Divergent {
		t.Fatal("diff file absent from every claim was not flagged")
	}
	if got, want := fmt.Sprint(observation.UnclaimedFiles), "[internal/workflow/unmentioned.go]"; got != want {
		t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
	}
	if got := observation.Changes[0].Grade; got != evidence.GradeObserved {
		t.Fatalf("corroborated claim grade = %q, want observed", got)
	}
	failed := failedIDs(ResultCheckInput{
		Action:      "implement",
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{"internal/workflow/claimed.go:1 — updated"}, TestsRun: []string{"go test ./..."}},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("unclaimed diff file was not readable by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesFlagsUnboundStructuredClaim(t *testing.T) {
	claim := "../src/tendwire/command_submission.py — deleted _clear_private_pane_composer"
	observation := compareResultChanges(
		[]string{claim},
		[]string{"src/tendwire/command_submission.py"},
	)
	if !observation.Divergent {
		t.Fatal("changes_made entry without a file path was not flagged")
	}
	if got, want := fmt.Sprint(observation.UnboundClaims), "["+claim+"]"; got != want {
		t.Fatalf("UnboundClaims = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(observation.UnclaimedFiles), "[src/tendwire/command_submission.py]"; got != want {
		t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
	}
}

func TestObserveResultChangesReadsTrackedAndUntrackedWorktreeDiff(t *testing.T) {
	repo := observationTestRepo(t)
	writeObservationFile(t, repo, "tracked.go", "package changed\n")
	writeObservationFile(t, repo, "new.go", "package new\n")
	runObservationGit(t, repo, "add", "tracked.go")

	observation := observeResultChanges(context.Background(), repo, AgentResult{
		ChangesMade: []string{"tracked.go:1 — updated", "new.go:1 — added"},
	}, execbackend.Local)
	if observation == nil || observation.Error != "" {
		t.Fatalf("observation = %+v, want successful worktree diff", observation)
	}
	if got, want := fmt.Sprint(observation.TouchedFiles), "[new.go tracked.go]"; got != want {
		t.Fatalf("TouchedFiles = %s, want %s", got, want)
	}
	if observation.Divergent {
		t.Fatalf("matching tracked + untracked claims diverged: %+v", observation)
	}
	for _, change := range observation.Changes {
		if change.Grade != evidence.GradeObserved {
			t.Fatalf("corroborated change grade = %q, want observed: %+v", change.Grade, change)
		}
	}
}

func TestObserveResultChangesNonLocalBackendCannotRunLocalGit(t *testing.T) {
	observation := observeResultChanges(context.Background(), t.TempDir(), AgentResult{
		ChangesMade: []string{"tracked.go:1 — updated"},
	}, execbackend.Backend("p2-probe"))
	if observation == nil || !strings.Contains(observation.Error, "p2-probe") || !strings.Contains(observation.Error, "no execution implementation") {
		t.Fatalf("observation = %+v, want missing p2-probe implementation", observation)
	}
}

func TestMailboxResultObservationFlowsIntoOffWarnBlockGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       ResultCheckMode
		wantErr    bool
		wantChecks bool
	}{
		{name: "off records without gating", mode: ResultChecksOff},
		{name: "warn surfaces and succeeds", mode: ResultChecksWarn, wantChecks: true},
		{name: "block surfaces and fails", mode: ResultChecksBlock, wantErr: true, wantChecks: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			repo := observationTestRepo(t)
			mailbox := Mailbox{store: store, resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver, resultCheckMode: tc.mode}
			output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["absent.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
			adapter := &fakeDelivery{outputs: []string{output}}
			jobID := "observe-" + string(tc.mode)
			if _, err := mailbox.Enqueue(ctx, JobRequest{
				ID: jobID, Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: repo,
			}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			_, runErr := mailbox.Run(ctx, jobID, shellAgent(), adapter)
			if tc.wantErr {
				var checksErr *ResultChecksError
				if !errors.As(runErr, &checksErr) {
					t.Fatalf("Run error = %T %v, want ResultChecksError", runErr, runErr)
				}
			} else if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			job, err := store.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			payload, err := unmarshalPayload(job.Payload)
			if err != nil {
				t.Fatalf("unmarshalPayload: %v", err)
			}
			if payload.ResultObservation == nil || !payload.ResultObservation.Divergent {
				t.Fatalf("persisted ResultObservation = %+v, want divergence in every mode", payload.ResultObservation)
			}
			_, sawObservationCheck := resultCheckByID(payload.ResultChecks, "implement-changes-observed")
			if sawObservationCheck != tc.wantChecks {
				t.Fatalf("observation check present = %v, want %v; checks=%+v", sawObservationCheck, tc.wantChecks, payload.ResultChecks)
			}
		})
	}
}

func TestMailboxResultObservationFlagsUnclaimedDiffFile(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	repo := observationTestRepo(t)
	writeObservationFile(t, repo, "claimed.go", "package changed\n")
	writeObservationFile(t, repo, "unmentioned.go", "package changed\n")

	mailbox := Mailbox{store: store, resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver, resultCheckMode: ResultChecksWarn}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["claimed.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	adapter := &fakeDelivery{outputs: []string{output}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "observe-unclaimed", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: repo,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "observe-unclaimed", shellAgent(), adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := store.GetJob(ctx, "observe-unclaimed")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if got, want := fmt.Sprint(payload.ResultObservation.UnclaimedFiles), "[unmentioned.go]"; got != want {
		t.Fatalf("UnclaimedFiles = %s, want %s", got, want)
	}
	if _, ok := resultCheckByID(payload.ResultChecks, "implement-changes-observed"); !ok {
		t.Fatalf("unclaimed diff file did not reach result gate: %+v", payload.ResultChecks)
	}
}

func TestMailboxObservationGapRecordsWithoutRefusingPersistence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	notGit := t.TempDir()
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver, resultCheckMode: ResultChecksBlock}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["foo.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	adapter := &fakeDelivery{outputs: []string{output}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "observe-gap", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: notGit,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "observe-gap", shellAgent(), adapter); err != nil {
		t.Fatalf("an unavailable observation must be recorded, not refused: %v", err)
	}
	job, err := store.GetJob(ctx, "observe-gap")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if payload.ResultObservation == nil || payload.ResultObservation.Error == "" {
		t.Fatalf("observation gap was not persisted: %+v", payload.ResultObservation)
	}
	if len(payload.ResultChecks) != 0 {
		t.Fatalf("an observation gap is not divergence and must not be refused: %+v", payload.ResultChecks)
	}
}

func TestMailboxImportsChangeSetBeforeResultObservation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host, sandbox, changes := observationChangeSet(t, "claimed.go", "package imported\n")
	mailbox := Mailbox{
		store:                   store,
		resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver,
		CollectChangeSet: func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
			return &changes, nil
		},
	}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["claimed.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "changeset-before-observation", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: host,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "changeset-before-observation", shellAgent(), &fakeDelivery{outputs: []string{output}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	job, err := store.GetJob(ctx, "changeset-before-observation")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ResultObservation == nil || fmt.Sprint(payload.ResultObservation.TouchedFiles) != "[claimed.go]" {
		t.Fatalf("ResultObservation = %+v, want imported claimed.go; delaying import past observation must fail this test", payload.ResultObservation)
	}
	if got := readObservationFile(t, host, "claimed.go"); got != readObservationFile(t, sandbox, "claimed.go") {
		t.Fatalf("host claimed.go = %q, sandbox = %q", got, readObservationFile(t, sandbox, "claimed.go"))
	}
}

func TestMailboxMalformedRedeliveryImportsFinalCumulativeChangeSet(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host, sandbox, _ := observationChangeSet(t, "claimed.go", "package versionA\n")
	base := strings.TrimSpace(runObservationGit(t, host, "rev-parse", "HEAD"))
	collections := 0
	mailbox := Mailbox{
		store:                   store,
		resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver,
		CollectChangeSet: func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
			collections++
			changes, err := execbackend.BuildChangeSet(context.Background(), sandbox, base)
			return &changes, err
		},
	}
	deliveries := 0
	adapter := &fakeDelivery{
		outputs: []string{"malformed result", `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["claimed.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`},
		onDeliver: func() {
			deliveries++
			if deliveries == 2 {
				writeObservationFile(t, sandbox, "claimed.go", "package versionB\n")
			}
		},
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "changeset-redelivery", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: host,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Run(ctx, "changeset-redelivery", shellAgent(), adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deliveries != 2 {
		t.Fatalf("deliveries = %d, want malformed delivery plus repair", deliveries)
	}
	if collections != 1 {
		t.Fatalf("ChangeSet collections = %d, want one final cumulative collection", collections)
	}
	if got := readObservationFile(t, host, "claimed.go"); got != "package versionB\n" {
		t.Fatalf("claimed.go after repair import = %q", got)
	}
}

func TestMailboxImportFailureStopsBeforeObservation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	host, _, changes := observationChangeSet(t, "claimed.go", "package imported\n")
	mailbox := Mailbox{
		store:                   store,
		resolveDeliveryWorktree: PayloadDeliveryWorktreeResolver,
		CollectChangeSet: func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
			return &changes, nil
		},
		ApplyChangeSet: func(context.Context, string, execbackend.ChangeSet) error {
			return errors.New("deliberate mid-materialize interruption")
		},
	}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["claimed.go:1 — updated"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "changeset-interrupted", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", WorktreePath: host,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Run(ctx, "changeset-interrupted", shellAgent(), &fakeDelivery{outputs: []string{output}}); err == nil || !strings.Contains(err.Error(), "mid-materialize") {
		t.Fatalf("Run error = %v, want import interruption", err)
	}
	job, err := store.GetJob(ctx, "changeset-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ResultObservation != nil {
		t.Fatalf("observation ran after failed import: %+v", payload.ResultObservation)
	}
	if job.State != string(JobFailed) {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	if got := readObservationFile(t, host, "claimed.go"); got != "package original\n" {
		t.Fatalf("host changed despite refused mailbox import: %q", got)
	}
}

func resultCheckByID(checks []ResultCheck, id string) (ResultCheck, bool) {
	for _, check := range checks {
		if check.ID == id {
			return check, true
		}
	}
	return ResultCheck{}, false
}

func observationTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runObservationGit(t, repo, "init")
	runObservationGit(t, repo, "config", "user.email", "tests@gitmoot.local")
	runObservationGit(t, repo, "config", "user.name", "Gitmoot Tests")
	writeObservationFile(t, repo, "tracked.go", "package original\n")
	writeObservationFile(t, repo, "claimed.go", "package original\n")
	writeObservationFile(t, repo, "unmentioned.go", "package original\n")
	runObservationGit(t, repo, "add", "-A")
	runObservationGit(t, repo, "commit", "-m", "base")
	return repo
}

func writeObservationFile(t *testing.T, repo, name, content string) {
	t.Helper()
	fullPath := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", name, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

func readObservationFile(t *testing.T, repo, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return string(content)
}

func observationChangeSet(t *testing.T, name, content string) (host, sandbox string, changes execbackend.ChangeSet) {
	t.Helper()
	host = observationTestRepo(t)
	base := strings.TrimSpace(runObservationGit(t, host, "rev-parse", "HEAD"))
	sandbox = filepath.Join(t.TempDir(), "sandbox")
	cmd := exec.Command("git", "clone", "--no-hardlinks", host, sandbox)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, output)
	}
	writeObservationFile(t, sandbox, name, content)
	var err error
	changes, err = execbackend.BuildChangeSet(context.Background(), sandbox, base)
	if err != nil {
		t.Fatalf("BuildChangeSet: %v", err)
	}
	return host, sandbox, changes
}

func runObservationGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
