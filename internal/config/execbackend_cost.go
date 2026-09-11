package config

import "fmt"

// ExecBackendCostConfig is the compute-dollar cap for cloud execution backends
// (#1540). It is deliberately SEPARATE from the model-token budget in
// OrchestratePolicy: that budget meters tokens billed by a model provider, this
// one meters compute billed by a sandbox provider, and one being generous must
// never imply the other is.
//
// UNSET DENIES. Every other budget in this codebase reads 0 as "unlimited"
// (OrchestratePolicy.MaxDelegationCostUSD, pinned as such by its own test, and
// admissionBudget's `> 0 &&` clauses). That default is safe for a counter and
// unsafe for a spender, so it is not mirrored: a deployment that has not decided
// a ceiling does not provision cloud compute at all.
type ExecBackendCostConfig struct {
	// MaxReservedUSD is the ceiling on summed worst-case reservations across all
	// billing attempts. Unset or <= 0 means cloud provision is DENIED.
	MaxReservedUSD float64
	// MaxConcurrent bounds billing attempts. Unset disables only this clause.
	MaxConcurrent int
	// PerAttemptUSD is the worst-case cost one attempt reserves before it runs.
	// Unset or <= 0 means DENIED: a zero reservation sums to zero however many
	// attempts run, so it would pass any dollar cap forever.
	PerAttemptUSD float64
}

// Validate reports why a configuration is malformed. It never silently
// downgrades to a permissive default.
func (c ExecBackendCostConfig) Validate() error {
	if c.MaxReservedUSD < 0 {
		return fmt.Errorf("[remote_exec].cost_max_reserved_usd must be non-negative, got %v: fix it in config.toml", c.MaxReservedUSD)
	}
	if c.PerAttemptUSD < 0 {
		return fmt.Errorf("[remote_exec].cost_per_attempt_usd must be non-negative, got %v: fix it in config.toml", c.PerAttemptUSD)
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("[remote_exec].cost_max_concurrent must be non-negative, got %d: fix it in config.toml", c.MaxConcurrent)
	}
	return nil
}

// Admits reports whether this configuration can admit any cloud provision, and
// why not when it cannot, so a caller can log the reason rather than discovering
// an unexplained refusal at the store.
func (c ExecBackendCostConfig) Admits() (bool, string) {
	if c.MaxReservedUSD <= 0 {
		return false, "no compute-dollar ceiling is set. Set [remote_exec].cost_max_reserved_usd in config.toml to a positive number of dollars. An unset ceiling denies rather than permitting unlimited spend."
	}
	if c.PerAttemptUSD <= 0 {
		return false, "no per-attempt reservation is set. Set [remote_exec].cost_per_attempt_usd in config.toml to the worst-case dollar cost of one sandbox. A zero reservation sums to zero however many attempts run, so it would pass any ceiling forever."
	}
	return true, ""
}
