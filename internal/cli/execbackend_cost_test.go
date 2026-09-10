package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// TestExecBackendStoreCapFailsClosed pins that a configuration which cannot
// admit reaches the store as a DENY rather than as a permissive zero. The
// partial cases matter most: a deployment that sets a ceiling but forgets the
// per-attempt amount would otherwise reserve $0 per attempt and pass any dollar
// cap forever, which is the state on main today.
func TestExecBackendStoreCapFailsClosed(t *testing.T) {
	for name, cfg := range map[string]config.ExecBackendCostConfig{
		"absent":               {},
		"ceiling only":         {MaxReservedUSD: 100},
		"per-attempt only":     {PerAttemptUSD: 1},
		"negative ceiling":     {MaxReservedUSD: -1, PerAttemptUSD: 1},
		"negative per-attempt": {MaxReservedUSD: 100, PerAttemptUSD: -1},
	} {
		if got := execBackendStoreCap(cfg); got.Configured {
			t.Errorf("%s: cap reached the store as Configured=true (%+v), want a deny", name, got)
		}
	}
	full := config.ExecBackendCostConfig{MaxReservedUSD: 100, PerAttemptUSD: 0.5, MaxConcurrent: 4}
	got := execBackendStoreCap(full)
	want := db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 100, PerAttemptUSD: 0.5, MaxConcurrent: 4}
	if got != want {
		t.Fatalf("complete configuration converted to %+v, want %+v", got, want)
	}
}

// TestLedgeredBackendPersistsTheConfiguredReservation is the regression for the
// defect this issue names: the only production writer passed the literal 0, so
// every reservation reserved nothing and no dollar cap could ever bind.
//
// It drives the real backend and reads the PERSISTED ROW. An earlier version of
// this test asserted on the config converter instead and survived a mutant that
// restored the literal - a test named after a behaviour it never exercised.
func TestLedgeredBackendPersistsTheConfiguredReservation(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	var output bytes.Buffer
	inner := &ledgerTestBackend{}
	policy := execBackendStoreCap(config.ExecBackendCostConfig{MaxReservedUSD: 10, PerAttemptUSD: 2.5})
	backend, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, "fence", "boot", &output, policy)
	if err != nil {
		t.Fatal(err)
	}
	scope := execbackend.JobScope{JobID: "cost-job", Attempt: 1, TTL: time.Minute}
	if _, err := backend.Provision(context.Background(), scope); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	attempt := execBackendAttemptForTest(t, store, db.ExecBackendAttemptKey{JobID: "cost-job", Attempt: 1})
	if attempt.CostReservedUSD == nil {
		t.Fatal("no cost was reserved for a provisioned attempt")
	}
	if *attempt.CostReservedUSD != 2.5 {
		t.Fatalf("persisted cost_reserved_usd = %v, want 2.5 (a zero here means no dollar cap can ever bind)", *attempt.CostReservedUSD)
	}
}

// TestLedgeredBackendRefusesWhenTheCapIsFull proves the gate reaches the real
// provisioning path: the provider must never be called for a refused attempt.
func TestLedgeredBackendRefusesWhenTheCapIsFull(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	var output bytes.Buffer
	provisions := 0
	inner := &ledgerTestBackend{provision: func(execbackend.JobScope) (*execbackend.Instance, error) {
		provisions++
		return &execbackend.Instance{ID: "sbx"}, nil
	}}
	policy := execBackendStoreCap(config.ExecBackendCostConfig{MaxReservedUSD: 4, PerAttemptUSD: 2.5})
	backend, err := newLedgeredExecutionBackend(store, inner, e2bAttemptProvider, "fence", "boot", &output, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "first", Attempt: 1, TTL: time.Minute}); err != nil {
		t.Fatalf("first provision refused: %v", err)
	}
	if _, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "second", Attempt: 1, TTL: time.Minute}); err == nil {
		t.Fatal("the second $2.50 attempt was admitted against a $4 cap")
	}
	if provisions != 1 {
		t.Fatalf("provider called %d times, want 1: a refused attempt must not reach the provider", provisions)
	}
}

// TestUnsetCapIsTheShippingStateAndDeniesWithAnOperand pins the arm that
// actually runs in production. The owner decided (2026-09-10) to ship with NO
// budget value set, so "no cap configured" is the operating state rather than
// an error path, and each of the four ways a cap can be missing must deny AND
// say which key is missing and where to set it.
func TestUnsetCapIsTheShippingStateAndDeniesWithAnOperand(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	cases := map[string]struct {
		cfg      config.ExecBackendCostConfig
		wantKeys []string
	}{
		"absent entirely":     {config.ExecBackendCostConfig{}, []string{"cost_max_reserved_usd"}},
		"ceiling only":        {config.ExecBackendCostConfig{MaxReservedUSD: 10}, []string{"cost_per_attempt_usd"}},
		"per-attempt only":    {config.ExecBackendCostConfig{PerAttemptUSD: 1}, []string{"cost_max_reserved_usd"}},
		"explicitly zero":     {config.ExecBackendCostConfig{MaxReservedUSD: 0, PerAttemptUSD: 0}, []string{"cost_max_reserved_usd"}},
		"malformed, negative": {config.ExecBackendCostConfig{MaxReservedUSD: -5, PerAttemptUSD: 1}, []string{"max_reserved_usd"}},
	}
	for name, tc := range cases {
		policy := execBackendStoreCap(tc.cfg)
		if policy.Configured {
			t.Errorf("%s: reached the store as configured, want a deny", name)
			continue
		}
		err := store.ReserveExecBackendAttempt(context.Background(), db.ExecBackendAttemptReservation{
			ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "unset-" + name, Attempt: 1},
			Provider:              e2bAttemptProvider,
			DaemonFencingToken:    "fence",
			BootID:                "boot",
			TTLExpiresAt:          time.Now().Add(time.Minute),
		}, policy)
		if err == nil {
			t.Errorf("%s: provision admitted with no configured cap", name)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, "unlimited") && !strings.Contains(msg, "not mean unlimited") && !strings.Contains(msg, "rather than permitting unlimited") {
			t.Errorf("%s: refusal suggests unlimited: %q", name, msg)
		}
		for _, key := range tc.wantKeys {
			if !strings.Contains(msg, key) {
				t.Errorf("%s: refusal does not name the missing key %q: %q", name, key, msg)
			}
		}
		if !strings.Contains(msg, "config.toml") {
			t.Errorf("%s: refusal does not say where to set the cap: %q", name, msg)
		}
	}
}

// TestZeroDeniesExplicitlyRatherThanByFallthrough pins the value where this
// codebase's other budgets mean "unlimited". A future reader must find an
// explicit refusal for 0, not an accident of ordering.
func TestZeroDeniesExplicitlyRatherThanByFallthrough(t *testing.T) {
	store := openExecBackendLedgerTestStore(t)
	// Bypass the converter: this is the store contract for a hand-built policy
	// that claims to be configured while carrying zeros.
	err := store.ReserveExecBackendAttempt(context.Background(), db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: "explicit-zero", Attempt: 1},
		Provider:              e2bAttemptProvider,
		DaemonFencingToken:    "fence",
		BootID:                "boot",
		TTLExpiresAt:          time.Now().Add(time.Minute),
	}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 0, PerAttemptUSD: 0})
	if err == nil {
		t.Fatal("a configured cap of $0 admitted a provision")
	}
	if !strings.Contains(err.Error(), "does not mean unlimited") {
		t.Fatalf("the zero refusal does not distinguish itself from the house idiom: %v", err)
	}
}
