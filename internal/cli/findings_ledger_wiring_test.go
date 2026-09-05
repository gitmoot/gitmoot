package cli

import (
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
)

// #1850 round 3 F1, P1, ADOPTED AS A PERMANENT REGRESSION. The reviewer built
// this probe, proved the defect with it, and deleted it; it is the only executed
// evidence the class exists, so it lives here now.
//
// THE INVARIANT, NOT THE ASSIGNMENT. The #1822 ledger has two consumers that
// must agree on what is mandatory: the merge gate, which refuses a verdict
// naming an unobserved finding's uid, and the review brief, which is the only
// way a reviewer can learn that uid. When they disagree the merge wedges
// permanently.
//
// Round 2 wedged because the brief passed an empty scope. Round 3 wedged AGAIN
// through a narrower door: an engine field documented as "the SAME resolver the
// merge gate uses" that NOTHING EVER ASSIGNED, while the gate's copy was wired.
// A test asserting "engine.X != nil" would pin the assignment and miss the next
// variant, so this pins the property: whatever ledger resolvers the gate gets
// from production wiring, the engine must get too. Both now read the SAME
// LedgerResolvers value, so a divergence requires deleting a wiring line, and
// then this fails by name.
func TestLedgerResolversAreWiredOnBothConsumers(t *testing.T) {
	// The construction shape the existing engine test uses: a real checkout path
	// and a GhClient. No store is needed because only the wired seams matter.
	runner := &p2ProbeSubprocessRunner{}
	gh := &github.GhClient{MaxRetries: 1, Limiter: github.NewRateLimiter(github.RateLimiterConfig{})}
	checkout := t.TempDir()

	engine := daemonWorkflowEngineForRunner(nil, gh, checkout, "", runner, nil)
	gate := newDaemonPolicyMergeGateForRunner(nil, gh, checkout, runner)

	gateChanged := gate.LedgerResolvers.ChangedSince != nil
	gatePathExists := gate.LedgerResolvers.PathExistsAtHead != nil
	engineChanged := engine.LedgerResolvers.ChangedSince != nil
	enginePathExists := engine.LedgerResolvers.PathExistsAtHead != nil

	if gatePathExists != enginePathExists {
		t.Fatalf("locator-existence resolver wired on gate=%v but engine=%v; the gate can demand an obligation the brief cannot disclose, which wedges the merge permanently",
			gatePathExists, enginePathExists)
	}
	if gateChanged != engineChanged {
		t.Fatalf("changed-files resolver wired on gate=%v but engine=%v; the two consumers would compute different obligation sets",
			gateChanged, engineChanged)
	}
	// POSITIVE CONTROL: with a checkout present BOTH must be wired, so the
	// equality above cannot be satisfied by both sides being nil.
	if !gatePathExists || !gateChanged {
		t.Fatalf("with a checkout present the gate must hold both ledger resolvers, got changed=%v pathExists=%v; the equality would otherwise be vacuous",
			gateChanged, gatePathExists)
	}
}
