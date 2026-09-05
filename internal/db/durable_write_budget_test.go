package db

import (
	"testing"
	"time"
)

// #1836's ROOT SHAPE, pinned. The defect was never one number being wrong: it
// was TWO CONSTANTS IN TWO PACKAGES whose relative order nothing enforced.
// internal/cli bounded a wake-outbox terminal-state write at 5s while this
// package's driver waited up to sqliteBusyTimeoutMillis = 15s for the write
// lock, so the caller abandoned a write the driver was still legitimately
// waiting to perform, and a DELIVERED wake decayed to delivery_unknown with
// policy=expire_without_retry.
//
// DurableWriteBudget is derived from sqliteBusyTimeoutMillis for exactly that
// reason, and this test is the thing that keeps them ordered. Hand-setting
// either one out of order fails here rather than silently losing deliveries in
// production, which is where the last one was found.
func TestDurableWriteBudgetExceedsBusyTimeout(t *testing.T) {
	busy := time.Duration(sqliteBusyTimeoutMillis) * time.Millisecond
	if DurableWriteBudget <= busy {
		t.Fatalf("DurableWriteBudget %s must exceed the store's own busy budget %s; a caller that gives up first abandons a write the driver is still waiting to perform, which is #1836",
			DurableWriteBudget, busy)
	}
	// AND IT MUST LEAVE ROOM TO EXECUTE. A budget equal to the busy timeout
	// plus nothing expires at the instant the driver stops waiting, so the
	// statement it just won the lock for never runs.
	if margin := DurableWriteBudget - busy; margin < time.Second {
		t.Fatalf("DurableWriteBudget margin over the busy budget is %s; that leaves no time to execute the write after winning the lock", margin)
	}
	// A positive control on the arithmetic itself, so a zeroed or unit-confused
	// constant cannot satisfy the comparisons above.
	if busy != 15*time.Second {
		t.Fatalf("busy budget = %s, want 15s; if this changed deliberately, re-read the derivation in DurableWriteBudget's comment", busy)
	}
}
