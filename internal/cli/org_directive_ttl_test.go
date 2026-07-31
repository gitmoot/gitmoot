package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func writeDirectiveTTLConfig(t *testing.T, home, supervisor string, ackTTL, doneTTL time.Duration, maxNudges int) config.OrgConfig {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`[org]
directive_ack_ttl = %q
directive_done_ttl = %q
directive_max_nudges = %d
[org.roles."owner"]
scope = ["*"]
[org.roles.%q]
parent = "owner"
scope = ["*"]
[org.roles."sender"]
parent = %q
scope = ["*"]
[org.roles."worker"]
parent = "sender"
scope = ["*"]
`, ackTTL.String(), doneTTL.String(), maxNudges, supervisor, supervisor)
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func openDirectiveTTLStore(t *testing.T, home string) *db.Store {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func seedDirectiveTTLNote(t *testing.T, store *db.Store, body string, doneTTL time.Duration, override bool) db.WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID:              "release/ttl",
		Author:                  "sender",
		Body:                    workflow.FormatOrgDirectiveNote("sender", "worker", "release/ttl", body),
		Repo:                    "gitmoot/gitmoot",
		DirectiveDoneTTLSeconds: int64(doneTTL / time.Second),
		DirectiveDoneTTLSet:     override,
	})
	if err != nil {
		t.Fatal(err)
	}
	return note
}

func acknowledgeDirectiveTTLNote(t *testing.T, store *db.Store, directive db.WorkflowNote) {
	t.Helper()
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: directive.WorkflowID,
		Author:     "worker",
		Body:       workflow.FormatOrgDirectiveAckNote(directive.ID, "worker"),
	}); err != nil {
		t.Fatal(err)
	}
}

func readDirectiveTTLObligation(t *testing.T, store *db.Store, id int64) db.OrgDirectiveObligation {
	t.Helper()
	items, err := store.ListOpenOrgDirectiveObligations(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("directive %d not found in %+v", id, items)
	return db.OrgDirectiveObligation{}
}

func TestDirectiveTTLNudgesOncePerInterval(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	directive := seedDirectiveTTLNote(t, store, "ack this", 0, false)
	now := time.Now().UTC().Add(20 * time.Minute)
	sink := &recordingSink{}

	for _, step := range []struct {
		at        time.Time
		wantCount int
		wantWakes int
	}{
		{at: now, wantCount: 1, wantWakes: 1},
		{at: now.Add(time.Minute), wantCount: 1, wantWakes: 1},
		{at: now.Add(11 * time.Minute), wantCount: 2, wantWakes: 2},
	} {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, step.at, directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
		item := readDirectiveTTLObligation(t, store, directive.ID)
		if item.NudgeCount != step.wantCount || len(sink.byType(events.EventOrgDirective)) != step.wantWakes {
			t.Fatalf("at %s nudge_count=%d wakes=%d, want %d/%d", step.at, item.NudgeCount, len(sink.byType(events.EventOrgDirective)), step.wantCount, step.wantWakes)
		}
	}
}

func TestDirectiveTTLNudgeCountSurvivesDaemonRestart(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	directive := seedDirectiveTTLNote(t, store, "persist the counter", 0, false)
	now := time.Now().UTC().Add(20 * time.Minute)
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, &recordingSink{}, cfg, io.Discard, now, directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh store and evaluator instance must continue from the durable row.
	store = openDirectiveTTLStore(t, home)
	defer store.Close()
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, &recordingSink{}, cfg, io.Discard, now.Add(11*time.Minute), directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	if got := readDirectiveTTLObligation(t, store, directive.ID).NudgeCount; got != 2 {
		t.Fatalf("nudge_count after restart = %d, want 2", got)
	}
}

func TestDirectiveTTLEscalatesAtMaxToCurrentSenderParent(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "old-supervisor", 10*time.Minute, 0, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	directive := seedDirectiveTTLNote(t, store, "needs supervision", 0, false)
	now := time.Now().UTC().Add(20 * time.Minute)
	sink := &recordingSink{}
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, now, directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 0 {
		t.Fatalf("escalations after first nudge = %d, want 0", got)
	}

	// Parentage is resolved at evaluation time, not copied from the send.
	cfg = writeDirectiveTTLConfig(t, home, "new-supervisor", 10*time.Minute, 0, 2)
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, now.Add(11*time.Minute), directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	escalations := sink.byType(events.EventJobNeedsAttention)
	if len(escalations) != 1 {
		t.Fatalf("escalations = %+v, want exactly one at max_nudges", escalations)
	}
	ev := escalations[0]
	if ev.WakeTargetRole != "new-supervisor" || ev.WakeTargetRole == "worker" || ev.Cause != "escalation" {
		t.Fatalf("escalation routing = %+v", ev)
	}
	for _, want := range []string{fmt.Sprint(directive.ID), "worker", "2 nudges"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("escalation detail %q missing %q", ev.Detail, want)
		}
	}
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, now.Add(22*time.Minute), directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.byType(events.EventJobNeedsAttention)); got != 1 {
		t.Fatalf("escalations after max = %d, want 1", got)
	}
}

func TestDirectiveTTLAckSkipsAckNudgesAndDoneOverrideWins(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	ackOnly := seedDirectiveTTLNote(t, store, "ack only", 0, false)
	overridden := seedDirectiveTTLNote(t, store, "completion override", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, ackOnly)
	acknowledgeDirectiveTTLNote(t, store, overridden)
	sink := &recordingSink{}
	now := time.Now().UTC().Add(20 * time.Minute)
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, now, directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	wakes := sink.byType(events.EventOrgDirective)
	if len(wakes) != 1 || wakes[0].JobID != fmt.Sprint(overridden.ID) || !strings.Contains(wakes[0].Detail, "completion") {
		t.Fatalf("completion wakes = %+v, want only overridden directive %d", wakes, overridden.ID)
	}
	if got := readDirectiveTTLObligation(t, store, ackOnly.ID).NudgeCount; got != 0 {
		t.Fatalf("acked directive got %d ack nudges, want 0", got)
	}
}

func TestDirectiveTTLEvaluatorIsConfigInertWithNoRules(t *testing.T) {
	home := t.TempDir()
	writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	directive := seedDirectiveTTLNote(t, store, "must remain untouched", 0, false)
	listCalls := 0
	deps := blockedRoleWakeDependencies{
		availability: &fakeBlockedRoleAvailability{available: false},
		eventSink:    enabledBlockedSinceEventSink,
		directives: directiveTTLDependencies{list: func(context.Context, *db.Store, int) ([]db.OrgDirectiveObligation, error) {
			listCalls++
			t.Fatal("directive query ran without an enabled org event rule")
			return nil, nil
		}},
	}
	runBlockedRoleWakeOnce(context.Background(), store, home, io.Discard, time.Now().UTC().Add(time.Hour), deps)
	if listCalls != 0 {
		t.Fatalf("directive queries beyond rule guard = %d, want 0", listCalls)
	}
	if got := readDirectiveTTLObligation(t, store, directive.ID).NudgeCount; got != 0 {
		t.Fatalf("config-inert nudge_count = %d, want 0", got)
	}
}
