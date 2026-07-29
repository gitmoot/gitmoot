package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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
		WakeRole: "maintainer", Scope: EventRuleScopeAddressed, Enabled: true, CreatedAt: "2026-07-22T12:00:00Z",
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

func TestEventRuleScopeObserverRoundTripAndValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	observer := EventRule{
		ID: "observer", OnKind: "attention", WakeRole: "maintainer",
		Scope: EventRuleScopeObserver, Enabled: true, CreatedAt: "2026-07-22T12:00:00Z",
	}
	if err := store.AddEventRule(ctx, observer); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0] != observer {
		t.Fatalf("rules = %#v, want %#v", rules, []EventRule{observer})
	}
	invalid := observer
	invalid.ID = "invalid"
	invalid.Scope = "broadcast"
	if err := store.AddEventRule(ctx, invalid); err == nil || !strings.Contains(err.Error(), "addressed or observer") {
		t.Fatalf("invalid scope error = %v, want addressed/observer validation", err)
	}
}

func TestEventRuleScopeMigrationDefaultsLegacyRowsToAddressed(t *testing.T) {
	ctx := context.Background()
	var scopeMigration int
	for i, migration := range migrations {
		if strings.Contains(migration, "ALTER TABLE event_rules ADD COLUMN scope") {
			scopeMigration = i
			break
		}
	}
	if scopeMigration == 0 {
		t.Fatal("event rule scope migration not found")
	}

	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: raw}
	t.Cleanup(func() { _ = store.Close() })
	for i, migration := range migrations[:scopeMigration] {
		if err := store.applyMigration(ctx, i+1, migration); err != nil {
			t.Fatalf("apply pre-scope migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO event_rules(
		id, on_kind, match_filter, wake_role, enabled, created_at
	) VALUES ('legacy', 'reply', '', 'owner', 1, '2026-07-22T12:00:00Z')`); err != nil {
		t.Fatalf("seed legacy event rule: %v", err)
	}
	for i, migration := range migrations[scopeMigration:] {
		version := scopeMigration + i + 1
		if err := store.applyMigration(ctx, version, migration); err != nil {
			t.Fatalf("apply scope/later migration %d: %v", version, err)
		}
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Scope != EventRuleScopeAddressed {
		t.Fatalf("legacy rules = %+v, want scope=%q", rules, EventRuleScopeAddressed)
	}
}

func TestEventRuleCatchAllMigrationPromotesOnlyNonReplyRulesToObserver(t *testing.T) {
	ctx := context.Background()
	var backfillMigration int
	for i, migration := range migrations {
		if strings.Contains(migration, "scope = 'observer'") && strings.Contains(migration, "on_kind <> 'reply'") {
			backfillMigration = i
			break
		}
	}
	if backfillMigration == 0 {
		t.Fatal("event rule observer catch-all migration not found")
	}
	if backfillMigration != len(migrations)-1 {
		t.Fatalf("observer catch-all migration index=%d, want append-only tail index=%d", backfillMigration, len(migrations)-1)
	}

	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "catch-all.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{db: raw}
	t.Cleanup(func() { _ = store.Close() })
	for i, migration := range migrations[:backfillMigration] {
		if err := store.applyMigration(ctx, i+1, migration); err != nil {
			t.Fatalf("apply pre-backfill migration %d: %v", i+1, err)
		}
	}
	seed := []EventRule{
		{ID: "escalation-all", OnKind: "escalation", WakeRole: "jarvis", Scope: EventRuleScopeAddressed, Enabled: true},
		{ID: "blocked-spaces", OnKind: "blocked", MatchFilter: "   ", WakeRole: "jarvis", Scope: EventRuleScopeAddressed, Enabled: true},
		{ID: "filtered", OnKind: "blocked", MatchFilter: "owner/repo", WakeRole: "jarvis", Scope: EventRuleScopeAddressed, Enabled: true},
		{ID: "reply-all", OnKind: "reply", WakeRole: "owner", Scope: EventRuleScopeAddressed, Enabled: true},
	}
	for _, rule := range seed {
		if err := store.AddEventRule(ctx, rule); err != nil {
			t.Fatalf("seed %s: %v", rule.ID, err)
		}
	}
	if err := store.applyMigration(ctx, backfillMigration+1, migrations[backfillMigration]); err != nil {
		t.Fatalf("apply observer catch-all migration: %v", err)
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != len(seed) {
		t.Fatalf("rules = %d, want %d; catch-all rules must not be deleted", len(rules), len(seed))
	}
	got := make(map[string]EventRuleScope, len(rules))
	for _, rule := range rules {
		got[rule.ID] = rule.Scope
	}
	want := map[string]EventRuleScope{
		"escalation-all": EventRuleScopeObserver,
		"blocked-spaces": EventRuleScopeObserver,
		"filtered":       EventRuleScopeAddressed,
		"reply-all":      EventRuleScopeAddressed,
	}
	for id, scope := range want {
		if got[id] != scope {
			t.Errorf("%s scope=%q, want %q", id, got[id], scope)
		}
	}
}
