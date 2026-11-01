package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2483 (herdrup#587): a review summary claimed screenshot 12 was inspected
// while its tool calls never opened it. The daemon measures which pinned files
// the reviewer's own tool calls named, from the retained omp transcript, and
// the verdict prompt delivered to the requester names every file it never
// opened. A directory listing names no file, and a file whose name appears
// only inside a longer sandbox name ("01-first.png" in "04-01-first.png") was
// not opened.
func TestReviewVerdictWakeNamesPinnedEvidenceTheReviewerNeverOpened(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := defaultJobWorker(store, io.Discard, home)

	// Production-length ids, so the delivered prompt's detail cut is real.
	const jobID = "local-review-gm-review-grok-18e3ec905c188fda-1"
	const head = "0123456789abcdef0123456789abcdef01234567"
	evidence := &workflow.ReviewEvidence{HeadSHA: head, Files: []workflow.ReviewEvidenceFile{
		{SandboxName: "01-first.png"}, {SandboxName: "02-skipped.png"},
		{SandboxName: "03-native.log"}, {SandboxName: "04-01-first.png"},
	}}
	dir := remoteReviewEvidenceDir
	lines := []string{
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"ls -la ` + dir + `"}}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"read","args":{"path":"` + dir + `/04-01-first.png"}}`,
		`{"type":"tool_execution_start","toolCallId":"c3","toolName":"bash","args":{"command":"cd ` + dir + ` && grep -n PASS 03-native.log"}}`,
	}
	logPath := transcript.JobLogPath(paths.Logs, jobID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := db.ReviewVerdictSubjectKey("themartianapp/herdrup", 587, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
		WaiterRole: "herdr-app", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	running, _ := json.Marshal(map[string]any{"repo": "themartianapp/herdrup", "pull_request": 587, "head_sha": head})
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "gm-review-grok", Type: "review", State: "running",
		Repo: "themartianapp/herdrup", PullRequest: 587, Payload: string(running),
	}, db.JobEvent{Kind: "running", Message: "started"}); err != nil {
		t.Fatal(err)
	}

	// The guard's inspect hook, exactly as the remote delivery calls it.
	guard := reviewEvidenceGuardAdapter{
		inner:  staticDeliveryAdapter{},
		verify: func(context.Context) error { return nil },
		inspect: func(ctx context.Context, agent runtime.Agent) {
			worker.recordReviewEvidenceInspection(ctx, jobID, evidence, runtime.OmpRuntime)
		},
	}
	if _, err := guard.Deliver(ctx, runtime.Agent{Name: "gm-review-grok"}, runtime.Job{ID: jobID}); err != nil {
		t.Fatal(err)
	}

	verdict, _ := json.Marshal(map[string]any{
		"repo": "themartianapp/herdrup", "pull_request": 587, "head_sha": head,
		"result": map[string]any{"decision": "approved", "summary": "Screenshots inspected: 01, 02, 04"},
	})
	if changed, err := store.TransitionJobStatePayloadWithEvent(ctx, jobID, "running", "succeeded", string(verdict),
		db.JobEvent{Kind: "succeeded", Message: "verdict"}); err != nil || !changed {
		t.Fatalf("succeeded transition changed=%t err=%v", changed, err)
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	var obligations []db.WakeOutboxObligation
	for _, wake := range pending {
		obligations = append(obligations, db.WakeOutboxObligation{
			ID: wake.ID, SourceKind: wake.SourceKind, SourceID: wake.SourceID,
			TargetRole: wake.TargetRole, CoalesceKey: wake.CoalesceKey, CreatedAt: wake.CreatedAt,
		})
	}
	if len(obligations) != 1 {
		t.Fatalf("pending wakes = %+v, want the one verdict wake", pending)
	}
	event, err := wakeOutboxEvent(obligations, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// What the requester's seat actually receives.
	prompt := eventRuleWakePrompt("fact", event)
	if !strings.Contains(prompt, "verdict approved; reviewer never opened 2 pinned evidence file(s): 01-first.png, 02-skipped.png") ||
		strings.Contains(prompt, "03-native.log") {
		t.Fatalf("delivered verdict prompt = %q, want exactly 01-first.png and 02-skipped.png named as never opened", prompt)
	}
	// The same wake can instead reach the seat through a turn hook, which
	// shows a truncated preview of the inbox message (#2489 review).
	messages, err := store.ListMessages(ctx, "herdr-app", 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("inbox = %+v err=%v, want the one verdict message", messages, err)
	}
	const lead = "verdict approved; reviewer never opened 2 pinned evidence file(s)"
	if !strings.HasPrefix(messages[0].Body, lead) {
		t.Fatalf("inbox body = %q, want it to start with the alert", messages[0].Body)
	}
	hook := renderTurnHookContext(turnHookUserPromptSubmit, "herdr-app", []db.WakeOutboxObligation{{
		ID: obligations[0].ID, MessageID: messages[0].ID, MessageKind: messages[0].Kind,
		MessageSender: messages[0].Sender, MessageBody: messages[0].Body,
	}}, false)
	if !strings.Contains(hook, lead) {
		t.Fatalf("turn-hook text = %q, want the alert", hook)
	}
}

// #2489 review: with many unopened files and the longest valid subject key and
// role, the decision word and count must still reach the requester. Worst
// case: 32 unopened files with long names, a 39-char owner and 100-char repo
// (GitHub maxima), purpose architecture with a 64-char part, and a 182-char
// role. The alert leads the delivered entry, ahead of the key.
func TestUnopenedEvidenceLeadKeepsTheDecisionInsideTheWakeCut(t *testing.T) {
	notOpened := make([]string, workflow.ReviewEvidenceMaxFiles)
	for i := range notOpened {
		notOpened[i] = fmt.Sprintf("%02d-%s.png", i+1, strings.Repeat("A1B2C3D4-", 6))
	}
	const head = "0123456789abcdef0123456789abcdef01234567"
	const jobID = "local-review-gm-review-grok-18e3ec905c188fda-1"
	role := strings.Repeat("r", 182)
	key := strings.Repeat("o", 39) + "/" + strings.Repeat("p", 100) + "#1@" + head + "|architecture:" + strings.Repeat("x", 64)
	payload, _ := json.Marshal(db.AwaitedFactWakePayload{
		ID: 4123, WaiterRole: role, SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: key, State: db.AwaitedFactStateSatisfied, JobID: jobID,
		Detail: "review verdict changes_requested from gm-review-grok job " + jobID + " at head " + head + "; findings=3 executed_checks=0 evidence=static_only; inspect with gitmoot job show " + jobID,
		Alert:  db.ReviewEvidenceUnopenedLead("changes_requested", notOpened),
	})
	event, err := wakeOutboxEvent([]db.WakeOutboxObligation{{
		ID: 1, SourceKind: db.WakeOutboxSourceAwaitedFact, SourceID: string(payload),
		TargetRole: role, CoalesceKey: "fact:" + role, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	prompt := eventRuleWakePrompt("fact", event)
	for _, want := range []string{"verdict changes_requested", "reviewer never opened 32 pinned evidence file(s)", notOpened[0]} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("delivered prompt lost %q: %q", want, prompt)
		}
	}
}

type staticDeliveryAdapter struct{}

func (staticDeliveryAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, nil
}
