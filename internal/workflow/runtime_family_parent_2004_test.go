package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2004: temp and ephemeral agents are deliberately absent from the agent
// registry, so ResolveRuntimeFamily could not name a family for them and the
// merge gate's independence check had to fall through to a name comparison.
// Measured on review and implement jobs since 2026-08-25: 119 of 1,903 resolved
// to no family, 86 of them temp-shaped and 16 ephemeral-shaped.
//
// The two shapes recover differently and that is the point of testing both.
// A temp name CARRIES its parent agent as a prefix. An ephemeral name does not
// carry its parent at all - ephemeralAgentName hashes the parent job id - so it
// recovers through the job row's parent_job_id column instead. #2004 as filed
// claimed both were name-recoverable; only one is.
func TestRuntimeFamilyResolvesATempAgentThroughItsNamedParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")

	// The live shape, verbatim from daemon_worker.go: base + "-temp-" + job.
	// The parent JOB id also contains dashes and the agent's own name, which is
	// why the split must take the FIRST infix.
	const tempAgent = "g7-review-temp-local-review-g7-review-18d0d354"
	insertCompletedJob(t, store, db.Job{ID: "job-temp", Agent: tempAgent, Type: "review"}, JobPayload{})

	family, ok, err := ResolveRuntimeFamily(ctx, store, "job-temp", tempAgent, "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if !ok || family != "codex" {
		t.Fatalf("temp agent resolved to (%q, %v), want (\"codex\", true); the parent agent is the name's own prefix", family, ok)
	}
}

func TestRuntimeFamilyResolvesAnEphemeralAgentThroughItsParentJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "coordinator", "claude", "sonnet")

	insertCompletedJob(t, store, db.Job{ID: "job-parent", Agent: "coordinator", Type: "review"}, JobPayload{})
	// ephemeralAgentName(delegationID, parentJobID) - the parent job survives only
	// as a hash, so nothing in this string identifies job-parent.
	ephemeral := ephemeralAgentName("lens-db-path", "job-parent")
	insertCompletedJob(t, store, db.Job{ID: "job-lens", Agent: ephemeral, Type: "review", ParentJobID: "job-parent"}, JobPayload{})

	family, ok, err := ResolveRuntimeFamily(ctx, store, "job-lens", ephemeral, "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if !ok || family != "claude" {
		t.Fatalf("ephemeral agent resolved to (%q, %v), want (\"claude\", true)", family, ok)
	}
}

// THE CONSUMER GUARD (#2004 acceptance 2). selectNativeReviewFamily asks about a
// configured reviewer NAME with no job id, and treats an unresolved family as a
// drop. If an ephemeral name resolved without a job, that call would start
// admitting reviewers it drops today. It cannot: the parent lives in the job row
// this call does not have.
//
// Measured alongside: of the 49 distinct reviewer names ever recorded in job
// payloads on this host, 0 are temp-shaped and 1 is ephemeral-shaped, so the
// fanout consumer sees no change either in principle or in this population.
func TestEphemeralAgentDoesNotResolveWithoutItsJob(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "coordinator", "claude", "sonnet")
	insertCompletedJob(t, store, db.Job{ID: "job-parent", Agent: "coordinator", Type: "review"}, JobPayload{})
	ephemeral := ephemeralAgentName("lens-db-path", "job-parent")
	insertCompletedJob(t, store, db.Job{ID: "job-lens", Agent: ephemeral, Type: "review", ParentJobID: "job-parent"}, JobPayload{})

	family, ok, err := ResolveRuntimeFamily(ctx, store, "", ephemeral, "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if ok {
		t.Fatalf("ephemeral name resolved to %q with no job id; the fanout consumer would change behaviour", family)
	}
}

// The control. Parent recovery must not become "everything resolves": the
// remaining residue is 11 rows with no agent and 6 naming an agent that is not
// registered, and neither has a parent to recover. Without this arm the two
// tests above are satisfied by a resolver that invents a family.
func TestUnregisteredPlainNameStillDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "job-plain", Agent: "gm-omp-nag", Type: "implement"}, JobPayload{})

	family, ok, err := ResolveRuntimeFamily(ctx, store, "job-plain", "gm-omp-nag", "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if ok {
		t.Fatalf("unregistered plain name resolved to %q; parent recovery must not invent a family", family)
	}
}

// A parent_job_id pointing at its own row is the cheap way to hang a walk that
// trusts the column. The hop bound is what stops it, and a bound with no test
// is a comment.
func TestParentWalkTerminatesOnASelfParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	ephemeral := ephemeralAgentName("lens-loop", "job-loop")
	insertCompletedJob(t, store, db.Job{ID: "job-loop", Agent: ephemeral, Type: "review", ParentJobID: "job-loop"}, JobPayload{})

	family, ok, err := ResolveRuntimeFamily(ctx, store, "job-loop", ephemeral, "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if ok {
		t.Fatalf("self-parent resolved to %q, want no family and a terminating walk", family)
	}
}
