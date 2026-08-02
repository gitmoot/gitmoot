package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestOmpIsNotInCrossFamilyUniverse is the tripwire for the ONE registration
// decision a future "add omp everywhere" cleanup PR is most likely to undo (#1428).
//
// omp is a multi-provider ROUTING harness: the provider that actually answered a
// turn is resolved per run from a profile and is opaque to Gitmoot. Scoring it as a
// model FAMILY would therefore manufacture diversity the merge gate trusts — an omp
// run routed to Anthropic "cross-family reviewing" a claude implement is the same
// model reviewing itself, wearing a different name. So omp is a DISPATCHABLE runtime
// that joins NO family structure:
//
//   - crossFamilyRotation — no ephemeral different-family target,
//   - crossFamilyJuryUniverse — never drawn as a distinct jury family,
//   - EphemeralRuntimes — delegations cannot mint an omp ephemeral worker (this one
//     is also why `go generate` must produce NO diff: EphemeralRuntimes is rendered
//     verbatim into internal/prompts/contract_generated.go).
//
// The correct fix is a per-seat provider declaration, not a guess; until then the
// honest behavior is the loud refusal asserted below.
func TestOmpIsNotInCrossFamilyUniverse(t *testing.T) {
	// omp IS registered — without this the rest of the test would pass vacuously on a
	// tree where the runtime was never added at all.
	if !containsRuntimeName(runtime.SupportedRuntimes(), runtime.OmpRuntime) {
		t.Fatalf("omp must be a supported runtime: %v", runtime.SupportedRuntimes())
	}

	if target, ok := crossFamilyRotation[runtime.OmpRuntime]; ok {
		t.Fatalf("omp must NOT be in crossFamilyRotation (found target %q): its resolved provider is opaque, so a rotation entry manufactures false diversity", target)
	}
	// The rotation must not TARGET omp either: an ephemeral omp reviewer would be a
	// reviewer of unknown provenance standing in for a known family.
	for family, target := range crossFamilyRotation {
		if strings.EqualFold(target, runtime.OmpRuntime) {
			t.Fatalf("crossFamilyRotation[%q] targets omp; an omp reviewer's family is unknowable", family)
		}
	}
	if containsRuntimeName(crossFamilyJuryUniverse, runtime.OmpRuntime) {
		t.Fatalf("omp must NOT be in crossFamilyJuryUniverse: %v", crossFamilyJuryUniverse)
	}
	if containsRuntimeName(EphemeralRuntimes, runtime.OmpRuntime) {
		t.Fatalf("omp must NOT be in EphemeralRuntimes (%v): adding it also moves the generated gitmoot_result contract, which go generate + git diff --exit-code would catch", EphemeralRuntimes)
	}
	// reviewerFamily must not quietly alias omp onto a real family either — that
	// would be the same false diversity by a different door.
	if got := reviewerFamily(runtime.OmpRuntime); got != runtime.OmpRuntime {
		t.Fatalf("reviewerFamily(omp) = %q, want %q: aliasing omp onto another family is exactly the false diversity this pins against", got, runtime.OmpRuntime)
	}
}

// TestPickCrossFamilyReviewerRefusesOmpLoudly proves the consequence of the
// exclusion above is an HONEST REFUSAL, not silence. Before #1428 an unmapped
// family returned (zero, false, nil): no review row, no error, no event — byte
// identical to "no reviewer was authed", so an unreviewable runtime was invisible.
// omp now returns an ERROR naming the runtime, which the engine records as a
// cross_family_review_failed job event (engine_routing_merge.go runReviewLeg) and
// skillopt's judge logs. No review row is written either way; only the visibility
// changes.
func TestPickCrossFamilyReviewerRefusesOmpLoudly(t *testing.T) {
	store := reviewListerGrant("owner/repo", reviewAgent("claude-reviewer", runtime.ClaudeRuntime, "owner/repo"))
	reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.OmpRuntime, "owner/repo", map[string]bool{runtime.ClaudeRuntime: true})
	if err == nil {
		t.Fatal("an omp implementer must REFUSE loudly, not silently yield no review row")
	}
	if ok {
		t.Fatalf("refusal must still yield ok=false (no review row), got reviewer %+v", reviewer)
	}
	if !strings.Contains(err.Error(), runtime.OmpRuntime) {
		t.Fatalf("refusal %q must name the runtime so the job event is actionable", err.Error())
	}
}

// TestPickCrossFamilyReviewerUnrecoverableFamilyStaysSilent pins the OTHER half of
// the split: the pre-existing SKIP-not-guess cases must NOT become noisy. An empty
// runtime (a deleted/migrated agent whose runtime could not be read back) and a name
// Gitmoot cannot dispatch at all are absences, not gaps — they stay (false, nil),
// byte-identical to before #1428.
// A case-variant of a MAPPED runtime is pinned here too ("Codex"): reviewerFamily
// does not lowercase and crossFamilyRotation is an exact map index, so "Codex"
// misses the rotation. It must reach the SILENT branch, exactly as it did before
// #1428 — a case-insensitive refusal predicate would instead emit an event
// asserting codex has no family mapping, which is simply false.
func TestPickCrossFamilyReviewerUnrecoverableFamilyStaysSilent(t *testing.T) {
	store := reviewListerGrant("owner/repo", reviewAgent("claude-reviewer", runtime.ClaudeRuntime, "owner/repo"))
	for _, implementer := range []string{"", "   ", "mystery-runtime", "Codex", "CLAUDE", "Kimi"} {
		_, ok, err := PickCrossFamilyReviewer(context.Background(), store, implementer, "owner/repo", map[string]bool{runtime.ClaudeRuntime: true})
		if err != nil {
			t.Fatalf("implementer %q: unrecoverable family must stay a silent skip, got error %v", implementer, err)
		}
		if ok {
			t.Fatalf("implementer %q: unrecoverable family must SKIP, never guess a reviewer", implementer)
		}
	}
	// shell is dispatchable but runs no model, so it has no family by construction —
	// "no cross-family reviewer" is its definition, not a gap. It must stay silent or
	// every no-LLM shell E2E that merges would start emitting refusal events.
	if _, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.ShellRuntime, "owner/repo", map[string]bool{runtime.ClaudeRuntime: true}); err != nil || ok {
		t.Fatalf("shell implementer must stay a silent skip, got ok=%v err=%v", ok, err)
	}
}

// TestCrossFamilyRefusalKeysOffDispatchabilityNotTheNameOmp pins the GENERALIZATION
// the loud branch actually has: it refuses for ANY dispatchable runtime with no
// crossFamilyRotation entry (shell excepted, which runs no model), not for the
// literal name "omp". Today that set is exactly {omp}, so the merged behavior delta
// is omp-only — but the next runtime registered without a rotation entry inherits
// refusal events on every merged implement job it produces, and this test is where
// that consequence is written down. A future runtime author who does not want it
// must declare a family, not delete the branch.
func TestCrossFamilyRefusalKeysOffDispatchabilityNotTheNameOmp(t *testing.T) {
	store := reviewListerGrant("owner/repo", reviewAgent("claude-reviewer", runtime.ClaudeRuntime, "owner/repo"))
	authed := map[string]bool{runtime.ClaudeRuntime: true}
	refused := make([]string, 0, 1)
	for _, name := range runtime.SupportedRuntimes() {
		_, known := crossFamilyRotation[reviewerFamily(name)]
		_, _, err := PickCrossFamilyReviewer(context.Background(), store, name, "owner/repo", authed)
		switch {
		case name == runtime.ShellRuntime || known:
			if err != nil {
				t.Fatalf("runtime %q has a family (or is shell) and must not refuse: %v", name, err)
			}
		default:
			if err == nil {
				t.Fatalf("runtime %q is dispatchable with no crossFamilyRotation entry and must refuse LOUDLY, not skip silently", name)
			}
			refused = append(refused, name)
		}
	}
	// Non-vacuity: at least one runtime must exercise the loud branch, and today the
	// set is exactly omp. If this fails because another runtime joined the set, that
	// runtime now emits cross_family_review_failed events — declare its family or
	// record the decision, do not just widen this list.
	if len(refused) != 1 || refused[0] != runtime.OmpRuntime {
		t.Fatalf("loud-refusal set = %v, want exactly [%s]", refused, runtime.OmpRuntime)
	}
}

func containsRuntimeName(names []string, want string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), want) {
			return true
		}
	}
	return false
}
