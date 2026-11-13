package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestTerminalWriteContextSurvivesRunCancellationAndHasGraceDeadline(t *testing.T) {
	runCtx, stopRun := context.WithCancel(context.Background())
	stopRun()

	writeCtx, cancel := terminalWriteContext(runCtx)
	defer cancel()
	if err := writeCtx.Err(); err != nil {
		t.Fatalf("terminal write context inherited run cancellation: %v", err)
	}
	deadline, ok := writeCtx.Deadline()
	if !ok {
		t.Fatal("terminal write context has no grace deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > terminalWriteGrace {
		t.Fatalf("terminal write grace remaining = %v, want >0 and <=%v", remaining, terminalWriteGrace)
	}
}

func TestMailboxFailPersistsAfterRunContextDeadline(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-terminal-write", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.claim(ctx, job); err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	cancelRun()
	if err := mailbox.fail(runCtx, job.ID, "deadline exceeded"); err != nil {
		t.Fatalf("mailbox.fail on expired run context: %v", err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("stored job state = %q, want failed", stored.State)
	}
}

func TestMailboxEnqueueCreatesQueuedJobAndEvent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:          "job-1",
		Agent:       "audit",
		Action:      "review",
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-005",
		PullRequest: 5,
		TaskID:      "task-5",
		TaskTitle:   "Job Mailbox",
		Sender:      "octocat",
		Constraints: []string{"  preserve behavior  ", ""},
	})

	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if job.State != string(JobQueued) {
		t.Fatalf("state = %q, want queued", job.State)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.Payload == "" || !strings.Contains(stored.Payload, `"repo":"gitmoot/gitmoot"`) {
		t.Fatalf("payload = %q", stored.Payload)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	// A review with no resolvable pool also carries the #2186 advisory: it has no
	// provider fallback, and that must be visible on the job rather than inferred.
	if len(events) != 2 || events[0].Kind != "queued" || events[1].Kind != "review_pool_unresolved" {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxPersistsActingOrgRoleProvenance(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	job, err := (Mailbox{store: store}).Enqueue(ctx, JobRequest{
		ID: "org-role-job", Agent: "audit", Action: "ask", Repo: "gitmoot/gitmoot", ActingOrgRole: " owner ",
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload() error = %v", err)
	}
	if payload.ActingOrgRole != "owner" || !strings.Contains(job.Payload, `"acting_org_role":"owner"`) {
		t.Fatalf("payload = %+v raw=%s", payload, job.Payload)
	}
}

func TestMailboxPersistsRuntimeIdentityUntilTerminal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "pid-job", Agent: "worker", Action: "ask", Repo: "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &fakeDelivery{
		pid: os.Getpid(),
		onDeliver: func() {
			stored, err := store.GetJob(ctx, "pid-job")
			if err != nil {
				t.Fatalf("GetJob during delivery: %v", err)
			}
			payload, err := ParseJobPayload(stored.Payload)
			if err != nil {
				t.Fatalf("ParseJobPayload during delivery: %v", err)
			}
			if payload.RuntimePID != os.Getpid() || payload.RuntimePIDStartTime == "" || payload.RuntimePGID <= 0 {
				t.Fatalf("runtime identity during delivery = pid %d pgid %d start %q, want pid %d with process identity", payload.RuntimePID, payload.RuntimePGID, payload.RuntimePIDStartTime, os.Getpid())
			}
		},
		outputs: []string{
			`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
	}
	if _, err := mailbox.Run(ctx, "pid-job", runtime.Agent{Name: "worker", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:pid-job"}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stored, err := store.GetJob(ctx, "pid-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.RuntimePID != 0 || payload.RuntimePIDStartTime != "" || payload.RuntimePGID != 0 {
		t.Fatalf("runtime identity after terminal = pid %d pgid %d start %q, want cleared", payload.RuntimePID, payload.RuntimePGID, payload.RuntimePIDStartTime)
	}
}

func TestMailboxEnqueuePersistsDelegationMetadata(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:              "job-child",
		Agent:           "audit",
		Action:          "ask",
		Repo:            "gitmoot/gitmoot",
		Branch:          "task-005",
		ParentJobID:     "job-parent",
		DelegationID:    "delegation-1",
		DelegationDepth: 2,
		DelegatedBy:     "lead",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if job.ParentJobID != "job-parent" || job.DelegationID != "delegation-1" || job.DelegationDepth != 2 || job.DelegatedBy != "lead" {
		t.Fatalf("returned job metadata = %+v", job)
	}

	stored, err := store.GetJob(ctx, "job-child")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.ParentJobID != "job-parent" || stored.DelegationID != "delegation-1" || stored.DelegationDepth != 2 || stored.DelegatedBy != "lead" {
		t.Fatalf("stored job metadata = %+v", stored)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.ParentJobID != "job-parent" || payload.DelegationID != "delegation-1" || payload.DelegationDepth != 2 || payload.DelegatedBy != "lead" {
		t.Fatalf("payload metadata = %+v", payload)
	}
}

func TestMailboxEnqueuePersistsEphemeralSpec(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "job-ephemeral",
		Agent:        "worker-ephemeral-abc123",
		Action:       "review",
		Repo:         "gitmoot/gitmoot",
		ParentJobID:  "job-parent",
		DelegationID: "worker",
		Ephemeral:    &EphemeralSpec{Runtime: "codex", Model: "gpt-5.4", Effort: "high"},
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-ephemeral")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	// The marshalled payload must carry the ephemeral key for downstream consumers.
	if !strings.Contains(stored.Payload, `"ephemeral"`) {
		t.Fatalf("payload = %q, want it to contain the ephemeral spec", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Ephemeral == nil {
		t.Fatalf("payload missing ephemeral spec: %+v", payload)
	}
	if payload.Ephemeral.Runtime != "codex" || payload.Ephemeral.Model != "gpt-5.4" || payload.Ephemeral.Effort != "high" {
		t.Fatalf("payload ephemeral spec = %+v", payload.Ephemeral)
	}
}

// TestMailboxClaimStampsRunnerBootID pins that the queued->running claim stamps
// the claiming process's boot id (#651), routing through Store.ClaimRunningJob. It
// asserts behaviorally via the foreign-boot recovery predicate: a claimed job looks
// "same-boot" to the current boot but "foreign" to a different boot id — which
// can only hold if claim recorded a concrete, non-empty
// runner_boot_id. On the unpatched claim (plain TransitionJobStateWithEvent)
// runner_boot_id stays empty and the foreign-boot query matches nothing, so this
// test fails without the fix.
func TestMailboxClaimStampsRunnerBootID(t *testing.T) {
	if db.BootID() == "" {
		t.Skip("kernel boot id unavailable on this platform")
	}
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-claim", Agent: "audit", Action: "ask", Repo: "gitmoot/gitmoot"})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := mailbox.claim(ctx, job); err != nil {
		t.Fatalf("claim returned error: %v", err)
	}
	running, err := store.GetJob(ctx, "job-claim")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if running.State != string(JobRunning) {
		t.Fatalf("job state after claim = %q, want running", running.State)
	}

	// Same boot: the claimed job is NOT foreign, so it is protected.
	if foreign, err := store.ListRunningJobIDsFromForeignBoot(ctx, db.BootID()); err != nil {
		t.Fatalf("ListRunningJobIDsFromForeignBoot(current) returned error: %v", err)
	} else if len(foreign) != 0 {
		t.Fatalf("same-boot foreign jobs = %v, want none (claim must stamp the CURRENT boot)", foreign)
	}

	// A different boot proves claim stamped a concrete boot id: the job now looks
	// foreign and is selected for evidence-preserving terminal recovery.
	foreign, err := store.ListRunningJobIDsFromForeignBoot(ctx, "foreign-"+db.BootID())
	if err != nil {
		t.Fatalf("ListRunningJobIDsFromForeignBoot(foreign) returned error: %v", err)
	}
	if len(foreign) != 1 || foreign[0] != "job-claim" {
		t.Fatalf("foreign-boot jobs = %v, want [job-claim] (claim must stamp runner_boot_id)", foreign)
	}
}

func TestMailboxEnqueuePersistsModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-model",
		Agent:  "audit",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
		Model:  "opus",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-model")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !strings.Contains(stored.Payload, `"model":"opus"`) {
		t.Fatalf("payload = %q, want it to contain model override", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Model != "opus" {
		t.Fatalf("payload.Model = %q, want %q", payload.Model, "opus")
	}
}

// TestMailboxEnqueuePersistsResolvedModel proves the enqueue chokepoint stores
// the selected model before any runtime delivery. It covers the complete
// precedence plus the two agent shapes that cannot safely reuse a registered
// default: runtime-overridden and ephemeral jobs.
func TestMailboxEnqueuePersistsResolvedModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, agent := range []db.Agent{
		{Name: "pinned", Runtime: runtime.OmpRuntime, Model: "agent-model"},
		{Name: "defaulted", Runtime: runtime.OmpRuntime},
		{Name: "overridden", Runtime: runtime.ShellRuntime, Model: "agent-model"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent(%s): %v", agent.Name, err)
		}
	}
	mailbox := Mailbox{store: store, RuntimeDefaultModel: func(runtimeName string) string {
		if runtimeName == runtime.OmpRuntime {
			return "k3"
		}
		return ""
	}}

	for _, tc := range []struct {
		name string
		req  JobRequest
		want string
	}{
		{
			name: "explicit override beats agent and runtime",
			req:  JobRequest{ID: "resolved-explicit", Agent: "pinned", Action: "ask", Repo: "owner/repo", Model: "job-model"},
			want: "job-model",
		},
		{
			name: "agent model beats runtime",
			req:  JobRequest{ID: "resolved-agent", Agent: "pinned", Action: "ask", Repo: "owner/repo"},
			want: "agent-model",
		},
		{
			name: "omp runtime default",
			req:  JobRequest{ID: "resolved-runtime", Agent: "defaulted", Action: "ask", Repo: "owner/repo"},
			want: "k3",
		},
		{
			name: "runtime override clears registered model",
			req: JobRequest{ID: "resolved-runtime-override", Agent: "overridden", Action: "ask", Repo: "owner/repo",
				RuntimeOverride: runtime.OmpRuntime, RuntimeOverrideRef: "fresh:override"},
			want: "k3",
		},
		{
			name: "ephemeral inline model",
			req: JobRequest{ID: "resolved-ephemeral", Agent: "worker-ephemeral-test", Action: "review", Repo: "owner/repo",
				ParentJobID: "parent", DelegationID: "worker", Ephemeral: &EphemeralSpec{Runtime: runtime.OmpRuntime, Model: "claude-fable-5"}},
			want: "claude-fable-5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queued, err := mailbox.Enqueue(ctx, tc.req)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if queued.Model != tc.want {
				t.Fatalf("returned Model = %q, want %q", queued.Model, tc.want)
			}
			persisted, err := store.GetJob(ctx, tc.req.ID)
			if err != nil {
				t.Fatalf("GetJob before Run: %v", err)
			}
			if persisted.Model != tc.want {
				t.Fatalf("persisted Model before Run = %q, want %q", persisted.Model, tc.want)
			}
		})
	}
}

func TestMailboxEnqueueOmitsEmptyModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-model",
		Agent:  "audit",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-no-model")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if strings.Contains(stored.Payload, `"model"`) {
		t.Fatalf("payload = %q, want no model key when override is empty", stored.Payload)
	}
}

func TestMailboxEnqueuePersistsPhase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-phase",
		Agent:  "audit",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
		Phase:  "design",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-phase")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !strings.Contains(stored.Payload, `"phase":"design"`) {
		t.Fatalf("payload = %q, want it to contain phase", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.Phase != "design" {
		t.Fatalf("payload.Phase = %q, want %q", payload.Phase, "design")
	}
}

func TestMailboxEnqueueOmitsEmptyPhase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-phase",
		Agent:  "audit",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-no-phase")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if strings.Contains(stored.Payload, `"phase"`) {
		t.Fatalf("payload = %q, want no phase key when phase is empty", stored.Payload)
	}
}

func TestMailboxRunDeliversModelAndEffortOverrides(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", Model: "opus", Effort: "high"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(adapter.models) != 1 || adapter.models[0] != "opus" {
		t.Fatalf("delivered runtime.Job models = %+v, want the payload model override [opus]", adapter.models)
	}
	if len(adapter.efforts) != 1 || adapter.efforts[0] != "high" {
		t.Fatalf("delivered runtime.Job efforts = %+v, want the payload effort override [high]", adapter.efforts)
	}
}

// TestMailboxRunThreadsRuntimeDefaultModel proves the #652 wiring: the Mailbox's
// home-aware RuntimeDefaultModel hook is consulted at delivery and threaded onto the
// delivered runtime.Job as the FINAL model fallback. With no agent --model and no
// job --model, the job carries the runtime's configured default (RuntimeDefaultModel
// set) while its explicit Model pin stays empty — so EffectiveModel uses the default,
// yet an agent/job pin (which would populate job.Model) still wins.
func TestMailboxRunThreadsRuntimeDefaultModel(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, RuntimeDefaultModel: func(rt string) string {
		if rt == runtime.ShellRuntime {
			return "gpt-5.5"
		}
		return ""
	}}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}

	// No agent Model, no job Model: the registry default_model must be threaded in.
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(adapter.models) != 1 || adapter.models[0] != "" {
		t.Fatalf("delivered job.Model = %+v, want no explicit pin []", adapter.models)
	}
	if len(adapter.runtimeDefault) != 1 || adapter.runtimeDefault[0] != "gpt-5.5" {
		t.Fatalf("delivered job.RuntimeDefaultModel = %+v, want [gpt-5.5]", adapter.runtimeDefault)
	}
}

func TestMailboxRunThreadsRuntimeDefaultEffort(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, RuntimeDefaultEffort: func(rt string) string {
		if rt == runtime.OmpRuntime {
			return "high"
		}
		return ""
	}}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:audit", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(adapter.efforts) != 1 || adapter.efforts[0] != "" {
		t.Fatalf("delivered job.Effort = %+v, want no explicit pin", adapter.efforts)
	}
	if len(adapter.runtimeDefaultEfforts) != 1 || adapter.runtimeDefaultEfforts[0] != "high" {
		t.Fatalf("delivered job.RuntimeDefaultEffort = %+v, want [high]", adapter.runtimeDefaultEfforts)
	}
}

// TestMailboxRunNilRuntimeDefaultHookByteIdentical proves the byte-identical
// default: with NO RuntimeDefaultModel hook (every pre-#652 construction site), the
// delivered runtime.Job carries an empty RuntimeDefaultModel, so nothing is forced.
func TestMailboxRunNilRuntimeDefaultHookByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(adapter.runtimeDefault) != 1 || adapter.runtimeDefault[0] != "" {
		t.Fatalf("delivered job.RuntimeDefaultModel = %+v, want empty (byte-identical)", adapter.runtimeDefault)
	}
}

// okDeliveryResult is a minimal well-formed gitmoot_result so a fake delivery
// classifies to a terminal state and the job persists its recorded usage.
const okDeliveryResult = `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func TestMailboxDeliverRecordsPartialUsageFromFailedRuntime(t *testing.T) {
	ctx := context.Background()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "reviewer", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:runtime", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	crash := errors.New("runtime exited before agent_end")
	adapter := &fakeDelivery{
		err:         crash,
		errorResult: runtime.Result{InputTokens: 8833, OutputTokens: 144, Raw: "partial transcript"},
		onDeliver:   cancelRun,
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "failed-usage", Agent: "reviewer", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(runCtx, "failed-usage", agent, adapter); err == nil {
		t.Fatal("Run succeeded despite runtime crash")
	}
	job, err := store.GetJob(ctx, "failed-usage")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(JobFailed) {
		t.Fatalf("failed runtime job state = %q, want failed", job.State)
	}
	if job.InputTokens != 8833 || job.OutputTokens != 144 {
		t.Fatalf("failed runtime usage = (%d, %d), want (8833, 144)", job.InputTokens, job.OutputTokens)
	}
}

func TestMailboxEnqueuePersistsRootJobID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:        "job-child",
		Agent:     "audit",
		Action:    "ask",
		Repo:      "gitmoot/gitmoot",
		Branch:    "task-005",
		RootJobID: "root-coordinator",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-child")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.RootJobID != "root-coordinator" {
		t.Fatalf("payload RootJobID = %q, want %q", payload.RootJobID, "root-coordinator")
	}
}

func TestMailboxEnqueueSnapshotsAgentTemplate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "abc123",
		Content:        "Review deeply.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "audit",
		Role:         "reviewer",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "gitmoot/gitmoot",
		TemplateID:   "thermo",
		Capabilities: []string{"review"},
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-1",
		Agent:  "audit",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "thermo",
		Name:           "Thermo",
		SourceRepo:     "cursor/plugins",
		SourceRef:      "main",
		SourcePath:     "cursor-team-kit/skills/thermo-nuclear-code-quality-review/SKILL.md",
		ResolvedCommit: "def456",
		Content:        "Updated instructions.",
	}); err != nil {
		t.Fatalf("second UpsertAgentTemplate returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "thermo" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("payload template snapshot = %+v", payload)
	}

	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "audit-pinned",
		Role:         "reviewer",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "gitmoot/gitmoot",
		TemplateID:   "thermo@v1",
		Capabilities: []string{"review"},
	}); err != nil {
		t.Fatalf("UpsertAgent pinned returned error: %v", err)
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-2",
		Agent:  "audit-pinned",
		Action: "review",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue pinned returned error: %v", err)
	}
	pinnedJob, err := store.GetJob(ctx, "job-2")
	if err != nil {
		t.Fatalf("GetJob pinned returned error: %v", err)
	}
	pinnedPayload, err := unmarshalPayload(pinnedJob.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload pinned returned error: %v", err)
	}
	if pinnedPayload.TemplateID != "thermo" || pinnedPayload.TemplateResolvedCommit != "abc123" || pinnedPayload.TemplateContent != "Review deeply." {
		t.Fatalf("pinned payload template snapshot = %+v", pinnedPayload)
	}
}

func TestMailboxEnqueueAppliesTemplateOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	// The agent carries its own template; a --recipe override should win over it
	// in the enqueued payload without rebinding the agent's identity.
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "agent-own",
		Name:           "Agent Own",
		SourceRepo:     "gitmoot/gitmoot",
		SourceRef:      "main",
		ResolvedCommit: "own123",
		Content:        "Agent's own prompt.",
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate own returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "planner",
		Role:         "coordinator",
		Runtime:      "codex",
		RuntimeRef:   "last",
		RepoScope:    "gitmoot/gitmoot",
		TemplateID:   "agent-own",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}

	override := db.AgentTemplate{
		ID:             "review-panel",
		Name:           "Review Panel",
		SourceRepo:     "gitmoot/gitmoot",
		SourceRef:      "main",
		ResolvedCommit: "recipe456",
		Content:        "Recipe prompt.",
	}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:               "job-override",
		Agent:            "planner",
		Action:           "ask",
		Repo:             "gitmoot/gitmoot",
		TemplateOverride: &override,
	}); err != nil {
		t.Fatalf("Enqueue with override returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-override")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "review-panel" || payload.TemplateResolvedCommit != "recipe456" || payload.TemplateContent != "Recipe prompt." {
		t.Fatalf("override payload template snapshot = %+v, want the recipe override", payload)
	}

	// Without an override the agent's own template still wins.
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:     "job-no-override",
		Agent:  "planner",
		Action: "ask",
		Repo:   "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue without override returned error: %v", err)
	}
	baselineJob, err := store.GetJob(ctx, "job-no-override")
	if err != nil {
		t.Fatalf("GetJob baseline returned error: %v", err)
	}
	baseline, err := unmarshalPayload(baselineJob.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload baseline returned error: %v", err)
	}
	if baseline.TemplateID != "agent-own" || baseline.TemplateContent != "Agent's own prompt." {
		t.Fatalf("baseline payload template snapshot = %+v, want the agent's own template", baseline)
	}
}

func TestMailboxRunIncludesTemplateSnapshotInPrompt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"clean","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}
	templateContent := agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   "thermo",
		Name:                 "Thermo",
		Description:          "Reviews deeply.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask", "review"},
		RuntimeCompatibility: []string{"codex"},
		Tags:                 []string{"review"},
		Inputs:               []string{"repo", "diff"},
		Outputs:              []string{"review_findings"},
	}, "# Thermo\n\nReview deeply.")
	payload, err := marshalPayload(JobPayload{
		Repo:                   "gitmoot/gitmoot",
		TemplateID:             "thermo",
		TemplateResolvedCommit: "abc123",
		TemplateContent:        templateContent,
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "job-1", Agent: "audit", Type: "review", State: string(JobQueued), Payload: payload}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(adapter.prompts) != 1 ||
		!strings.Contains(adapter.prompts[0], "Template instructions:\n# Thermo\n\nReview deeply.") ||
		strings.Contains(adapter.prompts[0], "kind: agent-template") {
		t.Fatalf("prompt = %+v", adapter.prompts)
	}
}

func TestUnmarshalPayloadMapsLegacyPresetSnapshot(t *testing.T) {
	payload, err := unmarshalPayload(`{"repo":"owner/repo","preset_id":"thermo","preset_resolved_commit":"abc123","preset_content":"Review deeply."}`)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.TemplateID != "thermo" || payload.TemplateResolvedCommit != "abc123" || payload.TemplateContent != "Review deeply." {
		t.Fatalf("legacy preset snapshot mapped to %+v", payload)
	}

	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if strings.Contains(encoded, "preset_") || !strings.Contains(encoded, `"template_id":"thermo"`) {
		t.Fatalf("payload was not rewritten with template fields: %s", encoded)
	}
}

func TestMailboxRunStoresResultAndSucceeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":["mailbox"],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot", Branch: "task-005", PullRequest: 5}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Decision != "implemented" {
		t.Fatalf("decision = %q", result.Decision)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded", stored.State)
	}
	if !strings.Contains(stored.Payload, `"result"`) || !strings.Contains(stored.Payload, `"raw_outputs"`) {
		t.Fatalf("payload = %q", stored.Payload)
	}
	if len(adapter.prompts) != 1 || !strings.Contains(adapter.prompts[0], "Required output") {
		t.Fatalf("prompts = %+v", adapter.prompts)
	}
}

func TestMailboxRunUsesAdapterSummaryWhenAvailable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:audit", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{`{"result":"wrapped by runtime"}`},
		summaries: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"parsed from summary","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "parsed from summary" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(adapter.prompts))
	}
}

func TestMailboxRunRetriesMalformedOutputOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"review complete, no json",
		`{"gitmoot_result":{"decision":"approved","summary":"clean after repair","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "clean after repair" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], "Previous raw output") {
		t.Fatalf("repair prompt = %s", adapter.prompts[1])
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "malformed_output") || !hasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxRunRepairsReviewVerdictMissingSeverity(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"changes_requested","summary":"fix the finding","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		`{"gitmoot_result":{"decision":"changes_requested","summary":"still missing severity","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		`{"gitmoot_result":{"decision":"changes_requested","severity":"P1","summary":"fix the finding","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-review-severity", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-review-severity", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Severity != "P1" {
		t.Fatalf("severity = %q, want P1", result.Severity)
	}
	if len(adapter.prompts) != 3 {
		t.Fatalf("deliveries = %d, want initial delivery plus two repairs", len(adapter.prompts))
	}
	for i, prompt := range adapter.prompts[1:] {
		for _, want := range []string{
			"severity is required when a review requests changes",
			"top-level severity",
			"P0|P1|P2|P3",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("repair prompt %d missing %q:\n%s", i+1, want, prompt)
			}
		}
	}
	if !strings.Contains(adapter.prompts[2], "still missing severity") {
		t.Fatalf("second repair prompt did not include the first repair output:\n%s", adapter.prompts[2])
	}
}

func TestMailboxRunSalvagesMissingEnvelopeAfterSecondRepair(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"findings posted, no json",
		"still no json",
		`{"gitmoot_result":{"decision":"approved","summary":"salvaged after second repair","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	result, err := mailbox.Run(ctx, "job-1", agent, adapter)

	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "salvaged after second repair" {
		t.Fatalf("summary = %q", result.Summary)
	}
	if len(adapter.prompts) != 3 {
		t.Fatalf("deliveries = %d, want 3", len(adapter.prompts))
	}
	if !strings.Contains(adapter.prompts[1], "Previous raw output") {
		t.Fatalf("first repair prompt = %s", adapter.prompts[1])
	}
	if !strings.Contains(adapter.prompts[2], "Previous raw output") {
		t.Fatalf("second repair prompt = %s", adapter.prompts[2])
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded", stored.State)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "malformed_output") || !hasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v", events)
	}
}

func TestMailboxRunFailsAfterExhaustingRepairs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		"no json here",
		"still no json",
		"and again no json",
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run succeeded despite exhausting all repair attempts")
	}
	if len(adapter.prompts) != 3 {
		t.Fatalf("deliveries = %d, want 3 (initial + maxRepairAttempts)", len(adapter.prompts))
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed", stored.State)
	}
}

func TestMailboxRunMarksBlockedDecision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"needs credentials","findings":[],"changes_made":[],"tests_run":[],"needs":["GITHUB_TOKEN"],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobBlocked) {
		t.Fatalf("state = %q, want blocked", stored.State)
	}
}

func TestMailboxRunRejectsNonQueuedJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"approved","summary":"should not run","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
		t.Fatalf("UpdateJobState returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run accepted a non-queued job")
	}
	if len(adapter.prompts) != 0 {
		t.Fatalf("adapter was called for non-queued job: %+v", adapter.prompts)
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
	if strings.Contains(stored.Payload, `"result"`) {
		t.Fatalf("cancelled job stored final result payload: %s", stored.Payload)
	}
}

func TestMailboxRunPreservesCancellationDuringDelivery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"completed after cancellation","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		onDeliver: func() {
			if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run completed a job cancelled during delivery")
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
}

func TestMailboxRunSkipsRepairAfterCancellation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{
		outputs: []string{"malformed"},
		onDeliver: func() {
			if err := store.UpdateJobState(ctx, "job-1", string(JobCancelled)); err != nil {
				t.Fatalf("UpdateJobState returned error: %v", err)
			}
		},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run repaired a job cancelled after malformed output")
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(adapter.prompts))
	}
	stored, err := store.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobCancelled) {
		t.Fatalf("state = %q, want cancelled", stored.State)
	}
}

func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func hasEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

type fakeDelivery struct {
	outputs               []string
	summaries             []string
	providers             []string
	prompts               []string
	models                []string
	efforts               []string
	runtimeDefault        []string
	runtimeDefaultEfforts []string
	agentEnvs             [][]string
	plans                 []bool
	planIntos             []string
	onDeliver             func()
	pid                   int
	err                   error
	errorResult           runtime.Result
}

func (f *fakeDelivery) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.prompts = append(f.prompts, job.Prompt)
	f.models = append(f.models, job.Model)
	f.efforts = append(f.efforts, job.Effort)
	f.runtimeDefault = append(f.runtimeDefault, job.RuntimeDefaultModel)
	f.runtimeDefaultEfforts = append(f.runtimeDefaultEfforts, job.RuntimeDefaultEffort)
	f.agentEnvs = append(f.agentEnvs, append([]string(nil), job.AgentEnv...))
	f.plans = append(f.plans, job.Plan)
	f.planIntos = append(f.planIntos, job.PlanInto)
	if f.pid > 0 && job.OnPID != nil {
		job.OnPID(f.pid)
	}
	if f.onDeliver != nil {
		f.onDeliver()
	}
	if f.err != nil {
		// A REAL adapter returns partial usage and plan evidence on failure. OMP
		// accumulates both before reporting a non-zero exit or truncated stream.
		result := f.errorResult
		result.PlanMode = runtime.PlanModeDescriptor(job.Plan, job.PlanInto)
		return result, f.err
	}
	index := len(f.prompts) - 1
	result := runtime.Result{}
	// Stand in for a plan-capable adapter: report the shape that was dispatched.
	// A job without plan mode yields "", so every non-plan test is unchanged.
	result.PlanMode = runtime.PlanModeDescriptor(job.Plan, job.PlanInto)
	if index >= len(f.outputs) {
		return result, nil
	}
	result.Raw = f.outputs[index]
	if index < len(f.summaries) {
		result.Summary = f.summaries[index]
	}
	if index < len(f.providers) {
		result.UpstreamProvider = f.providers[index]
	}
	return result, nil
}

func TestParseJobPayloadExported(t *testing.T) {
	encoded, err := marshalPayload(JobPayload{Repo: "o/r", PullRequest: 7, Instructions: "do it", Result: &AgentResult{Decision: "approved", Summary: "done"}})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	got, err := ParseJobPayload(encoded)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if got.Repo != "o/r" || got.PullRequest != 7 || got.Result == nil || got.Result.Decision != "approved" {
		t.Fatalf("round-trip wrong: %+v", got)
	}
	// Empty/malformed input errors (caller treats as no detail).
	if _, err := ParseJobPayload(""); err == nil {
		t.Fatal("empty payload should error")
	}
}

// #1753 deleted the cockpit pane wrapper and its three JobPayload fields, but
// every job row enqueued before that still carries "cockpit", "cockpit_session"
// and "cockpit_pane_key" in its persisted JSON. Decoding one must keep working:
// the daemon reads old rows on recovery, and a decode error there would fail the
// job rather than ignore a retired key.
func TestUnmarshalPayloadIgnoresRetiredCockpitKeys(t *testing.T) {
	legacy := `{"repo":"gitmoot/gitmoot","pull_request":7,"instructions":"do it","cockpit":true,"cockpit_session":"review-room","cockpit_pane_key":"seat"}`
	payload, err := unmarshalPayload(legacy)
	if err != nil {
		t.Fatalf("a payload carrying retired cockpit keys must decode: %v", err)
	}
	if payload.Repo != "gitmoot/gitmoot" || payload.PullRequest != 7 || payload.Instructions != "do it" {
		t.Fatalf("surviving fields lost: %+v", payload)
	}
	// JobPayload deliberately round-trips keys it does not know
	// (unknownJSONFields, job_payload_json.go), so re-encoding PRESERVES the
	// retired keys rather than silently rewriting a stored row. Pin that: the
	// alternative — dropping them on the first re-encode — would rewrite history
	// on every payload the daemon touches.
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	for _, key := range []string{`"cockpit":true`, `"cockpit_session":"review-room"`, `"cockpit_pane_key":"seat"`} {
		if !strings.Contains(encoded, key) {
			t.Fatalf("re-encoded payload dropped %s: %s", key, encoded)
		}
	}

	// A payload written TODAY carries none of them.
	fresh, err := marshalPayload(JobPayload{Repo: "gitmoot/gitmoot"})
	if err != nil {
		t.Fatalf("marshalPayload fresh: %v", err)
	}
	if strings.Contains(fresh, "cockpit") {
		t.Fatalf("a freshly built payload still emits a cockpit key: %s", fresh)
	}
}
