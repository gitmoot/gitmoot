package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1641: org role unavailability is RECORDED per runtime but was ENFORCED per
// role, so a claude quota wall refused healthy codex and kimi dispatches for the
// same role. Every test here drives a real entry point — `job run`, the queued
// listing the daemon schedules from, or the --org-role ingress — so it compiles
// and runs unchanged against the pre-fix tree; the cross-runtime cases are the
// red baseline.

const unavailableRuntimeShellScript = `printf '%s\n' '{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`

func seedRoleUnavailableJobHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, unavailableRuntimeShellScript,
		[]string{"ask"}, "owner/repo")
	return home, store
}

func seedRoleUnavailableJob(t *testing.T, store *db.Store, id string, payload workflow.JobPayload) {
	t.Helper()
	seedCLIJob(t, store, db.Job{
		ID:      id,
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, payload),
	}, "queued")
}

// TestJobRunDispatchesWhenAnotherRuntimeIsWalled is the #1641 reproduction: the
// walled runtime is claude, the job runs on shell, so the dispatch must proceed.
// RED before the fix (refused on the role alone), green after.
func TestJobRunDispatchesWhenAnotherRuntimeIsWalled(t *testing.T) {
	home, store := seedRoleUnavailableJobHome(t)
	seedRoleUnavailableJob(t, store, "job-cross-runtime", workflow.JobPayload{
		Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "run", "job-cross-runtime", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("claude wall refused a shell dispatch: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	job, err := store.GetJob(context.Background(), "job-cross-runtime")
	if err != nil || job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job = %+v err=%v, want succeeded", job, err)
	}
	// The wall itself must survive an unrelated runtime's dispatch: it is still
	// claude's incident and must still refuse claude.
	incident, found, err := store.GetActiveOrgRoleUnavailable(context.Background(), "review", time.Now().UTC())
	if err != nil || !found || incident.Runtime != "claude" {
		t.Fatalf("incident after cross-runtime dispatch = %+v found=%v err=%v", incident, found, err)
	}
}

// TestJobRunStillRefusesTheWalledRuntime is the matching-runtime control, with
// the reason/until presentation asserted so the fix cannot buy cross-runtime
// dispatch by weakening the real refusal.
func TestJobRunStillRefusesTheWalledRuntime(t *testing.T) {
	home, store := seedRoleUnavailableJobHome(t)
	seedRoleUnavailableJob(t, store, "job-same-runtime", workflow.JobPayload{
		Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", runtime.ShellRuntime, "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "run", "job-same-runtime", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `org role "review" is unavailable`) ||
		!strings.Contains(stderr.String(), "reason=quota") || !strings.Contains(stderr.String(), "dispatch refused") {
		t.Fatalf("same-runtime refusal: code=%d stderr=%q", code, stderr.String())
	}
	job, err := store.GetJob(context.Background(), "job-same-runtime")
	if err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("held job = %+v err=%v, want queued", job, err)
	}
}

// TestJobRunFailsClosedOnUnusableStoredRuntime covers both unusable stored
// values. An EMPTY runtime is not corruption — UpsertOrgRoleUnavailable writes
// it and the column was added with DEFAULT ” — so such a row keeps its
// pre-#1641 whole-role meaning. An unrecognized runtime is corruption, and
// neither is permission to dispatch.
func TestJobRunFailsClosedOnUnusableStoredRuntime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(store *db.Store, now time.Time) error
	}{
		{
			name: "empty stored runtime holds the whole role",
			write: func(store *db.Store, now time.Time) error {
				return store.UpsertOrgRoleUnavailable(context.Background(), "review", "quota", now.Add(time.Hour), now)
			},
		},
		{
			name: "unrecognized stored runtime is not permission",
			write: func(store *db.Store, now time.Time) error {
				return store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "not-a-runtime", "quota", now.Add(time.Hour), now)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, store := seedRoleUnavailableJobHome(t)
			seedRoleUnavailableJob(t, store, "job-fail-closed", workflow.JobPayload{
				Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
			})
			if err := tc.write(store, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := Run([]string{"job", "run", "job-fail-closed", "--home", home}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "dispatch refused") {
				t.Fatalf("fail-closed refusal: code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

// TestJobRunHonoursRuntimeOverrideInBothDirections: the decision must follow the
// runtime the job will ACTUALLY use, so an override INTO the walled runtime is
// refused even though the agent's own runtime is clear, and an override AWAY
// from it dispatches even though the agent's own runtime is walled.
func TestJobRunHonoursRuntimeOverrideInBothDirections(t *testing.T) {
	t.Run("override into the walled runtime refuses", func(t *testing.T) {
		home, store := seedRoleUnavailableJobHome(t)
		seedRoleUnavailableJob(t, store, "job-override-into", workflow.JobPayload{
			Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
			RuntimeOverride: "claude", RuntimeOverrideRef: "session-1",
		})
		now := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := Run([]string{"job", "run", "job-override-into", "--home", home}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "dispatch refused") {
			t.Fatalf("override into walled runtime: code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("override away from the walled runtime dispatches", func(t *testing.T) {
		home, store := seedRoleUnavailableJobHome(t)
		seedDaemonWorkerAgent(t, store, "walled", "claude", "", []string{"ask"}, "owner/repo")
		seedCLIJob(t, store, db.Job{
			ID:    "job-override-away",
			Agent: "walled",
			Type:  "ask",
			State: string(workflow.JobQueued),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
				RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: unavailableRuntimeShellScript,
			}),
		}, "queued")
		now := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := Run([]string{"job", "run", "job-override-away", "--home", home}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("override away from walled runtime: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

// TestQueuedListingHoldsOnlyTheWalledRuntime covers the LIVE half of #1641: the
// daemon schedules from this listing, so a claude wall previously held every
// queued job of the role regardless of which runtime each job runs on.
func TestQueuedListingHoldsOnlyTheWalledRuntime(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-worker", runtime.ShellRuntime, "true", []string{"ask"}, "gitmoot/gitmoot")
	seedDaemonWorkerAgent(t, store, "claude-worker", "claude", "", []string{"ask"}, "gitmoot/gitmoot")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-shell", Agent: "shell-worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "review",
	})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-claude", Agent: "claude-worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	pending, err := listPendingQueuedJobs(context.Background(), jobWorker{Store: store}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "job-shell" {
		t.Fatalf("pending under a claude wall = %+v, want only job-shell", pending)
	}
}

// TestQueuedListingFailsClosedOnAnUnresolvableSelectedRuntime is the other half
// of failing closed: when the runtime a job would run as cannot be recognized,
// the job cannot claim to be a runtime OTHER than the walled one, so it stays
// held. Without this the "not the walled runtime" branch would silently accept
// every unresolvable selection.
func TestQueuedListingFailsClosedOnAnUnresolvableSelectedRuntime(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-worker", runtime.ShellRuntime, "true", []string{"ask"}, "gitmoot/gitmoot")
	seedDaemonWorkerAgent(t, store, "mystery-worker", "not-a-runtime", "", []string{"ask"}, "gitmoot/gitmoot")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-shell", Agent: "shell-worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "review",
	})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-mystery", Agent: "mystery-worker", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	pending, err := listPendingQueuedJobs(context.Background(), jobWorker{Store: store}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "job-shell" {
		t.Fatalf("pending with an unresolvable runtime = %+v, want only job-shell", pending)
	}
}

// TestValidateAndTouchActingOrgRoleNoLongerDecidesUnavailability pins the moved
// boundary. The ingress still validates the registry and records presence, but
// it cannot decide unavailability because the selected runtime does not exist
// yet at that point — dispatchLocalAgentJob calls it before the agent (and
// therefore the runtime) is resolved. The refusal itself is proven at the real
// dispatch entry points above.
func TestValidateAndTouchActingOrgRoleNoLongerDecidesUnavailability(t *testing.T) {
	home, paths := setupQuotaUnavailableOrgHome(t)
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := validateAndTouchActingOrgRole(ctx, store, home, "review", "agent_run"); err != nil {
		t.Fatalf("available role refused: %v", err)
	}
	if err := validateAndTouchActingOrgRole(ctx, store, home, "nope", "agent_run"); err == nil ||
		!strings.Contains(err.Error(), `unknown org role "nope"`) {
		t.Fatalf("unknown role = %v, want loud rejection", err)
	}
	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx, "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := validateAndTouchActingOrgRole(ctx, store, home, "REVIEW", "agent_run"); err != nil {
		t.Fatalf("ingress refused on an unavailability row it can no longer judge: %v", err)
	}
	if _, found, err := store.GetOrgRolePresence(ctx, "review"); err != nil || !found {
		t.Fatalf("presence row after ingress found=%v err=%v", found, err)
	}
}

// TestRefuseUnavailableOrgRoleClearsAnExpiredRowWhateverTheRuntime keeps the
// eager-expiry behaviour that used to sit on the ingress: the store read must
// stay unconditional, so a stale row is cleared even when the runtime decision
// would not have refused anyway.
func TestRefuseUnavailableOrgRoleClearsAnExpiredRowWhateverTheRuntime(t *testing.T) {
	home, store := seedRoleUnavailableJobHome(t)
	seedRoleUnavailableJob(t, store, "job-expired", workflow.JobPayload{
		Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", "claude", "quota", now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "run", "job-expired", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("expired row refused: code=%d stderr=%q", code, stderr.String())
	}
	rows, err := store.ListActiveOrgRolesUnavailable(context.Background(), time.Now().UTC())
	if err != nil || len(rows) != 0 {
		t.Fatalf("active rows after expiry = %+v err=%v, want none", rows, err)
	}
}
