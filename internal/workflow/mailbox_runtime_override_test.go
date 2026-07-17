package workflow

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestMailboxRunSkipsRefreshedRefPersistForRuntimeOverride pins the #531
// session-safety invariant at the engine chokepoint: a job running under a
// per-job runtime override must NEVER write a refreshed session ref back onto
// the agent row — the stored ref belongs to the agent's DEFAULT runtime, and
// persisting an override-runtime ref there would corrupt the agent's session
// identity. Without the guard this test goes red: the fake delivery returns a
// RefreshedRuntimeRef and the stored codex agent would be re-pinned to a
// claude UUID.
func TestMailboxRunSkipsRefreshedRefPersistForRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	defaultRef := "codex-default-session"
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:       "shipper",
		Role:       "implementer",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: defaultRef,
		RepoScope:  "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}
	// The EFFECTIVE agent an override job runs as: override runtime + per-job ref.
	effective := runtime.Agent{Name: "shipper", Runtime: runtime.ClaudeRuntime, RuntimeRef: freshRef, RepoScope: "gitmoot/gitmoot", Role: "implementer"}
	adapter := &fakeDelivery{
		outputs: []string{
			`{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`,
		},
		refreshedRefs: []string{"550e8400-e29b-41d4-a716-446655440099"},
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                 "job-override",
		Agent:              "shipper",
		Action:             "ask",
		Repo:               "gitmoot/gitmoot",
		RuntimeOverride:    runtime.ClaudeRuntime,
		RuntimeOverrideRef: freshRef,
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-override", effective, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stored, err := store.GetAgent(ctx, "shipper")
	if err != nil {
		t.Fatalf("GetAgent returned error: %v", err)
	}
	if stored.Runtime != runtime.CodexRuntime || stored.RuntimeRef != defaultRef {
		t.Fatalf("override job polluted the agent's default-runtime session: runtime=%q ref=%q", stored.Runtime, stored.RuntimeRef)
	}
	events, err := store.ListJobEvents(ctx, "job-override")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasEvent(events, "session_refresh_retry") {
		t.Fatalf("override job must not emit session_refresh_retry (no re-pin), events=%+v", events)
	}
}

// TestMailboxEnqueuePersistsRuntimeOverride: the payload round-trips the
// override so a background daemon job honors it identically to foreground.
func TestMailboxEnqueuePersistsRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                 "job-override",
		Agent:              "shipper",
		Action:             "ask",
		Repo:               "gitmoot/gitmoot",
		RuntimeOverride:    runtime.ShellRuntime,
		RuntimeOverrideRef: "printf ok",
		ShellEnv:           []string{"GITMOOT_TRIGGER_BODY=first\n第二"},
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.RuntimeOverride != runtime.ShellRuntime || payload.RuntimeOverrideRef != "printf ok" {
		t.Fatalf("payload override = %q/%q", payload.RuntimeOverride, payload.RuntimeOverrideRef)
	}
	if !reflect.DeepEqual(payload.ShellEnv, []string{"GITMOOT_TRIGGER_BODY=first\n第二"}) {
		t.Fatalf("payload shell_env = %#v", payload.ShellEnv)
	}
}

func TestMailboxRunThreadsShellEnvironment(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	env := []string{"GITMOOT_TRIGGER_BODY=first\n第二", "GITMOOT_TRIGGER_SUBJECT=Snowman ☃"}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-shell-env", Agent: "runner", Action: "ask", Repo: "owner/repo", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok", ShellEnv: env}); err != nil {
		t.Fatal(err)
	}
	adapter := &fakeDelivery{outputs: []string{`{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}}
	agent := runtime.Agent{Name: "runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "runner"}
	if _, err := mailbox.Run(ctx, "job-shell-env", agent, adapter); err != nil {
		t.Fatal(err)
	}
	if len(adapter.shellEnvs) != 1 || !reflect.DeepEqual(adapter.shellEnvs[0], env) {
		t.Fatalf("delivered shell env = %#v", adapter.shellEnvs)
	}
}

func TestMailboxPipelineInputEnvDeliveryNeverEntersPromptOrFreeFormResult(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	const sentinel = "SERVICE_INPUT_SENTINEL_1011"
	inputEnv := []string{"GITMOOT_INPUT_APP_NAME=" + sentinel, "GITMOOT_INPUT_COUNT=3"}
	const resultJSON = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "service-shell-env", Agent: "runner", Action: "ask", Repo: "owner/repo",
		Instructions: "run the typed service stage", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok",
		ShellEnv: []string{"GITMOOT_PIPELINE_NAME=kit"}, PipelineInputEnv: inputEnv,
	}); err != nil {
		t.Fatal(err)
	}
	shellDelivery := &fakeDelivery{outputs: []string{resultJSON}}
	if _, err := mailbox.Run(ctx, "service-shell-env", runtime.Agent{Name: "runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo"}, shellDelivery); err != nil {
		t.Fatal(err)
	}
	wantShellEnv := []string{"GITMOOT_PIPELINE_NAME=kit", "GITMOOT_INPUT_APP_NAME=" + sentinel, "GITMOOT_INPUT_COUNT=3"}
	if !reflect.DeepEqual(shellDelivery.shellEnvs[0], wantShellEnv) {
		t.Fatalf("shell env = %#v, want %#v", shellDelivery.shellEnvs[0], wantShellEnv)
	}
	if len(shellDelivery.agentEnvs[0]) != 0 {
		t.Fatalf("shell delivery populated AgentEnv: %#v", shellDelivery.agentEnvs[0])
	}

	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "service-agent-env", Agent: "analyst", Action: "ask", Repo: "owner/repo",
		Instructions: "analyze the typed service stage", PipelineInputEnv: inputEnv,
	}); err != nil {
		t.Fatal(err)
	}
	agentDelivery := &fakeDelivery{outputs: []string{resultJSON}}
	if _, err := mailbox.Run(ctx, "service-agent-env", runtime.Agent{Name: "analyst", Runtime: runtime.CodexRuntime, RuntimeRef: runtime.FreshRefForJob("service-agent-env"), RepoScope: "owner/repo"}, agentDelivery); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agentDelivery.agentEnvs[0], inputEnv) {
		t.Fatalf("agent env = %#v, want %#v", agentDelivery.agentEnvs[0], inputEnv)
	}
	if len(agentDelivery.shellEnvs[0]) != 0 {
		t.Fatalf("agent delivery populated ShellEnv: %#v", agentDelivery.shellEnvs[0])
	}

	for _, deliveredPrompt := range []string{shellDelivery.prompts[0], agentDelivery.prompts[0]} {
		if strings.Contains(deliveredPrompt, sentinel) {
			t.Fatalf("typed input entered delivered prompt: %q", deliveredPrompt)
		}
	}
	for _, jobID := range []string{"service-shell-env", "service-agent-env"} {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(payload.PipelineInputEnv, "\n"), sentinel) {
			t.Fatalf("%s lost dedicated typed input field: %+v", jobID, payload.PipelineInputEnv)
		}
		freeForm := payload.Instructions + "\n" + strings.Join(payload.RawOutputs, "\n")
		if payload.Result != nil {
			freeForm += "\n" + payload.Result.Summary
		}
		if strings.Contains(freeForm, sentinel) {
			t.Fatalf("%s persisted typed input in prompt/result free-form fields: %q", jobID, freeForm)
		}
	}
}

func TestMailboxRejectsUnreservedPipelineInputEnv(t *testing.T) {
	mailbox := Mailbox{Store: openTestStore(t)}
	for _, env := range [][]string{{"PATH=/tmp"}, {"GITMOOT_PIPELINE_NAME=x"}, {"GITMOOT_INPUT_app=x"}, {"GITMOOT_INPUT_APP=x", "GITMOOT_INPUT_APP=y"}} {
		if _, err := mailbox.Enqueue(context.Background(), JobRequest{ID: "bad-input-env", Agent: "runner", Action: "ask", Repo: "owner/repo", PipelineInputEnv: env}); err == nil {
			t.Fatalf("Enqueue accepted pipeline input env %#v", env)
		}
	}
}

func TestMailboxShellUpstreamContextPersistsAcrossReopenAndDelivery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	contextJSON := "{\"schema_version\":1,\"complete\":true,\"stages\":{\"source\":{\"summary\":\"first\\n第二\"}}}"
	request := JobRequest{
		ID: "job-shell-context", Agent: "runner", Action: "ask", Repo: "owner/repo",
		RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok",
		ShellUpstreamContext: contextJSON,
	}
	job, err := (Mailbox{Store: store}).Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(job.Payload, "gitmoot-pipeline-upstream-") {
		t.Fatalf("payload persisted a temporary path: %s", job.Payload)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reopened, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := ParseJobPayload(reopened.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ShellUpstreamContext != contextJSON {
		t.Fatalf("reopened context = %q, want byte-identical %q", payload.ShellUpstreamContext, contextJSON)
	}

	adapter := &fakeDelivery{outputs: []string{`{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}}
	agent := runtime.Agent{Name: "runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "runner"}
	if _, err := (Mailbox{Store: store}).Run(ctx, job.ID, agent, adapter); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(adapter.shellUpstreamContexts, []string{contextJSON}) {
		t.Fatalf("delivered contexts = %#v", adapter.shellUpstreamContexts)
	}

	plain, err := (Mailbox{Store: store}).Enqueue(ctx, JobRequest{ID: "job-shell-plain", Agent: "runner", Action: "ask", Repo: "owner/repo", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Payload, "shell_upstream_context") {
		t.Fatalf("omitempty field present on unlabeled job: %s", plain.Payload)
	}
}

func TestMailboxShellUpstreamContextFailureRedactsPersistedTempPath(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	secretTempPrefix := filepath.Join(t.TempDir(), "operator-private-temp")
	t.Setenv("TMPDIR", secretTempPrefix) // Deliberately absent: CreateTemp must fail.
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-shell-context-redaction", Agent: "runner", Action: "ask", Repo: "owner/repo",
		RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: "printf ok",
		ShellUpstreamContext: `{"schema_version":1,"complete":true,"stages":{}}`,
	}); err != nil {
		t.Fatal(err)
	}
	agent := runtime.Agent{Name: "runner", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "owner/repo", Role: "runner"}
	if _, err := mailbox.Run(ctx, "job-shell-context-redaction", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded with an unavailable temp directory")
	} else if strings.Contains(err.Error(), secretTempPrefix) {
		t.Fatalf("returned delivery error leaked temp prefix: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "job-shell-context-redaction")
	if err != nil {
		t.Fatal(err)
	}
	var persisted strings.Builder
	for _, event := range events {
		persisted.WriteString(event.Message)
		persisted.WriteByte('\n')
	}
	if strings.Contains(persisted.String(), secretTempPrefix) || strings.Contains(persisted.String(), "gitmoot-pipeline-upstream-") {
		t.Fatalf("persisted failure leaked temporary path:\n%s", persisted.String())
	}
	if !strings.Contains(persisted.String(), "upstream context file: <redacted>: create failed") {
		t.Fatalf("persisted failure lacks redacted context:\n%s", persisted.String())
	}
}

// TestMailboxEnqueueRejectsInvalidRuntimeOverride: an invalid override fails
// at the enqueue chokepoint — BEFORE a job row exists — with an actionable
// error, for every producer.
func TestMailboxEnqueueRejectsInvalidRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{Store: store}
	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}

	for name, request := range map[string]JobRequest{
		"unknown runtime":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"},
		"missing ref":          {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ClaudeRuntime},
		"ref without override": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverrideRef: "x"},
		"shell fresh ref":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: freshRef},
		// "last" names no concrete session, so its lock key could never serialize
		// with the concrete session it would actually resume.
		"last ref on a resumable runtime": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ClaudeRuntime, RuntimeOverrideRef: runtime.LastRef},
	} {
		if _, err := mailbox.Enqueue(ctx, request); err == nil {
			t.Fatalf("%s: Enqueue accepted an invalid runtime override", name)
		}
		if _, err := store.GetJob(ctx, "job-bad"); err == nil {
			t.Fatalf("%s: an invalid override must not enqueue a job", name)
		}
	}

	// The unknown-runtime error enumerates the supported registry.
	_, err = mailbox.Enqueue(ctx, JobRequest{ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"})
	if err == nil {
		t.Fatal("Enqueue accepted an unknown override runtime")
	}
	for _, supported := range runtime.SupportedRuntimes() {
		if !strings.Contains(err.Error(), supported) {
			t.Fatalf("error %q must enumerate supported runtime %q", err.Error(), supported)
		}
	}
}
