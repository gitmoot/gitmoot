package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestOrgInterruptReportArithmetic pins the numbers #1983 exists to publish.
// Before this command the campaign's own table was derived by hand from
// wake_outbox, which is exactly why a fix in the sibling slices could not be
// verified and a regression could not be detected.
func TestOrgInterruptReportArithmetic(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) string {
		return base.Add(offset).Format(time.RFC3339Nano)
	}
	rules := []db.EventRule{
		{ID: "reply-busy", OnKind: db.WakeOutboxKindReply, WakeRole: "busy", Enabled: true},
		{ID: "reply-quiet", OnKind: db.WakeOutboxKindReply, WakeRole: "quiet", Enabled: true},
		// Deliberately absent: any `fact` route. That is the live condition
		// behind 49 obligations pending since 2026-08-02 (#1982).
	}
	activity := []db.WakeOutboxActivity{
		// busy: gaps of 1m, 2m, 30m. Median of {60,120,1800} is 120s and two
		// of the three gaps are under the five-minute threshold.
		{ID: 1, TargetRole: "busy", SourceKind: db.WakeOutboxSourceWorkflowNote, CoalesceKey: "reply:busy", State: db.WakeOutboxStateDelivered, CreatedAt: at(0)},
		{ID: 2, TargetRole: "busy", SourceKind: db.WakeOutboxSourceWorkflowNote, CoalesceKey: "reply:busy", State: db.WakeOutboxStateDelivered, CreatedAt: at(time.Minute)},
		{ID: 3, TargetRole: "busy", SourceKind: db.WakeOutboxSourceEscalation, CoalesceKey: "escalation:busy", State: db.WakeOutboxStateStalled, CreatedAt: at(3 * time.Minute)},
		{ID: 4, TargetRole: "busy", SourceKind: db.WakeOutboxSourceBlocked, CoalesceKey: "blocked:busy", State: db.WakeOutboxStateDeliveryUnknown, CreatedAt: at(33 * time.Minute)},
		// A collapsed row is an interrupt that did NOT happen: it must not
		// count as a wake, and it must not create a gap.
		{ID: 5, TargetRole: "busy", SourceKind: db.WakeOutboxSourceWorkflowNote, CoalesceKey: "reply:busy", State: db.WakeOutboxStateSuperseded, CreatedAt: at(34 * time.Minute), Collapsed: true},
		// quiet: one delivered wake, so no gap exists at all.
		{ID: 6, TargetRole: "quiet", SourceKind: db.WakeOutboxSourceWorkflowNote, CoalesceKey: "reply:quiet", State: db.WakeOutboxStateDelivered, CreatedAt: at(time.Hour)},
		// stranded: a pending obligation with no route for its kind.
		{ID: 7, TargetRole: "stranded", SourceKind: db.WakeOutboxSourceAwaitedFact, CoalesceKey: "fact:stranded", State: db.WakeOutboxStatePending, CreatedAt: at(2 * time.Hour)},
	}
	nudges := []db.OrgDirectiveNudgeRow{
		{ID: 10, Body: workflow.FormatOrgDirectiveNote("owner", "busy", "wf", "finish it"), AckNudges: 1, CompletionNudges: 3},
		{ID: 11, Body: workflow.FormatOrgDirectiveNote("owner", "quiet", "wf", "and this"), CompletionNudges: 0},
		{ID: 12, Body: "not a directive marker", CompletionNudges: 99},
	}

	report := buildOrgInterruptReport(activity, nudges, rules, base, base.Add(24*time.Hour), 24*time.Hour)

	if report.Wakes != 6 || report.Collapsed != 1 {
		t.Fatalf("report wakes=%d collapsed=%d, want 6 and 1", report.Wakes, report.Collapsed)
	}
	if report.Unproven != 2 {
		t.Fatalf("report unproven=%d, want the stalled and delivery_unknown rows", report.Unproven)
	}
	if report.RoutelessPending != 1 || report.RoutelessByKindLbl[db.WakeOutboxKindFact] != 1 {
		t.Fatalf("routeless=%d by kind=%v, want one fact obligation", report.RoutelessPending, report.RoutelessByKindLbl)
	}
	byRole := map[string]orgInterruptSeat{}
	for _, seat := range report.Seats {
		byRole[seat.Role] = seat
	}
	busy := byRole["busy"]
	if busy.Wakes != 4 || busy.Collapsed != 1 {
		t.Fatalf("busy wakes=%d collapsed=%d, want 4 and 1", busy.Wakes, busy.Collapsed)
	}
	if busy.MedianGapSeconds != 120 {
		t.Fatalf("busy median gap = %.0fs, want 120s from gaps of 60, 120 and 1800", busy.MedianGapSeconds)
	}
	if busy.Gaps != 3 || busy.GapsUnderShort != 2 {
		t.Fatalf("busy gaps=%d under-threshold=%d, want 3 and 2", busy.Gaps, busy.GapsUnderShort)
	}
	if busy.PerDay != 4 {
		t.Fatalf("busy per day = %.2f, want 4 over a 24h window", busy.PerDay)
	}
	if busy.Sources[db.WakeOutboxSourceWorkflowNote] != 2 ||
		busy.Sources[db.WakeOutboxSourceEscalation] != 1 ||
		busy.Sources[db.WakeOutboxSourceBlocked] != 1 {
		t.Fatalf("busy sources = %v, want the four kinds distinguished", busy.Sources)
	}
	if busy.Delivered != 2 || busy.Unproven != 2 {
		t.Fatalf("busy delivered=%d unproven=%d, want 2 and 2", busy.Delivered, busy.Unproven)
	}
	if busy.CompletionNags != 3 || busy.AckNudges != 1 {
		t.Fatalf("busy nags=%d ack=%d, want the persisted ladders", busy.CompletionNags, busy.AckNudges)
	}
	quiet := byRole["quiet"]
	if quiet.Gaps != 0 || quiet.MedianGapSeconds != 0 {
		t.Fatalf("quiet gaps=%d median=%.0f, want no gap from a single wake", quiet.Gaps, quiet.MedianGapSeconds)
	}
	stranded := byRole["stranded"]
	if stranded.Routeless != 1 || stranded.OldestRLAt != at(2*time.Hour) {
		t.Fatalf("stranded routeless=%d oldest=%q, want the unrouted obligation named", stranded.Routeless, stranded.OldestRLAt)
	}
	// An unparseable directive marker must not be attributed to anyone.
	for _, seat := range report.Seats {
		if seat.CompletionNags == 99 {
			t.Fatalf("seat %q absorbed a malformed directive's nudges", seat.Role)
		}
	}
	if report.Seats[0].Role != "busy" {
		t.Fatalf("seats = %+v, want the most interrupted seat first", report.Seats)
	}
}

// TestOrgInterruptsCommandReportsFromTheStore is the end-to-end path: the
// command must produce the same numbers from real rows, in text and JSON,
// without hand-written SQL.
func TestOrgInterruptsCommandReportsFromTheStore(t *testing.T) {
	store, sink, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	for index := range 3 {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/report", Author: "worker", Body: fmt.Sprint(index),
			AddressedTarget: "owner",
		}); err != nil {
			t.Fatal(err)
		}
	}
	drainReplyWakeAfterAllRowsAreDue(t, store, sink)

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"interrupts", "--home", home, "--window", "24h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("org interrupts code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Interrupt rate per seat") || !strings.Contains(stdout.String(), "owner") {
		t.Fatalf("text report = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"interrupts", "--home", home, "--window", "24h", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json code=%d err=%q", code, stderr.String())
	}
	var report orgInterruptReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Seats) != 1 || report.Seats[0].Role != "owner" {
		t.Fatalf("json seats = %+v", report.Seats)
	}
	// Three notes coalesced into one wake: one delivered interrupt and two
	// collapsed rows. Reading that from this command rather than from SQL is
	// the acceptance criterion for the #1978 slice.
	seat := report.Seats[0]
	if seat.Wakes != 1 || seat.Collapsed != 2 || seat.Delivered != 1 {
		t.Fatalf("seat = %+v, want one delivered wake and two collapsed rows", seat)
	}
	if report.Collapsed != 2 {
		t.Fatalf("report collapsed = %d, want 2", report.Collapsed)
	}

	// WIRING, not just arithmetic. A mutant that computed the route-history
	// boundary and never assigned it to the report survived until this existed:
	// the caveat test set the field by hand, so nothing proved the COMMAND
	// populates it from the store. Same shape as the #1938 tests that all
	// passed with the production fix deleted.
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "reply-doomed", OnKind: db.WakeOutboxKindReply, WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteEventRule(ctx, "reply-doomed"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"interrupts", "--home", home, "--window", "24h", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("json code=%d err=%q", code, stderr.String())
	}
	var withHistory orgInterruptReport
	if err := json.Unmarshal(stdout.Bytes(), &withHistory); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(withHistory.RouteHistoryStart) == "" {
		t.Fatal("command did not carry the recorded route-history boundary into the report")
	}
	if _, err := time.Parse(time.RFC3339Nano, withHistory.RouteHistoryStart); err != nil {
		t.Fatalf("route_history_start = %q, want an RFC3339 timestamp: %v", withHistory.RouteHistoryStart, err)
	}
}

func TestOrgInterruptWindowParsing(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		bad   bool
	}{
		{value: "", want: orgInterruptDefaultWindow},
		{value: "24h", want: 24 * time.Hour},
		{value: "7d", want: 7 * 24 * time.Hour},
		{value: "90m", want: 90 * time.Minute},
		{value: "0", want: 0},
		{value: "all", want: 0},
		{value: "-3h", bad: true},
		{value: "-2d", bad: true},
		{value: "soon", bad: true},
	} {
		got, err := parseOrgInterruptWindow(test.value)
		if test.bad {
			if err == nil {
				t.Fatalf("window %q parsed as %s, want a refusal", test.value, got)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Fatalf("window %q = %s, %v; want %s", test.value, got, err, test.want)
		}
	}
}

// TestOrgInterruptTextStatesItsOwnRouteHistoryLimit puts the limitation in the
// OUTPUT, not only in a PR body (phobos, on gm-integrity's framing: a
// limitation in a report instead of a record is the same defect class as
// recording nothing).
//
// The retired-versus-gap split in each row's `wake_unroutable` event reads the
// deletion tombstones. On this fleet they begin 2026-08-30, so every role
// retired earlier reads as never-configured, including the seven roles with
// delivered escalations and no current route, which are exactly the rows the
// question is about.
func TestOrgInterruptTextStatesItsOwnRouteHistoryLimit(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	activity := []db.WakeOutboxActivity{{
		ID: 1, TargetRole: "stranded", SourceKind: db.WakeOutboxSourceAwaitedFact,
		CoalesceKey: "fact:stranded", State: db.WakeOutboxStatePending,
		CreatedAt: base.Format(time.RFC3339Nano),
	}}
	report := buildOrgInterruptReport(activity, nil, nil, base, base.Add(time.Hour), time.Hour)
	if report.RoutelessPending != 1 {
		t.Fatalf("routeless = %d, want the unrouted row", report.RoutelessPending)
	}

	report.RouteHistoryStart = "2026-08-30T17:27:04Z"
	var withHistory bytes.Buffer
	writeOrgInterruptText(&withHistory, report)
	if !strings.Contains(withHistory.String(), "Route history begins 2026-08-30T17:27:04Z") ||
		!strings.Contains(withHistory.String(), db.WakeOutboxUnroutableNeverConfigured) {
		t.Fatalf("report does not state its categorisation limit: %q", withHistory.String())
	}

	// No tombstones at all is a STRONGER limit, not a missing one: every row
	// reads as never-configured whatever actually happened.
	report.RouteHistoryStart = ""
	var noHistory bytes.Buffer
	writeOrgInterruptText(&noHistory, report)
	if !strings.Contains(noHistory.String(), "No route deletions are recorded at all") {
		t.Fatalf("report with no route history does not say so: %q", noHistory.String())
	}

	// And the caveat is attached to the rows it qualifies, not printed
	// unconditionally: a fleet with every route in place should not be told
	// about a limit that cannot affect it.
	clean := buildOrgInterruptReport([]db.WakeOutboxActivity{{
		ID: 2, TargetRole: "owner", SourceKind: db.WakeOutboxSourceWorkflowNote,
		CoalesceKey: "reply:owner", State: db.WakeOutboxStateDelivered,
		CreatedAt: base.Format(time.RFC3339Nano),
	}}, nil, []db.EventRule{{
		ID: "reply-owner", OnKind: db.WakeOutboxKindReply, WakeRole: "owner", Enabled: true,
	}}, base, base.Add(time.Hour), time.Hour)
	clean.RouteHistoryStart = "2026-08-30T17:27:04Z"
	var cleanOut bytes.Buffer
	writeOrgInterruptText(&cleanOut, clean)
	if strings.Contains(cleanOut.String(), "Route history begins") {
		t.Fatalf("caveat printed with no routeless rows to qualify: %q", cleanOut.String())
	}
}
