package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1952 correction round (directive 126863). The wall fix was correct but the
// CENSUS was not: three more execute-path consumers still resolved a queued
// job's runtime from the agents table while the job ran on its ephemeral spec.
// The class rule is override > ephemeral spec > registered row on every
// execute-path decision — refuse, hold, probe, reserve, serialize.
//
// Every test here plants a same-name agent row of a DIFFERENT runtime, because
// that is the condition under which the old code looked right.

func ephemeralConsumerStore(t *testing.T) (*db.Store, string) {
	t.Helper()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	return store, home
}

// admissionHeldBack dispatches through the REAL tracked scheduler under a given
// memory cap and reports whether the job was refused admission. Nothing here
// calls perJobAdmissionEstimate: the estimate is observed through the decision
// the scheduler actually makes, which is the only place it matters.
//
// The verdict is the scheduler's own "held back" line, which names ADMISSION
// specifically — unlike final job state, which reads "queued" both for a job
// refused admission and for a job that was never selected, and so cannot tell
// the two apart. Two instrument corrections are baked in here, both of which
// cost a round:
//
//   - resetHeldBackWarnState() is REQUIRED. That line is throttled per
//     job+reason by heldBackWarnByJob for heldBackLogInterval (5 minutes) in
//     PACKAGE state, so without the reset a stdout check silently reports "not
//     held" on the second and third pass of -count=3. The codebase already had
//     this helper; TestTrackedDispatchLogsAdmissionNeverFit calls it.
//   - the job must be dispatch-ELIGIBLE (PullRequest set), or nothing is
//     selected, stdout is empty, and every assertion reads whatever the empty
//     case happens to imply.
func admissionHeldBack(t *testing.T, store *db.Store, jobID string, maxMemoryGB float64) (bool, string) {
	t.Helper()
	resetHeldBackWarnState()
	ctx := context.Background()
	stdout := &syncBuffer{}
	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	worker.Stdout = stdout
	worker.Admission = newAdmissionBudget(config.AdmissionPolicy{
		MaxMemoryGB:     maxMemoryGB,
		DefaultMemoryGB: 0.5,
	})
	tracker := newInflightJobTracker(ctx)
	if err := dispatchQueuedJobsTracked(ctx, worker, 2, 2, "owner/repo", "", tracker); err != nil {
		t.Fatalf("dispatchQueuedJobsTracked: %v", err)
	}
	out := stdout.String()
	job := mustWorkerJob(t, store, jobID)
	return strings.Contains(out, "job "+jobID+" held back:"), out + " | final state=" + job.State
}

// TestAdmissionStillAdmitsASessionlessJobUnderATinyCap: a session-less shell job
// contributes no RAM and must be admitted under a cap far below the default
// prior.
func TestAdmissionStillAdmitsASessionlessJobUnderATinyCap(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-admit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "shell-admission", Agent: "shell-admit", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	if held, out := admissionHeldBack(t, store, "shell-admission", 0.1); held {
		t.Fatalf("a session-less shell job was refused admission under a 0.1 GB cap; output=%q", out)
	}
}

// TestEphemeralGateTakesNoSessionForANonResumableSpec: a shell spec takes no
// session lock, exactly as a shell-registered agent does not. Without this a
// "make it non-empty" fix would invent a session for every ephemeral job — and
// note the stored row here is omp, so a key read off the stored row would be
// the wrong runtime for a job that runs on shell.
func TestEphemeralGateTakesNoSessionForANonResumableSpec(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-shell", runtime.OmpRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "eph-shell-job", Agent: "eph-shell", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ShellRuntime},
	})
	job := mustWorkerJob(t, store, "eph-shell-job")
	if key := queuedJobRuntimeResourceKey(context.Background(), store, job); key != "" {
		t.Fatalf("shell-spec ephemeral job took session key %q, want none", key)
	}
	if held, out := admissionHeldBack(t, store, "eph-shell-job", 0.1); held {
		t.Fatalf("a shell-spec ephemeral job was charged RAM and refused; output=%q", out)
	}
}

// #1952 review round 3 (directive 127125). The previous key test called
// queuedJobRuntimeResourceKey directly, so a compiling mutant that left that
// helper correct and routed ephemeral jobs through ONE shared key inside the
// production SELECTOR kept every committed test green while two distinct
// ephemeral jobs falsely serialized. The helper was never the risk; the routing
// was. Nothing below names the helper.
//
// The seam: queuedJobResourceSelector.selects computes a runtime key per job and
// refuses a job whose key is already claimed by one selected this pass. It is
// built in selectRunnableQueuedJobsSeeded and reached from
// selectRunnableQueuedJobsWithPolicy (runQueuedJobsForRepo) and the pool path.

// seedTwoEphemeralJobs puts the two jobs in DIFFERENT repos on purpose. Measured:
// with both in one repo they share the checkout key "repo:owner/repo" and the
// selector serializes them for that reason alone, while their runtime keys are
// already distinct — so a same-repo pair would have "passed" (or failed) on the
// checkout confound and proved nothing about runtime routing, which is the whole
// subject of this PR. Distinct repos leave the runtime key as the only thing that
// can serialize them.
func seedTwoEphemeralJobs(t *testing.T, store *db.Store, specRuntime string) []db.Job {
	t.Helper()
	for i, repo := range []string{"owner/repo-a", "owner/repo-b"} {
		seedDaemonWorkerRepo(t, store, repo, t.TempDir())
		agent := fmt.Sprintf("eph-pair-%d", i)
		seedDaemonWorkerAgent(t, store, agent, runtime.ShellRuntime, "unused", []string{"ask"}, repo)
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: fmt.Sprintf("eph-pair-%d", i), Agent: agent, Action: "ask", Repo: repo, Branch: "main", PullRequest: 1,
			Ephemeral: &workflow.EphemeralSpec{Runtime: specRuntime, Capabilities: []string{"ask"}, Role: "worker"},
		})
	}
	return []db.Job{mustWorkerJob(t, store, "eph-pair-0"), mustWorkerJob(t, store, "eph-pair-1")}
}

// TestSelectorSelectsTwoDistinctEphemeralJobsInOnePass drives the production
// selector. Two ephemeral jobs on the SAME spec runtime start independent
// sessions, so both must be selected in a single pass under a serializing
// same-session policy. A routing mutant that gives them one shared key sends the
// second to `remaining`, which no assertion about the key helper can see.
func TestSelectorSelectsTwoDistinctEphemeralJobsInOnePass(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	jobs := seedTwoEphemeralJobs(t, store, runtime.OmpRuntime)

	selected, remaining := selectRunnableQueuedJobsWithPolicy(context.Background(), store, jobs, 2,
		config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue})

	if len(selected) != 2 {
		t.Fatalf("selected %d of 2 distinct ephemeral jobs (remaining=%d); they start separate sessions and must not serialize",
			len(selected), len(remaining))
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want empty", remaining)
	}
	if selected[0].ID == selected[1].ID {
		t.Fatalf("selector returned the same job twice: %+v", selected)
	}
}

// TestQueuedDispatchEventuallyRunsBothDistinctEphemeralJobs asserts EVENTUAL
// EXECUTION of both jobs despite distinct keying — deliberately NOT overlap.
//
// THE PRODUCTION CONTRACT, read from runQueuedJobsForRepo
// (daemon_scheduler.go:1859-1909): the dispatcher loops while work remains; each
// pass selects up to `limit` runnable jobs, admits them, launches each in its own
// goroutine and joins them with ONE wg.Wait before the next pass. So jobs
// co-selected in a pass do overlap, but passes are strictly SERIAL, and whether
// two particular jobs share a pass is decided by the selector. There is no
// independent guarantee that any two given jobs run in one pass.
//
// Hence this test cannot prove overlap: a second job serialized into a later
// pass still finishes, so the assertion below holds either way. Its earlier name
// claimed "InOnePass" and was wrong — the review found that under mutant M14 the
// direct selector guard correctly fails while this test still passes. That guard
// (TestSelectorSelectsTwoDistinctEphemeralJobsInOnePass, above) is the
// discriminating regression for keying; this one only rules out the coarser
// failure where a job never runs at all.
//
// No overlap assertion was added instead, because a barrier test would only
// re-prove co-selection — which the selector guard already pins directly — while
// adding a deadlock-on-regression failure mode. Naming a parallelism guarantee
// production does not make would be worse than documenting eventual dispatch.
func TestQueuedDispatchEventuallyRunsBothDistinctEphemeralJobs(t *testing.T) {
	ctx := context.Background()
	store, home := ephemeralConsumerStore(t)
	seedTwoEphemeralJobs(t, store, runtime.OmpRuntime)

	starter := &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655441111"}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.StartAdapterFactory = func(execbackend.Backend, string, string) (runtime.Adapter, error) { return starter, nil }
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, nil
	}
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo: %v", err)
	}

	var stillQueued []string
	for _, id := range []string{"eph-pair-0", "eph-pair-1"} {
		if job := mustWorkerJob(t, store, id); job.State == string(workflow.JobQueued) {
			stillQueued = append(stillQueued, id)
		}
	}
	if len(stillQueued) != 0 {
		t.Fatalf("jobs still queued after the dispatcher drained: %v — a distinctly-keyed ephemeral job never ran at all (this asserts eventual execution, not overlap)", stillQueued)
	}
}

// TestBouncedBusyExclusionKeepsADistinctEphemeralJob covers the SECOND routing
// site, which a mutant found after the first was fixed: excludeBouncedBusy drops
// any still-pending job whose runtime key is in the bounced-busy set for this
// pool invocation, via memoizedRuntimeResourceKey. Route ephemeral jobs through
// one shared key there and a single busy bounce silently excludes EVERY other
// ephemeral job in the pass — a starvation defect the selector arm cannot see,
// because the selector never runs for the excluded jobs.
//
// The bounced set is built with the same helper production uses, because that is
// this function's INPUT; the assertion is about which jobs survive, not about the
// key's value.
func TestBouncedBusyExclusionKeepsADistinctEphemeralJob(t *testing.T) {
	ctx := context.Background()
	store, _ := ephemeralConsumerStore(t)
	jobs := seedTwoEphemeralJobs(t, store, runtime.OmpRuntime)
	bouncedFirst, other := jobs[0], jobs[1]

	// The bounced set MUST be built through the same memo map excludeBouncedBusy
	// will use. A first version passed a nil memo, which takes
	// memoizedRuntimeResourceKey's early-return path — so a mutant injected into
	// the memoized branch produced a shared key inside the function under test
	// while the fixture still held real per-job keys, the two never matched, and
	// the mutant survived. The fixture has to travel the production path too.
	worker := jobWorker{Store: store}
	memo := map[string]string{}
	bouncedRuntimes := map[string]bool{
		memoizedRuntimeResourceKey(ctx, store, bouncedFirst, memo): true,
	}
	kept := excludeBouncedBusy(ctx, worker, []db.Job{other},
		map[string]bool{bouncedFirst.ID: true}, bouncedRuntimes, memo)

	if len(kept) != 1 || kept[0].ID != other.ID {
		t.Fatalf("kept = %+v, want only %s: one ephemeral job bouncing busy must not exclude a DISTINCT ephemeral job",
			kept, other.ID)
	}
}
