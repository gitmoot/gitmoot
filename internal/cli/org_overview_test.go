package cli

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

func TestStoreOrgLiveSourcePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		presence bool
		jobState string
		blocked  string
		want     org.LifecycleState
	}{
		{name: "blocked wins over working", presence: true, jobState: "running", blocked: "role:review", want: org.StateBlocked},
		{name: "working wins over idle", presence: true, jobState: "running", want: org.StateWorking},
		{name: "queued is not working", presence: true, jobState: "queued", want: org.StateIdle},
		{name: "task episode does not block role", presence: true, blocked: "task:repo:review", want: org.StateIdle},
		{name: "presence is idle", presence: true, want: org.StateIdle},
		{name: "never seen", want: org.StateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, paths := setupOrgHome(t)
			store, err := db.Open(paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			if test.presence {
				if err := store.TouchOrgRolePresence(ctx, "review", "test"); err != nil {
					t.Fatal(err)
				}
			}
			if test.jobState != "" {
				if err := store.CreateJob(ctx, db.Job{
					ID: "review-job", Agent: "worker", Type: "ask", State: test.jobState,
					Payload: `{"acting_org_role":"review"}`,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.blocked != "" {
				now := time.Now().UTC()
				if err := store.UpsertBlockedEpisode(ctx, test.blocked, now, now); err != nil {
					t.Fatal(err)
				}
			}
			shared, err := loadOrgSharedState(ctx, paths, store)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			states, observedAt, version, err := storeOrgLiveSource(&shared)(ctx, shared.Config)
			after := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			if got := states["review"].State; got != test.want {
				t.Fatalf("review state = %q, want %q", got, test.want)
			}
			if version != "store" || observedAt.Before(before) || observedAt.After(after) {
				t.Fatalf("source metadata = observed %v version %q", observedAt, version)
			}
		})
	}
}
