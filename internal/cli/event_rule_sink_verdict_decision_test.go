package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1945: THE REVIEW-VERDICT WAKE MUST ANNOUNCE THE REVIEWER'S RECORDED DECISION.
//
// Measured twice on #1943, hours apart and with distinct reviewer payload
// shapes: the wake said `approved` while the persisted job row recorded
// `changes_requested`. The path is exact and short. engine_types.go computes a
// THRESHOLD-ADJUSTED decision, review_threshold.go:42 folds a
// changes_requested whose severity does not meet the repository bar into
// "approved", and event_rule_sink.go renders event.ReviewDecision verbatim into
// `gitmoot review verdict %s for ...`. A coordinator then acts on the word
// instead of rereading the row, and `approved` is precisely the word that
// suppresses that reflex.
//
// THIS TEST LIVES AT THE PRODUCTION EVENT-TO-WAKE PATH, not on a helper. It
// drives the real engine to a review terminal through the real mailbox, takes
// the event from the injected sink the daemon uses, and renders it with
// eventRuleWakePrompt, the same function the daemon calls. The blocking bar is
// installed by applyReviewPolicy from a real config file rather than by setting
// the field, so the configuration half is production too.
//
// It is RED at origin/main, where the prompt reads "approved".
func TestReviewVerdictWakeAnnouncesRecordedChangesRequested(t *testing.T) {
	ctx := context.Background()
	sink, event := runReviewVerdictWakeFixture(t, ctx, reviewVerdictFixture{
		jobID: "review-1945", pullRequest: 1945, taskID: "task-1945",
		// P3 is BELOW the configured P1 bar, so the threshold transform folds this
		// recorded changes_requested to "approved" for the merge gate. The gate is
		// right to do that; the notification is not.
		decision: "changes_requested", severity: "P3",
		blockingSeverity: "P1",
	})

	if event.Cause != events.EventCauseReviewVerdict {
		t.Fatalf("event cause = %q, want %q (all=%+v)", event.Cause, events.EventCauseReviewVerdict, sink.events)
	}
	if event.ReviewDecision != "changes_requested" {
		t.Fatalf("event.ReviewDecision = %q, want %q: the wake must carry the RECORDED reviewer decision, not the threshold-adjusted one",
			event.ReviewDecision, "changes_requested")
	}

	prompt := eventRuleWakePrompt(eventRuleKindReviewVerdict, event)
	if !strings.Contains(prompt, "changes_requested") {
		t.Fatalf("wake prompt does not announce the recorded decision: %q", prompt)
	}
	if strings.Contains(prompt, "approved") {
		t.Fatalf("wake prompt announces approved for a recorded changes_requested review, which is #1945: %q", prompt)
	}
}

// SHOULD-SUCCEED CONTROL: a genuinely approved review still announces approved.
// Without this a mutant that hardcoded "changes_requested" would satisfy the
// regression above while making every wake wrong in the other direction.
func TestReviewVerdictWakeStillAnnouncesGenuineApproved(t *testing.T) {
	ctx := context.Background()
	_, event := runReviewVerdictWakeFixture(t, ctx, reviewVerdictFixture{
		jobID: "review-approved", pullRequest: 1946, taskID: "task-1946",
		decision: "approved", severity: "", blockingSeverity: "P1",
	})

	if event.ReviewDecision != "approved" {
		t.Fatalf("event.ReviewDecision = %q, want approved", event.ReviewDecision)
	}
	prompt := eventRuleWakePrompt(eventRuleKindReviewVerdict, event)
	if !strings.Contains(prompt, "approved") || strings.Contains(prompt, "changes_requested") {
		t.Fatalf("a genuine approval must still announce approved: %q", prompt)
	}
}

// SHOULD-SUCCEED CONTROL, and it pins the OTHER defect in this family so my fix
// cannot re-open it. #1685: a coordinator fan-out is a dispatch record, not an
// inspection, so it must emit NO review-verdict wake at all. That exclusion is a
// DIFFERENT mechanism from #1945 - wrong source rather than wrong transform -
// and it lives on the admission guard, upstream of the value I changed.
func TestReviewVerdictWakeStaysExcludedForFanOut(t *testing.T) {
	ctx := context.Background()
	sink, _ := runReviewVerdictWakeFixtureAllowNoVerdict(t, ctx, reviewVerdictFixture{
		jobID: "review-fanout", pullRequest: 1947, taskID: "task-1947",
		decision: "approved", severity: "", blockingSeverity: "P1",
		delegations: `[{"id":"lens-a","agent":"audit","action":"review","prompt":"look at the diff"}]`,
	})

	for _, ev := range sink.events {
		if ev.Cause == events.EventCauseReviewVerdict {
			t.Fatalf("a fan-out emitted a review-verdict wake, re-opening #1685: %+v", ev)
		}
		if strings.TrimSpace(ev.ReviewDecision) != "" {
			t.Fatalf("a fan-out carried ReviewDecision %q; the delegates' own terminals carry the real verdicts", ev.ReviewDecision)
		}
	}
}

// SHOULD-SUCCEED CONTROL: non-review wake kinds are untouched. The directive
// prompt is rendered by the same function and must not change shape.
func TestDirectiveWakePromptUnchangedByVerdictFix(t *testing.T) {
	event := events.Event{
		Type:           events.EventJobFinished,
		Cause:          directiveCompletionOverdueCause,
		RootID:         "workflow_note:126324",
		WakeTargetRole: "gm-omp-fanout",
	}
	prompt := eventRuleWakePrompt("directive", event)
	for _, want := range []string{"gitmoot directive 126324", "gm-omp-fanout", "gitmoot org directive done 126324"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("directive wake prompt lost %q: %q", want, prompt)
		}
	}
	if strings.Contains(prompt, "review verdict") {
		t.Fatalf("directive wake prompt leaked review-verdict text: %q", prompt)
	}
}

// THE VOCABULARY INVARIANT the fix relies on instead of a defensive branch.
//
// engine_types.go admits a wake only when the THRESHOLD-ADJUSTED decision is
// approved or changes_requested, then announces the RECORDED one. That is only
// safe because the transform's single rule maps changes_requested -> approved,
// so an admitted effective word always implies an admitted recorded word. I did
// not add an unreachable fallback for a third word - an unfalsifiable guard is
// worse than none - so this test is what fails if a future transform breaks the
// implication and widens what a human can be told.
func TestReviewVerdictWakeVocabularyStaysTwoWords(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name             string
		decision         string
		severity         string
		blockingSeverity string
		want             string
	}{
		{"below bar folds for the gate, recorded word announced", "changes_requested", "P3", "P1", "changes_requested"},
		{"at bar blocks, recorded word announced", "changes_requested", "P1", "P1", "changes_requested"},
		{"approved stays approved", "approved", "", "P1", "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, event := runReviewVerdictWakeFixture(t, ctx, reviewVerdictFixture{
				jobID: "review-vocab", pullRequest: 1948, taskID: "task-1948",
				decision: tc.decision, severity: tc.severity, blockingSeverity: tc.blockingSeverity,
			})
			if event.ReviewDecision != tc.want {
				t.Fatalf("announced %q, want %q", event.ReviewDecision, tc.want)
			}
			if event.ReviewDecision != "approved" && event.ReviewDecision != "changes_requested" {
				t.Fatalf("the wake vocabulary widened to %q; a human can now be told a third word", event.ReviewDecision)
			}
		})
	}
}

type reviewVerdictFixture struct {
	jobID            string
	pullRequest      int
	taskID           string
	decision         string
	severity         string
	blockingSeverity string
	delegations      string
}

func runReviewVerdictWakeFixture(t *testing.T, ctx context.Context, fx reviewVerdictFixture) (*recordingSink, events.Event) {
	t.Helper()
	sink, event, ok := reviewVerdictWakeRun(t, ctx, fx)
	if !ok {
		t.Fatalf("no review-verdict event was emitted; all=%+v", sink.events)
	}
	return sink, event
}

func runReviewVerdictWakeFixtureAllowNoVerdict(t *testing.T, ctx context.Context, fx reviewVerdictFixture) (*recordingSink, events.Event) {
	t.Helper()
	sink, event, _ := reviewVerdictWakeRun(t, ctx, fx)
	return sink, event
}

// reviewVerdictWakeRun drives the REAL engine over the REAL mailbox to a review
// terminal and returns the emitted event. Nothing here stubs the decision path:
// the blocking bar arrives through applyReviewPolicy reading a config file, and
// the verdict arrives as adapter output parsed into the job payload.
func reviewVerdictWakeRun(t *testing.T, ctx context.Context, fx reviewVerdictFixture) (*recordingSink, events.Event, bool) {
	t.Helper()
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nblocking_severity = \"" + fx.blockingSeverity + "\"\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "printf ok", []string{"review"}, "owner/repo")

	sink := &recordingSink{}
	engine := workflow.Engine{
		Store:                   store,
		ResolveDeliveryWorktree: workflow.UnavailableDeliveryWorktreeResolver("1945 review wake test"),
		EventSink:               sink,
	}
	applyReviewPolicy(&engine, root)

	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("1945 review wake test"))
	if _, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: fx.jobID, Agent: "audit", Action: "review", Repo: "owner/repo",
		Branch: "task-branch", PullRequest: fx.pullRequest, HeadSHA: "head1945",
		TaskID: fx.taskID, TaskTitle: "Review", LeadAgent: "author",
		Reviewers: []string{"audit"}, ReviewRound: "review-1",
		Sender: "local", ActingOrgRole: "requester",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	delegations := "[]"
	if strings.TrimSpace(fx.delegations) != "" {
		delegations = fx.delegations
	}
	output := `{"gitmoot_result":{"decision":"` + fx.decision + `","severity":"` + fx.severity +
		`","summary":"verdict summary","findings":[],"changes_made":[],"tests_run":["go test ./... ok"],"needs":[],"delegations":` + delegations + `}}`
	adapter := &cliWorkerFakeAdapter{output: output}

	if _, err := engine.RunJob(ctx, fx.jobID, runtime.Agent{
		Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok",
		RepoScope: "owner/repo", Role: "reviewer",
	}, adapter); err != nil {
		t.Fatalf("RunJob returned error: %v", err)
	}

	for _, ev := range sink.events {
		if ev.Cause == events.EventCauseReviewVerdict {
			return sink, ev, true
		}
	}
	return sink, events.Event{}, false
}
