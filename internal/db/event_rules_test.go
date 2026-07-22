package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEventRuleRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	want := EventRule{
		ID: "rule-1", OnKind: "attention", MatchFilter: "Acme/Widget",
		WakeRole: "maintainer", Enabled: true, CreatedAt: "2026-07-22T12:00:00Z",
	}
	if err := store.AddEventRule(ctx, want); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0] != want {
		t.Fatalf("rules = %#v, want %#v", rules, []EventRule{want})
	}
	if err := store.DeleteEventRule(ctx, want.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEventRule(ctx, want.ID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	rules, err = store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Fatalf("rules after delete = %#v", rules)
	}
}
