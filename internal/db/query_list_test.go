package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestListMethodsPreserveTheirEmptyResultShape pins the nil-versus-empty
// contract of every list method converted to queryList in #1759.
//
// This is the assertion the refactor actually needs. A nil slice marshals to
// JSON `null` and an empty one to `[]`, so flipping either silently changes what
// an HTTP client iterating the dashboard API receives - and no length or
// round-trip assertion can see the difference. #1752 shipped exactly that defect
// when a slice initialiser was dropped during a deletion, which is why the shape
// is pinned here rather than left to whichever form the helper happened to
// produce.
func TestListMethodsPreserveTheirEmptyResultShape(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "shape.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Every table below is empty, which is the only state in which the two
	// shapes are distinguishable.
	t.Run("ListRecycleOverdueEpisodes promises non-nil", func(t *testing.T) {
		got, err := store.ListRecycleOverdueEpisodes(ctx)
		if err != nil {
			t.Fatalf("ListRecycleOverdueEpisodes: %v", err)
		}
		if got == nil {
			t.Fatal("returned nil; this method promised a non-nil empty slice before #1759 (it marshals to JSON [] not null)")
		}
		if len(got) != 0 {
			t.Fatalf("len = %d, want 0", len(got))
		}
	})

	t.Run("ListResourceLocks promises nil", func(t *testing.T) {
		got, err := store.ListResourceLocks(ctx)
		if err != nil {
			t.Fatalf("ListResourceLocks: %v", err)
		}
		if got != nil {
			t.Fatalf("returned %#v; this method returned nil for no locks before #1759 and callers may rely on that", got)
		}
	})
	// Every method converted to queryList in #1759 is pinned here, in the shape
	// it promised BEFORE the refactor. The split was measured, not chosen: five
	// methods returned nil and six returned an empty slice, and both groups have
	// HTTP consumers, so neither shape could be standardised away.
	t.Run("nil-returning methods", func(t *testing.T) {
		if got, err := store.ListPipelines(ctx); err != nil || got != nil {
			t.Fatalf("ListPipelines = %#v, err %v; want nil slice", got, err)
		}
		if got, err := store.ListAllCockpitPanes(ctx); err != nil || got != nil {
			t.Fatalf("ListAllCockpitPanes = %#v, err %v; want nil slice", got, err)
		}
		if got, err := store.ListActivePipelineRuns(ctx); err != nil || got != nil {
			t.Fatalf("ListActivePipelineRuns = %#v, err %v; want nil slice", got, err)
		}
		if got, err := store.ListAgents(ctx); err != nil || got != nil {
			t.Fatalf("ListAgents = %#v, err %v; want nil slice", got, err)
		}
	})

	t.Run("non-nil-returning methods", func(t *testing.T) {
		if got, err := store.ListRepos(ctx); err != nil || got == nil {
			t.Fatalf("ListRepos = %#v, err %v; want non-nil empty slice (marshals to [] not null)", got, err)
		}
		if got, err := store.ListAgentInstances(ctx); err != nil || got == nil {
			t.Fatalf("ListAgentInstances = %#v, err %v; want non-nil empty slice", got, err)
		}
		if got, err := store.ListAgentTemplates(ctx); err != nil || got == nil {
			t.Fatalf("ListAgentTemplates = %#v, err %v; want non-nil empty slice", got, err)
		}
		if got, err := store.ListGoals(ctx); err != nil || got == nil {
			t.Fatalf("ListGoals = %#v, err %v; want non-nil empty slice", got, err)
		}
		// Served straight to the dashboard HTTP client, so null-vs-[] is visible
		// to a browser, not just to Go callers.
		if got, err := store.ListDashboardBlockedJobs(ctx); err != nil || got == nil {
			t.Fatalf("ListDashboardBlockedJobs = %#v, err %v; want non-nil empty slice", got, err)
		}
	})
}
