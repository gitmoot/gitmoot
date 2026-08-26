package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestEventRuleRoundTrip(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	deletions, err := store.ListDeletedEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletions) != 1 || deletions[0].EventRule != want || strings.TrimSpace(deletions[0].DeletedAt) == "" {
		t.Fatalf("deleted rules = %#v, want one complete tombstone for %#v", deletions, want)
	}
}

func TestDeleteEventRulesForRoleRecordsDeletionHistory(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	rules := []EventRule{
		{ID: "owner-reply", OnKind: "reply", WakeRole: "owner", Enabled: true},
		{ID: "owner-blocked", OnKind: "blocked", WakeRole: "owner", Enabled: true},
		{ID: "worker-reply", OnKind: "reply", WakeRole: "worker", Enabled: true},
	}
	if err := store.AddEventRules(ctx, rules); err != nil {
		t.Fatal(err)
	}

	removed, err := store.DeleteEventRulesForRole(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed rules = %#v, want two owner rules", removed)
	}
	active, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "worker-reply" {
		t.Fatalf("active rules = %#v, want worker rule only", active)
	}
	deletions, err := store.ListDeletedEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletions) != 2 || deletions[0].WakeRole != "owner" || deletions[1].WakeRole != "owner" {
		t.Fatalf("deleted rules = %#v, want two owner tombstones", deletions)
	}
}

func TestEventRuleScopeObserverRoundTripAndValidation(t *testing.T) {
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
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
	const (
		wantBackfillMigration  = 107
		wantDirectiveMigration = 108
		// #1250 appended acting_org_role at 109. The append-only rule (#1265) freezes
		// EARLIER indices; it does not freeze which migration happens to be last, so
		// this pins both existing indices AND that the new tail is the expected one —
		// a reorder still fails, an append does not.
		wantActingOrgRoleMigration = 109
	)
	var backfillMigration int
	var directiveMigration int
	for i, migration := range migrations {
		if strings.Contains(migration, "scope = 'observer'") && strings.Contains(migration, "on_kind <> 'reply'") {
			backfillMigration = i
		}
		if strings.Contains(migration, "directive_nudge_count") && strings.Contains(migration, "directive_last_nudged_at") {
			directiveMigration = i
		}
	}
	if backfillMigration == 0 {
		t.Fatal("event rule observer catch-all migration not found")
	}
	if directiveMigration == 0 {
		t.Fatal("directive supervision migration not found")
	}
	if backfillMigration != wantBackfillMigration {
		t.Fatalf("observer catch-all migration index=%d, want preserved append-only index=%d", backfillMigration, wantBackfillMigration)
	}
	if directiveMigration != wantDirectiveMigration {
		t.Fatalf("directive supervision migration index=%d, want preserved append-only index=%d", directiveMigration, wantDirectiveMigration)
	}
	// Pin the INDEX, never the tail. #1265 freezes where existing migrations sit;
	// it says nothing about which one happens to be last, so a future append must
	// stay green while any reorder goes red.
	if len(migrations) <= wantActingOrgRoleMigration {
		t.Fatalf("acting_org_role migration index=%d is beyond the slice (len=%d)", wantActingOrgRoleMigration, len(migrations))
	}
	if !strings.Contains(migrations[wantActingOrgRoleMigration], "acting_org_role") {
		t.Fatalf("migration %d is not the acting_org_role migration (#1250) — existing migrations must never be reordered (#1265)", wantActingOrgRoleMigration)
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
