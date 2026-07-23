package db

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCountActiveJobsByOrgRole(t *testing.T) {
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
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", job.ID, err)
		}
	}
	for _, test := range []struct {
		role string
		want int
	}{
		{role: "owner", want: 2},
		{role: "review", want: 1},
		{role: "unrelated", want: 0},
		// role arg is trim+lowered to match persisted (normalized) acting_org_role.
		{role: "OWNER", want: 2},
		{role: "  owner  ", want: 2},
	} {
		count, err := store.CountActiveJobsByOrgRole(ctx, test.role)
		if err != nil || count != test.want {
			t.Fatalf("CountActiveJobsByOrgRole(%q) = %d, %v; want %d", test.role, count, err, test.want)
		}
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
