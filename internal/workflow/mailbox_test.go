package workflow

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func TestMailboxEnqueueCreatesQueuedJobAndEvent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:          "job-1",
		Agent:       "audit",
		Action:      "review",
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-005",
		PullRequest: 5,
		TaskID:      "task-5",
		TaskTitle:   "Job Mailbox",
		Sender:      "octocat",
		Constraints: []string{"  preserve behavior  ", ""},
	})

	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if job.State != string(JobQueued) {
		t.Fatalf("state = %q, want queued", job.State)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.Payload == "" || !strings.Contains(stored.Payload, `"repo":"jerryfane/gitmoot"`) {
		t.Fatalf("payload = %q", stored.Payload)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "queued" {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxEnqueuePersistsDelegationMetadata(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:              "job-child",
		Agent:           "audit",
		Action:          "ask",
		Repo:            "jerryfane/gitmoot",
		Branch:          "task-005",
		ParentJobID:     "job-parent",
		DelegationID:    "delegation-1",
		DelegationDepth: 2,
		DelegatedBy:     "lead",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if job.ParentJobID != "job-parent" || job.DelegationID != "delegation-1" || job.DelegationDepth != 2 || job.DelegatedBy != "lead" {
		t.Fatalf("returned job metadata = %+v", job)
	}

	stored, err := store.GetJob(ctx, "job-child")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.ParentJobID != "job-parent" || stored.DelegationID != "delegation-1" || stored.DelegationDepth != 2 || stored.DelegatedBy != "lead" {
		t.Fatalf("stored job metadata = %+v", stored)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.ParentJobID != "job-parent" || payload.DelegationID != "delegation-1" || payload.DelegationDepth != 2 || payload.DelegatedBy != "lead" {
		t.Fatalf("payload metadata = %+v", payload)
	}
}

func TestMailboxEnqueuePersistsEphemeralSpec(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "job-ephemeral",
		Agent:        "worker-ephemeral-abc123",
		Action:       "review",
		Repo:         "jerryfane/gitmoot",
		ParentJobID:  "job-parent",
		DelegationID: "worker",
		Ephemeral:    &EphemeralSpec{Runtime: "codex", Model: "gpt-5.4"},
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-ephemeral")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	// The marshalled payload must carry the ephemeral key for downstream consumers.
	if !strings.Contains(stored.Payload, `"ephemeral"`) {
		t.Fatalf("payload = %q, want it to contain the ephemeral spec", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Ephemeral == nil {
		t.Fatalf("payload missing ephemeral spec: %+v", payload)
	}
	if payload.Ephemeral.Runtime != "codex" || payload.Ephemeral.Model != "gpt-5.4" {
		t.Fatalf("payload ephemeral spec = %+v", payload.Ephemeral)
	}
}

// TestMailboxClaimStampsRunnerBootID pins that the queued->running claim stamps
// the claiming process's boot id (#651), routing through Store.ClaimRunningJob. It
// asserts behaviorally via the foreign-boot requeue predicate: a claimed job looks
// "same-boot" to the current boot (never requeued) but "foreign" to a different
// boot id (requeued) — which can only hold if claim recorded a concrete, non-empty
// runner_boot_id. On the unpatched claim (plain TransitionJobStateWithEvent)
// runner_boot_id stays '' and the foreign-boot requeue matches nothing, so this
// test fails without the fix.
func TestMailboxClaimStampsRunnerBootID(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-claim", Agent: "audit", Action: "ask", Repo: "jerryfane/gitmoot"})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := mailbox.claim(ctx, job); err != nil {
		t.Fatalf("claim returned error: %v", err)
	}
	running, err := store.GetJob(ctx, "job-claim")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if running.State != string(JobRunning) {
		t.Fatalf("job state after claim = %q, want running", running.State)
	}

	// Same boot: the claimed job is NOT foreign, so it is protected.
	if requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, db.BootID()); err != nil {
		t.Fatalf("RequeueRunningJobsFromForeignBoot(current) returned error: %v", err)
	} else if len(requeued) != 0 {
		t.Fatalf("same-boot requeue = %v, want none (claim must stamp the CURRENT boot)", requeued)
	}

	// A different boot proves claim stamped a concrete boot id: the job now looks
	// foreign and is requeued.
	requeued, err := store.RequeueRunningJobsFromForeignBoot(ctx, "foreign-"+db.BootID())
	if err != nil {
		t.Fatalf("RequeueRunningJobsFromForeignBoot(foreign) returned error: %v", err)
	}
	if len(requeued) != 1 || requeued[0] != "job-claim" {
		t.Fatalf("foreign-boot requeue = %v, want [job-claim] (claim must stamp runner_boot_id)", requeued)
	}
}

func TestMailboxEnqueuePersistsModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-model",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
		Model:  "opus",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-model")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !strings.Contains(stored.Payload, `"model":"opus"`) {
		t.Fatalf("payload = %q, want it to contain model override", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Model != "opus" {
		t.Fatalf("payload.Model = %q, want %q", payload.Model, "opus")
	}
}

func TestMailboxEnqueueOmitsEmptyModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-model",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-no-model")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if strings.Contains(stored.Payload, `"model"`) {
		t.Fatalf("payload = %q, want no model key when override is empty", stored.Payload)
	}
}

func TestMailboxEnqueuePersistsPhase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-phase",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
		Phase:  "design",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-phase")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !strings.Contains(stored.Payload, `"phase":"design"`) {
		t.Fatalf("payload = %q, want it to contain phase", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Phase != "design" {
		t.Fatalf("payload.Phase = %q, want %q", payload.Phase, "design")
	}
}

func TestMailboxEnqueueOmitsEmptyPhase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-phase",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-no-phase")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if strings.Contains(stored.Payload, `"phase"`) {
		t.Fatalf("payload = %q, want no phase key when phase is empty", stored.Payload)
	}
}

func TestMailboxRunDeliversModelOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot", Model: "opus"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(adapter.models) != 1 || adapter.models[0] != "opus" {
		t.Fatalf("delivered runtime.Job models = %+v, want the payload model override [opus]", adapter.models)
	}
}

// okDeliveryResult is a minimal well-formed gitmoot_result so a fake delivery
// classifies to a terminal state and the job persists its recorded usage.
const okDeliveryResult = `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

// TestMailboxDeliverDeltasCumulativeUsage pins the #661 wiring: when the adapter
// flags usage CUMULATIVE (a codex resumed thread reports the session's running
// total), the mailbox records only this job's per-session delta, not the whole
// cumulative. Two sequential deliveries on the SAME agent runtime ref report
// cumulatives 1000/100 then 1500/150; the first job persists the full 1000/100
// (baseline 0) and the second persists only the 500/50 increase.
func TestMailboxDeliverDeltasCumulativeUsage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "planner", Runtime: runtime.CodexRuntime, RuntimeRef: "019f3041-cfed-7e82-8766-b5ca75cf92da", RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs:      []string{okDeliveryResult, okDeliveryResult},
		inputTokens:  []int{1000, 1500},
		outputTokens: []int{100, 150},
		cumulative:   []bool{true, true},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-a", Agent: "planner", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue job-a: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-a", agent, adapter); err != nil {
		t.Fatalf("Run job-a: %v", err)
	}
	jobA, err := store.GetJob(ctx, "job-a")
	if err != nil {
		t.Fatalf("GetJob job-a: %v", err)
	}
	if jobA.InputTokens != 1000 || jobA.OutputTokens != 100 {
		t.Fatalf("job-a usage = (%d, %d), want (1000, 100) — full cumulative on a fresh session baseline", jobA.InputTokens, jobA.OutputTokens)
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-b", Agent: "planner", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue job-b: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-b", agent, adapter); err != nil {
		t.Fatalf("Run job-b: %v", err)
	}
	jobB, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b: %v", err)
	}
	if jobB.InputTokens != 500 || jobB.OutputTokens != 50 {
		t.Fatalf("job-b usage = (%d, %d), want (500, 50) — only the per-session delta, not the 1500/150 cumulative", jobB.InputTokens, jobB.OutputTokens)
	}
}

// TestMailboxDeliverRecordsNonCumulativeVerbatim guards #664 at the mailbox seam:
// usage NOT flagged cumulative (fresh sessions, single-use ephemeral/temp workers,
// and every non-codex runtime) is recorded verbatim and NEVER routed through the
// per-session delta table. Two deliveries on the same ref report independent per-job
// counts and each lands in full — a delta would have clamped the second (smaller)
// count against the first.
func TestMailboxDeliverRecordsNonCumulativeVerbatim(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "impl", Runtime: runtime.CodexRuntime, RuntimeRef: "fresh-thread-xyz", RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs:      []string{okDeliveryResult, okDeliveryResult},
		inputTokens:  []int{3000, 2000},
		outputTokens: []int{300, 200},
		cumulative:   []bool{false, false},
	}

	for _, tc := range []struct {
		id      string
		wantIn  int
		wantOut int
	}{
		{"job-c", 3000, 300},
		{"job-d", 2000, 200},
	} {
		if _, err := mailbox.Enqueue(ctx, JobRequest{ID: tc.id, Agent: "impl", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
			t.Fatalf("Enqueue %s: %v", tc.id, err)
		}
		if _, err := mailbox.Run(ctx, tc.id, agent, adapter); err != nil {
			t.Fatalf("Run %s: %v", tc.id, err)
		}
		job, err := store.GetJob(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", tc.id, err)
		}
		if job.InputTokens != tc.wantIn || job.OutputTokens != tc.wantOut {
			t.Fatalf("%s usage = (%d, %d), want (%d, %d) verbatim — a false flag must not be delta'd", tc.id, job.InputTokens, job.OutputTokens, tc.wantIn, tc.wantOut)
		}
	}
}

// TestMailboxDeliverCumulativeAmbiguousRefContributesZero pins the fallback: a
// cumulative-flagged delivery on an empty or "last" runtime ref names no concrete,
// stable session to key a delta on, so — as before #661 — it contributes 0 rather
// than mis-attributing the whole cumulative to the job.
func TestMailboxDeliverCumulativeAmbiguousRefContributesZero(t *testing.T) {
	for _, ref := range []string{"", runtime.LastRef} {
		t.Run("ref="+ref, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			mailbox := Mailbox{Store: store}
			agent := runtime.Agent{Name: "impl", Runtime: runtime.CodexRuntime, RuntimeRef: ref, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
			adapter := &fakeDelivery{
				outputs:      []string{okDeliveryResult},
				inputTokens:  []int{2000},
				outputTokens: []int{200},
				cumulative:   []bool{true},
			}
			if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-e", Agent: "impl", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if _, err := mailbox.Run(ctx, "job-e", agent, adapter); err != nil {
				t.Fatalf("Run: %v", err)
			}
			job, err := store.GetJob(ctx, "job-e")
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.InputTokens != 0 || job.OutputTokens != 0 {
				t.Fatalf("ambiguous-ref usage = (%d, %d), want (0, 0)", job.InputTokens, job.OutputTokens)
			}
		})
	}
}

func TestMailboxEnqueuePersistsRootJobID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:        "job-child",
		Agent:     "audit",
		Action:    "ask",
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		RootJobID: "root-coordinator",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-child")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.RootJobID != "root-coordinator" {
		t.Fatalf("payload RootJobID = %q, want %q", payload.RootJobID, "root-coordinator")
	}
}

func TestMailboxEnqueueSnapshotsAgentTemplate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "audit",
		Role:         "reviewer",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "jerryfane/gitmoot",
		TemplateID:   "thermo",
		Capabilities: []string{"review"},
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-1",
		Agent:  "audit",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "def456",
		Content:        "Updated instructions.",
	}); err != nil {
		t.Fatalf("second UpsertAgentTemplate returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "thermo" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("payload template snapshot = %+v", payload)
	}

	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "audit-pinned",
		Role:         "reviewer",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "jerryfane/gitmoot",
		TemplateID:   "thermo@v1",
		Capabilities: []string{"review"},
	}); err != nil {
		t.Fatalf("UpsertAgent pinned returned error: %v", err)
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-2",
		Agent:  "audit-pinned",
		Action: "review",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue pinned returned error: %v", err)
	}
	pinnedJob, err := store.GetJob(ctx, "job-2")
	if err != nil {
		t.Fatalf("GetJob pinned returned error: %v", err)
	}
	pinnedPayload, err := unmarshalPayload(pinnedJob.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload pinned returned error: %v", err)
	}
	if pinnedPayload.TemplateID != "thermo" || pinnedPayload.TemplateResolvedCommit != "abc123" || pinnedPayload.TemplateContent != "Review deeply." {
		t.Fatalf("pinned payload template snapshot = %+v", pinnedPayload)
	}
}

func TestMailboxEnqueueAppliesTemplateOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	// The agent carries its own template; a --recipe override should win over it
	// in the enqueued payload without rebinding the agent's identity.
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "agent-own",
		Name:           "Agent Own",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		ResolvedCommit: "own123",
		Content:        "Agent's own prompt.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate own returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "planner",
		Role:         "coordinator",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "jerryfane/gitmoot",
		TemplateID:   "agent-own",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	override := db.AgentTemplate{
		ID:             "review-panel",
		Name:           "Review Panel",
		SourceRepo:     "jerryfane/gitmoot",
		SourceRef:      "main",
		ResolvedCommit: "recipe456",
		Content:        "Recipe prompt.",
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:               "job-override",
		Agent:            "planner",
		Action:           "ask",
		Repo:             "jerryfane/gitmoot",
		TemplateOverride: &override,
	}); err != nil {
		t.Fatalf("Enqueue with override returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-override")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "review-panel" || payload.TemplateResolvedCommit != "recipe456" || payload.TemplateContent != "Recipe prompt." {
		t.Fatalf("override payload template snapshot = %+v, want the recipe override", payload)
	}

	// Without an override the agent's own template still wins.
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-override",
		Agent:  "planner",
		Action: "ask",
		Repo:   "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue without override returned error: %v", err)
	}
	baselineJob, err := store.GetJob(ctx, "job-no-override")
	if err != nil {
		t.Fatalf("GetJob baseline returned error: %v", err)
	}
	baseline, err := unmarshalPayload(baselineJob.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload baseline returned error: %v", err)
	}
	if baseline.TemplateID != "agent-own" || baseline.TemplateContent != "Agent's own prompt." {
		t.Fatalf("baseline payload template snapshot = %+v, want the agent's own template", baseline)
	}
}

func TestMailboxRunIncludesTemplateSnapshotInPrompt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"clean","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	templateContent := agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   "thermo",
		Name:                 "Thermo",
		Description:          "Reviews deeply.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask", "review"},
		RuntimeCompatibility: []string{"codex"},
		Tags:                 []string{"review"},
		Inputs:               []string{"repo", "diff"},
		Outputs:              []string{"review_findings"},
	}, "# Thermo\n\nReview deeply.")
	payload, err := marshalPayload(JobPayload{
		Repo:                   "jerryfane/gitmoot",
		TemplateID:             "thermo",
		TemplateResolvedCommit: "abc123",
		TemplateContent:        templateContent,
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(JobQueued), Payload: payload}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(adapter.prompts) != 1 ||
		!strings.Contains(adapter.prompts[0], "Template instructions:\n# Thermo\n\nReview deeply.") ||
		strings.Contains(adapter.prompts[0], "kind: agent-template") {
		t.Fatalf("prompt = %+v", adapter.prompts)
	}
}

func TestUnmarshalPayloadMapsLegacyPresetSnapshot(t *testing.T) {
	payload, err := unmarshalPayload(`{"repo":"owner/repo","preset_id":"thermo","preset_resolved_commit":"abc123","preset_content":"Review deeply."}`)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "thermo" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("legacy preset snapshot mapped to %+v", payload)
	}

	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if strings.Contains(encoded, "preset_") || !strings.Contains(encoded, `"template_id":"thermo"`) {
		t.Fatalf("payload was not rewritten with template fields: %s", encoded)
	}
}

func TestMailboxRunStoresResultAndSucceeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["mailbox"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "jerryfane/gitmoot", Branch: "task-005", PullRequest: 5}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("decision = %q", result.Decision)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded", stored.State)
	}
	if !strings.Contains(stored.Payload, `"result"`) || !strings.Contains(stored.Payload, `"raw_outputs"`) {
		t.Fatalf("payload = %q", stored.Payload)
	}
	if len(adapter.prompts) != 1 || !strings.Contains(adapter.prompts[0], "Required output") {
		t.Fatalf("prompts = %+v", adapter.prompts)
	}
}

func TestMailboxRunUsesAdapterSummaryWhenAvailable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ClaudeRuntime, RuntimeRef: runtime.LastRef, RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{`{"result":"wrapped by runtime"}`},
		summaries: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"parsed from summary","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "parsed from summary" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(adapter.prompts))
	}
}

func TestMailboxRunPersistsRefreshedRuntimeRef(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	oldRef := "550e8400-e29b-41d4-a716-446655440002"
	newRef := "550e8400-e29b-41d4-a716-446655440099"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: oldRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent := runtime.Agent{Name: "shipper", Runtime: runtime.ClaudeRuntime, RuntimeRef: oldRef, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["x"],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{newRef},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "shipper", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stored, err := store.GetAgent(ctx, "shipper")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.RuntimeRef != newRef {
		t.Fatalf("agent runtime_ref = %q, want re-pinned %q", stored.RuntimeRef, newRef)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "session_refresh_retry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a session_refresh_retry event, got %+v", events)
	}
}

// TestMailboxRunRepairRetryResumesRefreshedRef pins the invariant from #443: when
// the first delivery self-heals a dead session (returning a fresh ref) but emits
// malformed output, the repair retry must resume the freshly-minted session — not
// re-resume the dead UUID, which would self-heal a second time and orphan the
// first healed session. We assert the in-memory agent handed to the second Deliver
// carries the refreshed ref.
func TestMailboxRunRepairRetryResumesRefreshedRef(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	oldRef := "550e8400-e29b-41d4-a716-446655440002"
	newRef := "550e8400-e29b-41d4-a716-446655440099"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: oldRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent := runtime.Agent{Name: "shipper", Runtime: runtime.ClaudeRuntime, RuntimeRef: oldRef, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		// First delivery self-heals (newRef) but is malformed; the repair delivery
		// returns a clean result without further refresh.
		outputs: []string{
			"healed but no json",
			`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["x"],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{newRef},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "shipper", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(adapter.agentRefs) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.agentRefs))
	}
	if adapter.agentRefs[0] != oldRef {
		t.Fatalf("first delivery agent ref = %q, want dead %q", adapter.agentRefs[0], oldRef)
	}
	if adapter.agentRefs[1] != newRef {
		t.Fatalf("repair delivery agent ref = %q, want refreshed %q (must not re-resume the dead ref)", adapter.agentRefs[1], newRef)
	}
}

func TestMailboxRunFreshRefRepairUsesConcreteSessionWithoutPersisting(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	freshRef := "fresh:council-codex"
	concreteRef := "codex-thread-123"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: freshRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent := runtime.Agent{Name: "shipper", Runtime: runtime.CodexRuntime, RuntimeRef: freshRef, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs: []string{
			"fresh job session but no json",
			`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["x"],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{concreteRef},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "shipper", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(adapter.agentRefs) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.agentRefs))
	}
	if adapter.agentRefs[0] != freshRef {
		t.Fatalf("first delivery ref = %q, want registered fresh ref", adapter.agentRefs[0])
	}
	if adapter.agentRefs[1] != concreteRef {
		t.Fatalf("repair delivery ref = %q, want concrete job session", adapter.agentRefs[1])
	}
	stored, err := store.GetAgent(ctx, "shipper")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.RuntimeRef != freshRef {
		t.Fatalf("stored runtime_ref = %q, want registered fresh ref %q", stored.RuntimeRef, freshRef)
	}
}

// TestMailboxRunEphemeralRefAdoptedButNotPersisted pins F1 (#665): a delivery
// that mints a by-design per-job session (SessionEphemeral=true — the
// last+template coordinator path and the #531 fresh-ref path route through
// deliverFresh) must ADOPT the concrete session in-memory so a same-job repair
// resumes it, but must NEVER persist it onto the agent's stored "last"
// registration, and must NOT emit the dead-session self-heal event. Without the
// ephemeral guard the "last" agent (registeredFreshRef=false) would be re-pinned
// to the minted UUID and lose isolation after job 1.
func TestMailboxRunEphemeralRefAdoptedButNotPersisted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	concreteRef := "claude-ephemeral-session-1"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "coordinator",
		Role:       "agent",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: runtime.LastRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent := runtime.Agent{Name: "coordinator", Runtime: runtime.ClaudeRuntime, RuntimeRef: runtime.LastRef, RepoScope: "jerryfane/gitmoot", Role: "agent", TemplateID: "coordinator"}
	adapter := &fakeDelivery{
		// First delivery mints an ephemeral session but is malformed; the repair
		// delivery must resume that concrete session, not "last".
		outputs: []string{
			"ephemeral session but no json",
			`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["x"],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{concreteRef},
		ephemeral:     []bool{true},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "coordinator", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(adapter.agentRefs) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.agentRefs))
	}
	if adapter.agentRefs[0] != runtime.LastRef {
		t.Fatalf("first delivery ref = %q, want %q", adapter.agentRefs[0], runtime.LastRef)
	}
	if adapter.agentRefs[1] != concreteRef {
		t.Fatalf("repair delivery ref = %q, want in-memory-adopted concrete session %q", adapter.agentRefs[1], concreteRef)
	}
	stored, err := store.GetAgent(ctx, "coordinator")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.RuntimeRef != runtime.LastRef {
		t.Fatalf("stored runtime_ref = %q, want unchanged %q — an ephemeral session must never be persisted", stored.RuntimeRef, runtime.LastRef)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, e := range events {
		if e.Kind == "session_refresh_retry" {
			t.Fatalf("unexpected session_refresh_retry event on a healthy ephemeral delivery: %+v", events)
		}
	}
}

// TestMailboxRunEphemeralRefStaysFreshAcrossJobs pins the user-visible F1 (#665)
// invariant: after a first ephemeral delivery, a SECOND job for the same agent
// must again take the fresh path. Because job 1 never persisted its minted UUID,
// the agent's stored ref stays "last", so job 2 (loaded from the store, as the
// daemon does) delivers on "last" again — never resuming job 1's session.
func TestMailboxRunEphemeralRefStaysFreshAcrossJobs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "coordinator",
		Role:       "agent",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: runtime.LastRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	adapter := &fakeDelivery{
		outputs:       []string{okDeliveryResult, okDeliveryResult},
		refreshedRefs: []string{"claude-ephemeral-job1", "claude-ephemeral-job2"},
		ephemeral:     []bool{true, true},
	}

	for _, jobID := range []string{"job-1", "job-2"} {
		// Reload the agent from the store before each job, exactly as the daemon
		// does: if job 1 had persisted its minted UUID, job 2 would load it here and
		// resume the wrong session.
		stored, err := store.GetAgent(ctx, "coordinator")
		if err != nil {
			t.Fatalf("GetAgent before %s returned error: %v", jobID, err)
		}
		if stored.RuntimeRef != runtime.LastRef {
			t.Fatalf("stored runtime_ref before %s = %q, want %q — an ephemeral session leaked into the stored ref", jobID, stored.RuntimeRef, runtime.LastRef)
		}
		agent := runtime.Agent{Name: "coordinator", Runtime: runtime.ClaudeRuntime, RuntimeRef: stored.RuntimeRef, RepoScope: "jerryfane/gitmoot", Role: "agent", TemplateID: "coordinator"}
		if _, err := mailbox.Enqueue(ctx, JobRequest{ID: jobID, Agent: "coordinator", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
			t.Fatalf("Enqueue %s returned error: %v", jobID, err)
		}
		if _, err := mailbox.Run(ctx, jobID, agent, adapter); err != nil {
			t.Fatalf("Run %s returned error: %v", jobID, err)
		}
	}

	if len(adapter.agentRefs) != 2 {
		t.Fatalf("deliveries = %d, want 2 (one per job)", len(adapter.agentRefs))
	}
	for i, ref := range adapter.agentRefs {
		if ref != runtime.LastRef {
			t.Fatalf("delivery %d ref = %q, want %q — job 2 must not resume job 1's ephemeral session", i, ref, runtime.LastRef)
		}
	}
}

func TestMailboxRunNoRefreshLeavesRefUnchanged(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	oldRef := "550e8400-e29b-41d4-a716-446655440002"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.ClaudeRuntime,
		RuntimeRef: oldRef,
		RepoScope:  "jerryfane/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	agent := runtime.Agent{Name: "shipper", Runtime: runtime.ClaudeRuntime, RuntimeRef: oldRef, RepoScope: "jerryfane/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["x"],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "shipper", Action: "implement", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stored, err := store.GetAgent(ctx, "shipper")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.RuntimeRef != oldRef {
		t.Fatalf("agent runtime_ref = %q, want unchanged %q", stored.RuntimeRef, oldRef)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, e := range events {
		if e.Kind == "session_refresh_retry" {
			t.Fatalf("unexpected session_refresh_retry event with no refresh: %+v", events)
		}
	}
}

func TestMailboxRunRetriesMalformedOutputOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"review complete, no json",
		`{"gitmoot_result":{"decision":"approved","summary":"clean after repair","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "clean after repair" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], "Previous raw output") {
		t.Fatalf("repair prompt = %s", adapter.prompts[1])
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "malformed_output") || !hasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxRunSalvagesMissingEnvelopeAfterSecondRepair(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"findings posted, no json",
		"still no json",
		`{"gitmoot_result":{"decision":"approved","summary":"salvaged after second repair","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "salvaged after second repair" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 3 {
		t.Fatalf("deliveries = %d, want 3", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], "Previous raw output") {
		t.Fatalf("first repair prompt = %s", adapter.prompts[1])
	}
	if !strings.Contains(adapter.prompts[2], "Previous raw output") {
		t.Fatalf("second repair prompt = %s", adapter.prompts[2])
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded", stored.State)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "malformed_output") || !hasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxRunFailsAfterExhaustingRepairs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"no json here",
		"still no json",
		"and again no json",
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run succeeded despite exhausting all repair attempts")
	}
	if len(adapter.prompts) != 3 {
		t.Fatalf("deliveries = %d, want 3 (initial + maxRepairAttempts)", len(adapter.prompts))
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed", stored.State)
	}
}

func TestMailboxRunMarksBlockedDecision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"needs credentials","findings":[],"changes_made":[],"tests_run":[],"needs":["GITHUB_TOKEN"],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobBlocked) {
		t.Fatalf("state = %q, want blocked", stored.State)
	}
}

func TestMailboxRunRejectsNonQueuedJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"should not run","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run accepted a non-queued job")
	}
	if len(adapter.prompts) != 0 {
		t.Fatalf("adapter was called for non-queued job: %+v", adapter.prompts)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
	if strings.Contains(stored.Payload, `"result"`) {
		t.Fatalf("cancelled job stored final result payload: %s", stored.Payload)
	}
}

func TestMailboxRunPreservesCancellationDuringDelivery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"completed after cancellation","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		onDeliver: func() {
			if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run completed a job cancelled during delivery")
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
}

func TestMailboxRunSkipsRepairAfterCancellation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{"malformed"},
		onDeliver: func() {
			if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "jerryfane/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run repaired a job cancelled after malformed output")
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(adapter.prompts))
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
}

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func hasEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

type fakeDelivery struct {
	outputs       []string
	summaries     []string
	refreshedRefs []string
	ephemeral     []bool
	inputTokens   []int
	outputTokens  []int
	cumulative    []bool
	prompts       []string
	models        []string
	agentRefs     []string
	onDeliver     func()
	err           error
}

func (f *fakeDelivery) Deliver(_ context.Context, agent runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.prompts = append(f.prompts, job.Prompt)
	f.models = append(f.models, job.Model)
	f.agentRefs = append(f.agentRefs, agent.RuntimeRef)
	if f.onDeliver != nil {
		f.onDeliver()
	}
	if f.err != nil {
		return runtime.Result{}, f.err
	}
	index := len(f.prompts) - 1
	result := runtime.Result{}
	if index >= len(f.outputs) {
		return result, nil
	}
	result.Raw = f.outputs[index]
	if index < len(f.summaries) {
		result.Summary = f.summaries[index]
	}
	if index < len(f.refreshedRefs) {
		result.RefreshedRuntimeRef = f.refreshedRefs[index]
	}
	if index < len(f.ephemeral) {
		result.SessionEphemeral = f.ephemeral[index]
	}
	if index < len(f.inputTokens) {
		result.InputTokens = f.inputTokens[index]
	}
	if index < len(f.outputTokens) {
		result.OutputTokens = f.outputTokens[index]
	}
	if index < len(f.cumulative) {
		result.CumulativeUsage = f.cumulative[index]
	}
	return result, nil
}

func TestMailboxEnqueuePersistsCockpitFields(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:             "job-cockpit",
		Agent:          "audit",
		Action:         "ask",
		Repo:           "jerryfane/gitmoot",
		Branch:         "task-005",
		Sender:         "local",
		Cockpit:        true,
		CockpitSession: "  review-room  ",
		CockpitPaneKey: "  seat-1  ",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-cockpit")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !payload.Cockpit {
		t.Fatalf("payload.Cockpit = false, want true")
	}
	// CockpitSession/CockpitPaneKey are trimmed on enqueue.
	if payload.CockpitSession != "review-room" {
		t.Fatalf("payload.CockpitSession = %q, want %q", payload.CockpitSession, "review-room")
	}
	if payload.CockpitPaneKey != "seat-1" {
		t.Fatalf("payload.CockpitPaneKey = %q, want %q", payload.CockpitPaneKey, "seat-1")
	}
}

func TestJobPayloadCockpitRoundTrip(t *testing.T) {
	encoded, err := marshalPayload(JobPayload{Cockpit: true, CockpitSession: "room", CockpitPaneKey: "job"})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	got, err := unmarshalPayload(encoded)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if !got.Cockpit || got.CockpitSession != "room" || got.CockpitPaneKey != "job" {
		t.Fatalf("round-trip wrong: %+v", got)
	}
	// Cockpit defaults are omitempty: a zero payload encodes without the keys.
	zero, err := marshalPayload(JobPayload{})
	if err != nil {
		t.Fatalf("marshalPayload zero: %v", err)
	}
	if strings.Contains(zero, "cockpit") {
		t.Fatalf("zero payload should omit cockpit keys: %s", zero)
	}
}

func TestParseJobPayloadExported(t *testing.T) {
	encoded, err := marshalPayload(JobPayload{Repo: "o/r", PullRequest: 7, Instructions: "do it", Result: &AgentResult{Decision: "approved", Summary: "done"}})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	got, err := ParseJobPayload(encoded)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if got.Repo != "o/r" || got.PullRequest != 7 || got.Result == nil || got.Result.Decision != "approved" {
		t.Fatalf("round-trip wrong: %+v", got)
	}
	// Empty/malformed input errors (caller treats as no detail).
	if _, err := ParseJobPayload(""); err == nil {
		t.Fatal("empty payload should error")
	}
}

// seedCanaryAgent installs a "planner" template (v1 current champion) + a pending
// v2 candidate with a DISTINCT resolved_commit, promotes v2 to a canary at the
// given sample, and binds an agent "planner-agent" to the template (unpinned). It
// returns the store ready for an Enqueue with an injected CanaryRand.
func seedCanaryAgent(t *testing.T, store *db.Store, sample float64) {
	t.Helper()
	ctx := context.Background()
	base := db.AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "commit-v1", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	v2 := base
	v2.ResolvedCommit = "commit-v2"
	v2.Content = "v2 content"
	pending, err := store.AddPendingAgentTemplateVersion(ctx, v2)
	if err != nil {
		t.Fatalf("add v2: %v", err)
	}
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, sample); err != nil {
		t.Fatalf("canary promote: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "planner-agent", Role: "planner", Runtime: "codex", RuntimeRef: "last", RepoScope: "o/r", TemplateID: "planner", Capabilities: []string{"ask"}}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
}

func enqueueAndResolve(t *testing.T, mailbox Mailbox, jobID string) JobPayload {
	t.Helper()
	ctx := context.Background()
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: jobID, Agent: "planner-agent", Action: "ask", Repo: "o/r"}); err != nil {
		t.Fatalf("Enqueue %s: %v", jobID, err)
	}
	stored, err := mailbox.Store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob %s: %v", jobID, err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload %s: %v", jobID, err)
	}
	return payload
}

// TestMailboxCanaryRoutingHitsCanary proves a draw BELOW the sample routes the
// resolution to the canary version's snapshot (its distinct resolved_commit +
// content), so the #465 harvester later attributes the outcome to the canary.
func TestMailboxCanaryRoutingHitsCanary(t *testing.T) {
	store := openTestStore(t)
	seedCanaryAgent(t, store, 1.0)
	mailbox := Mailbox{Store: store, CanaryEnabled: true, CanaryRand: func() float64 { return 0.0 }}
	payload := enqueueAndResolve(t, mailbox, "job-canary")
	if payload.TemplateResolvedCommit != "commit-v2" || payload.TemplateContent != "v2 content" {
		t.Fatalf("draw below sample must route to canary, got commit=%q content=%q", payload.TemplateResolvedCommit, payload.TemplateContent)
	}
	if payload.TemplateID != "planner" {
		t.Fatalf("template id = %q, want planner", payload.TemplateID)
	}
}

// TestMailboxCanaryRoutingMissesCanary proves a draw AT/ABOVE the sample resolves
// the champion (byte-identical to the no-canary champion snapshot).
func TestMailboxCanaryRoutingMissesCanary(t *testing.T) {
	store := openTestStore(t)
	seedCanaryAgent(t, store, 0.5)
	mailbox := Mailbox{Store: store, CanaryEnabled: true, CanaryRand: func() float64 { return 0.9 }}
	payload := enqueueAndResolve(t, mailbox, "job-champ")
	if payload.TemplateResolvedCommit != "commit-v1" || payload.TemplateContent != "v1 content" {
		t.Fatalf("draw at/above sample must route to champion, got commit=%q content=%q", payload.TemplateResolvedCommit, payload.TemplateContent)
	}
}

// TestMailboxCanaryDisabledIgnoresLiveCanary proves the #484 routing seam is gated
// on the SAME policy the daemon's comparator uses: with an ACTIVE canary row but
// CanaryEnabled=false (the knob off), even a forced-hit draw (0.0) resolves the
// champion, never the canary. This is what stops a canary that outlived the
// manager from continuing to serve sampled traffic with no auto-rollback once the
// knob is turned off.
func TestMailboxCanaryDisabledIgnoresLiveCanary(t *testing.T) {
	store := openTestStore(t)
	seedCanaryAgent(t, store, 1.0)
	// CanaryEnabled defaults false; a forced-hit rng would route if the gate were
	// open, so resolving the champion proves the gate short-circuits before the draw.
	mailbox := Mailbox{Store: store, CanaryRand: func() float64 { return 0.0 }}
	payload := enqueueAndResolve(t, mailbox, "job-gated")
	if payload.TemplateResolvedCommit != "commit-v1" || payload.TemplateContent != "v1 content" {
		t.Fatalf("canary disabled must resolve champion, got commit=%q content=%q", payload.TemplateResolvedCommit, payload.TemplateContent)
	}
}

// TestMailboxCanaryOffByDefaultByteIdentical proves that with NO canary row the
// resolution is byte-identical to today even when a CanaryRand that would always
// hit (0.0) is injected: no canary exists, so no routing path is taken.
func TestMailboxCanaryOffByDefaultByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	base := db.AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "commit-v1", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "planner-agent", Role: "planner", Runtime: "codex", RuntimeRef: "last", RepoScope: "o/r", TemplateID: "planner", Capabilities: []string{"ask"}}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	// Default mailbox (nil CanaryRand) and a forced-hit mailbox must resolve the SAME
	// champion snapshot when no canary row exists.
	def := enqueueAndResolve(t, Mailbox{Store: store}, "job-default")
	forced := enqueueAndResolve(t, Mailbox{Store: store, CanaryRand: func() float64 { return 0.0 }}, "job-forced")
	if def.TemplateResolvedCommit != "commit-v1" || def.TemplateContent != "v1 content" {
		t.Fatalf("default resolution changed: %+v", def)
	}
	if forced.TemplateResolvedCommit != def.TemplateResolvedCommit || forced.TemplateContent != def.TemplateContent {
		t.Fatalf("forced-hit resolution differs with no canary row: %+v vs %+v", forced, def)
	}
}

// TestMailboxCanaryRoutingConcurrent proves the routing seam is concurrency-safe
// and ALWAYS resolves a valid version under concurrent Enqueues against an active
// canary (a mid-canary concurrent resolve never returns no-template/broken).
func TestMailboxCanaryRoutingConcurrent(t *testing.T) {
	store := openTestStore(t)
	seedCanaryAgent(t, store, 0.5)
	mailbox := Mailbox{Store: store, CanaryEnabled: true} // real global rng — concurrency-safe
	const n = 40
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			ctx := context.Background()
			id := fmt.Sprintf("job-conc-%d", i)
			if _, err := mailbox.Enqueue(ctx, JobRequest{ID: id, Agent: "planner-agent", Action: "ask", Repo: "o/r"}); err != nil {
				errs <- err
				return
			}
			stored, err := store.GetJob(ctx, id)
			if err != nil {
				errs <- err
				return
			}
			payload, err := unmarshalPayload(stored.Payload)
			if err != nil {
				errs <- err
				return
			}
			// EVERY resolution must be a valid version — either the champion or the
			// canary, never empty/broken.
			commit := payload.TemplateResolvedCommit
			if commit != "commit-v1" && commit != "commit-v2" {
				errs <- fmt.Errorf("job %s resolved invalid commit %q", id, commit)
				return
			}
			if payload.TemplateContent == "" {
				errs <- fmt.Errorf("job %s resolved empty template content", id)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}
}
