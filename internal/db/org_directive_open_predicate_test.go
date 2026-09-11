package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// explainPlan returns the query plan for the SHIPPED sql text.
func explainPlan(t *testing.T, store *Store, sql string, args ...any) string {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		out.WriteString(detail)
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return out.String()
}

// The open-directive predicate must not be CORRELATED (#2138).
//
// This is the defect test, and it asserts the shape rather than a duration: a
// timing bound is flaky and a row count cannot see the difference. The shipped
// predicate re-scanned every same-workflow sibling note for every candidate,
// because its comparison literal was built from d.id - measured on the live
// store as 46,232,418 substr evaluations and 16.96s to return the number 4,
// twice per pass on a one-minute ticker, which burned a full core forever.
//
// SQLite names that shape in its own plan output: the old text produced
// `CORRELATED SCALAR SUBQUERY`. Materializing the marker set once produces
// `MATERIALIZE` plus an index probe instead. A mutant restoring the correlated
// predicate fails here, which no assertion about the returned rows would.
func TestOpenOrgDirectivePredicateIsNotCorrelated(t *testing.T) {
	store := openWorkflowTestStore(t)
	for _, test := range []struct {
		name string
		plan string
	}{
		{name: "count", plan: explainPlan(t, store, countOpenOrgDirectiveObligationsSQL)},
		{name: "list", plan: explainPlan(t, store, listOpenOrgDirectiveObligationsSQL, 10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.Contains(strings.ToUpper(test.plan), "CORRELATED") {
				t.Fatalf("%s plan is correlated per outer row, so cost is quadratic in siblings:\n%s", test.name, test.plan)
			}
			// Positive control: absence of "CORRELATED" must mean the marker set
			// really is built once, not that the planner reported nothing.
			if !strings.Contains(strings.ToUpper(test.plan), "MATERIALIZE") {
				t.Fatalf("%s plan does not materialize the marker set, so the absence of CORRELATED proves nothing:\n%s", test.name, test.plan)
			}
		})
	}
}

// The rewrite derives the target id by TEXT extraction instead of building a
// per-row literal, and these are the cases where a careless extraction silently
// changes which directives read as open. Each one is a wrong ANSWER, not a
// slowdown, so they are the risk the performance fix carries.
func TestOpenOrgDirectiveTerminalMarkerScoping(t *testing.T) {
	for _, test := range []struct {
		name     string
		marker   func(id int64) (workflow string, body string)
		wantOpen bool
	}{
		{
			name: "done in the same workflow retires it",
			marker: func(id int64) (string, string) {
				return "release/directives", fmt.Sprintf("[org:directive-done id=%d by=worker] finished", id)
			},
			wantOpen: false,
		},
		{
			name: "cancel in the same workflow retires it",
			marker: func(id int64) (string, string) {
				return "release/directives", fmt.Sprintf("[org:directive-cancel id=%d by=owner] withdrawn", id)
			},
			wantOpen: false,
		},
		{
			// The predicate this replaced required r.workflow_id = d.workflow_id.
			// A first draft of the rewrite dropped that scope, which retired a
			// directive on a marker belonging to an unrelated workflow.
			name: "done in a FOREIGN workflow must not retire it",
			marker: func(id int64) (string, string) {
				return "unrelated/workflow", fmt.Sprintf("[org:directive-done id=%d by=worker] finished", id)
			},
			wantOpen: true,
		},
		{
			// The old text compared '<prefix>' || d.id || ' ', so the id had to be
			// followed by a space. Extraction must not accept a bare tail.
			name: "marker with no space after the id must not retire it",
			marker: func(id int64) (string, string) {
				return "release/directives", fmt.Sprintf("[org:directive-done id=%d]", id)
			},
			wantOpen: true,
		},
		{
			// '007' never equalled '7' under the old text comparison. Casting the
			// extracted target to an integer would make it match.
			name: "zero-padded id must not retire the unpadded directive",
			marker: func(id int64) (string, string) {
				return "release/directives", fmt.Sprintf("[org:directive-done id=0%d by=worker] finished", id)
			},
			wantOpen: true,
		},
		{
			name: "a marker naming a different directive must not retire it",
			marker: func(id int64) (string, string) {
				return "release/directives", fmt.Sprintf("[org:directive-done id=%d by=worker] finished", id+9000)
			},
			wantOpen: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			directive, err := store.InsertWorkflowNote(ctx, WorkflowNote{
				WorkflowID: "release/directives", Author: "owner",
				Body: "[org:directive to=worker from=owner wf=release/directives] do the thing",
			})
			if err != nil {
				t.Fatal(err)
			}
			workflowID, body := test.marker(directive.ID)
			if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: workflowID, Author: "worker", Body: body}); err != nil {
				t.Fatal(err)
			}

			count, err := store.CountOpenOrgDirectiveObligations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			items, err := store.ListOpenOrgDirectiveObligations(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if test.wantOpen {
				wantCount = 1
			}
			if count != wantCount || len(items) != wantCount {
				t.Fatalf("count=%d list=%d, want %d open for marker %q in %q", count, len(items), wantCount, body, workflowID)
			}
		})
	}
}

// The ack timestamp was a correlated MIN() per candidate and is now a grouped
// materialized join, which is the half that measured 21.16s. The earliest of
// ack/delivered must survive that change, including when both exist.
//
// The timestamps are set EXPLICITLY. created_at defaults to CURRENT_TIMESTAMP at
// one-second granularity, so two notes inserted in the same test tick share a
// value and MIN == MAX - under which a mutant taking the LATEST marker passes.
// That is not hypothetical: such a mutant survived the first version of this
// test, and the fixture was the defect rather than the assertion.
func TestOpenOrgDirectiveAckTimestampTakesEarliestMarker(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "owner",
		Body: "[org:directive to=worker from=owner wf=release/directives] do the thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	const earliest = "2026-09-01 08:00:00"
	const latest = "2026-09-02 09:30:00"
	for _, marker := range []struct {
		body      string
		createdAt string
	}{
		{body: fmt.Sprintf("[org:directive-delivered id=%d by=transport] relayed", directive.ID), createdAt: earliest},
		{body: fmt.Sprintf("[org:directive-ack id=%d by=worker] seen", directive.ID), createdAt: latest},
	} {
		note, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release/directives", Author: "worker", Body: marker.body})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE workflow_notes SET created_at = ? WHERE id = ?`, marker.createdAt, note.ID); err != nil {
			t.Fatal(err)
		}
	}

	items, err := store.ListOpenOrgDirectiveObligations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].AckedAt != earliest {
		t.Fatalf("obligations = %+v, want one row acked at %q (earliest of ack/delivered)", items, earliest)
	}
}

// A foreign-workflow ACK must not be attributed either - the same scoping the
// terminal marker needs, on the other join.
func TestOpenOrgDirectiveAckIgnoresForeignWorkflow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	directive, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "release/directives", Author: "owner",
		Body: "[org:directive to=worker from=owner wf=release/directives] do the thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "unrelated/workflow", Author: "worker",
		Body: fmt.Sprintf("[org:directive-ack id=%d by=worker] seen", directive.ID),
	}); err != nil {
		t.Fatal(err)
	}

	items, err := store.ListOpenOrgDirectiveObligations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].AckedAt != "" {
		t.Fatalf("obligations = %+v, want one row with no ack attributed from a foreign workflow", items)
	}
}
