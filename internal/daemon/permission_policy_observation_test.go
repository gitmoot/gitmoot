package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
)

func TestPermissionPolicyObservationBaselineRatchetsDownAndWarnsOnGrowth(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	upsert := func(name string) {
		t.Helper()
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "shell", RuntimeRef: "printf ok", AutonomyPolicy: "auto", Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	upsert("existing-auto")
	d := Daemon{Store: store}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err := store.PermissionPolicyObservationBaseline(ctx)
	if err != nil || !ok || baseline.AffectedCount != 1 {
		t.Fatalf("initial baseline = %#v, ok=%t, err=%v", baseline, ok, err)
	}

	upsert("new-auto")
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, permissionpolicy.ObservationJobID)
	if err != nil {
		t.Fatal(err)
	}
	var growth permissionPolicyBaselineEvent
	found := false
	for _, event := range events {
		if event.Kind != permissionpolicy.BaselineGrowthEventKind {
			continue
		}
		if err := json.Unmarshal([]byte(event.Message), &growth); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found || growth.Baseline != 1 || growth.Current != 2 || growth.Delta != 1 || len(growth.Configs) != 1 {
		t.Fatalf("growth event = %#v, want one named config above 1->2", growth)
	}

	removed, err := store.RemoveAgent(ctx, "existing-auto")
	if err != nil || !removed {
		t.Fatalf("RemoveAgent = %t, %v", removed, err)
	}
	removed, err = store.RemoveAgent(ctx, "new-auto")
	if err != nil || !removed {
		t.Fatalf("RemoveAgent(new-auto) = %t, %v", removed, err)
	}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err = store.PermissionPolicyObservationBaseline(ctx)
	if err != nil || !ok || baseline.AffectedCount != 0 {
		t.Fatalf("lowered baseline = %#v, ok=%t, err=%v", baseline, ok, err)
	}
}

func TestPermissionPolicyObservationDoesNotLowerAcrossNewConfigs(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	upsert := func(name string) {
		t.Helper()
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "shell", RuntimeRef: "printf ok", AutonomyPolicy: "auto", Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	baselineNames := []string{"baseline-a", "baseline-b", "baseline-c", "baseline-d", "baseline-e", "baseline-f", "baseline-g", "baseline-h", "baseline-i", "baseline-j"}
	for _, name := range baselineNames {
		upsert(name)
	}
	d := Daemon{Store: store}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range baselineNames[:4] {
		removed, err := store.RemoveAgent(ctx, name)
		if err != nil || !removed {
			t.Fatalf("RemoveAgent(%q) = %t, %v", name, removed, err)
		}
	}
	for _, name := range []string{"new-x", "new-y", "new-z"} {
		upsert(name)
	}

	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err := store.PermissionPolicyObservationBaseline(ctx)
	if err != nil || !ok || baseline.AffectedCount != 10 {
		t.Fatalf("baseline after 10 -> 9 churn = %#v, ok=%t, err=%v; new configs must not be absorbed by a lower count", baseline, ok, err)
	}
	event := permissionPolicyObservationEventOfKind(t, store, permissionpolicy.BaselineGrowthEventKind)
	if event.Baseline != 10 || event.Current != 9 || event.Delta != -1 {
		t.Fatalf("new-config event = %#v, want baseline=10 current=9 delta=-1", event)
	}
	for _, name := range []string{"new-x", "new-y", "new-z"} {
		if !strings.Contains(strings.Join(event.Configs, "\n"), fmt.Sprintf("agent=%q", name)) {
			t.Fatalf("new-config event = %#v, want reported agent %q", event, name)
		}
	}
}

func TestPermissionPolicyObservationReportsEqualCountChurn(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	upsert := func(name string) {
		t.Helper()
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "shell", RuntimeRef: "printf ok", AutonomyPolicy: "auto", Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"baseline-a", "baseline-b", "baseline-c"} {
		upsert(name)
	}
	d := Daemon{Store: store}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"baseline-a", "baseline-b", "baseline-c"} {
		removed, err := store.RemoveAgent(ctx, name)
		if err != nil || !removed {
			t.Fatalf("RemoveAgent(%q) = %t, %v", name, removed, err)
		}
	}
	for _, name := range []string{"new-x", "new-y", "new-z"} {
		upsert(name)
	}

	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	event := permissionPolicyObservationEventOfKind(t, store, permissionpolicy.BaselineGrowthEventKind)
	if event.Baseline != 3 || event.Current != 3 || event.Delta != 0 || len(event.Configs) != 3 {
		t.Fatalf("equal-count churn event = %#v, want three new configs at 3 -> 3", event)
	}
}

func TestPermissionPolicyObservationJobHistoryDoesNotJamLowering(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for _, name := range []string{"keep-auto", "fix-auto"} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "shell", RuntimeRef: "printf ok", AutonomyPolicy: "auto", Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	d := Daemon{Store: store}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	for index, agent := range []string{"ephemeral-history-a", "ephemeral-history-b"} {
		if err := store.CreateJob(ctx, db.Job{ID: fmt.Sprintf("history-job-%d", index), Agent: agent, Type: "ask", State: "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.RemoveAgent(ctx, "fix-auto")
	if err != nil || !removed {
		t.Fatalf("RemoveAgent(fix-auto) = %t, %v", removed, err)
	}

	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, ok, err := store.PermissionPolicyObservationBaseline(ctx)
	if err != nil || !ok || baseline.AffectedCount != 1 {
		t.Fatalf("baseline with unresolved job history = %#v, ok=%t, err=%v; live remediation must still lower", baseline, ok, err)
	}
	event := permissionPolicyObservationEventOfKind(t, store, permissionpolicy.BaselineLoweredEventKind)
	if event.UnresolvedJobAgents != 2 {
		t.Fatalf("lowered event unresolved_job_agents = %d, want 2 reported outside the baseline", event.UnresolvedJobAgents)
	}
}

func TestPermissionPolicyObservationLoweredEventNamesRemediatedConfigs(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for _, name := range []string{"keep-auto", "fixed-auto"} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "shell", RuntimeRef: "printf ok", AutonomyPolicy: "auto", Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	d := Daemon{Store: store}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveAgent(ctx, "fixed-auto")
	if err != nil || !removed {
		t.Fatalf("RemoveAgent(fixed-auto) = %t, %v", removed, err)
	}
	if err := d.reconcilePermissionPolicyObservation(ctx); err != nil {
		t.Fatal(err)
	}

	event := permissionPolicyObservationEventOfKind(t, store, permissionpolicy.BaselineLoweredEventKind)
	if len(event.Configs) != 1 || !strings.Contains(event.Configs[0], `agent="fixed-auto"`) {
		t.Fatalf("lowered event configs = %v, want the remediated fixed-auto config", event.Configs)
	}
}

func permissionPolicyObservationEventOfKind(t *testing.T, store *db.Store, kind string) permissionPolicyBaselineEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), permissionpolicy.ObservationJobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		var decoded permissionPolicyBaselineEvent
		if err := json.Unmarshal([]byte(event.Message), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	t.Fatalf("permission-policy observation event %q was not emitted", kind)
	return permissionPolicyBaselineEvent{}
}
