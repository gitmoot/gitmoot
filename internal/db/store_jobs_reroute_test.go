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

// Two scheduler passes can race for the last cloud slot. The count and the
// switch are one statement, so only one of them can take it.
func TestRouteQueuedJobRemoteTakesOnlyTheSlotsLeft(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	insertWorktreeRefJob(t, store, "running-remote", "running", `{"repo":"o/r","exec_backend":"remote"}`)
	insertWorktreeRefJob(t, store, "a", "queued", `{"repo":"o/r"}`)
	insertWorktreeRefJob(t, store, "b", "queued", `{"repo":"o/r"}`)
	remote := `{"repo":"o/r","exec_backend":"remote"}`
	ev := JobEvent{Kind: "disk_guard_routed_remote", Message: "m"}
	first, err := store.RouteQueuedJobRemote(ctx, "a", remote, 0, 2, ev)
	if err != nil || !first {
		t.Fatalf("first route with one slot left: %v %v", first, err)
	}
	second, err := store.RouteQueuedJobRemote(ctx, "b", remote, 0, 2, ev)
	if err != nil || second {
		t.Fatalf("second route with no slot left: routed=%v err=%v", second, err)
	}
	if job, _ := store.GetJob(ctx, "b"); job.Payload != `{"repo":"o/r"}` {
		t.Fatalf("refused route changed the payload: %q", job.Payload)
	}
}
