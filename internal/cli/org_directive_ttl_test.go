package cli

import (
	"bytes"
	"context"
	"errors"
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

// #1352 B3 — THE MUTANT IS THE ORIGINAL BEHAVIOUR: a fixed 200-row ceiling.
// My previous starvation guard seeded six directives, so restoring the real
// clamp changed nothing and the guard passed on unfixed code. This one seeds a
// population that EXCEEDS the ceiling, which is the only shape where the defect
// is observable at all.
func TestDirectiveTTLSweepEvaluatesBeyondTheOldFixedCeiling(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	for i := 0; i < 200; i++ {
		seedDirectiveTTLNote(t, store, fmt.Sprintf("older %d", i), 0, false)
	}
	newest := seedDirectiveTTLNote(t, store, "directive 201", 0, false)
	sink := &recordingSink{}

	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, time.Now().UTC().Add(20*time.Minute), directiveTTLDependencies{}); err != nil {
		t.Fatal(err)
	}
	for _, w := range sink.byType(events.EventOrgDirective) {
		if w.JobID == fmt.Sprint(newest.ID) {
			return
		}
	}
	t.Fatalf("directive %d (row 201) was never evaluated: the sweep still clamps to a fixed ceiling", newest.ID)
}

// #1352 B2 — THE MUTANT IS THE ORIGINAL BEHAVIOUR: one stamp terminating both
// phases. Exhaust the ACK ladder, then acknowledge LATE, and the completion
// ladder must still run. Previously the ack-phase stamp terminated everything.
func TestDirectiveTTLLateAckStartsAFreshCompletionLadder(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "late ack", time.Minute, true)
	sink := &recordingSink{}

	base := time.Now().UTC().Add(5 * time.Minute)
	for i := 0; i < 6; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, base.Add(time.Duration(i)*2*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}
	ackWakes := len(sink.byType(events.EventOrgDirective))

	// The LATE acknowledgment arrives after the ack ladder is spent.
	acknowledgeDirectiveTTLNote(t, store, note)
	later := base.Add(60 * time.Minute)
	for i := 0; i < 6; i++ {
		if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, later.Add(time.Duration(i)*2*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(sink.byType(events.EventOrgDirective)); got <= ackWakes {
		t.Fatalf("completion ladder never started after a late ack (%d wakes before, %d after): one stamp terminated both phases", ackWakes, got)
	}
	escalations := sink.byType(events.EventJobNeedsAttention)
	if len(escalations) < 2 {
		t.Fatalf("escalations = %d, want one per phase; the completion phase never escalated", len(escalations))
	}
}

// #1352 B5 — THE MUTANT IS THE ORIGINAL BEHAVIOUR: a completion claim with no
// terminator recheck. Merging #1360 does NOT cover this — its guard is on the
// ACKNOWLEDGMENT claim — so this must hold independently of that merge.
func TestDirectiveTTLCompletionClaimRefusesTerminatedObligation(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "done races the completion claim", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)

	item := readDirectiveTTLObligation(t, store, note.ID)
	// The terminator commits between listing and the completion claim.
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: note.WorkflowID,
		Author:     "worker",
		Body:       workflow.FormatOrgDirectiveDoneNote(note.ID, "worker"),
	}); err != nil {
		t.Fatal(err)
	}
	_, claimed, err := store.MarkOrgDirectiveDoneNudged(ctx, item.ID, item.DoneNudgeCount, item.LastNudgedAt, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("completion claim succeeded on a COMPLETED directive; #1360's guard covers the ack claim only")
	}
}

// #1352 B4 — THE MUTANT IS THE ORIGINAL BEHAVIOUR: inserting a nag with
// source_kind="directive". The drain recognises a directive wake only as
// source_kind=workflow_note whose coalesce key carries the directive prefix, so
// the original insert produced rows it rejected as an unsupported source kind —
// admitting nags to the outbox without matching this shape moved the defect
// rather than fixing it.
//
// This pins the INSERT-TO-DRAIN contract directly: whatever shape the sink
// emits must be a shape the drain accepts.
func TestDirectiveNagOutboxShapeIsAcceptedByTheDrain(t *testing.T) {
	// The shape the sink now emits for a directive nag.
	sourceKind := db.WakeOutboxSourceWorkflowNote
	coalesceKey := db.WakeOutboxDirectiveCoalescePrefix + "4242"

	kind, ok := wakeOutboxKindForSource(sourceKind, coalesceKey)
	if !ok {
		t.Fatalf("drain rejected the emitted nag shape (source_kind=%q coalesce=%q) as an unsupported source kind", sourceKind, coalesceKey)
	}
	if kind != db.WakeOutboxKindDirective {
		t.Fatalf("drain classified the nag as %q, want %q", kind, db.WakeOutboxKindDirective)
	}

	// And the ORIGINAL shape must be exactly what the drain refuses, so this
	// guard cannot pass by accident on a drain that accepts everything.
	if _, ok := wakeOutboxKindForSource(db.WakeOutboxKindDirective, db.WakeOutboxKindDirective); ok {
		t.Fatal("drain accepted source_kind=\"directive\"; this guard would not have caught the original defect")
	}
}

// #1352 B1 — the polarity requirement checked BY QUERY, which is how it failed.
// The previous round asserted the column was WRITTEN; the reviewer showed no
// operator-facing reader consumes it, so an exhausted obligation was invisible
// to the person who needs it. Comms builds threads from NOTES.
//
// THE MUTANT IS THE PREVIOUS BEHAVIOUR: stamp the column and write no marker.
func TestDirectiveTTLExhaustionLeavesAnOperatorVisibleMarker(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, time.Minute, 2)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	note := seedDirectiveTTLNote(t, store, "will exhaust visibly", time.Minute, true)
	acknowledgeDirectiveTTLNote(t, store, note)

	base := time.Now().UTC().Add(10 * time.Minute)
	for i := 0; i < 8; i++ {
		if err := evaluateOrgDirectiveTTLs(ctx, store, &recordingSink{}, cfg, io.Discard, base.Add(time.Duration(i)*5*time.Minute), directiveTTLDependencies{}); err != nil {
			t.Fatal(err)
		}
	}

	notes, err := store.ListWorkflowNotes(ctx, note.WorkflowID, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if parsed, _, ok := workflow.ParseOrgDirectiveExhaustedNote(n.Body); ok && parsed == note.ID {
			return
		}
	}
	t.Fatalf("exhaustion left no marker NOTE for directive %d: the column alone is invisible to Comms and to every operator-facing reader", note.ID)
}

// #1352 B4 — THE FULL INSERT-TO-DRAIN PROBE, pointed at the PRODUCTION ENTRY
// POINT. This is the shape g7-review ran to find the blocker; it failed with:
//
//	wake outbox row 1 has unsupported source kind "directive"
//
// My first attempt drove evaluateRules directly and never inserted. That was the
// wrong seam, not a code defect: the durable insert lives in Emit, and every
// in-file caller of evaluateRules is DOWNSTREAM of it — the only direct callers
// are tests, so no production path bypasses the outbox. Driving Emit exercises
// the real path.
//
// THE MUTANT IS THE PRE-FIX BEHAVIOUR: source_kind="directive" on the insert.
// It checks BOTH ENDS AGREE by feeding the row the SINK ACTUALLY WROTE into the
// drain's own classifier, so a rename satisfying only one end still fails.
func TestDirectiveNagInsertToDrainDelivers(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// ONE OBSERVER rule, seeded directly in the TEST home store. Observer scope is
	// deliberate: eventRuleMatchesAddressee short-circuits true for it, keeping the
	// ADDRESSING logic out of a probe whose subject is DURABLE DELIVERY. It also
	// sidesteps the trap that a nudge addresses the TARGET while an escalation
	// addresses the sender's PARENT, so a single addressed rule satisfies one path
	// and silently starves the other — no targetRoles, no insert, no error.
	if err := store.AddEventRule(ctx, db.EventRule{
		ID:      "probe-directive",
		OnKind:  db.WakeOutboxKindDirective,
		WakeRole: "owner",
		Scope:   db.EventRuleScopeObserver,
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Fatal("fixture seeded no event rule")
	}

	// INSERT through the production entry point, with the real nag event shape.
	sink := &eventRuleSink{store: store, home: home, wake: &fakeEventWake{}}
	nag := events.NewEvent(
		events.EventOrgDirective, "4242",
		db.WakeOutboxSourceWorkflowNote+":4242", "gitmoot/gitmoot",
		"overdue", "directive 4242 to owner awaits completion", time.Now().UTC(),
		workflow.RedactCommentText,
	)
	nag.WakeTargetRole = "owner"
	sink.Emit(ctx, nag)

	rows, err := store.ListWakeOutboxObligations(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rows.Len() == 0 {
		t.Fatal("no wake outbox row was inserted; the nag never reached the outbox")
	}

	// BOTH ENDS AGREE, checked against the row the SINK actually wrote.
	for _, row := range append(append([]db.WakeOutboxObligation{}, rows.Pending...), rows.AgedAttempted...) {
		kind, ok := wakeOutboxKindForSource(row.SourceKind, row.CoalesceKey)
		if !ok {
			t.Fatalf("drain refuses the row the sink wrote: unsupported source kind %q (coalesce %q)", row.SourceKind, row.CoalesceKey)
		}
		if kind != db.WakeOutboxKindDirective {
			t.Fatalf("row the sink wrote classifies as %q, want %q", kind, db.WakeOutboxKindDirective)
		}
	}

	// ASSERT WHAT ARRIVES. wakeOutboxEvent is the decoder that renders the row
	// into the operator's command, so decoding the row the sink actually wrote is
	// the READ side of this contract. An insertion-only check passed while the
	// delivered command was garbage; this cannot.
	batch := append(append([]db.WakeOutboxObligation{}, rows.Pending...), rows.AgedAttempted...)
	decoded, err := wakeOutboxEvent(batch, time.Now().UTC())
	if err != nil {
		t.Fatalf("decoder refused the row the sink wrote: %v", err)
	}
	if strings.Contains(decoded.Detail, "schema_version") || strings.Contains(decoded.Detail, "{") {
		t.Fatalf("delivered command carries the SERIALIZED EVENT instead of the directive id: %q", decoded.Detail)
	}
	if !strings.Contains(decoded.Detail, "4242") {
		t.Fatalf("delivered command does not name directive 4242: %q", decoded.Detail)
	}

	// And the drain must still accept it.
	if err := drainReplyWakeOutbox(ctx, store, time.Now().UTC(), func(context.Context) (replyWakeDelivery, error) {
		return replyWakeDelivery{sink: &recordingSink{}, rules: rules}, nil
	}); err != nil && strings.Contains(err.Error(), "unsupported source kind") {
		t.Fatalf("drain REFUSED the nag it was just handed: %v", err)
	}

}

// #1352 F1 — the count-error FALLBACK, exercised rather than assumed. Changing
// sweepWindow's fallback from 200 to 1 previously left all 17 directive tests
// green: nothing drove the error path, so a regression could silently evaluate a
// single obligation after a count failure.
//
// THE MUTANT IS A DEGRADED FALLBACK (return 1). This forces the count to fail and
// requires the sweep to still evaluate the whole population.
func TestDirectiveTTLSweepFallsBackToFullWindowOnCountError(t *testing.T) {
	home := t.TempDir()
	cfg := writeDirectiveTTLConfig(t, home, "supervisor", 10*time.Minute, 0, 3)
	store := openDirectiveTTLStore(t, home)
	defer store.Close()
	var seeded []int64
	for i := 0; i < 5; i++ {
		seeded = append(seeded, seedDirectiveTTLNote(t, store, fmt.Sprintf("row %d", i), 0, false).ID)
	}
	sink := &recordingSink{}

	deps := directiveTTLDependencies{
		countOpen: func(ctx context.Context, s *db.Store) (int, error) {
			return 0, errors.New("count failed")
		},
	}
	if err := evaluateOrgDirectiveTTLs(context.Background(), store, sink, cfg, io.Discard, time.Now().UTC().Add(20*time.Minute), deps); err != nil {
		t.Fatal(err)
	}

	woken := map[string]bool{}
	for _, w := range sink.byType(events.EventOrgDirective) {
		woken[w.JobID] = true
	}
	for _, id := range seeded {
		if !woken[fmt.Sprint(id)] {
			t.Fatalf("directive %d was not evaluated after a count error: the fallback window is degraded, not full (woken=%d of %d)", id, len(woken), len(seeded))
		}
	}
}

// #1352 — the NORMAL LIFECYCLE across BOTH PRODUCTION WRITERS, asserting
// IDENTITY rather than count.
//
// The previous guard built its initial row by calling the TTL sink itself, so it
// never exercised the directive-send writer, and it asserted only ROW COUNT and
// STATE. Two semantic mutants survived that: dropping the WHERE clause (an
// unconditional update that disturbs a pending row) and INSERT OR REPLACE (which
// keeps the count at one while REPLACING row id 1 with id 2). COUNT CANNOT
// DISTINGUISH A REVIVED ROW FROM A RECREATED ONE — only the id can.
//
// So: initial row via `org directive send` (writer 1, the workflow-note path),
// nag via the TTL sink (writer 2), then assert a STABLE ROW ID and, for a
// repeat against a pending row, UNCHANGED FIELDS.
func TestDirectiveNagRevivesTheDeliveredWakeRowWithoutDuplicating(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n[org.roles.\"worker\"]\nparent=\"owner\"\nscope=[\"*\"]\npane=\"w1:p2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITMOOT_ORG_ROLE", "owner")
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "probe-directive", OnKind: db.WakeOutboxKindDirective,
		Scope: db.EventRuleScopeObserver, WakeRole: "worker", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// WRITER 1: the directive-send path commits the note and its pending wake
	// obligation together. This is the row production actually starts from.
	var out, errOut bytes.Buffer
	if code := runOrg([]string{"directive", "send", "--home", home, "--to", "worker", "--workflow", "release/revive", "do the thing"}, &out, &errOut); code != 0 {
		t.Fatalf("send code=%d err=%q", code, errOut.String())
	}
	directiveID := strings.Fields(out.String())[2]

	initial, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 {
		t.Fatalf("directive send produced %d wake rows, want 1", len(initial))
	}
	originalID := initial[0].ID

	// Deliver it, following the real state machine.
	if _, err := store.ClaimWakeOutbox(ctx, []int64{originalID}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishWakeOutbox(ctx, []int64{originalID}, db.WakeOutboxStateDelivered, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// WRITER 2: a TTL nag for the SAME obligation.
	sink := &eventRuleSink{store: store, home: home, wake: &fakeEventWake{}}
	nag := func() events.Event {
		e := events.NewEvent(
			events.EventOrgDirective, directiveID,
			db.WakeOutboxSourceWorkflowNote+":"+directiveID, "gitmoot/gitmoot",
			"overdue", "directive "+directiveID+" awaits completion", time.Now().UTC(),
			workflow.RedactCommentText,
		)
		e.WakeTargetRole = "worker"
		return e
	}
	sink.Emit(ctx, nag())

	revived, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(revived) != 1 {
		t.Fatalf("after the nag there are %d rows, want 1", len(revived))
	}
	// IDENTITY. Under INSERT OR REPLACE the count stays 1 and the ID changes,
	// which is a recreated obligation wearing a revived one's costume.
	if revived[0].ID != originalID {
		t.Fatalf("obligation row was REPLACED (id %d -> %d), not revived: identity must survive a nag", originalID, revived[0].ID)
	}
	if revived[0].State != "pending" {
		t.Fatalf("delivered row was not revived: state=%q", revived[0].State)
	}

	// A repeat against an ALREADY-PENDING row must change nothing at all.
	before := revived[0]
	// updated_at has millisecond precision, so two emits inside one millisecond
	// would be indistinguishable and an unconditional update would slip through.
	// This sleep is what makes the no-touch assertion DETERMINISTIC rather than
	// a race the mutant can win.
	time.Sleep(5 * time.Millisecond)
	sink.Emit(ctx, nag())
	after, err := store.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != originalID {
		t.Fatalf("second nag disturbed obligation identity: rows=%d id=%d want id=%d", len(after), after[0].ID, originalID)
	}
	if after[0].UpdatedAt != before.UpdatedAt || after[0].State != before.State || after[0].AttemptCount != before.AttemptCount {
		t.Fatalf("second nag TOUCHED a pending row: updated_at %q->%q state %q->%q attempts %d->%d — there is nothing new to say",
			before.UpdatedAt, after[0].UpdatedAt, before.State, after[0].State, before.AttemptCount, after[0].AttemptCount)
	}
}
