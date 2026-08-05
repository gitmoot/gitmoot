package permissionpolicy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func observationStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func warningFor(t *testing.T, store *db.Store, jobID string) Warning {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == WarningEventKind {
			var warning Warning
			if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
				t.Fatal(err)
			}
			return warning
		}
	}
	t.Fatalf("job %s has no permission-policy warning", jobID)
	return Warning{}
}

func TestShellRiskAcceptanceDependsOnCommandProvenance(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		command    string
		acceptance bool
	}{
		{name: "operator command", command: "./scripts/audit-repo.sh", acceptance: true},
		{name: "claude model command", command: "claude -p review", acceptance: false},
		{name: "codex model command", command: "env FOO=1 codex exec -- review", acceptance: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := observationStore(t)
			job := db.Job{ID: "job", Agent: "shell-agent", Type: "review"}
			agent := runtime.Agent{Name: job.Agent, Runtime: runtime.ShellRuntime, RuntimeRef: tc.command, AutonomyPolicy: runtime.AutonomyPolicyReadOnly}
			if _, err := RecordWarning(context.Background(), store, job, agent, StaticProvider{Property: runtime.PermissionPolicyNotApplied}, now); err != nil {
				t.Fatal(err)
			}
			got := warningFor(t, store, job.ID).RiskAcceptance != ""
			if got != tc.acceptance {
				t.Fatalf("risk acceptance present = %t, want %t", got, tc.acceptance)
			}
		})
	}
}

func TestWarningConsumerUsesProviderDeclaration(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		property runtime.PermissionPolicyApplication
		warn     bool
	}{
		{name: "synthetic not applied", property: runtime.PermissionPolicyNotApplied, warn: true},
		{name: "same synthetic adapter applied", property: runtime.PermissionPolicyApplied, warn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := observationStore(t)
			job := db.Job{ID: "synthetic-job", Agent: "synthetic", Type: "ask"}
			agent := runtime.Agent{Name: job.Agent, Runtime: "synthetic", AutonomyPolicy: runtime.AutonomyPolicyReadOnly}
			warned, err := RecordWarning(context.Background(), store, job, agent, StaticProvider{Property: tc.property}, now)
			if err != nil {
				t.Fatal(err)
			}
			if warned != tc.warn {
				t.Fatalf("warned = %t, want %t", warned, tc.warn)
			}
		})
	}
}

func TestWarningCoalescesPerAgentConfigWindow(t *testing.T) {
	store := observationStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	agent := runtime.Agent{Name: "same", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", AutonomyPolicy: runtime.AutonomyPolicyAuto}
	for index, jobID := range []string{"first", "second"} {
		warned, err := RecordWarning(context.Background(), store, db.Job{ID: jobID, Agent: agent.Name, Type: "ask"}, agent, StaticProvider{Property: runtime.PermissionPolicyNotApplied}, now)
		if err != nil {
			t.Fatal(err)
		}
		if warned != (index == 0) {
			t.Fatalf("dispatch %d warned = %t, want %t", index, warned, index == 0)
		}
	}
}

func TestInventoryLabelsMissingAgentAsUnresolved(t *testing.T) {
	ctx := context.Background()
	store := observationStore(t)
	if err := store.CreateJob(ctx, db.Job{
		ID:    "missing-agent-job",
		Agent: "deleted-agent",
		Type:  "ask",
		State: "queued",
	}); err != nil {
		t.Fatal(err)
	}

	configs, err := Inventory(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("inventory = %#v, want one missing-agent config", configs)
	}
	if got := configs[0]; got.Agent != "deleted-agent" || got.Runtime != "" || got.Policy != "" || got.Property != runtime.PermissionPolicyUnresolved {
		t.Fatalf("missing-agent config = %#v, want agent deleted-agent with unknown runtime and policy and property %q", got, runtime.PermissionPolicyUnresolved)
	}
}
