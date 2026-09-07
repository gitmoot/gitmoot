package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"

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

// subscribeShellAskAgent registers a shell-runtime agent whose session script
// returns a terminal approved result, so an `agent ask` dispatch through the
// real path can reach success rather than stalling on delivery.
func subscribeShellAskAgent(t *testing.T, home, name, repo string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"agent", "subscribe", name,
		"--home", home,
		"--runtime", "shell",
		"--session", unavailableRuntimeShellScript,
		"--role", "review",
		"--repo", repo,
		"--capability", "ask",
		"--policy", "workspace-write",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agent subscribe %s exit=%d stderr=%q", name, code, stderr.String())
	}
}

// TestLocalAgentDispatchScopesRoleUnavailabilityToTheSelectedRuntime is the test
// arm for the agent_dispatch.go callsite (#1641, authorised in 126292). It drives
// the REAL `agent ask --org-role` path — dispatchLocalAgentJob — rather than the
// refusal helper, so a routing mutant that left this path refusing role-wide
// cannot pass it. The two subtests differ ONLY in which runtime the wall names.
func TestLocalAgentDispatchScopesRoleUnavailabilityToTheSelectedRuntime(t *testing.T) {
	setup := func(t *testing.T, walledRuntime string) (string, config.Paths) {
		t.Helper()
		home, paths := setupQuotaUnavailableOrgHome(t)
		subscribeShellAskAgent(t, home, "asker", "gitmoot/gitmoot")
		checkout := t.TempDir()
		runGit(t, checkout, "init")
		runGit(t, checkout, "branch", "-m", "main")
		runGit(t, checkout, "remote", "add", "origin", "https://github.com/gitmoot/gitmoot.git")
		if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, checkout, "add", "README.md")
		runGit(t, checkout, "-c", "user.name=Gitmoot Test", "-c", "user.email=gitmoot@example.com", "commit", "-m", "initial")
		withWorkingDirectory(t, checkout)

		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", walledRuntime, "quota", now.Add(time.Hour), now); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return home, paths
	}

	t.Run("another runtime is walled so the dispatch proceeds", func(t *testing.T) {
		home, _ := setup(t, "claude")
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"agent", "ask", "asker", "what is the state of the repo?",
			"--home", home, "--repo", "gitmoot/gitmoot", "--org-role", "review", "--json",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("claude wall refused a shell agent dispatch: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if strings.Contains(stderr.String(), "dispatch refused") {
			t.Fatalf("cross-runtime dispatch reported a refusal: stderr=%q", stderr.String())
		}
		var output localAgentJobOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatalf("parse ask output %q: %v", stdout.String(), err)
		}
		if output.State != string(workflow.JobSucceeded) {
			t.Fatalf("ask state = %q, want succeeded", output.State)
		}
	})

	t.Run("the selected runtime is walled so the dispatch is refused", func(t *testing.T) {
		home, paths := setup(t, runtime.ShellRuntime)
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"agent", "ask", "asker", "what is the state of the repo?",
			"--home", home, "--repo", "gitmoot/gitmoot", "--org-role", "review", "--json",
		}, &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), `org role "review" is unavailable`) ||
			!strings.Contains(stderr.String(), "dispatch refused") {
			t.Fatalf("same-runtime dispatch not refused: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if jobs, err := store.ListJobs(context.Background()); err != nil || len(jobs) != 0 {
			t.Fatalf("jobs after a refused dispatch = %+v err=%v, want none", jobs, err)
		}
	})
}

// TestRunTaskRunScopesRoleUnavailabilityToTheOwnersRuntime is the test arm for
// the workflow.go callsite. `task run` has no --runtime flag, so the owner
// agent's runtime IS the selection. The refused half asserts exactly what the
// pre-existing TestRunTaskRunRefusesUnavailableRoleBeforeWorktreeAllocation
// asserts; the proceeding half asserts the same facts inverted, so the pair is
// directly comparable rather than differently shaped.
func TestRunTaskRunScopesRoleUnavailabilityToTheOwnersRuntime(t *testing.T) {
	setup := func(t *testing.T, walledRuntime string) (string, config.Paths) {
		t.Helper()
		home, paths := setupQuotaUnavailableOrgHome(t)
		goalPath := filepath.Join(t.TempDir(), "GOAL.md")
		if err := os.WriteFile(goalPath, []byte("# Build Gitmoot\n\n### Task 1: Bootstrap\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"goal", "import", "--home", home, "--file", goalPath, "--repo", "gitmoot/gitmoot"}, &stdout, &stderr); code != 0 {
			t.Fatalf("goal import code=%d stderr=%q", code, stderr.String())
		}
		subscribeShellImplementAgent(t, home, "lead", "gitmoot/gitmoot")
		checkout := t.TempDir()
		runGit(t, checkout, "init")
		runGit(t, checkout, "branch", "-m", "main")
		runGit(t, checkout, "remote", "add", "origin", "https://github.com/gitmoot/gitmoot.git")
		if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, checkout, "add", "README.md")
		runGit(t, checkout, "-c", "user.name=Gitmoot Test", "-c", "user.email=gitmoot@example.com", "commit", "-m", "initial")
		withWorkingDirectory(t, checkout)

		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", walledRuntime, "quota", now.Add(time.Hour), now); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return home, paths
	}

	t.Run("another runtime is walled so task run proceeds", func(t *testing.T) {
		home, paths := setup(t, "claude")
		var stdout, stderr bytes.Buffer
		Run([]string{
			"task", "run", "task-001", "--home", home, "--repo", "gitmoot/gitmoot",
			"--owner", "lead", "--org-role", "review",
		}, &stdout, &stderr)
		if strings.Contains(stderr.String(), "dispatch refused") {
			t.Fatalf("claude wall refused a shell owner's task run: stderr=%q", stderr.String())
		}
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		// The pre-existing refusal test asserts the task is untouched and no job
		// exists. Under a wall on a DIFFERENT runtime both must have happened.
		task, err := store.GetTask(context.Background(), "task-001")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(task.WorktreePath) == "" {
			t.Fatalf("task never allocated a worktree, so the dispatch did not proceed: %+v", task)
		}
		jobs, err := store.ListJobs(context.Background())
		if err != nil || len(jobs) == 0 {
			t.Fatalf("jobs after a proceeding task run = %+v err=%v, want at least one", jobs, err)
		}
	})

	t.Run("the owner's runtime is walled so task run is refused", func(t *testing.T) {
		home, paths := setup(t, runtime.ShellRuntime)
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"task", "run", "task-001", "--home", home, "--repo", "gitmoot/gitmoot",
			"--owner", "lead", "--org-role", "review",
		}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), `org role "review" is unavailable`) ||
			!strings.Contains(stderr.String(), "dispatch refused") {
			t.Fatalf("task run on the walled runtime: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		store, err := dbtest.Open(t, paths.Database)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		task, err := store.GetTask(context.Background(), "task-001")
		if err != nil {
			t.Fatal(err)
		}
		if task.State != string(workflow.TaskPlanned) || strings.TrimSpace(task.WorktreePath) != "" {
			t.Fatalf("task mutated before the refusal: %+v", task)
		}
		if jobs, err := store.ListJobs(context.Background()); err != nil || len(jobs) != 0 {
			t.Fatalf("jobs after a refused task run = %+v err=%v, want none", jobs, err)
		}
	})
}

// #1952 review P2: the wall must be checked against the runtime that will
// EXECUTE. daemon_worker materializes an ephemeral worker unconditionally from
// payload.Ephemeral.Runtime and upserts it over any same-name agent row, so a
// resolver that consults the agents table first checks a runtime that never
// runs. Every test below plants an extant same-name agent row of a DIFFERENT
// runtime, which is the condition that made the defect invisible.

const ephemeralWalledJobAgent = "wave-impl-ephemeral"

func seedEphemeralWalledJob(t *testing.T, store *db.Store, home, jobID, storedRuntime, specRuntime, walledRuntime string) {
	t.Helper()
	// The agents-table row the old resolver would have believed.
	seedDaemonWorkerAgent(t, store, ephemeralWalledJobAgent, storedRuntime, unavailableRuntimeShellScript,
		[]string{"ask"}, "owner/repo")
	seedCLIJob(t, store, db.Job{
		ID:    jobID,
		Agent: ephemeralWalledJobAgent,
		Type:  "ask",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
			Ephemeral: &workflow.EphemeralSpec{Runtime: specRuntime},
		}),
	}, "queued")
	now := time.Now().UTC()
	if walledRuntime != "" {
		if err := store.UpsertOrgRoleUnavailableForRuntime(context.Background(), "review", walledRuntime, "quota", now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	_ = home
}

// TestJobRunRefusesAnEphemeralJobWhoseSpecRuntimeIsWalled is the #1952 P2
// regression on the `job run` path: stored row says shell, the spec says claude,
// claude is walled. The old resolver read the stored row and dispatched.
func TestJobRunRefusesAnEphemeralJobWhoseSpecRuntimeIsWalled(t *testing.T) {
	home, store := seedRoleUnavailableJobHome(t)
	seedEphemeralWalledJob(t, store, home, "job-eph-walled", runtime.ShellRuntime, "claude", "claude")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "run", "job-eph-walled", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `org role "review" is unavailable`) ||
		!strings.Contains(stderr.String(), "dispatch refused") {
		t.Fatalf("ephemeral job on a walled spec runtime was not refused: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	job, err := store.GetJob(context.Background(), "job-eph-walled")
	if err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("job = %+v err=%v, want still queued (refused before start)", job, err)
	}
}

// TestJobRunDispatchesAnEphemeralJobWhenAnotherRuntimeIsWalled is the
// should-succeed control: the fix must not buy correctness by refusing every
// ephemeral job. Spec runtime is shell, the wall names claude, and the stored
// row deliberately says claude so a resolver that still preferred the agents
// table would refuse here.
func TestJobRunDispatchesAnEphemeralJobWhenAnotherRuntimeIsWalled(t *testing.T) {
	home, store := seedRoleUnavailableJobHome(t)
	seedEphemeralWalledJob(t, store, home, "job-eph-clear", "claude", runtime.ShellRuntime, "claude")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "run", "job-eph-clear", "--home", home}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "dispatch refused") {
		t.Fatalf("a wall on a non-selected runtime refused an ephemeral dispatch: code=%d stderr=%q", code, stderr.String())
	}
	job, err := store.GetJob(context.Background(), "job-eph-clear")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == string(workflow.JobQueued) {
		t.Fatalf("job never left queued, so the wall held it: %+v stderr=%q", job, stderr.String())
	}
}

// TestJobRunFailsClosedOnAnEphemeralSpecRuntimeItCannotUse keeps the fail-closed
// half honest for the new precedence: an ephemeral spec naming nothing usable
// must refuse rather than be treated as "some runtime other than the walled one".
func TestJobRunFailsClosedOnAnEphemeralSpecRuntimeItCannotUse(t *testing.T) {
	for _, specRuntime := range []string{"", "not-a-runtime"} {
		name := specRuntime
		if name == "" {
			name = "empty spec runtime"
		}
		t.Run(name, func(t *testing.T) {
			home, store := seedRoleUnavailableJobHome(t)
			seedEphemeralWalledJob(t, store, home, "job-eph-closed", runtime.ShellRuntime, specRuntime, "claude")
			var stdout, stderr bytes.Buffer
			code := Run([]string{"job", "run", "job-eph-closed", "--home", home}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), "dispatch refused") {
				t.Fatalf("unusable ephemeral spec runtime %q not refused: code=%d stderr=%q", specRuntime, code, stderr.String())
			}
		})
	}
}

// TestQueuedEphemeralJobIsHeldWhenItsSpecRuntimeIsWalled is the same regression
// through the DAEMON path the reviewer's adversary used: runQueuedJobsForRepo.
// It asserts the hold lands before start AND before delivery — the adapter
// factory fails the test if it is ever reached, which is what "refused before
// delivery" has to mean.
func TestQueuedEphemeralJobIsHeldWhenItsSpecRuntimeIsWalled(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, ephemeralWalledJobAgent, runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "queued-eph-walled", Agent: ephemeralWalledJobAgent, Action: "ask",
		Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
		Ephemeral: &workflow.EphemeralSpec{Runtime: "claude"},
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx, "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(agent runtime.Agent, _ string) (workflow.DeliveryAdapter, error) {
		t.Fatalf("delivery reached for a job whose selected runtime %q is walled", agent.Runtime)
		return nil, nil
	}
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "queued-eph-walled")
	if err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("queued ephemeral job = %+v err=%v, want held in queued", job, err)
	}
}

// TestQueuedEphemeralJobRunsWhenAnotherRuntimeIsWalled is the daemon-path
// should-succeed control, and the reason it matters: the stored row names the
// WALLED runtime while the spec names a clear one, so a resolver that preferred
// the agents table would hold a job that is perfectly safe to run.
func TestQueuedEphemeralJobRunsWhenAnotherRuntimeIsWalled(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, ephemeralWalledJobAgent, "claude", "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "queued-eph-clear", Agent: ephemeralWalledJobAgent, Action: "ask",
		Repo: "owner/repo", Branch: "main", ActingOrgRole: "review",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ShellRuntime},
	})
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx, "review", "claude", "quota", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}

	pending, err := listPendingQueuedJobs(ctx, jobWorker{Store: store}, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "queued-eph-clear" {
		t.Fatalf("pending = %+v, want the ephemeral job eligible: its spec runtime is not the walled one", pending)
	}
}
