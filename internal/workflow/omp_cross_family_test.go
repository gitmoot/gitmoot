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

// TestPickCrossFamilyReviewerNeverPicksAnOmpSeatAsCrossFamily is the REVIEWER-side
// half of the exclusion above. The refusal in TestPickCrossFamilyReviewerRefusesOmpLoudly
// only covers omp as the IMPLEMENTER; step 1 of the picker walks every registered
// review-capable agent and returns the first whose family differs from the
// implementer's, with SelfFamily=false. omp advertises `review` and is a registered
// runtime, so without this exclusion a registered omp seat is returned as the
// "cross-family" reviewer of a claude/codex/kimi implement job — an omp run routed
// to Anthropic reviewing a claude implement, recorded by the merge gate and the
// auto-trace as genuine diversity. That is the same model reviewing itself, which is
// precisely what this file exists to prevent, arriving through the other door.
//
// Kills: removing the crossFamilyRotation filter from listReviewAgents.
func TestPickCrossFamilyReviewerNeverPicksAnOmpSeatAsCrossFamily(t *testing.T) {
	ompSeat := reviewAgent("aaa-omp-reviewer", runtime.OmpRuntime, "owner/repo")
	ompOnly := reviewListerGrant("owner/repo", ompSeat)

	t.Run("an omp seat is not a DIFFERENT family, it is an unknown one", func(t *testing.T) {
		// Nothing authed, so the omp seat is the only candidate anywhere. The honest
		// answer is "no reviewer" (ok=false, no review row), never "a cross-family one".
		reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), ompOnly, runtime.ClaudeRuntime, "owner/repo", nil)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if ok {
			t.Fatalf("picked %+v: an omp seat's provider is opaque, so it can never stand in as a different family", reviewer)
		}
	})

	t.Run("a mapped candidate in the SAME store is still picked", func(t *testing.T) {
		// Non-vacuity, and an ordering trap: the omp seat sorts FIRST by name, so an
		// unfiltered step 1 returns it before ever reaching the codex reviewer.
		store := reviewListerGrant("owner/repo", ompSeat, reviewAgent("zzz-codex-reviewer", runtime.CodexRuntime, "owner/repo"))
		reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.ClaudeRuntime, "owner/repo", nil)
		if err != nil || !ok {
			t.Fatalf("a registered codex reviewer must still be picked: ok=%v err=%v", ok, err)
		}
		if reviewer.RegisteredAgent != "zzz-codex-reviewer" || reviewer.SelfFamily {
			t.Fatalf("picked %+v, want the codex reviewer as a genuine cross-family pick", reviewer)
		}
	})

	t.Run("the ephemeral rotation wins over an omp seat", func(t *testing.T) {
		reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), ompOnly, runtime.ClaudeRuntime, "owner/repo",
			map[string]bool{runtime.CodexRuntime: true})
		if err != nil || !ok {
			t.Fatalf("expected the rotation's ephemeral codex leg: ok=%v err=%v", ok, err)
		}
		if reviewer.Runtime != runtime.CodexRuntime || reviewer.Ephemeral == nil || reviewer.RegisteredAgent != "" {
			t.Fatalf("picked %+v, want an ephemeral codex reviewer", reviewer)
		}
	})

	t.Run("the same-family fallback does not resurrect it", func(t *testing.T) {
		// REFINEMENT #1's fallback prefers a REGISTERED reviewer. The omp seat must not
		// be that reviewer either: it is dropped from the candidate set outright, so the
		// fallback materializes an ephemeral claude leg tagged SelfFamily instead.
		reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), ompOnly, runtime.ClaudeRuntime, "owner/repo",
			map[string]bool{runtime.ClaudeRuntime: true})
		if err != nil || !ok {
			t.Fatalf("expected the same-family fallback: ok=%v err=%v", ok, err)
		}
		if reviewer.RegisteredAgent != "" || reviewer.Runtime != runtime.ClaudeRuntime || !reviewer.SelfFamily {
			t.Fatalf("picked %+v, want an ephemeral claude reviewer tagged SelfFamily", reviewer)
		}
	})

	t.Run("no implementer family may be cross-reviewed by it", func(t *testing.T) {
		// Every family the rotation knows, including the kimi-cli alias: an omp seat is
		// never returned at all, and above all never with SelfFamily=false.
		for _, implementer := range []string{runtime.ClaudeRuntime, runtime.CodexRuntime, runtime.KimiRuntime, runtime.KimiCLIRuntime} {
			reviewer, _, err := PickCrossFamilyReviewer(context.Background(), ompOnly, implementer, "owner/repo", nil)
			if err != nil {
				t.Fatalf("implementer %q: error %v", implementer, err)
			}
			if reviewer.RegisteredAgent == ompSeat.Name {
				t.Fatalf("implementer %q: picked the omp seat %+v (SelfFamily=%v)", implementer, reviewer, reviewer.SelfFamily)
			}
		}
	})

	t.Run("a shell reviewer is deliberately NOT excluded", func(t *testing.T) {
		// The exclusion is keyed on unmappedFamilyIsRefusal, which excepts shell for the
		// reason stated there: it runs NO model, so a shell "reviewer" is a deterministic
		// script that cannot self-prefer — and it is the seam this repo's no-LLM review
		// E2Es are built on (internal/cli/cross_family_review_integration_test.go
		// registers exactly this shape and asserts a review row appears). Widening the
		// filter to "any candidate with no rotation entry" silently deletes those E2Es,
		// which is a different decision from #1428's and does not belong to it.
		store := reviewListerGrant("owner/repo", reviewAgent("shell-reviewer", runtime.ShellRuntime, "owner/repo"))
		reviewer, ok, err := PickCrossFamilyReviewer(context.Background(), store, runtime.CodexRuntime, "owner/repo", nil)
		if err != nil || !ok {
			t.Fatalf("a shell reviewer must still be selectable: ok=%v err=%v", ok, err)
		}
		if reviewer.RegisteredAgent != "shell-reviewer" {
			t.Fatalf("picked %+v, want the shell reviewer (the no-LLM E2E seam)", reviewer)
		}
	})

	t.Run("the excluded-reviewer set is exactly the loud-refusal set", func(t *testing.T) {
		// The two halves must stay keyed on ONE predicate. If a future runtime is
		// excluded as a reviewer but not refused as an implementer (or the reverse),
		// the family structure has two different answers to "can Gitmoot name this
		// family", and one of them is wrong.
		store := reviewListerGrant("owner/repo", reviewAgent("claude-reviewer", runtime.ClaudeRuntime, "owner/repo"))
		authed := map[string]bool{runtime.ClaudeRuntime: true}
		excludedNames := make([]string, 0, 1)
		for _, name := range runtime.SupportedRuntimes() {
			_, _, err := PickCrossFamilyReviewer(context.Background(), store, name, "owner/repo", authed)
			excluded := unmappedReviewerIsExcluded(name)
			if excluded != (err != nil) {
				t.Fatalf("runtime %q: excluded as reviewer = %v but refused as implementer = %v; the two halves must key off the same predicate",
					name, excluded, err != nil)
			}
			if excluded {
				excludedNames = append(excludedNames, name)
			}
		}
		// Non-vacuity: an exclusion that excludes nothing proves nothing.
		if len(excludedNames) != 1 || excludedNames[0] != runtime.OmpRuntime {
			t.Fatalf("excluded reviewer set = %v, want exactly [%s]", excludedNames, runtime.OmpRuntime)
		}
	})

	t.Run("the jury never draws it either", func(t *testing.T) {
		// Safe by construction (the jury matches candidates against
		// crossFamilyJuryUniverse), pinned so the reviewer-side filter is never
		// "fixed" by moving the exclusion into the jury path alone.
		jury, err := PickCrossFamilyJury(context.Background(), ompOnly, runtime.ClaudeRuntime, "owner/repo",
			map[string]bool{runtime.CodexRuntime: true, runtime.KimiRuntime: true}, 2)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(jury) != 2 {
			t.Fatalf("jury = %+v, want the two ephemeral non-claude families", jury)
		}
		for _, juror := range jury {
			if juror.RegisteredAgent == ompSeat.Name || juror.Runtime == runtime.OmpRuntime {
				t.Fatalf("jury %+v drew the omp seat", jury)
			}
		}
	})
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
