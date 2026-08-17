package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

var _ func(context.Context, *db.Store, string, int, string, []string) (ReviewLoopMatch, bool, error) = DetectReviewLoop

// Review-loop family-count tests (#1528). The guard must count DISTINCT
// REVIEWER FAMILIES at the head, not agreement among whoever happened to run.
// Every test pins the fixture property it is named for — asserted or derived
// from the store, never assumed — so a silently weakened fixture goes red
// instead of passing for the wrong reason.

// seedReviewLoopAgent registers an agent with an EXPLICIT runtime (and model),
// because family — not agent name — is what the guard must count.
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

// seedReviewLoopVerdict stores a succeeded review job and READS IT BACK,
// asserting the fixture's distinguishing properties (head, decision, recorded
// runtime) actually landed — a fixture that can be silently weakened is not a
// test.
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

// Acceptance 1 — same family, same head, unanimous approval: STILL REFUSED.
// The anti-loop property itself; must fail if the guard is removed.
func TestDetectReviewLoopSameFamilySameHeadRefused(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "prior-review", "g7-review", "head-a", "approved", "codex")

	match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"g7-review"})
	if err != nil {
		t.Fatalf("DetectReviewLoop: %v", err)
	}
	if !detected {
		t.Fatal("same family re-reviewing a unanimously approved head must be refused")
	}
	// Pin the fixture: the requester IS the verdict's agent.
	if match.Agent != "g7-review" {
		t.Fatalf("matched agent = %q, want g7-review (fixture weakened)", match.Agent)
	}
	if match.Family != "codex" {
		t.Fatalf("match.Family = %q, want codex named in the refusal", match.Family)
	}
	reason := match.Reason()
	if !strings.Contains(reason, `runtime family "codex"`) || !strings.Contains(reason, "prior-review") {
		t.Fatalf("refusal must name the represented family and the evidence job: %q", reason)
	}
	if strings.Contains(reason, "push a new commit") {
		t.Fatalf("refusal must not prescribe manufacturing a commit: %q", reason)
	}
	if got := countJobEvents(t, store, "prior-review", ReviewLoopDetectedEventKind); got != 1 {
		t.Fatalf("review_loop_detected events = %d, want one claimed event", got)
	}
}

// Acceptance 2 — unrepresented family, same head, unanimous approval: NOW
// ALLOWED. This is the case the old guard blocked on PR #1527 (gm-review-kimi
// refused after a claude approval) and the test that fails on the old code.
func TestDetectReviewLoopUnrepresentedFamilyAllowed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "gm-review-opus", "claude", "claude-opus-4-8")
	seedReviewLoopAgent(t, store, "gm-review-kimi", "kimi", "kimi-for-coding")
	seedReviewLoopVerdict(t, store, "prior-review", "gm-review-opus", "head-a", "approved", "claude")

	// Pin the fixture: the two agents are DIFFERENT families, and the head's
	// history is a unanimous approval (the shape the old guard refused).
	holder, err := store.GetAgent(ctx, "gm-review-opus")
	if err != nil {
		t.Fatalf("GetAgent(gm-review-opus): %v", err)
	}
	requester, err := store.GetAgent(ctx, "gm-review-kimi")
	if err != nil {
		t.Fatalf("GetAgent(gm-review-kimi): %v", err)
	}
	if holder.Runtime == requester.Runtime {
		t.Fatalf("fixture weakened: %s and %s must be different runtime families", holder.Name, requester.Name)
	}

	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"gm-review-kimi"}); err != nil {
		t.Fatalf("DetectReviewLoop: %v", err)
	} else if detected {
		t.Fatal("a review from an unrepresented family is new information; the guard must allow it")
	}
}

// Acceptance 3 — THE DISCRIMINATOR: different NAME, SAME family -> REFUSED.
// g7-review holds the approval; af-review-sol requests one; both are
// codex/gpt-5.6-sol. A name-keyed implementation passes tests 1 and 2 and
// fails ONLY here.
func TestDetectReviewLoopDifferentNameSameFamilyRefused(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopAgent(t, store, "af-review-sol", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "prior-review", "g7-review", "head-a", "approved", "codex")

	// Pin the fixture INDEPENDENTLY of the resolver under test: different
	// names, same runtime AND same model, read straight from the registry.
	holder, err := store.GetAgent(ctx, "g7-review")
	if err != nil {
		t.Fatalf("GetAgent(g7-review): %v", err)
	}
	requester, err := store.GetAgent(ctx, "af-review-sol")
	if err != nil {
		t.Fatalf("GetAgent(af-review-sol): %v", err)
	}
	if holder.Name == requester.Name {
		t.Fatal("fixture weakened: the discriminator needs DIFFERENT agent names")
	}
	if holder.Runtime != requester.Runtime || holder.Model != requester.Model {
		t.Fatalf("fixture weakened: both agents must be the same family (runtime+model), got %s/%s vs %s/%s",
			holder.Runtime, holder.Model, requester.Runtime, requester.Model)
	}

	match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"af-review-sol"})
	if err != nil {
		t.Fatalf("DetectReviewLoop: %v", err)
	}
	if !detected {
		t.Fatal("af-review-sol is a different NAME but the SAME codex family as g7-review; the guard must refuse")
	}
	if match.Family != "codex" {
		t.Fatalf("match.Family = %q, want codex", match.Family)
	}
}

// Acceptance 4 — mixed decisions at the head: still allowed (unchanged),
// even when the requester's family IS represented. Guards against the
// narrowing accidentally widening the mixed-decision path.
func TestDetectReviewLoopMixedDecisionsAllowed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "gm-review-opus", "claude", "claude-opus-4-8")
	seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
	seedReviewLoopVerdict(t, store, "prior-approved", "gm-review-opus", "head-a", "approved", "claude")
	seedReviewLoopVerdict(t, store, "prior-changes", "g7-review", "head-a", "changes_requested", "codex")

	// Pin the fixture: BOTH verdicts sit at the SAME head and DISAGREE.
	verdicts, err := store.SucceededReviewVerdicts(ctx, "owner/repo", 227)
	if err != nil {
		t.Fatalf("SucceededReviewVerdicts: %v", err)
	}
	atHead := map[string]string{}
	for _, verdict := range verdicts {
		if verdict.HeadSHA == "head-a" {
			atHead[verdict.JobID] = verdict.Decision
		}
	}
	if len(atHead) != 2 || atHead["prior-approved"] == atHead["prior-changes"] {
		t.Fatalf("fixture weakened: want two disagreeing verdicts at head-a, got %v", atHead)
	}

	// The requester's family (claude) IS represented at the head; only the
	// mixed decisions may allow this.
	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"gm-review-opus"}); err != nil {
		t.Fatalf("DetectReviewLoop: %v", err)
	} else if detected {
		t.Fatal("mixed decisions at the head must still proceed; the earlier claim is unstable")
	}
}

// Acceptance 5 — empty requested head: allowed before any succeeded history,
// refused after, regardless of families (unchanged rule).
func TestDetectReviewLoopEmptyHeadRule(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "gm-review-kimi", "kimi", "kimi-for-coding")

	if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "", []string{"gm-review-kimi"}); err != nil {
		t.Fatalf("DetectReviewLoop(empty, no history): %v", err)
	} else if detected {
		t.Fatal("empty head with no succeeded history must proceed")
	}

	// Even a verdict from a DIFFERENT family must not let an empty head
	// through once history exists: the empty head cannot prove a new commit.
	seedReviewLoopAgent(t, store, "gm-review-opus", "claude", "claude-opus-4-8")
	seedReviewLoopVerdict(t, store, "prior-review", "gm-review-opus", "head-a", "approved", "claude")
	match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "", []string{"gm-review-kimi"})
	if err != nil {
		t.Fatalf("DetectReviewLoop(empty, with history): %v", err)
	}
	if !detected || !match.EmptyHead {
		t.Fatalf("empty head after succeeded history must fail closed: detected=%v match=%+v", detected, match)
	}
	if !strings.Contains(match.Reason(), "requested head SHA is empty") {
		t.Fatalf("empty-head refusal must keep its own message branch: %q", match.Reason())
	}
}

// Acceptance 8 — unknown family FAILS CLOSED. An unknown that counted as "not
// yet represented" would turn the guard into a way to add unlimited reviews.
func TestDetectReviewLoopUnknownFamilyFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedReviewLoopAgent(t, store, "gm-review-kimi", "kimi", "kimi-for-coding")

	t.Run("verdict family unknown", func(t *testing.T) {
		// The verdict's agent is absent from the registry and recorded no
		// runtime: its family cannot be determined.
		seedReviewLoopVerdict(t, store, "ghost-review", "ghost", "head-a", "approved", "")
		if _, err := store.GetAgent(ctx, "ghost"); err == nil {
			t.Fatal("fixture weakened: ghost must be ABSENT from the registry")
		}
		match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-a", []string{"gm-review-kimi"})
		if err != nil {
			t.Fatalf("DetectReviewLoop: %v", err)
		}
		if !detected {
			t.Fatal("a verdict whose family cannot be determined must fail closed, not read as unrepresented")
		}
		if match.Family != "" {
			t.Fatalf("fail-closed refusal names no family, got %q", match.Family)
		}
		if !strings.Contains(match.Reason(), "fail-closed") {
			t.Fatalf("fail-closed refusal must say so: %q", match.Reason())
		}
	})

	t.Run("requester family unknown", func(t *testing.T) {
		seedReviewLoopAgent(t, store, "g7-review", "codex", "gpt-5.6-sol")
		seedReviewLoopVerdict(t, store, "prior-review", "g7-review", "head-b", "approved", "codex")
		if _, err := store.GetAgent(ctx, "ghost-requester"); err == nil {
			t.Fatal("fixture weakened: ghost-requester must be ABSENT from the registry")
		}
		if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-b", []string{"ghost-requester"}); err != nil {
			t.Fatalf("DetectReviewLoop: %v", err)
		} else if !detected {
			t.Fatal("a requester whose family cannot be determined cannot prove new information; must fail closed")
		}
	})

	t.Run("mixed requester batch with unknown family", func(t *testing.T) {
		seedReviewLoopAgent(t, store, "g7-review-batch", "codex", "gpt-5.6-sol")
		seedReviewLoopVerdict(t, store, "prior-review-batch", "g7-review-batch", "head-c", "approved", "codex")
		if _, err := store.GetAgent(ctx, "ghost-requester-batch"); err == nil {
			t.Fatal("fixture weakened: ghost-requester-batch must be ABSENT from the registry")
		}
		// Pin the anchor the ordering cases below lean on: without the ghost,
		// the batch PROVABLY brings a new family and is allowed — the unknown
		// requester is the ONLY thing refusing.
		if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-c", []string{"gm-review-kimi"}); err != nil {
			t.Fatalf("DetectReviewLoop(known-only baseline): %v", err)
		} else if detected {
			t.Fatal("fixture weakened: the known-new requester alone must be ALLOWED, or this case cannot isolate the unknown's refusal")
		}
		match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-c", []string{"ghost-requester-batch", "gm-review-kimi"})
		if err != nil {
			t.Fatalf("DetectReviewLoop: %v", err)
		}
		if !detected {
			t.Fatal("one known-new requester must not hide an unresolved requester in the same batch")
		}
		if match.Family != "" {
			t.Fatalf("mixed unknown batch must use the fail-closed refusal, got family %q", match.Family)
		}
	})

	// Ordering cases (#1528 review): the original defect was order-dependent,
	// so the fail-closed boundary must be order-INDEPENDENT. Each case pins the
	// same anchor — the known requesters alone would be ALLOWED — so an
	// "allow immediately on the first known-new family" implementation goes
	// red here instead of surviving.
	t.Run("mixed requester batch unknown LAST", func(t *testing.T) {
		seedReviewLoopAgent(t, store, "g7-review-last", "codex", "gpt-5.6-sol")
		seedReviewLoopVerdict(t, store, "prior-review-last", "g7-review-last", "head-d", "approved", "codex")
		if _, err := store.GetAgent(ctx, "ghost-requester-last"); err == nil {
			t.Fatal("fixture weakened: ghost-requester-last must be ABSENT from the registry")
		}
		if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-d", []string{"gm-review-kimi"}); err != nil {
			t.Fatalf("DetectReviewLoop(known-only baseline): %v", err)
		} else if detected {
			t.Fatal("fixture weakened: the known-new requester alone must be ALLOWED, or this case cannot isolate the unknown's refusal")
		}
		match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-d", []string{"gm-review-kimi", "ghost-requester-last"})
		if err != nil {
			t.Fatalf("DetectReviewLoop: %v", err)
		}
		if !detected {
			t.Fatal("an unresolved requester in LAST position must still fail closed; an allow-on-first-known-new implementation passes for the wrong reason")
		}
		if match.Family != "" {
			t.Fatalf("unknown-last batch must use the fail-closed refusal, got family %q", match.Family)
		}
	})

	t.Run("mixed requester batch unknown BETWEEN two resolvable requesters", func(t *testing.T) {
		seedReviewLoopAgent(t, store, "g7-review-between", "codex", "gpt-5.6-sol")
		seedReviewLoopAgent(t, store, "gm-review-opus-between", "claude", "claude-opus-4-8")
		seedReviewLoopVerdict(t, store, "prior-review-between", "g7-review-between", "head-e", "approved", "codex")
		if _, err := store.GetAgent(ctx, "ghost-requester-between"); err == nil {
			t.Fatal("fixture weakened: ghost-requester-between must be ABSENT from the registry")
		}
		// Baseline: BOTH flanking requesters are resolvable, and kimi is a
		// provably NEW family — without the ghost the batch is allowed.
		if _, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-e", []string{"gm-review-kimi", "gm-review-opus-between"}); err != nil {
			t.Fatalf("DetectReviewLoop(known-only baseline): %v", err)
		} else if detected {
			t.Fatal("fixture weakened: the two resolvable requesters alone must be ALLOWED, or this case cannot isolate the unknown's refusal")
		}
		match, detected, err := DetectReviewLoop(ctx, store, "owner/repo", 227, "head-e", []string{"gm-review-kimi", "ghost-requester-between", "gm-review-opus-between"})
		if err != nil {
			t.Fatalf("DetectReviewLoop: %v", err)
		}
		if !detected {
			t.Fatal("an unresolved requester BETWEEN two resolvable ones must still fail closed; an allow-on-first-known-new implementation passes for the wrong reason")
		}
		if match.Family != "" {
			t.Fatalf("unknown-between batch must use the fail-closed refusal, got family %q", match.Family)
		}
	})
}

// Acceptance 7 — resolver precedence: a recorded effective runtime beats the
// registry default when they disagree (the override-run case), the registry
// covers unrecorded jobs, and the unknown case reports ok=false.
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
