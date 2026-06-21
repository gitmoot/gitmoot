package workflow

import (
	"context"
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
	outputs   []string
	summaries []string
	prompts   []string
	models    []string
	onDeliver func()
}

func (f *fakeDelivery) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.prompts = append(f.prompts, job.Prompt)
	f.models = append(f.models, job.Model)
	if f.onDeliver != nil {
		f.onDeliver()
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
