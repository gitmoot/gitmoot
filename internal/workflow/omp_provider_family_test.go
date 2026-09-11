package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const approvedProviderTestResult = `{"gitmoot_result":{"decision":"approved","summary":"approved","findings":[],"changes_made":[],"tests_run":["go test ./internal/workflow"],"needs":[],"delegations":[]}}`

func TestMailboxPersistsSuccessfulOmpProviderEvidence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "omp-review", Agent: "reviewer", Action: "review", Repo: "gitmoot/gitmoot", Model: "kimi-code/k3",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &fakeDelivery{outputs: []string{approvedProviderTestResult}, providers: []string{"kimi-code"}}
	if _, err := mailbox.Run(ctx, "omp-review", runtime.Agent{Name: "reviewer", Runtime: runtime.OmpRuntime}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	family, ok, err := ResolveRuntimeFamily(ctx, store, "omp-review", "reviewer", runtime.OmpRuntime)
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if !ok || family != runtime.KimiRuntime {
		t.Fatalf("family = %q, ok=%v; want native Kimi family from successful OMP evidence", family, ok)
	}
	events, err := store.ListJobEvents(ctx, "omp-review")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == ompProviderVerifiedEventKind && event.Runtime == runtime.OmpRuntime && event.Provider == "kimi-code" {
			return
		}
	}
	t.Fatalf("verified provider event missing: %+v", events)
}

func TestOmpProviderEvidenceProductionPath(t *testing.T) {
	ctx := context.Background()
	binDir := t.TempDir()
	stream := `{"type":"session","id":"01a00000-0000-7000-8000-000000000001"}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` +
		strconv.Quote(approvedProviderTestResult) +
		`}],"provider":"kimi-code","model":"k3","usage":{"input":1,"output":1},"stopReason":"stop"}}` + "\n" +
		`{"type":"agent_end","isTerminal":true}` + "\n"
	ompPath := filepath.Join(binDir, "omp")
	if err := os.WriteFile(ompPath, []byte("#!/bin/sh\ncat <<'EOF'\n"+stream+"EOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "omp-production-review", Agent: "reviewer", Action: "review",
		Repo: "gitmoot/gitmoot", Model: "devin/swe-2",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	agent := runtime.Agent{Name: "reviewer", Role: "reviewer", Runtime: runtime.OmpRuntime}
	if _, err := mailbox.Run(ctx, "omp-production-review", agent, runtime.OmpAdapter{Runner: subprocess.ExecRunner{}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	family, ok, err := ResolveRuntimeFamily(ctx, store, "omp-production-review", "reviewer", runtime.OmpRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || family != runtime.KimiRuntime {
		t.Fatalf("production OMP delivery resolved to family %q, ok=%v; want Kimi from runtime output, not requested Devin", family, ok)
	}
}

func TestMailboxPersistsMissingOmpProviderAsFailClosedEvidence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "omp-review-missing-provider", Agent: "reviewer", Action: "review", Repo: "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &fakeDelivery{outputs: []string{approvedProviderTestResult}}
	if _, err := mailbox.Run(ctx, "omp-review-missing-provider", runtime.Agent{Name: "reviewer", Runtime: runtime.OmpRuntime}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "omp-review-missing-provider")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == ompProviderVerifiedEventKind && event.Runtime == runtime.OmpRuntime && event.Provider == "" {
			family, ok, err := ResolveRuntimeFamily(ctx, store, "omp-review-missing-provider", "reviewer", runtime.OmpRuntime)
			if err != nil {
				t.Fatal(err)
			}
			if ok || family != "" {
				t.Fatalf("missing-provider delivery resolved to family %q, ok=%v", family, ok)
			}
			return
		}
	}
	t.Fatalf("missing-provider evidence tombstone absent: %+v", events)
}

func TestOmpProviderFamilyRequiresSuccessfulExecutionEvidence(t *testing.T) {
	ctx := context.Background()
	t.Run("prospective OMP registry runtime is unverified", func(t *testing.T) {
		store := openTestStore(t)
		seedFamilyAgent(t, store, "prospective-reviewer", runtime.OmpRuntime)
		family, ok, err := ResolveRuntimeFamily(ctx, store, "", "prospective-reviewer", "")
		if err != nil {
			t.Fatal(err)
		}
		if ok || family != "" {
			t.Fatalf("prospective OMP reviewer resolved from caller-only data to family %q, ok=%v", family, ok)
		}
	})

	t.Run("requested model alone is unverified", func(t *testing.T) {
		store := openTestStore(t)
		insertCompletedJob(t, store, db.Job{ID: "spoofed", Agent: "implementer", Type: "implement"}, JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 1, EffectiveRuntime: runtime.OmpRuntime, Model: "devin/swe-2",
		})
		family, ok, err := ResolveRuntimeFamily(ctx, store, "spoofed", "implementer", runtime.OmpRuntime)
		if err != nil {
			t.Fatal(err)
		}
		if ok || family != "" {
			t.Fatalf("caller-supplied model manufactured family %q, ok=%v", family, ok)
		}
	})

	t.Run("failed job evidence is unavailable", func(t *testing.T) {
		store := openTestStore(t)
		seedFamilyAgent(t, store, "failed-reviewer", runtime.OmpRuntime)
		payload, err := marshalPayload(JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 2, EffectiveRuntime: runtime.OmpRuntime, Model: "kimi-code/k3"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CreateJobWithEvent(ctx, db.Job{ID: "failed", Agent: "failed-reviewer", Type: "review", State: string(JobFailed), Payload: payload}, db.JobEvent{
			Kind: ompProviderVerifiedEventKind, Runtime: runtime.OmpRuntime, Provider: "kimi-code", Message: "failed-path spoof",
		}); err != nil {
			t.Fatal(err)
		}
		family, ok, err := ResolveRuntimeFamily(ctx, store, "failed", "failed-reviewer", runtime.OmpRuntime)
		if err != nil {
			t.Fatal(err)
		}
		if ok || family != "" {
			t.Fatalf("failed job manufactured family %q, ok=%v", family, ok)
		}
	})
	t.Run("latest missing evidence invalidates an earlier attempt", func(t *testing.T) {
		store := openTestStore(t)
		insertSucceededOmpProviderJob(t, store, "retried", "reviewer", "review", "kimi-code/k3", "kimi-code")
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID: "retried", Kind: ompProviderVerifiedEventKind, Runtime: runtime.OmpRuntime,
			Message: "later successful delivery omitted provider metadata",
		}); err != nil {
			t.Fatal(err)
		}
		family, ok, err := ResolveRuntimeFamily(ctx, store, "retried", "reviewer", runtime.OmpRuntime)
		if err != nil {
			t.Fatal(err)
		}
		if ok || family != "" {
			t.Fatalf("later missing evidence inherited stale family %q, ok=%v", family, ok)
		}
	})
}

func TestOmpProviderFamiliesDriveMergeIndependence(t *testing.T) {
	ctx := context.Background()
	for _, reviewerProvider := range []string{"kimi-code", "devin"} {
		t.Run("openai versus "+reviewerProvider, func(t *testing.T) {
			store := openTestStore(t)
			insertSucceededOmpProviderJob(t, store, "implement-job", "implementer", "implement", "openai-codex/gpt-5.6-sol", "openai-codex")
			insertSucceededOmpProviderJob(t, store, "review-job", "reviewer", "review", reviewerProvider+"/model", reviewerProvider)
			gate := PolicyMergeGate{Store: store}
			same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "reviewer", false, runtime.OmpRuntime, map[string]implementerIdentity{
				"implementer": {Name: "implementer", JobID: "implement-job", RecordedRuntime: runtime.OmpRuntime},
			})
			if err != nil {
				t.Fatal(err)
			}
			if same {
				t.Fatalf("distinct proven providers were refused: %s", reason)
			}
		})
	}

	for _, tc := range []struct {
		name          string
		nativeRuntime string
		ompProvider   string
	}{
		{name: "Anthropic matches Claude", nativeRuntime: runtime.ClaudeRuntime, ompProvider: "anthropic"},
		{name: "Kimi Code matches Kimi", nativeRuntime: runtime.KimiRuntime, ompProvider: "kimi-code"},
		{name: "OpenAI matches Codex", nativeRuntime: runtime.CodexRuntime, ompProvider: "openai"},
		{name: "OpenAI Codex matches Codex", nativeRuntime: runtime.CodexRuntime, ompProvider: "openai-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			insertSucceededNativeFamilyJob(t, store, "implement-job", "implementer", "implement", tc.nativeRuntime)
			insertSucceededOmpProviderJob(t, store, "review-job", "reviewer", "review", tc.ompProvider+"/model", tc.ompProvider)
			gate := PolicyMergeGate{Store: store}
			same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "reviewer", false, runtime.OmpRuntime, map[string]implementerIdentity{
				"implementer": {Name: "implementer", JobID: "implement-job", RecordedRuntime: tc.nativeRuntime},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !same || !strings.Contains(reason, tc.nativeRuntime) {
				t.Fatalf("OMP provider %q and native runtime %q were not one family: same=%v reason=%q", tc.ompProvider, tc.nativeRuntime, same, reason)
			}
		})
	}

	t.Run("different Devin models and agents remain same family", func(t *testing.T) {
		store := openTestStore(t)
		insertSucceededOmpProviderJob(t, store, "implement-job", "implementer", "implement", "devin/swe-2", "devin")
		insertSucceededOmpProviderJob(t, store, "review-job", "other-reviewer", "review", "devin/sonnet-4.6", "devin")
		gate := PolicyMergeGate{Store: store}
		same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "other-reviewer", false, runtime.OmpRuntime, map[string]implementerIdentity{
			"implementer": {Name: "implementer", JobID: "implement-job", RecordedRuntime: runtime.OmpRuntime},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !same || !strings.Contains(reason, "omp:devin") {
			t.Fatalf("same Devin provider was not refused with evidence: same=%v reason=%q", same, reason)
		}
	})

	t.Run("OMP reviewer cannot prove an in-session implementer provider", func(t *testing.T) {
		store := openTestStore(t)
		insertSucceededOmpProviderJob(t, store, "review-job", "reviewer", "review", "kimi-code/k3", "kimi-code")
		gate := PolicyMergeGate{Store: store}
		same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "reviewer", false, runtime.OmpRuntime, map[string]implementerIdentity{
			"gm-integrity": {Name: "gm-integrity", FromActingRole: true, JobID: "session-implement"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !same || !strings.Contains(reason, "in-session implementer") || !strings.Contains(reason, "provider") {
			t.Fatalf("in-session provider gap did not fail closed: same=%v reason=%q", same, reason)
		}
	})
}

func insertSucceededNativeFamilyJob(t *testing.T, store *db.Store, jobID, agent, action, runtimeName string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: jobID, Agent: agent, Type: action}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, EffectiveRuntime: runtimeName,
		Result: &AgentResult{Decision: "approved", Summary: "done", TestsRun: []string{"focused check"}},
	})
	seedFamilyAgent(t, store, agent, runtimeName)
}

func insertSucceededOmpProviderJob(t *testing.T, store *db.Store, jobID, agent, action, model, provider string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: jobID, Agent: agent, Type: action}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, EffectiveRuntime: runtime.OmpRuntime, Model: model,
		Result: &AgentResult{Decision: "approved", Summary: "done", TestsRun: []string{"focused check"}},
	})
	seedFamilyAgent(t, store, agent, runtime.OmpRuntime)
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID: jobID, Kind: ompProviderVerifiedEventKind, Runtime: runtime.OmpRuntime, Provider: provider, Message: "successful OMP delivery verified upstream provider",
	}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
}
