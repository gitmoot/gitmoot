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

// #1352 DEFECT 1 — the acked-phase ladder was UNBOUNDED. The unacked branch
// capped at DirectiveMaxNudges with one escalation; the acked-with-done-TTL
// branch checked no max, so an acknowledged directive re-nudged forever.
//
// Behavioural, not transcriptional: this counts the wakes the evaluator actually
// emits across many intervals, so it fails on the OBSERVED noise rather than on
// any particular counter's value.
func TestDirectiveTTLAckedPhaseLadderCapsAndStopsNudging(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "acked and overdue", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)
	sink := &recordingSink{}

	base := time.Now().UTC().Add(10 * time.Minute)
	// Ten intervals — far beyond the cap of 3.
	for i := 0; i < 10; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}

	wakes := sink.byType(events.EventOrgDirective)
	if len(wakes) > 3 {
		t.Fatalf("completion ladder emitted %d nudges over 10 intervals with max=3: the acked phase is unbounded", len(wakes))
	}
	if len(wakes) == 0 {
		t.Fatal("completion ladder emitted nothing; the cap must bound the ladder, not delete it")
	}
}

// #1352 DEFECT 1, THE POLARITY GUARD the issue demands explicitly: a capped
// ladder must leave the obligation VISIBLE. Ending in silence would be worse
// than nudging forever — the obligation would simply vanish from view while
// remaining unmet.
func TestDirectiveTTLExhaustedObligationStaysQueryable(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "will exhaust", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)

	base := time.Now().UTC().Add(10 * time.Minute)
	for i := 0; i < 8; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, &recordingSink{}, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}

	// STILL LISTED — the ladder ended in a queryable state, not in nothing.
	item := readDirectiveTTLObligation(t, store, note.ID)
	if strings.TrimSpace(item.ExhaustedAt) == "" {
		t.Fatalf("ladder ended without stamping a terminal state: %+v", item)
	}
}

// #1352 DEFECT 1 — the terminal escalation must fire, and fire ONCE, for the
// completion phase. Before this the acked branch escalated never.
func TestDirectiveTTLCompletionPhaseEscalatesOnceAtCap(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "escalating completion", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)
	sink := &recordingSink{}

	base := time.Now().UTC().Add(10 * time.Minute)
	for i := 0; i < 8; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}

	escalations := sink.byType(events.EventJobNeedsAttention)
	if len(escalations) != 1 {
		t.Fatalf("completion escalations = %d, want exactly 1 terminal escalation", len(escalations))
	}
	if !strings.Contains(escalations[0].Detail, "incomplete") {
		t.Fatalf("completion escalation says %q; it must name the COMPLETION obligation, not acknowledgment", escalations[0].Detail)
	}
}

// #1352 DEFECT 2 — sweep-window starvation. The window was a fixed oldest-N, so
// once N immortal rows occupied it (which defect 1 guaranteed) newer directives
// were never evaluated at all. The window is now sized from the live population,
// which is the remedy the blocked-task evaluator already uses.
//
// Behavioural: seed more open directives than the OLD fixed window would admit
// for a newcomer, then require the newest one to still receive its nudge.
func TestDirectiveTTLSweepDoesNotStarveNewerDirectives(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	for i := 0; i < 5; i++ {
		seedDirectiveTTLNote(t, store, fmt.Sprintf("older %d", i), 0, false)
	}
	newest := seedDirectiveTTLNote(t, store, "newest", 0, false)
	sink := &recordingSink{}

	// A window of 1 models the starved case: without population sizing the
	// newest directive can never be reached.
	deps := directiveTTLDependencies{
		countOpen: func(ctx context.Context, s *db.Store) (int, error) {
			return s.CountOpenOrgDirectiveObligations(ctx)
		},
	}
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, time.Now().UTC().Add(20*time.Minute), deps); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, w := range sink.byType(events.EventOrgDirective) {
		if w.JobID == fmt.Sprint(newest.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the newest directive %d was never evaluated; the sweep window starves later ids", newest.ID)
	}
}

// #1352 DEFECT 1, SITE-SPECIFIC guard for the completion CAP itself.
//
// My first attempt at pinning the cap was not load-bearing: removing the
// DoneNudgeCount check left every guard green, because the exhausted STAMP also
// halts the ladder, so the two mechanisms masked each other. The cap's real job
// is FAIL-SAFE — it must bound the ladder even when the terminal stamp does not
// land (a store error, a racing writer). That is the property this pins, and it
// is the only condition under which the cap is observable on its own.
func TestDirectiveTTLCompletionCapBoundsLadderEvenIfExhaustStampFails(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "stamp keeps failing", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)
	sink := &recordingSink{}

	// The terminal stamp never lands. Only the counter cap can stop the ladder.
	deps := directiveTTLDependencies{
		exhaust: func(ctx context.Context, s *db.Store, id int64, at time.Time) (bool, error) {
			return false, nil
		},
	}
	base := time.Now().UTC().Add(10 * time.Minute)
	for i := 0; i < 10; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), deps); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(sink.byType(events.EventOrgDirective)); got > 2 {
		t.Fatalf("with the terminal stamp failing, the ladder emitted %d nudges over 10 intervals at max=2: the completion CAP is not bounding it", got)
	}
}

// #1352 — the evaluator's TERMINAL check, pinned independently of the store's.
//
// Exhaustion is enforced in two places: MarkOrgDirectiveDoneNudged refuses an
// exhausted row, and directiveTTLDue returns not-due for one. The store refusal
// MASKED the evaluator check, so removing the evaluator's terminal guard left
// every test green. Both layers are wanted — the store is the atomic backstop,
// the evaluator avoids the pointless claim — so each is pinned where the other
// cannot cover for it. Here the store claim always succeeds, leaving the
// evaluator as the only thing that can end the ladder.
func TestDirectiveTTLExhaustedIsTerminalAtTheEvaluatorToo(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "already exhausted", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)
	if _, err := store.MarkOrgDirectiveExhausted(context.Background(), note.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}

	claims := 0
	deps := directiveTTLDependencies{
		// The store backstop is disabled: it always claims. Only the evaluator's
		// terminal check can prevent a nudge now.
		markDone: func(ctx context.Context, s *db.Store, id int64, expected int, last string, at time.Time) (int, bool, error) {
			claims++
			return expected + 1, true, nil
		},
	}
	base := time.Now().UTC().Add(20 * time.Minute)
	for i := 0; i < 5; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), deps); err != nil {
			t.Fatal(err)
		}
	}

	if claims != 0 {
		t.Fatalf("evaluator attempted %d claims on an EXHAUSTED obligation; the terminal state is not terminal without the store backstop", claims)
	}
	if got := len(sink.byType(events.EventOrgDirective)); got != 0 {
		t.Fatalf("exhausted obligation still emitted %d nudges", got)
	}
}
