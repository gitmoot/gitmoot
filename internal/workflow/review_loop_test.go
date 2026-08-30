package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

var _ func(context.Context, *db.Store, string, int, string, []string) (ReviewLoopMatch, bool, error) = DetectReviewLoop

// Review-loop tests pin the agent-identity boundary. Runtime family is
// deliberately not part of the refusal: a different agent of the same family
// remains an independent reviewer for the merge gate.

func seedReviewLoopAgent(t *testing.T, store *db.Store, name, runtimeName, model string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "agent",
		Runtime:        runtimeName,
		RuntimeRef:     "ref-" + name,
		RepoScope:      "owner/repo",
		Model:          model,
		Capabilities:   []string{"review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent(%s): %v", name, err)
	}
}

func seedReviewLoopVerdict(t *testing.T, store *db.Store, jobID, agent, headSHA, decision, effectiveRuntime string) {
	t.Helper()
	ctx := context.Background()
	encoded, err := marshalPayload(JobPayload{
		Repo: "owner/repo", Branch: "main", PullRequest: 227, HeadSHA: headSHA,
		TaskID: "review-pr-227", ReviewRound: "review-1",
		EffectiveRuntime: effectiveRuntime,
		Result:           &AgentResult{Decision: decision, Summary: "historical evidence"},
	})
	if err != nil {
		t.Fatalf("marshalPayload(%s): %v", jobID, err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: agent, Type: "review", State: string(JobSucceeded), Payload: encoded,
	}, db.JobEvent{Kind: string(JobSucceeded), Message: decision}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	decoded, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(%s): %v", jobID, err)
	}
	if decoded.HeadSHA != headSHA || decoded.Result == nil || decoded.Result.Decision != decision {
		t.Fatalf("seeded verdict %q drifted: head=%q decision=%+v, want head=%q decision=%q",
			jobID, decoded.HeadSHA, decoded.Result, headSHA, decision)
	}
	if decoded.EffectiveRuntime != effectiveRuntime {
		t.Fatalf("seeded verdict %q effective_runtime = %q, want %q", jobID, decoded.EffectiveRuntime, effectiveRuntime)
	}
}

func TestDetectReviewLoopSameAgentSameHeadRefused(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "review-g7", "g7-review", "head-a", "approved", "codex")

	match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"})
	if err != nil {
		t.Fatalf("DetectReviewLoop: %v", err)
	}
	if !detected {
		t.Fatal("same agent at the same head must be refused")
	}
	if match.Agent != "g7-review" || match.JobID != "review-g7" || match.Decision != "approved" {
		t.Fatalf("match = %+v", match)
	}
	if !strings.Contains(match.Reason(), "agent g7-review already holds") {
		t.Fatalf("reason = %q", match.Reason())
	}

	events, err := store.ListJobEvents(ctx, "review-g7")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var claims int
	for _, event := range events {
		if event.Kind == ReviewLoopDetectedEventKind {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("review-loop claim count = %d, want 1", claims)
	}
}

func TestDetectReviewLoopDifferentAgentSameFamilyAllowed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopAgent(t, store, "g6-review-sol", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "review-g7", "g7-review", "head-a", "approved", "codex")

	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"}); err != nil || !detected {
		t.Fatalf("control same-agent detection = %v, err=%v; want true", detected, err)
	}
	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"g6-review-sol"}); err != nil {
		t.Fatalf("DetectReviewLoop different agent: %v", err)
	} else if detected {
		t.Fatal("different agent in the same runtime family must remain eligible")
	}
}

func TestFindRepeatedReviewersFiltersOnlyAgentsWithVerdicts(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopAgent(t, store, "g6-review-sol", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "review-g7", "g7-review", "head-a", "changes_requested", "codex")

	matches, err := FindRepeatedReviewers(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review", "g6-review-sol"})
	if err != nil {
		t.Fatalf("FindRepeatedReviewers: %v", err)
	}
	if len(matches) != 1 || matches[0].Agent != "g7-review" {
		t.Fatalf("matches = %+v, want only g7-review", matches)
	}
}

func TestDetectReviewLoopSameAgentNewHeadAllowed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "review-g7", "g7-review", "head-a", "approved", "codex")

	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"}); err != nil || !detected {
		t.Fatalf("control same-head detection = %v, err=%v; want true", detected, err)
	}
	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-b", []string{"g7-review"}); err != nil {
		t.Fatalf("DetectReviewLoop new head: %v", err)
	} else if detected {
		t.Fatal("a new head must permit the same reviewer")
	}
}

func TestDetectReviewLoopEmptyHeadRule(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")

	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "", []string{"g7-review"}); err != nil {
		t.Fatalf("DetectReviewLoop before history: %v", err)
	} else if detected {
		t.Fatal("empty head before any succeeded history must be allowed")
	}
	seedReviewLoopVerdict(t, store, "review-g7", "g7-review", "head-a", "approved", "codex")
	match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "", []string{"g7-review"})
	if err != nil {
		t.Fatalf("DetectReviewLoop after history: %v", err)
	}
	if !detected || !match.EmptyHead {
		t.Fatalf("empty-head match = %+v, detected=%v", match, detected)
	}
}

// Runtime-family resolution still backs native one-family selection: a recorded
// effective runtime wins, the registry covers unrecorded jobs, and unknown
// agents report ok=false.
func TestResolveRuntimeFamilyPrecedence(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "wave-impl", "codex", "gpt-5.6-sol")

	for _, tc := range []struct {
		name     string
		agent    string
		recorded string
		want     string
		wantOK   bool
	}{
		{name: "recorded beats registry", agent: "wave-impl", recorded: "kimi", want: "kimi", wantOK: true},
		{name: "recorded normalized", agent: "wave-impl", recorded: " Kimi ", want: "kimi", wantOK: true},
		{name: "registry fallback", agent: "wave-impl", recorded: "", want: "codex", wantOK: true},
		{name: "unregistered with recording", agent: "ghost", recorded: "kimi", want: "kimi", wantOK: true},
		{name: "unregistered without recording", agent: "ghost", recorded: "", want: "", wantOK: false},
		{name: "empty name without recording", agent: "", recorded: "", want: "", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			family, ok, err := ResolveRuntimeFamily(ctx, store, tc.agent, tc.recorded)
			if err != nil {
				t.Fatalf("ResolveRuntimeFamily: %v", err)
			}
			if family != tc.want || ok != tc.wantOK {
				t.Fatalf("ResolveRuntimeFamily(%q, %q) = (%q, %v), want (%q, %v)",
					tc.agent, tc.recorded, family, ok, tc.want, tc.wantOK)
			}
		})
	}
}
