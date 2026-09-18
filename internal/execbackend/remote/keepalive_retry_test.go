package remote

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRenewWithinLeadRetriesAfterATransientFailure is the regression for the
// defect that made the keepalive reintroduce the failure it exists to prevent.
//
// Ticks are 50 minutes apart and the sandbox expires 60 minutes after the last
// successful renewal, so a SINGLE ignored failure left the next attempt 40
// minutes too late: a 3h review died at ~1h. The renewal must keep trying while
// the 10-minute lead still has room.
//
// MUTATION: make renewWithinLead return after its first SetTimeout call and this
// goes red with attempts = 1.
func TestRenewWithinLeadRetriesAfterATransientFailure(t *testing.T) {
	attempts := 0
	backend := &Backend{}
	backend.renewWithinLeadUsing(context.Background(), "sandbox-retry", time.Now().Add(3*time.Hour),
		func(context.Context, string, time.Duration) error {
			attempts++
			if attempts < 3 {
				return errors.New("transient provider blip")
			}
			return nil
		})

	if attempts != 3 {
		t.Fatalf("SetTimeout attempts = %d, want 3: one transient failure must not end the renewal", attempts)
	}
}

// TestRenewWithinLeadStopsWhenTheDeadlineHasPassed pins that the renewal is
// bounded by the REQUESTED TTL. An unbounded refresher turns a lost owner into
// an immortal billed sandbox, which is the failure #1539's reaper exists to
// prevent.
func TestRenewWithinLeadStopsWhenTheDeadlineHasPassed(t *testing.T) {
	attempts := 0
	backend := &Backend{}
	backend.renewWithinLeadUsing(context.Background(), "sandbox-expired", time.Now().Add(-time.Minute),
		func(context.Context, string, time.Duration) error { attempts++; return nil })

	if attempts != 0 {
		t.Fatalf("SetTimeout attempts = %d, want 0: the requested TTL has already elapsed", attempts)
	}
}

// TestRenewWithinLeadClampsToTheProviderCeiling pins that a renewal never asks
// for more than the provider accepts — the same ceiling that made every review
// fail to provision in the first place.
func TestRenewWithinLeadClampsToTheProviderCeiling(t *testing.T) {
	var asked time.Duration
	backend := &Backend{}
	backend.renewWithinLeadUsing(context.Background(), "sandbox-clamp", time.Now().Add(5*time.Hour),
		func(_ context.Context, _ string, ttl time.Duration) error { asked = ttl; return nil })

	if asked > ProviderMaxTTL {
		t.Fatalf("renewal asked for %s, want at most %s", asked, ProviderMaxTTL)
	}
}

// TestRenewWithinLeadStopsOnContextCancel pins that stopping the keepalive
// actually ends the retry loop, so a torn-down sandbox cannot be handed a fresh
// hour by a retry still in flight.
func TestRenewWithinLeadStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	backend := &Backend{}
	backend.renewWithinLeadUsing(ctx, "sandbox-cancel", time.Now().Add(3*time.Hour),
		func(context.Context, string, time.Duration) error {
			attempts++
			cancel()
			return errors.New("fail so the loop would otherwise retry")
		})

	if attempts != 1 {
		t.Fatalf("SetTimeout attempts = %d, want 1: a cancelled keepalive must stop retrying", attempts)
	}
}
