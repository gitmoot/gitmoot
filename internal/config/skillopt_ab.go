package config

import "fmt"

// DefaultLiveABBanditMinSamples is the conservative traffic floor live-traffic
// A/B (#482) falls back to when the [skillopt] section sets live_ab_sample_rate
// but leaves bandit_min_samples unset: an agent's champion bandit arm must have
// accrued at least this many pulls before interception fires. It keeps low-
// traffic / bespoke agents (e.g. the researcher) from being auto-A/B'd until
// they have a meaningful sample size. It reuses the same documented default as
// the existing DefaultBanditMinSamples so the two never diverge.
const DefaultLiveABBanditMinSamples = DefaultBanditMinSamples

// SkillOptABPolicy is the resolved, off-by-default knob set for live-traffic A/B
// interception (#482), projected from the shared [skillopt] SkillOptPolicy so
// there is ONE parser for the section and no duplicate bandit_min_samples key.
// Every existing home (and the DefaultConfig in init.go, which emits NO
// [skillopt] section) resolves LiveABSampleRate=0.0 → the interceptor is a no-op
// → the foreground `agent ask` path stays byte-identical.
type SkillOptABPolicy struct {
	// LiveABSampleRate is the probability in [0,1] that a single foreground ask
	// (on a managed agent above the traffic floor) is intercepted into a champion-
	// vs-challenger A/B. 0.0 (the default, and what an absent/unset knob resolves
	// to) means NEVER intercept.
	LiveABSampleRate float64
	// BanditMinSamples is the traffic floor: the champion arm's bandit pull count
	// must be >= this before any interception. It reuses [skillopt].bandit_min_
	// samples (the same #473 tiering signal) so only high-traffic agents auto-A/B.
	BanditMinSamples int
}

// DefaultSkillOptABPolicy returns the off-by-default resolved policy: rate 0.0
// (never intercept) and the conservative min-samples floor. A config file
// without a [skillopt] section resolves to exactly this.
func DefaultSkillOptABPolicy() SkillOptABPolicy {
	return SkillOptABPolicy{
		LiveABSampleRate: 0.0,
		BanditMinSamples: DefaultLiveABBanditMinSamples,
	}
}

// LoadSkillOptABPolicy resolves the live-traffic A/B knobs from the shared
// [skillopt] section by REUSING LoadSkillOptPolicy (no second scan of the file,
// no duplicate parser). When live_ab_sample_rate is absent/unset it resolves
// LiveABSampleRate=0.0 → the interceptor never fires. bandit_min_samples, when
// unset, falls back to the conservative default floor. The [0,1] / >=1 bounds
// are enforced here so `gitmoot config set` (via validateConfigFile) rejects an
// out-of-range knob.
func LoadSkillOptABPolicy(paths Paths) (SkillOptABPolicy, error) {
	policy, err := LoadSkillOptPolicy(paths)
	if err != nil {
		return SkillOptABPolicy{}, err
	}
	resolved := DefaultSkillOptABPolicy()
	if policy.LiveABSampleRate != nil {
		resolved.LiveABSampleRate = *policy.LiveABSampleRate
	}
	if policy.BanditMinSamples != nil {
		resolved.BanditMinSamples = *policy.BanditMinSamples
	}
	if err := validateSkillOptABPolicy(resolved); err != nil {
		return SkillOptABPolicy{}, err
	}
	return resolved, nil
}

func validateSkillOptABPolicy(policy SkillOptABPolicy) error {
	if policy.LiveABSampleRate < 0 || policy.LiveABSampleRate > 1 {
		return fmt.Errorf("skillopt.live_ab_sample_rate must be in [0, 1], got %v", policy.LiveABSampleRate)
	}
	if policy.BanditMinSamples < 1 {
		return fmt.Errorf("skillopt.bandit_min_samples must be >= 1, got %d", policy.BanditMinSamples)
	}
	return nil
}
