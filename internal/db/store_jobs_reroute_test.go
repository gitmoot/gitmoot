package db

import (
	"context"
	"testing"
	"time"
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
	cap := ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, PerAttemptUSD: 1, MaxConcurrent: 2}
	remote := `{"repo":"o/r","exec_backend":"remote"}`
	ev := JobEvent{Kind: "disk_guard_routed_remote", Message: "m"}
	if first, err := store.RouteQueuedJobRemote(ctx, "a", remote, 0, cap, ev); err != nil || !first {
		t.Fatalf("first route with one slot left: %v %v", first, err)
	}
	if second, err := store.RouteQueuedJobRemote(ctx, "b", remote, 0, cap, ev); err != nil || second {
		t.Fatalf("second route with no slot left: routed=%v err=%v", second, err)
	}
	if job, _ := store.GetJob(ctx, "b"); job.Payload != `{"repo":"o/r"}` {
		t.Fatalf("refused route changed the payload: %q", job.Payload)
	}
}

// The route must admit exactly what ReserveExecBackendAttempt will: attempts
// still billing count at the dollars they reserved, even after the configured
// per-attempt price changed (#2273 review).
func TestRouteQueuedJobRemoteCountsBillingAttemptsAtTheirReservedDollars(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	insertWorktreeRefJob(t, store, "old", "running", `{"repo":"o/r","exec_backend":"remote"}`)
	for _, id := range []string{"a", "b"} {
		insertWorktreeRefJob(t, store, id, "queued", `{"repo":"o/r"}`)
	}
	if err := store.ReserveExecBackendAttempt(ctx, ExecBackendAttemptReservation{
		ExecBackendAttemptKey: ExecBackendAttemptKey{JobID: "old", Attempt: 1},
		Provider:              "e2b", DaemonFencingToken: "t", BootID: "b",
		TTLExpiresAt: time.Now().Add(time.Hour), CostReservedUSD: 3,
	}, ExecBackendCostCap{Configured: true, MaxReservedUSD: 4, PerAttemptUSD: 3}); err != nil {
		t.Fatal(err)
	}
	// The price drops to $1 with a $4 cap: $3 is still reserved, so exactly
	// one more $1 attempt fits.
	cap := ExecBackendCostCap{Configured: true, MaxReservedUSD: 4, PerAttemptUSD: 1}
	remote := `{"repo":"o/r","exec_backend":"remote"}`
	ev := JobEvent{Kind: "disk_guard_routed_remote", Message: "m"}
	if ok, err := store.RouteQueuedJobRemote(ctx, "a", remote, 0, cap, ev); err != nil || !ok {
		t.Fatalf("the one $1 slot left: routed=%v err=%v", ok, err)
	}
	if ok, err := store.RouteQueuedJobRemote(ctx, "b", remote, 0, cap, ev); err != nil || ok {
		t.Fatalf("a second $1 route over a $4 cap with $3 billing: routed=%v err=%v", ok, err)
	}
}

// The routing cap is E2B's. A running opted-in Mac review and its Mac attempt
// hold Mac capacity, so neither may take the one E2B slot from a waiting
// review; an E2B attempt still fills it.
func TestRouteQueuedJobRemoteCountsOnlyTheRoutedProvider(t *testing.T) {
	ctx := context.Background()
	store := openStoreOperationsTestStore(t)
	insertWorktreeRefJob(t, store, "mac-running", "running", `{"repo":"o/r","exec_backend":"remote","exec_provider":"mac"}`)
	insertWorktreeRefJob(t, store, "mac-billing", "running", `{"repo":"o/r","exec_backend":"remote","exec_provider":"mac"}`)
	if err := store.ReserveExecBackendAttempt(ctx, ExecBackendAttemptReservation{
		ExecBackendAttemptKey: ExecBackendAttemptKey{JobID: "mac-billing", Attempt: 1},
		Provider:              "mac", DaemonFencingToken: "t", BootID: "b", TTLExpiresAt: time.Now().Add(time.Hour),
	}, ExecBackendCostCap{Configured: true, CapacityOnly: true, MaxConcurrent: 2}); err != nil {
		t.Fatal(err)
	}
	insertWorktreeRefJob(t, store, "a", "queued", `{"repo":"o/r"}`)
	insertWorktreeRefJob(t, store, "b", "queued", `{"repo":"o/r"}`)
	cap := ExecBackendCostCap{Configured: true, MaxReservedUSD: 1, PerAttemptUSD: 1, MaxConcurrent: 1}
	remote := `{"repo":"o/r","exec_backend":"remote"}`
	ev := JobEvent{Kind: "disk_guard_routed_remote", Message: "m"}
	if routed, err := store.RouteQueuedJobRemote(ctx, "a", remote, 0, cap, ev); err != nil || !routed {
		t.Fatalf("Mac work took the E2B slot: routed=%v err=%v", routed, err)
	}
	if routed, err := store.RouteQueuedJobRemote(ctx, "b", remote, 0, cap, ev); err != nil || routed {
		t.Fatalf("second E2B route with the slot taken: routed=%v err=%v", routed, err)
	}
}
