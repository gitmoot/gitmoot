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
	mailbox := Mailbox{Store: store, CanaryRand: func() float64 { return 0.0 }}
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
	mailbox := Mailbox{Store: store, CanaryRand: func() float64 { return 0.9 }}
	payload := enqueueAndResolve(t, mailbox, "job-champ")
	if payload.TemplateResolvedCommit != "commit-v1" || payload.TemplateContent != "v1 content" {
		t.Fatalf("draw at/above sample must route to champion, got commit=%q content=%q", payload.TemplateResolvedCommit, payload.TemplateContent)
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
	mailbox := Mailbox{Store: store} // real global rng — concurrency-safe
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
