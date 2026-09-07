package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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

func ephemeralClaudePayload() workflow.JobPayload {
	return workflow.JobPayload{
		Repo:      "owner/repo",
		Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime},
	}
}

// countClaudeLiveChecks swaps the live Claude credential check for a counter.
// The count is the whole point: the defect was ZERO live probes for a job that
// runs on Claude, and "did it probe" cannot be inferred from the verdict alone
// because Unknown is also what a probe-less path returns.
func countClaudeLiveChecks(t *testing.T, result error) *int {
	t.Helper()
	calls := 0
	original := claudeAuthLiveCheck
	claudeAuthLiveCheck = func(context.Context, subprocess.Runner, string, []string) error {
		calls++
		return result
	}
	t.Cleanup(func() { claudeAuthLiveCheck = original })
	return &calls
}

// TestAuthProbeProbesTheEphemeralSpecRuntimeNotTheStoredRow is the P2 auth-probe
// regression. Stored row says shell, the spec says Claude, so the job runs on
// Claude — the probe must actually contact Claude and return a real verdict.
// Before the fix: zero probes and Unknown, and because authProbeAllowsRedispatch
// releases Unknown, a Claude-auth-deferred job re-dispatched with no credential
// check at all.
func TestAuthProbeProbesTheEphemeralSpecRuntimeNotTheStoredRow(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-probe", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "eph-auth-probe", Agent: "eph-probe", Type: "ask"}

	calls := countClaudeLiveChecks(t, errors.Join(errors.New("rejected"), runtime.ErrClaudeAuthFailed))
	verdict := worker.defaultAuthProbe(context.Background(), job, ephemeralClaudePayload())

	if *calls == 0 {
		t.Fatalf("live Claude probe calls = 0: the job runs on Claude but nothing validated its credential")
	}
	if verdict == authProbeUnknown {
		t.Fatalf("verdict = unknown after %d live probe(s); an unknown verdict is released by authProbeAllowsRedispatch", *calls)
	}
	if verdict != authProbeInvalid {
		t.Fatalf("verdict = %v, want invalid: the stubbed credential check rejected", verdict)
	}
}

// TestAuthProbeLeavesANonEphemeralNonClaudeJobAlone is the should-SUCCEED
// control: the fix must not make everything probe Claude. An ordinary shell
// agent job still performs zero live checks and stays Unknown.
func TestAuthProbeLeavesANonEphemeralNonClaudeJobAlone(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "shell-only", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "plain-shell-probe", Agent: "shell-only", Type: "ask"}

	calls := countClaudeLiveChecks(t, nil)
	if verdict := worker.defaultAuthProbe(context.Background(), job, workflow.JobPayload{Repo: "owner/repo"}); verdict != authProbeUnknown {
		t.Fatalf("non-Claude verdict = %v, want unknown", verdict)
	}
	if *calls != 0 {
		t.Fatalf("live Claude probe calls = %d for a shell job, want 0", *calls)
	}
}

// TestAuthProbeDedupKeyDistinguishesEphemeralRuntimes: the dedup key names the
// credential domain, so two ephemeral jobs on different runtimes must not share
// one verdict. With the key read off the stored row they collided on
// "runtime:shell" and one probe's result decided both.
func TestAuthProbeDedupKeyDistinguishesEphemeralRuntimes(t *testing.T) {
	store, home := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-probe", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	worker := defaultJobWorker(store, io.Discard, home)
	job := db.Job{ID: "eph-dedup", Agent: "eph-probe", Type: "ask"}
	ctx := context.Background()

	claudeKey := worker.authProbeDedupKey(ctx, job, ephemeralClaudePayload())
	codexPayload := ephemeralClaudePayload()
	codexPayload.Ephemeral = &workflow.EphemeralSpec{Runtime: runtime.CodexRuntime}
	codexKey := worker.authProbeDedupKey(ctx, job, codexPayload)
	storedKey := worker.authProbeDedupKey(ctx, job, workflow.JobPayload{Repo: "owner/repo"})

	if claudeKey == codexKey {
		t.Fatalf("ephemeral Claude and Codex jobs share the probe key %q: one verdict would decide both", claudeKey)
	}
	if claudeKey == storedKey {
		t.Fatalf("ephemeral Claude job shares the stored shell agent's key %q", storedKey)
	}
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
		CodexMemoryGB:   0.2,
		ClaudeMemoryGB:  0.85,
		KimiMemoryGB:    0.5,
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

func seedEphemeralAdmissionJob(t *testing.T, store *db.Store, jobID, storedRuntime string, spec *workflow.EphemeralSpec) {
	t.Helper()
	seedDaemonWorkerAgent(t, store, "eph-admit-"+jobID, storedRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "eph-admit-" + jobID, Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1,
		Ephemeral: spec,
	})
}

// TestAdmissionChargesTheEphemeralSpecRuntimeThroughTheScheduler is the P2
// admission regression, driven through dispatchQueuedJobsTracked rather than by
// calling the estimate. It pins Claude's 0.85 GB prior FROM BOTH SIDES: a 0.5 GB
// cap must refuse the job and a 0.9 GB cap must admit it. A one-sided assertion
// would also pass for "charges everything infinite RAM", which is the failure
// mode a correctness fix can easily introduce.
//
// Before the fix the stale shell row made this session-less and free, so it was
// admitted under ANY cap — ephemeral sessions evaded both opt-in limits.
func TestAdmissionChargesTheEphemeralSpecRuntimeThroughTheScheduler(t *testing.T) {
	t.Run("a cap below Claude's prior refuses it", func(t *testing.T) {
		store, _ := ephemeralConsumerStore(t)
		seedEphemeralAdmissionJob(t, store, "eph-admission-tight", runtime.ShellRuntime,
			&workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime})
		held, out := admissionHeldBack(t, store, "eph-admission-tight", 0.5)
		if !held {
			t.Fatalf("a 0.85 GB Claude session was admitted under a 0.5 GB cap; output=%q", out)
		}
	})

	t.Run("a cap above Claude's prior admits it", func(t *testing.T) {
		store, _ := ephemeralConsumerStore(t)
		seedEphemeralAdmissionJob(t, store, "eph-admission-loose", runtime.ShellRuntime,
			&workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime})
		held, out := admissionHeldBack(t, store, "eph-admission-loose", 0.9)
		if held {
			t.Fatalf("a 0.85 GB Claude session was refused under a 0.9 GB cap; output=%q", out)
		}
	})
}

// TestAdmissionStillAdmitsASessionlessJobUnderATinyCap is the should-SUCCEED
// control: a genuinely session-less shell job contributes no RAM and must be
// admitted under a cap far below every runtime prior. Without it, "charge
// everything" would pass the regression above.
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

// TestEphemeralGateKeyIsJobUniqueAndNotASessionRef pins what the gate key must
// actually satisfy, replacing an earlier assertion of mine that was circular: it
// computed the expected key from the same helper under test and claimed the gate
// was byte-identical to the worker's lock. MEASURED, that is false — the gate
// yields runtime:claude:fresh:job:<hash> while the worker's journalled lock is
// runtime:claude:<ref returned by adapter.Start>. It cannot be otherwise: the
// session does not exist until Start returns, so no pre-dispatch value can equal
// it.
//
// What the key must therefore be: non-empty so admission counts the session,
// job-unique so two ephemeral jobs never serialize against each other, and never
// equal to a real session ref so it cannot collide with a live lock.
func TestEphemeralGateKeyIsJobUniqueAndNotASessionRef(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-key", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	for _, id := range []string{"eph-key-a", "eph-key-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: id, Agent: "eph-key", Action: "ask", Repo: "owner/repo", Branch: "main",
			Ephemeral: &workflow.EphemeralSpec{Runtime: runtime.ClaudeRuntime},
		})
	}
	ctx := context.Background()
	keyA := queuedJobRuntimeResourceKey(ctx, store, mustWorkerJob(t, store, "eph-key-a"))
	keyB := queuedJobRuntimeResourceKey(ctx, store, mustWorkerJob(t, store, "eph-key-b"))

	if keyA == "" {
		t.Fatal("gate key is empty for a job that will hold a Claude session; admission would price it free")
	}
	if keyA == keyB {
		t.Fatalf("two ephemeral jobs share the key %q and would falsely serialize", keyA)
	}
	if !strings.HasPrefix(keyA, "runtime:"+runtime.ClaudeRuntime+":") {
		t.Fatalf("gate key %q does not name the spec runtime", keyA)
	}
	// A real session ref is not fresh-prefixed, so the gate key can never be
	// mistaken for one. This is the property the circular test should have made.
	if !runtime.IsFreshRef(strings.TrimPrefix(keyA, "runtime:"+runtime.ClaudeRuntime+":")) {
		t.Fatalf("gate key %q is shaped like a live session ref and could collide with a real lock", keyA)
	}
}

// TestEphemeralGateTakesNoSessionForANonResumableSpec: a shell spec takes no
// session lock, exactly as a shell-registered agent does not. Without this a
// "make it non-empty" fix would invent a session for every ephemeral job — and
// note the stored row here is Claude, so the pre-fix code took a session for a
// job that runs on shell.
func TestEphemeralGateTakesNoSessionForANonResumableSpec(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	seedDaemonWorkerAgent(t, store, "eph-shell", runtime.ClaudeRuntime, "unused", []string{"ask"}, "owner/repo")
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
	jobs := seedTwoEphemeralJobs(t, store, runtime.ClaudeRuntime)

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

// TestQueuedDispatchRunsTwoDistinctEphemeralJobsInOnePass is the same property
// one level up, at the behaviour an operator would notice: two independent
// ephemeral workers run in one dispatch pass rather than one waiting on the
// other's session.
func TestQueuedDispatchRunsTwoDistinctEphemeralJobsInOnePass(t *testing.T) {
	ctx := context.Background()
	store, home := ephemeralConsumerStore(t)
	seedTwoEphemeralJobs(t, store, runtime.ClaudeRuntime)

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
		t.Fatalf("jobs still queued after a 2-slot pass: %v — one ephemeral job waited on the other's session", stillQueued)
	}
}

// TestSelectorStillSerializesOneRegisteredSession is the should-SUCCEED control,
// and it is what stops the cheap fix: independence must come from keying
// ephemeral jobs separately, NOT from weakening runtime serialization. Two
// non-ephemeral jobs on the SAME registered resumable session must still yield
// exactly one selected job under a queueing policy (#684).
//
// Green at both heads by construction — it is a control, not a regression — so
// it is paired with a mutant that removes serialization entirely.
func TestSelectorStillSerializesOneRegisteredSession(t *testing.T) {
	store, _ := ephemeralConsumerStore(t)
	// Different repos again, so the single selection is attributable to the shared
	// RUNTIME session key rather than to a shared checkout key.
	for _, repo := range []string{"owner/repo-a", "owner/repo-b"} {
		seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	}
	seedDaemonWorkerAgent(t, store, "shared-session", runtime.ClaudeRuntime, "session-shared", []string{"ask"}, "owner/repo-a,owner/repo-b")
	for i, repo := range []string{"owner/repo-a", "owner/repo-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: fmt.Sprintf("reg-%d", i), Agent: "shared-session", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1,
		})
	}
	jobs := []db.Job{mustWorkerJob(t, store, "reg-0"), mustWorkerJob(t, store, "reg-1")}

	selected, remaining := selectRunnableQueuedJobsWithPolicy(context.Background(), store, jobs, 2,
		config.ParallelSessionPolicy{SameSession: config.ParallelSessionQueue})

	if len(selected) != 1 || len(remaining) != 1 {
		t.Fatalf("selected=%d remaining=%d, want 1/1: two jobs sharing one registered session must serialize",
			len(selected), len(remaining))
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
	jobs := seedTwoEphemeralJobs(t, store, runtime.ClaudeRuntime)
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

// TestBouncedBusyExclusionStillDropsTheSameSession is that arm's should-SUCCEED
// control: exclusion must keep working for jobs that genuinely share a runtime
// session, or "keep everything" would pass the test above.
func TestBouncedBusyExclusionStillDropsTheSameSession(t *testing.T) {
	ctx := context.Background()
	store, _ := ephemeralConsumerStore(t)
	for _, repo := range []string{"owner/repo-a", "owner/repo-b"} {
		seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	}
	seedDaemonWorkerAgent(t, store, "bounce-shared", runtime.ClaudeRuntime, "session-bounce", []string{"ask"}, "owner/repo-a,owner/repo-b")
	for i, repo := range []string{"owner/repo-a", "owner/repo-b"} {
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: fmt.Sprintf("bounce-%d", i), Agent: "bounce-shared", Action: "ask", Repo: repo, Branch: "main", PullRequest: 1,
		})
	}
	first := mustWorkerJob(t, store, "bounce-0")
	second := mustWorkerJob(t, store, "bounce-1")

	memo := map[string]string{}
	bouncedRuntimes := map[string]bool{memoizedRuntimeResourceKey(ctx, store, first, memo): true}
	kept := excludeBouncedBusy(ctx, jobWorker{Store: store}, []db.Job{second},
		map[string]bool{first.ID: true}, bouncedRuntimes, memo)

	if len(kept) != 0 {
		t.Fatalf("kept = %+v, want empty: a job sharing the bounced session must stay excluded", kept)
	}
}
