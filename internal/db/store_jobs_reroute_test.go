package db

import (
	"context"
	"testing"
)

// A queued job may be claimed between the scheduler's read and its reroute.
// The reroute must then change nothing: a running job's backend is fixed.
func TestRerouteQueuedJobPayloadLeavesAClaimedJobAlone(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	insertWorktreeRefJob(t, store, "queued", "queued", `{"repo":"o/r"}`)
	insertWorktreeRefJob(t, store, "running", "running", `{"repo":"o/r"}`)
	event := JobEvent{Kind: "disk_guard_routed_remote", Message: "m"}

	for id, want := range map[string]bool{"queued": true, "running": false} {
		event.JobID = id
		ok, err := store.RerouteQueuedJobPayload(ctx, id, `{"repo":"o/r","exec_backend":"remote"}`, 0, event)
		if err != nil || ok != want {
			t.Fatalf("%s: rerouted=%v err=%v, want %v", id, ok, err, want)
		}
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		events, err := store.ListJobEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		changed := job.Payload != `{"repo":"o/r"}`
		if changed != want || (len(events) == 1) != want {
			t.Fatalf("%s: payload %q, %d events; rerouted must be all or nothing", id, job.Payload, len(events))
		}
	}
}
