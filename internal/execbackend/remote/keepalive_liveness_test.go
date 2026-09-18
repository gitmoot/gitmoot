package remote

import (
	"context"
	"testing"
	"time"
)

// TestKeepaliveTickStopsWhenLivenessSaysNo is the #1539 amplification fix,
// tested through the REAL loop body rather than around it.
//
// `job kill` is graceful by contract: it stops new delegations and lets
// in-flight work finish, so a killed tree's running remote job keeps its
// sandbox. That was harmless while the sandbox lapsed at the provider's
// one-hour ceiling — but #2226's keepalive renews to the full requested TTL, so
// a review-class job held a billed instance for three hours after the operator
// killed its tree. Round 2 review of #2226 named the amplification.
//
// Renewal is the only thing that stops; the job still runs to completion.
//
// MUTATION: remove the shouldRenew check in keepaliveTick and this goes red
// with a renewal the predicate refused.
func TestKeepaliveTickStopsWhenLivenessSaysNo(t *testing.T) {
	renewals := 0
	setTimeout := func(context.Context, string, time.Duration) error { renewals++; return nil }
	backend := &Backend{}
	deadline := time.Now().Add(3 * time.Hour)

	// A live job renews and the loop continues.
	if !backend.keepaliveTickUsing(context.Background(), "sandbox-live", deadline, func() bool { return true }, setTimeout) {
		t.Fatal("keepaliveTick reported stop for a live job")
	}
	if renewals != 1 {
		t.Fatalf("renewals while live = %d, want 1", renewals)
	}

	// A killed or finished job does not renew, and the loop ends.
	if backend.keepaliveTickUsing(context.Background(), "sandbox-killed", deadline, func() bool { return false }, setTimeout) {
		t.Fatal("keepaliveTick reported continue after liveness said no")
	}
	if renewals != 1 {
		t.Fatalf("renewals after liveness said no = %d, want 1: a killed tree must not buy more provider time", renewals)
	}
}

// TestKeepaliveTickStopsAtTheDeadline pins that the requested TTL still bounds
// everything. An unbounded keepalive turns a lost owner into an immortal billed
// sandbox, which is worse than the amplification it would cure.
func TestKeepaliveTickStopsAtTheDeadline(t *testing.T) {
	renewals := 0
	backend := &Backend{}

	if backend.keepaliveTickUsing(context.Background(), "sandbox-expired", time.Now().Add(-time.Second),
		func() bool { return true },
		func(context.Context, string, time.Duration) error { renewals++; return nil }) {
		t.Fatal("keepaliveTick reported continue past the requested TTL")
	}
	if renewals != 0 {
		t.Fatalf("renewals past the deadline = %d, want 0", renewals)
	}
}

// TestKeepaliveTickWithoutLivenessRenews pins that a nil predicate keeps the
// deadline-only behaviour, which is correct for any caller that cannot observe
// job state.
func TestKeepaliveTickWithoutLivenessRenews(t *testing.T) {
	renewals := 0
	backend := &Backend{}

	if !backend.keepaliveTickUsing(context.Background(), "sandbox-nil-predicate", time.Now().Add(3*time.Hour), nil,
		func(context.Context, string, time.Duration) error { renewals++; return nil }) {
		t.Fatal("keepaliveTick reported stop with no liveness predicate")
	}
	if renewals != 1 {
		t.Fatalf("renewals with a nil predicate = %d, want 1", renewals)
	}
}
