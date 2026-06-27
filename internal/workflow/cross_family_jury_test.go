package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestPickCrossFamilyJuryDistinctFamilies: with all three families available, a
// codex implementer's jury is the two OTHER families (claude + codex's rotation),
// each DISTINCT and never the implementer's own family.
func TestPickCrossFamilyJuryDistinctFamilies(t *testing.T) {
	store := stubAgentLister{}
	authed := map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true, runtime.CodexRuntime: true}
	jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed, 3)
	if err != nil {
		t.Fatalf("PickCrossFamilyJury error: %v", err)
	}
	// codex implementer -> at most 2 cross-family judges (claude, kimi).
	if len(jury) != 2 {
		t.Fatalf("jury size = %d, want 2 (claude + kimi; never codex)", len(jury))
	}
	seen := map[string]bool{}
	for _, j := range jury {
		if j.Runtime == runtime.CodexRuntime {
			t.Fatalf("jury included the implementer's own family (codex): %+v", j)
		}
		if seen[j.Runtime] {
			t.Fatalf("jury contains a duplicate family %q (must be deduped)", j.Runtime)
		}
		seen[j.Runtime] = true
		if j.SelfFamily {
			t.Fatalf("a cross-family juror must not be self-family: %+v", j)
		}
	}
	if !seen[runtime.ClaudeRuntime] || !seen[runtime.KimiRuntime] {
		t.Fatalf("jury families = %v, want claude+kimi", seen)
	}
}

// TestPickCrossFamilyJuryDedupesNeverPads: even with size 5 and many registered
// same-family agents, the jury never exceeds the count of DISTINCT cross-families
// (diversity over headcount — no padding with near-identical families).
func TestPickCrossFamilyJuryDedupesNeverPads(t *testing.T) {
	store := reviewListerGrant("owner/repo",
		reviewAgent("claude-1", runtime.ClaudeRuntime, "owner/repo"),
		reviewAgent("claude-2", runtime.ClaudeRuntime, "owner/repo"),
		reviewAgent("kimi-1", runtime.KimiRuntime, "owner/repo"),
	)
	jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", nil, 5)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(jury) != 2 {
		t.Fatalf("jury size = %d, want 2 distinct families (claude once, kimi once) despite size 5", len(jury))
	}
	families := map[string]int{}
	for _, j := range jury {
		families[j.Runtime]++
	}
	if families[runtime.ClaudeRuntime] != 1 || families[runtime.KimiRuntime] != 1 {
		t.Fatalf("expected exactly one juror per distinct family, got %v", families)
	}
}

// TestPickCrossFamilyJuryGracefulDegradation: when only ONE other family is
// available, the picker returns that one (size 1) — the CALLER then falls back to
// the single-judge path (a jury of one is just the single judge). With NO other
// family available it returns empty.
func TestPickCrossFamilyJuryGracefulDegradation(t *testing.T) {
	t.Run("one family available returns one", func(t *testing.T) {
		store := stubAgentLister{}
		authed := map[string]bool{runtime.ClaudeRuntime: true} // only claude
		jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed, 3)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(jury) != 1 || jury[0].Runtime != runtime.ClaudeRuntime {
			t.Fatalf("jury = %+v, want exactly one claude juror (caller falls back to single judge)", jury)
		}
	})

	t.Run("no cross-family available returns empty", func(t *testing.T) {
		store := stubAgentLister{}
		// Only the implementer's own family authed -> no cross-family juror.
		authed := map[string]bool{runtime.CodexRuntime: true}
		jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed, 3)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		if len(jury) != 0 {
			t.Fatalf("jury = %+v, want empty (no cross-family family available)", jury)
		}
	})
}

// TestPickCrossFamilyJurySizeBelowTwoIsOff: size < 2 is the OFF signal — the
// picker returns nil so the caller takes the byte-identical single-judge path,
// even when many distinct families are available.
func TestPickCrossFamilyJurySizeBelowTwoIsOff(t *testing.T) {
	store := stubAgentLister{}
	authed := map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true}
	for _, size := range []int{0, 1} {
		jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed, size)
		if err != nil {
			t.Fatalf("size %d: error: %v", size, err)
		}
		if jury != nil {
			t.Fatalf("size %d: jury = %+v, want nil (jury off, single-judge path)", size, jury)
		}
	}
}

// TestPickCrossFamilyJuryUnknownImplementerSkips: an unknown implementer family
// yields nil (skip — never a possibly-same-family jury).
func TestPickCrossFamilyJuryUnknownImplementerSkips(t *testing.T) {
	store := stubAgentLister{}
	authed := map[string]bool{runtime.ClaudeRuntime: true, runtime.KimiRuntime: true}
	jury, err := PickCrossFamilyJury(context.Background(), store, "mystery-runtime", "owner/repo", authed, 3)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if jury != nil {
		t.Fatalf("jury = %+v, want nil for an unknown implementer family", jury)
	}
}

// TestPickCrossFamilyJuryPrefersRegisteredOverEphemeral: a registered
// different-family agent is preferred for its family; the remaining family with no
// registered agent but authed falls back to an ephemeral read-only leg.
func TestPickCrossFamilyJuryPrefersRegisteredOverEphemeral(t *testing.T) {
	store := reviewListerGrant("owner/repo",
		reviewAgent("claude-rev", runtime.ClaudeRuntime, "owner/repo"),
	)
	authed := map[string]bool{runtime.KimiRuntime: true} // kimi only via ephemeral
	jury, err := PickCrossFamilyJury(context.Background(), store, runtime.CodexRuntime, "owner/repo", authed, 3)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(jury) != 2 {
		t.Fatalf("jury size = %d, want 2 (claude registered + kimi ephemeral)", len(jury))
	}
	byFamily := map[string]CrossFamilyReviewer{}
	for _, j := range jury {
		byFamily[j.Runtime] = j
	}
	if byFamily[runtime.ClaudeRuntime].RegisteredAgent != "claude-rev" {
		t.Fatalf("claude juror = %+v, want the registered claude-rev", byFamily[runtime.ClaudeRuntime])
	}
	if byFamily[runtime.KimiRuntime].Ephemeral == nil {
		t.Fatalf("kimi juror = %+v, want an ephemeral leg", byFamily[runtime.KimiRuntime])
	}
}
