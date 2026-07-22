package db

import (
	"context"
	"path/filepath"
	"testing"
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
