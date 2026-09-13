package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
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

func TestOmpProviderFamiliesInformMergeAdvisory(t *testing.T) {
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
				t.Fatalf("distinct proven providers produced a same-family advisory: %s", reason)
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
			if !same || !strings.Contains(reason, tc.nativeRuntime) || !strings.Contains(reason, "advisory") {
				t.Fatalf("OMP provider %q and native runtime %q did not produce a same-family advisory: same=%v reason=%q", tc.ompProvider, tc.nativeRuntime, same, reason)
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
		if !same || !strings.Contains(reason, "omp:devin") || !strings.Contains(reason, "advisory") {
			t.Fatalf("same Devin provider did not produce an advisory: same=%v reason=%q", same, reason)
		}
	})

	t.Run("OMP reviewer reports unavailable in-session implementer provider", func(t *testing.T) {
		store := openTestStore(t)
		insertSucceededOmpProviderJob(t, store, "review-job", "reviewer", "review", "kimi-code/k3", "kimi-code")
		gate := PolicyMergeGate{Store: store}
		same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "reviewer", false, runtime.OmpRuntime, map[string]implementerIdentity{
			"gm-integrity": {Name: "gm-integrity", FromActingRole: true, JobID: "session-implement"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !same || !strings.Contains(reason, "in-session implementer") || !strings.Contains(reason, "provider") || !strings.Contains(reason, "advisory") {
			t.Fatalf("in-session provider gap did not produce an advisory: same=%v reason=%q", same, reason)
		}
	})
}

func TestPolicyMergeGateKeepsRuntimeFamilyDiversityAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name             string
		implementerAgent string
		implementerRole  string
		implementerModel string
		implementer      string
		reviewerAgent    string
		reviewerModel    string
		reviewer         string
		wantMerge        bool
		wantEventKind    string
	}{
		{
			name:            "OMP reviewer over acting-role implementer",
			implementerRole: "gm-findings",
			reviewerAgent:   "gm-review-opus",
			reviewerModel:   "devin/swe-2",
			reviewer:        "devin",
			wantMerge:       true,
			wantEventKind:   mergeGateFamilyUnresolvedEventKind,
		},
		{
			name:             "reviewer is the implementer",
			implementerAgent: "gm-review-opus",
			implementerModel: "devin/swe-2",
			implementer:      "devin",
			reviewerAgent:    "gm-review-opus",
			reviewerModel:    "devin/sonnet-4.6",
			reviewer:         "devin",
			wantMerge:        false,
		},
		{
			name:             "same provider with independent identities",
			implementerAgent: "gm-implementer",
			implementerModel: "devin/swe-2",
			implementer:      "devin",
			reviewerAgent:    "gm-review-opus",
			reviewerModel:    "devin/sonnet-4.6",
			reviewer:         "devin",
			wantMerge:        true,
			wantEventKind:    mergeGateFamilyAdvisoryEventKind,
		},
		{
			name:             "different providers with independent identities",
			implementerAgent: "gm-implementer",
			implementerModel: "openai-codex/gpt-5.6-sol",
			implementer:      "openai-codex",
			reviewerAgent:    "gm-review-opus",
			reviewerModel:    "devin/swe-2",
			reviewer:         "devin",
			wantMerge:        true,
		},
		{
			name:             "unresolved implementer family",
			implementerAgent: "unregistered-implementer",
			reviewerAgent:    "gm-review-opus",
			reviewerModel:    "devin/swe-2",
			reviewer:         "devin",
			wantMerge:        true,
			wantEventKind:    mergeGateFamilyUnresolvedEventKind,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			payload := JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
				HeadSHA: "head123", TaskID: "task-9",
			}

			implementPayload := payload
			implementPayload.HeadSHA = ""
			implementPayload.ActingOrgRole = tc.implementerRole
			implementPayload.EffectiveRuntime = runtime.OmpRuntime
			implementPayload.Model = tc.implementerModel
			implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
			insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: tc.implementerAgent, Type: "implement"}, implementPayload)
			if tc.implementerAgent != "" && tc.implementer != "" {
				seedFamilyAgent(t, store, tc.implementerAgent, runtime.OmpRuntime)
				if err := store.AddJobEvent(ctx, db.JobEvent{
					JobID: "implement-job", Kind: ompProviderVerifiedEventKind,
					Runtime: runtime.OmpRuntime, Provider: tc.implementer,
				}); err != nil {
					t.Fatal(err)
				}
			}

			reviewPayload := payload
			reviewPayload.ReviewRound = "review-1"
			reviewPayload.EffectiveRuntime = runtime.OmpRuntime
			reviewPayload.Model = tc.reviewerModel
			reviewPayload.Result = &AgentResult{
				Decision: "approved", Summary: "approved",
				Evidence: "executed", EvidenceDeclared: true,
				TestsRun: []string{"focused production-path check"},
			}
			insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: tc.reviewerAgent, Type: "review"}, reviewPayload)
			seedFamilyAgent(t, store, tc.reviewerAgent, runtime.OmpRuntime)
			if err := store.AddJobEvent(ctx, db.JobEvent{
				JobID: "review-job", Kind: ompProviderVerifiedEventKind,
				Runtime: runtime.OmpRuntime, Provider: tc.reviewer,
			}); err != nil {
				t.Fatal(err)
			}

			mergeable := true
			gh := &fakeMergeGateGitHub{
				pr: github.PullRequest{
					Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
					HeadSHA: "head123", Mergeable: &mergeable,
				},
				status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
				checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
				mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
			}
			gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}
			decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if decision.Merged != tc.wantMerge || (len(gh.merges) == 1) != tc.wantMerge {
				t.Fatalf("decision=%+v merges=%d, want merge=%v", decision, len(gh.merges), tc.wantMerge)
			}
			if !tc.wantMerge && !strings.Contains(decision.Reason.Render(), "implementing agent") {
				t.Fatalf("identity refusal reason = %q, want implementing agent", decision.Reason.Render())
			}
			events, err := store.ListJobEvents(ctx, "review-job")
			if err != nil {
				t.Fatal(err)
			}
			foundFamilyEvent := ""
			for _, event := range events {
				if event.Kind == mergeGateFamilyAdvisoryEventKind || event.Kind == mergeGateFamilyUnresolvedEventKind {
					foundFamilyEvent = event.Kind
				}
			}
			if foundFamilyEvent != tc.wantEventKind {
				t.Fatalf("family event=%q, want %q; events=%+v", foundFamilyEvent, tc.wantEventKind, events)
			}
		})
	}
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
