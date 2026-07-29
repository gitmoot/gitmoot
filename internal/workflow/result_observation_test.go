package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/evidence"
)

func TestCompareResultChangesFlagsClaimedFileAbsentFromDiff(t *testing.T) {
	observation := compareResultChanges(
		[]string{"updated internal/workflow/absent.go"},
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
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{"updated internal/workflow/absent.go"}, TestsRun: []string{"go test ./..."}},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("claimed-only divergence was not readable by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesFlagsDiffFileNoClaimMentions(t *testing.T) {
	observation := compareResultChanges(
		[]string{"updated internal/workflow/claimed.go"},
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
		Result:      AgentResult{Decision: "implemented", ChangesMade: []string{"updated internal/workflow/claimed.go"}, TestsRun: []string{"go test ./..."}},
		Observation: observation,
	})
	if _, ok := failed["implement-changes-observed"]; !ok {
		t.Fatalf("unclaimed diff file was not readable by the result gate; failed=%v", keys(failed))
	}
}

func TestCompareResultChangesFlagsUnboundStructuredClaim(t *testing.T) {
	claim := "Deleted _clear_private_pane_composer and removed its call"
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
		ChangesMade: []string{"updated tracked.go", "added new.go"},
	})
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
			mailbox := Mailbox{Store: store, resultCheckMode: tc.mode}
			output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["updated absent.go"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
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

	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksWarn}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["updated claimed.go"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
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
	mailbox := Mailbox{Store: store, resultCheckMode: ResultChecksBlock}
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["updated foo.go"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
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

func runObservationGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
