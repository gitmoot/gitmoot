package daemon

import (
	"context"
	"encoding/json"
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
