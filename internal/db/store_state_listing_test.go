package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Startup's two recovery reads take only their state's rows through an index
// (#2740): ListCancelledJobs through idx_jobs_terminal_state_updated_at, which
// state = 'cancelled' alone cannot use, and ListJobsByState("running") through
// idx_jobs_running_updated_at. Each returns exactly its state's jobs.
func TestStartupRecoveryReadsUseTheirStateIndex(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, state := range []string{"queued", "running", "blocked", "succeeded", "failed", "cancelled"} {
		for _, n := range []string{"2", "1"} {
			if err := store.CreateJob(ctx, Job{ID: state + "-" + n, Agent: "lead", Type: "ask", State: state, Payload: "{}"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if plan := explainQueryPlan(t, store, listCancelledJobsSQL); !strings.Contains(plan, "SEARCH jobs USING INDEX idx_jobs_terminal_state_updated_at (state=?)") {
		t.Fatalf("cancelled listing plan does not search idx_jobs_terminal_state_updated_at:\n%s", plan)
	}
	if plan := explainQueryPlan(t, store, listJobsByStateSQL, "running"); !strings.Contains(plan, "USING INDEX idx_jobs_running_updated_at") {
		t.Fatalf("running listing plan does not use idx_jobs_running_updated_at:\n%s", plan)
	}
	for state, list := range map[string]func(context.Context) ([]Job, error){
		"cancelled": store.ListCancelledJobs,
		"running":   func(ctx context.Context) ([]Job, error) { return store.ListJobsByState(ctx, "running") },
	} {
		jobs, err := list(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(jobs))
		for _, job := range jobs {
			ids = append(ids, job.ID)
		}
		if got, want := strings.Join(ids, ","), state+"-1,"+state+"-2"; got != want {
			t.Fatalf("%s listing = %s, want %s", state, got, want)
		}
	}
}
