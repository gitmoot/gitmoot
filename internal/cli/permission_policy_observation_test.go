package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const permissionPolicySuccessfulResult = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

type permissionPolicyTestAdapter struct {
	property runtime.PermissionPolicyApplication
	calls    int
}

func (a *permissionPolicyTestAdapter) PermissionPolicyApplication(runtime.Agent) runtime.PermissionPolicyApplication {
	return a.property
}

func (a *permissionPolicyTestAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	a.calls++
	return runtime.Result{Raw: permissionPolicySuccessfulResult}, nil
}

func runPermissionPolicyJob(t *testing.T, property runtime.PermissionPolicyApplication) (*db.Store, db.Job, *permissionPolicyTestAdapter) {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "policy-agent", runtime.ShellRuntime, "printf done", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "policy-job", Agent: "policy-agent", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "policy-job")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &permissionPolicyTestAdapter{property: property}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) { return adapter, nil }
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	return store, job, adapter
}

func TestPermissionPolicyNotAppliedWarnsAndJobRuns(t *testing.T) {
	store, job, adapter := runPermissionPolicyJob(t, runtime.PermissionPolicyNotApplied)
	if adapter.calls != 1 {
		t.Fatalf("delivery calls = %d, want 1: warning must not refuse the job", adapter.calls)
	}
	stored, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", stored.State)
	}
	events := mustListJobEvents(t, store, job.ID)
	var warnings []permissionpolicy.Warning
	for _, event := range events {
		if event.Kind != permissionpolicy.WarningEventKind {
			continue
		}
		var warning permissionpolicy.Warning
		if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
			t.Fatalf("decode warning: %v", err)
		}
		warnings = append(warnings, warning)
	}
	if len(warnings) != 1 {
		t.Fatalf("permission-policy warnings = %#v, want exactly one", warnings)
	}
	warning := warnings[0]
	if warning.Runtime != runtime.ShellRuntime || warning.Policy != runtime.AutonomyPolicyAuto || warning.Capability != "ask" || warning.JobID != job.ID || warning.Property != runtime.PermissionPolicyNotApplied || warning.Agent != "policy-agent" {
		t.Fatalf("warning fields = %#v, want all dispatch dimensions", warning)
	}
	if warning.Warning != permissionpolicy.WarningText {
		t.Fatalf("warning text = %q, want %q", warning.Warning, permissionpolicy.WarningText)
	}
}

func TestPermissionPolicyAppliedDoesNotWarn(t *testing.T) {
	store, job, adapter := runPermissionPolicyJob(t, runtime.PermissionPolicyApplied)
	if adapter.calls != 1 {
		t.Fatalf("delivery calls = %d, want 1", adapter.calls)
	}
	for _, event := range mustListJobEvents(t, store, job.ID) {
		if event.Kind == permissionpolicy.WarningEventKind {
			t.Fatalf("applied policy emitted warning: %s", event.Message)
		}
	}
}

func TestPermissionPolicyUnresolvedAgentWarns(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "missing-agent-job", Agent: "deleted-agent", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "missing-agent-job")
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard)
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	events := mustListJobEvents(t, store, job.ID)
	for _, event := range events {
		if event.Kind != permissionpolicy.WarningEventKind {
			continue
		}
		var warning permissionpolicy.Warning
		if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
			t.Fatal(err)
		}
		if warning.Property != runtime.PermissionPolicyUnresolved || warning.Agent != "deleted-agent" || warning.Runtime != "" || warning.Policy != "" {
			t.Fatalf("unresolved warning = %#v", warning)
		}
		return
	}
	t.Fatal("missing agent row emitted no unresolved permission-policy warning")
}

func TestPermissionPolicyDoctorReportsLiveDelta(t *testing.T) {
	ctx := context.Background()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "new-auto", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", AutonomyPolicy: runtime.AutonomyPolicyAuto, Capabilities: []string{"ask"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InitializePermissionPolicyObservationBaseline(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	check, ok := permissionPolicyObservationDoctorCheck(paths)
	if !ok || check.OK || check.Detail != "current=1 baseline=0 delta=+1" {
		t.Fatalf("doctor check = %#v, present=%t", check, ok)
	}
}

func TestPermissionPolicyDoctorRejectsEqualCountChurn(t *testing.T) {
	ctx := context.Background()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	baseline := permissionpolicy.Keys([]permissionpolicy.Config{
		{Agent: "baseline-a", Runtime: runtime.ShellRuntime, Policy: runtime.AutonomyPolicyAuto, Property: runtime.PermissionPolicyNotApplied},
		{Agent: "baseline-b", Runtime: runtime.ShellRuntime, Policy: runtime.AutonomyPolicyAuto, Property: runtime.PermissionPolicyNotApplied},
		{Agent: "baseline-c", Runtime: runtime.ShellRuntime, Policy: runtime.AutonomyPolicyAuto, Property: runtime.PermissionPolicyNotApplied},
	})
	if _, _, err := store.InitializePermissionPolicyObservationBaseline(ctx, baseline); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"new-x", "new-y", "new-z"} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", AutonomyPolicy: runtime.AutonomyPolicyAuto, Capabilities: []string{"ask"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	check, ok := permissionPolicyObservationDoctorCheck(paths)
	if !ok || check.OK || check.Detail != "current=3 baseline=3 delta=+0" {
		t.Fatalf("doctor equal-count churn check = %#v, present=%t; new configs must not report OK", check, ok)
	}
}

func TestPermissionPolicyDoctorReportsUnresolvedHistoryWithoutGatingRatchet(t *testing.T) {
	ctx := context.Background()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "live-auto", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", AutonomyPolicy: runtime.AutonomyPolicyAuto, Capabilities: []string{"ask"}}); err != nil {
		t.Fatal(err)
	}
	configs, err := permissionpolicy.Inventory(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InitializePermissionPolicyObservationBaseline(ctx, permissionpolicy.Keys(configs)); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"ephemeral-history-a", "ephemeral-history-b"} {
		if err := store.CreateJob(ctx, db.Job{ID: "job-" + agent, Agent: agent, Type: "ask", State: "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	ratchet, ok := permissionPolicyObservationDoctorCheck(paths)
	if !ok || !ratchet.OK || ratchet.Detail != "current=1 baseline=1 delta=+0" {
		t.Fatalf("ratchet doctor check = %#v, present=%t; unresolved history must not gate OK", ratchet, ok)
	}
	history, ok := permissionPolicyUnresolvedJobHistoryDoctorCheck(paths)
	if !ok || !history.OK || history.Name != "permission-policy unresolved job history" || history.Detail != "count=2; historical jobs excluded from the live remediation baseline" {
		t.Fatalf("unresolved-history doctor check = %#v, present=%t", history, ok)
	}
}

func TestPermissionPolicyTransientAgentLookupDoesNotClaimUnresolved(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "policy-agent", runtime.ShellRuntime, "printf done", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "transient-lookup-job", Agent: "policy-agent", Action: "ask", Repo: "owner/repo", Branch: "main"})
	job, err := store.GetJob(ctx, "transient-lookup-job")
	if err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("database is locked")
	var output bytes.Buffer
	worker := defaultJobWorker(store, &output)
	worker.AgentLookup = func(context.Context, string) (db.Agent, error) {
		return db.Agent{}, lookupErr
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, event := range mustListJobEvents(t, store, job.ID) {
		if event.Kind == permissionpolicy.WarningEventKind {
			t.Fatalf("transient agent lookup wrote unresolved claim: %s", event.Message)
		}
	}
	if !strings.Contains(output.String(), "permission-policy observation skipped: agent lookup failed: database is locked") {
		t.Fatalf("worker output = %q, want transient lookup failure log", output.String())
	}
}
