package cli

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2191. A review's duration is a property of the PROMPT CLASS, not of the
// agent that happens to answer it. Before this, the deadline came from the
// agent - 10m, 30m, 45m and 2h all existed on this box - so rotating reviewers
// rotated the deadline. The first router-rotated review drew a 10-minute agent
// for a class whose observed maximum is 164 minutes and aborted after nine.
func TestReviewClassDeadlineFloorsAShortAgentTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute, // gm-review-devin-omp's real value
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	review := workflow.JobPayload{ReviewPurpose: "code"}

	got := resolveEffectiveJobTimeout(review, managed)
	if got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("review timeout = %s, want the class floor %s: a 10-minute agent still truncates a 164-minute class",
			got.Timeout, config.DefaultReviewJobTimeout)
	}
	if got.Source != "review class" {
		t.Fatalf("source = %q, want %q so the reason is legible in the clamp event", got.Source, "review class")
	}
}

// A FLOOR, not an assignment: an agent configured longer keeps its own value.
func TestReviewClassDeadlineDoesNotLowerALongerAgentTimeout(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        4 * time.Hour, // longer than the class floor
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     8 * time.Hour,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code"}, managed)
	if got.Timeout != 4*time.Hour {
		t.Fatalf("timeout = %s, want the agent's longer %s", got.Timeout, 4*time.Hour)
	}
}

// NON-REVIEW jobs are untouched. The floor is scoped to the class it measures;
// applying it everywhere would turn every hung job into a three-hour slot lease.
func TestReviewClassDeadlineLeavesNonReviewJobsAlone(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{}, managed)
	if got.Timeout != 10*time.Minute {
		t.Fatalf("non-review timeout = %s, want the agent's own %s", got.Timeout, 10*time.Minute)
	}
}

// The operator's ceiling still wins: the floor cannot raise a review above
// [daemon].job_timeout_max, and the clamp stays reported.
func TestReviewClassDeadlineStillObeysTheDaemonMaximum(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     30 * time.Minute,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewPurpose: "code"}, managed)
	if got.Timeout != 30*time.Minute || !got.Clamped {
		t.Fatalf("timeout = %s clamped=%v, want the daemon maximum enforced", got.Timeout, got.Clamped)
	}
}

// A pool-bearing review with no purpose is still a review: `agent review`
// dispatches carry the pool from the chokepoint and may leave purpose empty.
func TestReviewClassDeadlineRecognisesAPoolBearingReview(t *testing.T) {
	managed := managedJobRuntimeConfig{
		JobTimeout:        10 * time.Minute,
		JobTimeoutDefault: config.DefaultDaemonJobTimeoutDefault,
		JobTimeoutMax:     config.DefaultDaemonJobTimeoutMax,
		ReviewJobTimeout:  config.DefaultReviewJobTimeout,
	}
	got := resolveEffectiveJobTimeout(workflow.JobPayload{ReviewModelPool: []string{"devin/swe-2"}}, managed)
	if got.Timeout != config.DefaultReviewJobTimeout {
		t.Fatalf("pool-bearing review timeout = %s, want the class floor", got.Timeout)
	}
}

// Q6: the floor must be OPERATOR-SETTABLE, not a constant with a config knob
// that nothing reads. A mutant discarding the parsed value survived every test
// above, because they all construct managedJobRuntimeConfig by hand.
func TestReviewClassDeadlineComesFromTheOperatorsConfig(t *testing.T) {
	store, home := blockerE2EHome(t)
	paths := config.PathsForHome(home)
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n[review_router]\njob_timeout = \"90m\"\ncode = [\"sentinel/router-a\"]\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	worker := defaultJobWorker(store, io.Discard, home)
	managed, err := worker.managedJobConfig(context.Background(), "opus-reviewer")
	if err != nil {
		t.Fatalf("managedJobConfig: %v", err)
	}
	if managed.ReviewJobTimeout != 90*time.Minute {
		t.Fatalf("review class timeout = %s, want the configured 90m: the knob is documented but unread",
			managed.ReviewJobTimeout)
	}
}

// And an absent knob keeps the built-in floor rather than removing it.
func TestReviewClassDeadlineDefaultsWhenUnconfigured(t *testing.T) {
	store, home := blockerE2EHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	worker := defaultJobWorker(store, io.Discard, home)
	managed, err := worker.managedJobConfig(context.Background(), "opus-reviewer")
	if err != nil {
		t.Fatalf("managedJobConfig: %v", err)
	}
	if managed.ReviewJobTimeout != config.DefaultReviewJobTimeout {
		t.Fatalf("review class timeout = %s, want the built-in %s", managed.ReviewJobTimeout, config.DefaultReviewJobTimeout)
	}
}
