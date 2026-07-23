package db

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCountCurrentJobsByOrgRoleUsesActiveIndexes(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "owner-queued", Agent: "a", Type: "ask", State: "queued", Payload: `{"acting_org_role":"owner"}`},
		{ID: "owner-running", Agent: "a", Type: "ask", State: "running", Payload: `{"acting_org_role":"owner"}`},
		{ID: "owner-terminal", Agent: "a", Type: "ask", State: "succeeded", Payload: `{"acting_org_role":"owner"}`},
		{ID: "anchor", Agent: "a", Type: "ask", State: "queued", Payload: `{}`},
		{ID: "review-queued", Agent: "a", Type: "ask", State: "queued", Payload: `{"acting_org_role":"review"}`},
		{ID: "malformed", Agent: "a", Type: "ask", State: "running", Payload: `{`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
	}
	got, err := store.CountCurrentJobsByOrgRole(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"owner":  {"running": 1, "queued": 1},
		"review": {"queued": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %#v, want %#v", got, want)
	}
	for _, query := range []struct {
		name, sql, index string
	}{
		{name: "running", sql: countCurrentJobsByOrgRoleRunningSQL, index: "idx_jobs_running_updated_at"},
		{name: "queued", sql: countCurrentJobsByOrgRoleQueuedSQL, index: "idx_jobs_queued_created"},
	} {
		t.Run(query.name, func(t *testing.T) {
			rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query.sql)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(plan, "\n"), query.index) {
				t.Fatalf("plan does not use %s:\n%s", query.index, strings.Join(plan, "\n"))
			}
		})
	}
}

func TestCountJobsByOrgRoleSince(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, job := range []Job{
		{ID: "owner-running", Agent: "a", Type: "ask", State: "running", Payload: `{"acting_org_role":"owner"}`},
		{ID: "owner-succeeded", Agent: "a", Type: "ask", State: "succeeded", Payload: `{"acting_org_role":"owner"}`},
		{ID: "review-queued", Agent: "a", Type: "ask", State: "queued", Payload: `{"acting_org_role":"review"}`},
		{ID: "old-owner", Agent: "a", Type: "ask", State: "queued", Payload: `{"acting_org_role":"owner"}`},
		{ID: "anchor", Agent: "a", Type: "ask", State: "running", Payload: `{}`},
		{ID: "empty-role", Agent: "a", Type: "ask", State: "running", Payload: `{"acting_org_role":""}`},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().UTC().Add(-time.Hour)
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
		since.Add(-time.Hour).Format("2006-01-02 15:04:05"), "old-owner"); err != nil {
		t.Fatal(err)
	}
	got, err := store.CountJobsByOrgRoleSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"owner":  {"running": 1, "succeeded": 1},
		"review": {"queued": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %#v, want %#v", got, want)
	}
	all, err := store.CountJobsByOrgRoleSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all["owner"]["queued"] != 1 {
		t.Fatalf("zero-since counts = %#v, want old owner job", all)
	}
}
