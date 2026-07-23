package cli

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/org"
)

func TestStoreOrgLiveSourcePersistedFreshness(t *testing.T) {
	fresh := time.Now().UTC().Add(-time.Minute)
	tests := []struct {
		name       string
		state      org.LifecycleState
		observedAt time.Time
		want       org.LifecycleState
	}{
		{name: "fresh blocked", state: org.StateBlocked, observedAt: fresh, want: org.StateBlocked},
		{name: "fresh working", state: org.StateWorking, observedAt: fresh, want: org.StateWorking},
		{name: "fresh idle", state: org.StateIdle, observedAt: fresh, want: org.StateIdle},
		{name: "stale", state: org.StateWorking, observedAt: fresh.Add(-storeOrgLivePresenceMaxAge), want: org.StateUnknown},
		{name: "done", state: org.StateDone, observedAt: fresh, want: org.StateUnknown},
		{name: "unknown", state: org.StateUnknown, observedAt: fresh, want: org.StateUnknown},
		{name: "absent", want: org.StateUnknown},
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
			if !test.observedAt.IsZero() {
				if err := store.UpsertRoleLivePresence(ctx, "review", string(test.state), test.observedAt); err != nil {
					t.Fatal(err)
				}
			}
			shared, err := loadOrgSharedState(ctx, paths, store)
			if err != nil {
				t.Fatal(err)
			}
			states, observedAt, version, err := storeOrgLiveSource(&shared)(ctx, shared.Config)
			if err != nil {
				t.Fatal(err)
			}
			if got := states["review"].State; got != test.want {
				t.Fatalf("review state = %q, want %q", got, test.want)
			}
			if version != "store" {
				t.Fatalf("source metadata = observed %v version %q", observedAt, version)
			}
			if test.want == org.StateUnknown && !observedAt.IsZero() && test.state != org.StateDone && test.state != org.StateUnknown {
				t.Fatalf("unknown source observedAt = %v, want zero", observedAt)
			}
		})
	}
}
