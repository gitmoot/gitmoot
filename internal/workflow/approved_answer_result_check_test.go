package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// #2653 (jerryfane/herdr#409 head a4ab657f): a whole-head review approved with
// evidence static_only and declared f1-f4 answered with no rationale. The
// ledger recorded each answer QUOTED, which never discharges, so the approval
// settled the review while the head still owed every finding. The result-check
// audit now flags such an approval, under the result_checks mode: block fails
// it, warn records it, off leaves it alone.
func TestResultChecksFlagApprovalWithUndischargeableAnswers(t *testing.T) {
	const answered = `{"id":"f1","severity":"P2","title":"retry loop leaks","state":"answered"}`
	const static = `{"id":"f1","severity":"P2","title":"retry loop leaks","state":"answered","file":"internal/retry.go","rationale":"the loop now returns on ctx.Done"}`
	verdict := func(decision, evidence, finding string, testsRun string) string {
		severity := ""
		if decision == "changes_requested" {
			severity = `"severity":"P2",`
		}
		return fmt.Sprintf(`{"gitmoot_result":{"decision":%q,%s"summary":"re-checked every prior finding at this head","evidence":%q,"findings":[%s],"changes_made":[],"tests_run":[%s],"needs":[]}}`,
			decision, severity, evidence, finding, testsRun)
	}
	bare := verdict("approved", "static_only", answered, `"read internal/retry.go"`)
	cases := []struct {
		name      string
		mode      ResultCheckMode
		output    string
		wantState JobState
		flagged   bool
	}{
		{name: "block, approved, answered with no evidence", mode: ResultChecksBlock, output: bare, wantState: JobFailed, flagged: true},
		{name: "warn, approved, answered with no evidence", mode: ResultChecksWarn, output: bare, wantState: JobSucceeded, flagged: true},
		{name: "off, approved, answered with no evidence", mode: ResultChecksOff, output: bare, wantState: JobSucceeded},
		{name: "block, approved, answered with executed checks", mode: ResultChecksBlock, output: verdict("approved", "executed", answered, `"go test ./internal/retry/"`), wantState: JobSucceeded},
		{name: "block, approved, static answer with file and rationale", mode: ResultChecksBlock, output: verdict("approved", "static_only", static, ""), wantState: JobSucceeded},
		{name: "block, changes requested, answered with no evidence", mode: ResultChecksBlock, output: verdict("changes_requested", "static_only", answered, `"read internal/retry.go"`), wantState: JobSucceeded},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			mailbox := Mailbox{store: store, resultCheckMode: tc.mode}
			id := fmt.Sprintf("job-approved-answer-%d", i)
			if _, err := mailbox.Enqueue(ctx, JobRequest{ID: id, Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			_, runErr := mailbox.Run(ctx, id, shellAgent(), &fakeDelivery{outputs: []string{tc.output}})
			job, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.State != string(tc.wantState) {
				t.Fatalf("state = %q (run err %v), want %q", job.State, runErr, tc.wantState)
			}
			payload, err := unmarshalPayload(job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var flagged *ResultCheck
			for i := range payload.ResultChecks {
				if payload.ResultChecks[i].ID == "review-approval-answers-discharge" {
					flagged = &payload.ResultChecks[i]
				}
			}
			if !tc.flagged {
				if flagged != nil {
					t.Fatalf("stored checks %+v; want no approval-answer failure", payload.ResultChecks)
				}
				return
			}
			if flagged == nil {
				t.Fatalf("stored checks %+v (run err %v); want the approval-answer failure recorded", payload.ResultChecks, runErr)
			}
			var checksErr *ResultChecksError
			if (tc.mode == ResultChecksBlock) != errors.As(runErr, &checksErr) {
				t.Fatalf("mode %s run err %v; a ResultChecksError is wanted exactly in block mode", tc.mode, runErr)
			}
			// The failure must name the finding and both ways to discharge it.
			for _, want := range []string{"f1 answered", "QUOTED", `"executed"`, `"rationale"`} {
				if !strings.Contains(flagged.Explanation, want) {
					t.Fatalf("explanation %q does not contain %q", flagged.Explanation, want)
				}
			}
		})
	}
}
