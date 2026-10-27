//go:build linux

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

func enforcedInspect(t *testing.T, world producerCleanupFixture) CleanupInspection {
	t.Helper()
	d, err := world.engine.InspectProducerCleanup(context.Background(), world.jobID, CleanupPolicy{Mode: "ordinary", BudgetGate: CleanupBudgetGateEnforced})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return d
}

func refusalsFor(t *testing.T, world producerCleanupFixture) []db.CleanupRefusal {
	t.Helper()
	refusals, err := world.store.ListCleanupRefusals(context.Background(), db.CleanupObligationResourceID(world.jobID, world.path))
	if err != nil {
		t.Fatalf("list refusals: %v", err)
	}
	return refusals
}

func hasReason(d CleanupInspection, code string) bool {
	for _, r := range d.ReasonCodes {
		if r == code {
			return true
		}
	}
	return false
}

// #2360 v2, positive-cap success: the consumer decision carries the approved budget object and a
// complete measurement within it; apply removes the resource and its prepared receipt binds the
// enforced gate and the exact budget.
func TestBudgetGatedCleanupRemovesWithinApprovedCap(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	d := enforcedInspect(t, world)
	if !d.Eligible || d.SchemaVersion != 2 || d.BudgetGate != CleanupBudgetGateEnforced {
		t.Fatalf("decision = %+v", d)
	}
	if d.ApprovedSizeBudget == nil || d.ApprovedSizeBudget.LimitBytes != 1<<30 || d.ApprovedSizeBudget.Owner != "fixture-owner" {
		t.Fatalf("approved budget = %+v", d.ApprovedSizeBudget)
	}
	if d.SizeMeasurement == nil || !d.SizeMeasurement.Complete || d.SizeMeasurement.Bytes == nil || *d.SizeMeasurement.Bytes <= 0 {
		t.Fatalf("measurement = %+v", d.SizeMeasurement)
	}
	result, err := world.engine.ApplyProducerCleanup(context.Background(), world.jobID, d.ResourceID, d.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err != nil || result.State != "removed" {
		t.Fatalf("apply = %+v %v", result, err)
	}
	world.assertRemoved(t)
	var evidence cleanupEvidence
	if err := json.Unmarshal([]byte(result.Receipt.Evidence), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Policy.BudgetGate != CleanupBudgetGateEnforced || evidence.Policy.Budget == nil || evidence.Policy.Budget.LimitBytes != 1<<30 {
		t.Fatalf("receipt policy does not bind the enforced budget: %+v", evidence.Policy)
	}
	if n := len(refusalsFor(t, world)); n != 0 {
		t.Fatalf("a successful apply recorded %d refusals", n)
	}
}

// #2360 v2: no approved cap (null) and an over-budget tree both refuse, keep the resource, and a
// refused apply leaves a durable refusal receipt with the evidence.
func TestBudgetGatedCleanupRefusesUnknownAndOverBudget(t *testing.T) {
	t.Run("null budget", func(t *testing.T) {
		world := newProducerCleanupFixture(t, false)
		world.engine.CleanupBudget = func(string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error) { return nil, nil, nil }
		d := enforcedInspect(t, world)
		if d.Eligible || !hasReason(d, CleanupRefusalSizeBudgetUnknown) || d.ApprovedSizeBudget != nil || d.EligibilityToken != "" {
			t.Fatalf("null budget decision = %+v", d)
		}
		result, err := world.engine.ApplyProducerCleanup(context.Background(), world.jobID, d.ResourceID, "stale-token", CleanupPolicy{Mode: "ordinary"})
		if err != nil || result.State != "refused" || result.RefusalReceipt == nil {
			t.Fatalf("apply = %+v %v", result, err)
		}
		world.assertPreserved(t)
		refusals := refusalsFor(t, world)
		if len(refusals) != 1 || refusals[0].SubmittedToken != "stale-token" || refusals[0].BudgetGate != CleanupBudgetGateEnforced ||
			refusals[0].ApprovedSizeBudget != "null" || refusals[0].ReasonCodes[0] != CleanupRefusalSizeBudgetUnknown || refusals[0].SchemaVersion != 2 {
			t.Fatalf("refusal receipts = %+v", refusals)
		}
	})
	t.Run("config error is unknown", func(t *testing.T) {
		world := newProducerCleanupFixture(t, false)
		world.engine.CleanupBudget = func(string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error) {
			return nil, nil, errors.New("config unreadable")
		}
		if d := enforcedInspect(t, world); d.Eligible || !hasReason(d, CleanupRefusalSizeBudgetUnknown) {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("one block over", func(t *testing.T) {
		world := newProducerCleanupFixture(t, false)
		probe := enforcedInspect(t, world)
		if probe.SizeMeasurement == nil || probe.SizeMeasurement.Bytes == nil {
			t.Fatalf("probe = %+v", probe)
		}
		exact := *probe.SizeMeasurement.Bytes
		world.engine.CleanupBudget = fixtureCleanupBudget(exact - 512)
		d := enforcedInspect(t, world)
		if d.Eligible || !hasReason(d, CleanupRefusalSizeOverBudget) || d.SizeMeasurement == nil || *d.SizeMeasurement.Bytes != exact {
			t.Fatalf("over-budget decision = %+v", d)
		}
		world.engine.CleanupBudget = fixtureCleanupBudget(exact)
		if d := enforcedInspect(t, world); !d.Eligible {
			t.Fatalf("exact equality refused: %+v", d)
		}
	})
}

// #2360 v2: the measurement is not part of the token, so the FINAL gate re-measures immediately
// before removal: a tree that grew past the cap after admission is refused and kept.
func TestBudgetGatedCleanupFinalGateRemeasures(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	probe := enforcedInspect(t, world)
	world.engine.CleanupBudget = fixtureCleanupBudget(*probe.SizeMeasurement.Bytes + 64*1024)
	d := enforcedInspect(t, world)
	if !d.Eligible {
		t.Fatalf("admission = %+v", d)
	}
	if err := os.WriteFile(filepath.Join(world.path, "grew.bin"), []byte(strings.Repeat("g", 512*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := world.engine.ApplyProducerCleanup(context.Background(), world.jobID, d.ResourceID, d.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err != nil || result.State != "refused" || !hasReason(result.Decision, CleanupRefusalSizeOverBudget) {
		t.Fatalf("apply after growth = %+v %v", result, err)
	}
	world.assertPreserved(t)
	if _, err := os.Stat(filepath.Join(world.path, "grew.bin")); err != nil {
		t.Fatalf("grown file lost: %v", err)
	}
	if refusals := refusalsFor(t, world); len(refusals) != 1 || !strings.Contains(refusals[0].SizeMeasurement, `"complete":true`) {
		t.Fatalf("refusal evidence = %+v", refusals)
	}
}

// #2360 v2: a token binds the cap and its provenance; changing either between inspect and apply
// refuses without touching the resource.
func TestBudgetGatedCleanupTokenBindsBudgetPolicy(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	d := enforcedInspect(t, world)
	world.engine.CleanupBudget = func(string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error) {
		b, l, _ := fixtureCleanupBudget(1 << 30)("")
		b.Approval = "a-different-decision"
		return b, l, nil
	}
	result, err := world.engine.ApplyProducerCleanup(context.Background(), world.jobID, d.ResourceID, d.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err != nil || result.State != "refused" || !hasReason(result.Decision, "eligibility_changed") {
		t.Fatalf("apply after approval change = %+v %v", result, err)
	}
	world.assertPreserved(t)
}

// #2360 v2 (workspace-janitor 366497, option A): automatic reclaim is NOT budget-gated and its
// receipt says so; it never presents a null budget as approved.
func TestAutomaticReclaimIsNotBudgetGated(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	world.engine.CleanupBudget = nil
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(context.Background(), world.jobID)
	if err != nil || !removed {
		t.Fatalf("automatic reclaim removed=%t err=%v", removed, err)
	}
	world.assertRemoved(t)
	receipt, err := world.store.GetCleanupReceipt(context.Background(), db.CleanupObligationResourceID(world.jobID, world.path), 0)
	if err != nil {
		t.Fatal(err)
	}
	var evidence cleanupEvidence
	if err := json.Unmarshal([]byte(receipt.Evidence), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Policy.BudgetGate != CleanupBudgetGateNotApplicable || evidence.Policy.Budget != nil {
		t.Fatalf("automatic receipt policy = %+v, want budget_gate not_applicable and no budget", evidence.Policy)
	}
}

type failingRemoveGit struct{ gitutil.Client }

func (failingRemoveGit) RemoveWorktreeForce(context.Context, string) error {
	return errors.New("injected removal failure")
}

// #2360 v2: an unfinished budget-gated episode is replayed only under the policy it was prepared
// with; a cap change after preparation refuses with budget_policy_changed and deletes nothing.
func TestBudgetGatedEpisodeReplayRefusesChangedBudget(t *testing.T) {
	world := newProducerCleanupFixture(t, false)
	d := enforcedInspect(t, world)
	world.engine.DelegationWorktrees = failingRemoveGit{world.manager}
	result, err := world.engine.ApplyProducerCleanup(context.Background(), world.jobID, d.ResourceID, d.EligibilityToken, CleanupPolicy{Mode: "ordinary"})
	if err == nil || result.Receipt == nil {
		t.Fatalf("interrupted apply = %+v %v, want a prepared receipt and an error", result, err)
	}
	world.engine.DelegationWorktrees = world.manager
	world.engine.CleanupBudget = fixtureCleanupBudget(1 << 29)
	// Past the obligation's retry delay, so the replay is admissible on timing alone.
	later := time.Now().Add(2 * CleanupRetryDelay)
	world.engine.Now = func() time.Time { return later }
	again := enforcedInspect(t, world)
	if again.Eligible || !hasReason(again, CleanupRefusalBudgetPolicyChanged) {
		t.Fatalf("replay under a changed cap = %+v", again)
	}
	world.assertPreserved(t)
}
