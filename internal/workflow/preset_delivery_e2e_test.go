package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// presetJobPayload builds a stored job payload that snapshots a preset, mirroring
// what Enqueue writes. The snapshot (id/commit/content) is what auditability and
// retry rely on; the delivery mode only changes how it is rendered.
func presetJobPayload(t *testing.T) string {
	t.Helper()
	payload, err := marshalPayload(JobPayload{
		Repo:                   "jerryfane/gitmoot",
		TemplateID:             "thermo",
		TemplateResolvedCommit: "abc123",
		TemplateContent:        "Review deeply.",
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	return payload
}

// TestPresetDeliveryReferencedFiresOnlyWithPriorState is the deterministic no-LLM
// E2E for #33 using the shell runtime + a captured-prompt fake adapter. It proves:
//   - the FIRST delivery on a session with no recorded state sends the FULL preset
//     (byte-identical to today) and records the loaded-state marker;
//   - the SECOND delivery on the SAME session (referenced mode, state now present)
//     sends the SHORT reference instead of the whole preset body.
func TestPresetDeliveryReferencedFiresOnlyWithPriorState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	// A shell agent with a concrete, resumable session ref (a command) in
	// referenced mode. referenced trusts the operator and fires on any runtime once
	// state exists.
	agent := runtime.Agent{
		Name:           "audit",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "printf ok",
		RepoScope:      "jerryfane/gitmoot",
		Role:           "reviewer",
		PresetDelivery: db.PresetDeliveryReferenced,
	}
	adapter := &fakeDelivery{outputs: []string{okDeliveryResult, okDeliveryResult}}

	// First job: no state yet -> full delivery.
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
		t.Fatalf("CreateJob job-1: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run job-1: %v", err)
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[0], "Template instructions:\nReview deeply.") {
		t.Fatalf("first delivery must send the FULL preset:\n%s", adapter.prompts[0])
	}
	if strings.Contains(adapter.prompts[0], "Use your installed thermo preset") {
		t.Fatalf("first delivery unexpectedly sent the reference (no prior state):\n%s", adapter.prompts[0])
	}
	// The full delivery must have recorded the loaded-state marker.
	has, err := store.HasPresetSessionState(ctx, runtime.ShellRuntime, "printf ok", "thermo", "abc123")
	if err != nil {
		t.Fatalf("HasPresetSessionState: %v", err)
	}
	if !has {
		t.Fatalf("full delivery did not record preset session state")
	}

	// Second job: same session, state present -> short reference.
	if err := store.CreateJob(ctx, db.Job{ID: "job-2", Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
		t.Fatalf("CreateJob job-2: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-2", agent, adapter); err != nil {
		t.Fatalf("Run job-2: %v", err)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], "Use your installed thermo preset (commit abc123)") {
		t.Fatalf("second delivery must send the SHORT reference:\n%s", adapter.prompts[1])
	}
	if strings.Contains(adapter.prompts[1], "Template instructions:\nReview deeply.") {
		t.Fatalf("second delivery unexpectedly re-pasted the full preset:\n%s", adapter.prompts[1])
	}
}

// TestPresetDeliveryFullModeAlwaysSendsFullEvenWithState pins the byte-identical
// default: a `full` agent (and the unset default) always sends the whole preset,
// and never records or consults state, even when a state row happens to exist.
func TestPresetDeliveryFullModeAlwaysSendsFullEvenWithState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	// Pre-seed a matching state row: a full agent must ignore it entirely.
	if err := store.RecordPresetSessionState(ctx, runtime.ShellRuntime, "printf ok", "thermo", "abc123"); err != nil {
		t.Fatalf("seed RecordPresetSessionState: %v", err)
	}
	for _, mode := range []string{db.PresetDeliveryFull, ""} {
		agent := runtime.Agent{
			Name:           "audit",
			Runtime:        runtime.ShellRuntime,
			RuntimeRef:     "printf ok",
			RepoScope:      "jerryfane/gitmoot",
			Role:           "reviewer",
			PresetDelivery: mode,
		}
		adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}
		id := "job-full-" + mode
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
		if _, err := mailbox.Run(ctx, id, agent, adapter); err != nil {
			t.Fatalf("Run %s: %v", id, err)
		}
		if !strings.Contains(adapter.prompts[0], "Template instructions:\nReview deeply.") {
			t.Fatalf("full-mode (%q) delivery must send the full preset:\n%s", mode, adapter.prompts[0])
		}
		if strings.Contains(adapter.prompts[0], "Use your installed thermo preset") {
			t.Fatalf("full-mode (%q) delivery unexpectedly sent the reference:\n%s", mode, adapter.prompts[0])
		}
	}
}

// TestPresetDeliveryAutoRequiresPersistedRuntime pins that auto mode falls back to
// full on a runtime that does not persist sessions (shell) even with a state row,
// but shortens on a persisted runtime. The state row is recorded manually so both
// legs start from an established marker.
func TestPresetDeliveryAutoRequiresPersistedRuntime(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	// Shell leg: auto must NOT shorten (shell is not a persisted runtime).
	if err := store.RecordPresetSessionState(ctx, runtime.ShellRuntime, "printf ok", "thermo", "abc123"); err != nil {
		t.Fatalf("seed shell state: %v", err)
	}
	shellAgent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer", PresetDelivery: db.PresetDeliveryAuto}
	shellAdapter := &fakeDelivery{outputs: []string{okDeliveryResult}}
	if err := store.CreateJob(ctx, db.Job{ID: "job-auto-shell", Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
		t.Fatalf("CreateJob job-auto-shell: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-auto-shell", shellAgent, shellAdapter); err != nil {
		t.Fatalf("Run job-auto-shell: %v", err)
	}
	if !strings.Contains(shellAdapter.prompts[0], "Template instructions:\nReview deeply.") {
		t.Fatalf("auto on shell must send full (not a persisted runtime):\n%s", shellAdapter.prompts[0])
	}

	// Codex leg: auto shortens with a matching state row and a concrete session.
	concrete := "019f3041-cfed-7e82-8766-b5ca75cf92da"
	if err := store.RecordPresetSessionState(ctx, runtime.CodexRuntime, concrete, "thermo", "abc123"); err != nil {
		t.Fatalf("seed codex state: %v", err)
	}
	codexAgent := runtime.Agent{Name: "impl", Runtime: runtime.CodexRuntime, RuntimeRef: concrete, RepoScope: "jerryfane/gitmoot", Role: "reviewer", PresetDelivery: db.PresetDeliveryAuto}
	codexAdapter := &fakeDelivery{outputs: []string{okDeliveryResult}}
	if err := store.CreateJob(ctx, db.Job{ID: "job-auto-codex", Agent: "impl", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
		t.Fatalf("CreateJob job-auto-codex: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-auto-codex", codexAgent, codexAdapter); err != nil {
		t.Fatalf("Run job-auto-codex: %v", err)
	}
	if !strings.Contains(codexAdapter.prompts[0], "Use your installed thermo preset (commit abc123)") {
		t.Fatalf("auto on codex with state must send the reference:\n%s", codexAdapter.prompts[0])
	}
}

// TestPresetDeliveryReferencedFallsBackOnCommitMismatch pins the auditability
// invariant that a preset commit change invalidates the loaded-state evidence: a
// state row at an OLD commit must not satisfy a job snapshotted at a NEW commit.
func TestPresetDeliveryReferencedFallsBackOnCommitMismatch(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	// Marker recorded at an old commit; the job snapshots a newer commit.
	if err := store.RecordPresetSessionState(ctx, runtime.ShellRuntime, "printf ok", "thermo", "old000"); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "jerryfane/gitmoot", Role: "reviewer", PresetDelivery: db.PresetDeliveryReferenced}
	adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}
	if err := store.CreateJob(ctx, db.Job{ID: "job-mismatch", Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-mismatch", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(adapter.prompts[0], "Template instructions:\nReview deeply.") {
		t.Fatalf("a commit mismatch must fall back to the full preset:\n%s", adapter.prompts[0])
	}
}
