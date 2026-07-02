package cli

import (
	"context"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// daemonOutcomeHarvester returns the best-effort Mode-A trace-harvester for this
// home, or nil when [skillopt].auto_trace_enabled is OFF (the default, or any
// config load failure — fail-safe to disabled so a malformed config never starts
// harvesting). When nil, the engine constructs no Outcome and calls no Harvest,
// so daemon behavior and every human-run TrainingPackage stay byte-identical
// (#465). It mirrors daemonEventSink's off-by-default admission gate.
//
// Unlike the webhook sink, the harvester owns no goroutine and holds only a store
// + a read-only GitHub status reader, so it is constructed per engine without
// caching — the gate read is the only cost, and that only matters once enabled.
func daemonOutcomeHarvester(store *db.Store, gh github.Client, home string) workflow.OutcomeHarvester {
	if store == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.Enabled() {
		return nil
	}
	return skillopt.NewOutcomeHarvester(store, gh)
}

// loadSkillOptPolicy resolves the [skillopt] policy for a home, fail-safe to the
// disabled default when the home or config cannot be resolved/parsed so the
// trace-harvester stays OFF rather than erroring the daemon (mirrors
// loadEventsPolicy / the #446 fail-safe-to-disabled pattern).
func loadSkillOptPolicy(home string) (config.SkillOptPolicy, error) {
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.DefaultSkillOptPolicy(), nil
	}
	return config.LoadSkillOptPolicy(config.Paths{ConfigFile: cfg})
}

// resolveRevertDetectionEnabled resolves the daemon's corrective revert-detection
// gate for a home (#467): true only when [skillopt].auto_trace_enabled AND the
// optional opt-out revert_detection_enabled (nil=on) both hold. It is FAIL-SAFE to
// false on any config-load error, so a malformed config never turns the (delayed,
// corrective) revert overwrites on — matching daemonOutcomeHarvester's
// fail-safe-to-disabled gate. With auto_trace off (the default) this is always
// false, so the daemon parses no PR bodies — byte-identical default.
func resolveRevertDetectionEnabled(home string) bool {
	policy, err := loadSkillOptPolicy(home)
	if err != nil {
		return false
	}
	return policy.RevertDetectionEnabled()
}

// canaryRoutingEnabled resolves the #484 canary ROUTING gate for a home: true only
// when [skillopt].auto_promote_canary AND a valid auto_promote_canary_sample are
// configured (config.SkillOptPolicy.CanaryEnabled()). It is the SAME gate the
// daemon's regression comparator (daemonOutcomeHarvesterWithCanary) uses, so the
// routing and comparator seams turn on and off together — disabling the knob (or
// unsetting the sample) and restarting stops BOTH sampled routing and
// graduate/rollback, so a stranded canary row can never keep serving traffic. It is
// FAIL-SAFE to false on any config-load error, mirroring the other gates. With
// canary off (the default) every Mailbox is constructed with CanaryEnabled=false,
// so routeCanary returns before its query and resolution is byte-identical.
func canaryRoutingEnabled(home string) bool {
	policy, err := loadSkillOptPolicy(home)
	if err != nil {
		return false
	}
	return policy.CanaryEnabled()
}

// daemonReviewLegDispatcher returns the best-effort cross-family review-leg
// dispatcher for this home (#469), or nil when the review knob is OFF — the
// default, or any config-load failure (fail-safe to disabled). ReviewEnabled()
// requires BOTH cross_family_review_enabled AND auto_trace_enabled, so the soft
// review row is only ever written inside an enabled auto-trace run. When nil, the
// engine constructs no review leg and writes no review row, so daemon behavior is
// byte-identical. It mirrors daemonOutcomeHarvester's off-by-default gate.
func daemonReviewLegDispatcher(store *db.Store, gh github.Client, checkout string, home string) workflow.ReviewLegDispatcher {
	if store == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.ReviewEnabled() {
		return nil
	}
	return &crossFamilyReviewDispatcher{
		store:        store,
		diff:         gh,
		buildAdapter: buildRuntimeAdapter,
		authed:       daemonAuthedRuntimes(checkout),
		checkout:     checkout,
	}
}

// daemonDeterministicCheckerDispatcher returns the best-effort OBJECTIVE
// deterministic-checker dispatcher for this home (#485), or nil when the checker
// knob is OFF — the default, or any config-load failure (fail-safe to disabled).
// DeterministicCheckersEnabled() requires BOTH deterministic_checkers_enabled AND
// auto_trace_enabled, so the objective row is only ever written inside an enabled
// auto-trace run. When nil, the engine constructs no checker leg and writes no
// checker row, so daemon behavior is byte-identical. It mirrors
// daemonReviewLegDispatcher's off-by-default gate. The resolved per-checker selector
// (defaulted to the safe diff_size-only set) is passed in so an operator can run
// only the always-available metric on a tool-less host.
func daemonDeterministicCheckerDispatcher(store *db.Store, gh github.Client, checkout string, home string) workflow.DeterministicCheckerDispatcher {
	if store == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.DeterministicCheckersEnabled() {
		return nil
	}
	return &deterministicCheckerDispatcher{
		store:    store,
		diff:     gh,
		runner:   subprocess.ExecRunner{},
		checkout: checkout,
		checkers: policy.ResolvedDeterministicCheckers(),
	}
}

// daemonHardVerifierDispatcher returns the best-effort deterministic HARD-verifier
// dispatcher for this home (#474), or nil when the tier is OFF — the default, or any
// config-load failure (fail-safe to disabled). HardVerifiersEnabled() requires BOTH
// hard_verifiers_enabled AND auto_trace_enabled AND at least one configured command,
// so the hard row is only ever written inside an enabled auto-trace run with real
// verifiers to run. When nil, the engine constructs no verifier leg and writes no
// hard row, so daemon behavior is byte-identical. It mirrors
// daemonDeterministicCheckerDispatcher's off-by-default gate. The sandbox is a fresh
// detached worktree off the daemon's checkout at the merged head (reusing gitmoot's
// worktree isolation, no external sandbox dep).
func daemonHardVerifierDispatcher(store *db.Store, checkout string, home string) workflow.HardVerifierDispatcher {
	if store == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.HardVerifiersEnabled() {
		return nil
	}
	return &hardVerifierDispatcher{
		store:  store,
		runner: subprocess.ExecRunner{},
		sandbox: worktreeSandboxProvisioner{
			base:   checkout,
			home:   home,
			runner: subprocess.ExecRunner{},
		},
		commands: policy.ResolvedHardVerifierCommands(),
	}
}

// daemonAuthedRuntimes probes which of the cross-family runtimes (codex/claude/
// kimi) are authed/available, best-effort, via each adapter's Health check with a
// synthetic read-only agent. A runtime that errors (not installed / not authed) is
// reported unavailable so the cross-family selector never materializes an
// ephemeral leg on a runtime that cannot run. It is the seam tests substitute.
func daemonAuthedRuntimes(checkout string) reviewAuthedRuntimes {
	return func(ctx context.Context) map[string]bool {
		authed := map[string]bool{}
		factory := newRuntimeFactory()
		for _, rt := range workflow.EphemeralRuntimes {
			adapter, err := factory.Adapter(rt)
			if err != nil {
				continue
			}
			probe := runtime.Agent{Name: "gitmoot-review-probe", Runtime: rt, AutonomyPolicy: runtime.AutonomyPolicyReadOnly}
			if err := adapter.Health(ctx, probe); err == nil {
				authed[rt] = true
			}
		}
		return authed
	}
}
