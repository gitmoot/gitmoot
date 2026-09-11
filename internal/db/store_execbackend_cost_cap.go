package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExecBackendBillingStates are the attempt states in which a provider resource
// is believed to exist and therefore to be COSTING MONEY. They are the
// population the compute-dollar cap sums over.
//
// orphaned IS billing and that is the load-bearing entry (#1540). An orphaned
// attempt is one whose sandbox was created and never confirmed destroyed, so it
// is the single class of spend nobody is managing. Excluding it would make the
// meter blind at exactly the moment spend is out of control, which is the
// fail-open direction this gate exists to close.
//
// destroyed is excluded because destruction is confirmed. failed is excluded
// because it is only reachable for an attempt that never obtained a handle: the
// ledger deliberately keeps a transport-failed provision RECOVERABLE rather than
// failing it, precisely so a possibly-live sandbox becomes orphaned instead.
var ExecBackendBillingStates = []string{
	ExecBackendAttemptStateReserved,
	ExecBackendAttemptStateProvisioning,
	ExecBackendAttemptStateRunning,
	ExecBackendAttemptStateCollecting,
	ExecBackendAttemptStateDestroying,
	ExecBackendAttemptStateOrphaned,
}

// ExecBackendNonBillingStates are the terminal states in which no provider
// resource is believed to exist. Every state constant MUST appear in exactly one
// of the two lists; TestExecBackendStatesAreClassified enforces that, so adding a
// state without deciding whether it bills fails the suite rather than silently
// leaving spend uncounted.
var ExecBackendNonBillingStates = []string{
	ExecBackendAttemptStateDestroyed,
	ExecBackendAttemptStateFailed,
}

// ExecBackendCostCap is the admission policy for one provision.
//
// THE ZERO VALUE DENIES. Configured=false means no cap has been configured and
// every cloud provision is refused, which is the inverse of this codebase's
// other budget idioms (config.MaxDelegationCostUSD and admissionBudget both read
// 0 as "unlimited", and orchestrate_test.go pins that). Mirroring that default
// here would mean an unconfigured deployment provisions without any ceiling, so
// the default is deliberately not mirrored.
type ExecBackendCostCap struct {
	Configured bool
	// MaxReservedUSD bounds the summed worst-case reservation of all billing
	// attempts. Must be > 0 when Configured.
	MaxReservedUSD float64
	// MaxConcurrent bounds the number of billing attempts. 0 disables only the
	// concurrency clause; it never disables the dollar clause.
	MaxConcurrent int
	// PerAttemptUSD is the worst-case dollar amount one attempt reserves before
	// the provider is called. Must be > 0 when Configured: a zero reservation
	// sums to zero however many attempts run, so it would pass any dollar cap
	// forever. That is precisely the state on main today, where the only
	// production writer passes the literal 0.
	PerAttemptUSD float64
	// DenyReason explains WHY this policy denies, in the words of whatever
	// produced it. The unset policy is the shipping state (owner decision,
	// 2026-09-10), so its refusal is the first thing an operator enabling cloud
	// will see: it must name the missing key and where to set it, not "denied".
	DenyReason string
}

// ExecBackendCapRefusal reports a refused admission AND THE OPERANDS THAT CAUSED
// IT. A cap that refuses without naming what filled it is indistinguishable from
// a correctly-full budget, and the two have opposite remedies: one waits, the
// other reconciles leaked sandboxes. Because reconciliation currently runs once
// per process (#1539), a phantom reservation can hold budget indefinitely, so the
// per-state counts and the oldest age are the difference between a one-query
// diagnosis and an evening.
type ExecBackendCapRefusal struct {
	Clause         string
	RequestUSD     float64
	ReservedUSD    float64
	MaxReservedUSD float64
	LiveCount      int
	MaxConcurrent  int
	ByState        map[string]int
	OldestState    string
	OldestAge      time.Duration
	OperandsErr    error
	DenyReason     string
}

func (r *ExecBackendCapRefusal) Error() string {
	var b strings.Builder
	switch r.Clause {
	case "unconfigured":
		if strings.TrimSpace(r.DenyReason) != "" {
			return "cloud provision denied: " + r.DenyReason
		}
		return "cloud provision denied: no execution backend cost cap is configured. Set [remote_exec].cost_max_reserved_usd and [remote_exec].cost_per_attempt_usd in config.toml. An unset cap denies rather than permitting unlimited spend."
	case "concurrency":
		fmt.Fprintf(&b, "execution backend concurrency cap refused this provision: %d billing attempts, cap %d",
			r.LiveCount, r.MaxConcurrent)
	default:
		fmt.Fprintf(&b, "execution backend cost cap refused this provision: reserving $%.4f against $%.4f already reserved, cap $%.4f",
			r.RequestUSD, r.ReservedUSD, r.MaxReservedUSD)
	}
	if r.OperandsErr != nil {
		fmt.Fprintf(&b, "; operands unavailable: %v", r.OperandsErr)
		return b.String()
	}
	if len(r.ByState) > 0 {
		states := make([]string, 0, len(r.ByState))
		for s := range r.ByState {
			states = append(states, s)
		}
		sort.Strings(states)
		parts := make([]string, 0, len(states))
		for _, s := range states {
			parts = append(parts, fmt.Sprintf("%s %d", s, r.ByState[s]))
		}
		fmt.Fprintf(&b, "; holding it: %s", strings.Join(parts, ", "))
	}
	if r.OldestState != "" {
		fmt.Fprintf(&b, "; oldest %s is %s old", r.OldestState, r.OldestAge.Round(time.Second))
	}
	if n := r.ByState[ExecBackendAttemptStateOrphaned]; n > 0 {
		fmt.Fprintf(&b, "; %d ORPHANED attempt(s) are holding budget - reconcile before raising the cap", n)
	}
	return b.String()
}

// IsExecBackendCapRefusal reports whether err is a cap refusal.
func IsExecBackendCapRefusal(err error) bool {
	var refusal *ExecBackendCapRefusal
	return errors.As(err, &refusal)
}

func billingStatePlaceholders() (string, []any) {
	marks := make([]string, len(ExecBackendBillingStates))
	args := make([]any, len(ExecBackendBillingStates))
	for i, s := range ExecBackendBillingStates {
		marks[i] = "?"
		args[i] = s
	}
	return strings.Join(marks, ","), args
}

// describeBillingLoad reads the operands for a refusal message. It is diagnostic
// only: it runs AFTER the admission has already been refused, so its failure
// degrades the message and never converts a refusal into an admission.
func (s *Store) describeBillingLoad(ctx context.Context, refusal *ExecBackendCapRefusal) {
	marks, args := billingStatePlaceholders()
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*), MIN(created_at)
		FROM execbackend_attempts WHERE state IN (`+marks+`) GROUP BY state`, args...)
	if err != nil {
		refusal.OperandsErr = err
		return
	}
	defer rows.Close()
	refusal.ByState = map[string]int{}
	oldest := time.Time{}
	for rows.Next() {
		var state, created string
		var count int
		if err := rows.Scan(&state, &count, &created); err != nil {
			refusal.OperandsErr = err
			return
		}
		refusal.ByState[state] = count
		if t, perr := parseExecBackendTime(created); perr == nil {
			if oldest.IsZero() || t.Before(oldest) {
				oldest, refusal.OldestState = t, state
			}
		}
	}
	if err := rows.Err(); err != nil {
		refusal.OperandsErr = err
		return
	}
	if !oldest.IsZero() {
		refusal.OldestAge = time.Since(oldest)
	}
}

func parseExecBackendTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsable timestamp %q", value)
}
