package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1558. The POSITIVE is one line; the four NEGATIVES are the suite, because a
// health verdict that reads a stale row and calls it current would replace a
// false green with a false red - the same defect with the sign flipped.
var servingNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

type servingJobSpec struct {
	id               string
	agent            string
	state            workflow.JobState
	blockerClass     string
	retryAt          string
	effectiveRuntime string
	effectiveEvent   string
}

func seedServingJob(t *testing.T, store *db.Store, spec servingJobSpec) {
	t.Helper()
	payload := workflow.JobPayload{
		Repo:             "owner/repo",
		BlockerClass:     spec.blockerClass,
		BlockerRetryAt:   spec.retryAt,
		EffectiveRuntime: spec.effectiveRuntime,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	job := db.Job{ID: spec.id, Agent: spec.agent, Type: "review", State: string(spec.state), Payload: string(encoded)}
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob(%s) returned error: %v", spec.id, err)
	}
	if spec.effectiveEvent != "" {
		if err := store.AddJobEvent(context.Background(), db.JobEvent{
			JobID:   spec.id,
			Kind:    "effective_runtime",
			Message: "job runs on runtime " + spec.effectiveEvent + " (agent default " + spec.effectiveEvent + "); session lock runtime:" + spec.effectiveEvent,
		}); err != nil {
			t.Fatalf("AddJobEvent(%s) returned error: %v", spec.id, err)
		}
	}
}

func servingStatusForStore(t *testing.T, seed func(*testing.T, *db.Store)) *doctor.RuntimeServingStatus {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	seed(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	return runtimeServingStatusAt(paths, servingNow)
}

// THE POSITIVE: an installed, contract-valid runtime whose engine-recorded hold
// has NOT expired is reported as not serving, and the row names the evidence.
func TestRuntimeServingReportsALiveProviderHold(t *testing.T) {
	status := servingStatusForStore(t, func(t *testing.T, store *db.Store) {
		seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
		seedServingJob(t, store, servingJobSpec{
			id: "held-1", agent: "gm-review-kimi", state: workflow.JobQueued,
			blockerClass: "runtime_quota",
			retryAt:      servingNow.Add(30 * time.Minute).Format(time.RFC3339Nano),
		})
	})
	if status == nil || len(status.Blocks) != 1 {
		t.Fatalf("status = %+v, want exactly one block", status)
	}
	block := status.Blocks[0]
	if block.Runtime != "kimi" || block.Class != "runtime_quota" || block.JobID != "held-1" {
		t.Fatalf("block = %+v, want kimi/runtime_quota/held-1", block)
	}
	check := doctor.CheckRuntimeServing(*status)
	if check.OK {
		t.Fatal("CheckRuntimeServing reported OK while a hold is live")
	}
	if check.State != "warn" {
		t.Fatalf("check.State = %q, want warn: the runtime is installed and contract-valid, the provider is refusing", check.State)
	}
	for _, want := range []string{"kimi", "runtime_quota", "held-1", "NOT serving"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail %q is missing %q", check.Detail, want)
		}
	}
}

// THE FOUR NEGATIVES. Each is a state the store contains in bulk on a real host
// and none of them may degrade a runtime.
func TestRuntimeServingStaysSilentOnEveryNonHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, *db.Store)
	}{
		{
			// Measured: 12 jobs on this host succeeded on a runtime AFTER carrying
			// a runtime_quota block, and 4 more after runtime_auth. A newest-wins
			// rule would call every one of those runtimes dead.
			name: "a job that reached a terminal state",
			seed: func(t *testing.T, store *db.Store) {
				seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
				seedServingJob(t, store, servingJobSpec{
					id: "done-1", agent: "gm-review-kimi", state: workflow.JobSucceeded,
					blockerClass: "runtime_quota",
					retryAt:      servingNow.Add(30 * time.Minute).Format(time.RFC3339Nano),
				})
			},
		},
		{
			name: "a hold whose retry time has already passed",
			seed: func(t *testing.T, store *db.Store) {
				seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
				seedServingJob(t, store, servingJobSpec{
					id: "aged-1", agent: "gm-review-kimi", state: workflow.JobQueued,
					blockerClass: "runtime_auth",
					retryAt:      servingNow.Add(-time.Minute).Format(time.RFC3339Nano),
				})
			},
		},
		{
			// Measured: 42 of 55 succeeded checkout_contention rows carry no retry
			// time, so absence is the COMMON case on at least one path and must
			// never mean "unknown therefore bad".
			name: "a blocker row with no retry time",
			seed: func(t *testing.T, store *db.Store) {
				seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
				seedServingJob(t, store, servingJobSpec{
					id: "noretry-1", agent: "gm-review-kimi", state: workflow.JobQueued,
					blockerClass: "runtime_quota",
				})
			},
		},
		{
			// Measured: 3 of 284 blocked rows are attributable by none of the three
			// sources. Small is exactly why the rule is written down - nobody would
			// notice three wrong rows, which is what makes guessing them attractive.
			name: "a hold whose runtime cannot be attributed",
			seed: func(t *testing.T, store *db.Store) {
				seedServingJob(t, store, servingJobSpec{
					id: "orphan-1", agent: "agent-that-no-longer-exists", state: workflow.JobQueued,
					blockerClass: "runtime_quota",
					retryAt:      servingNow.Add(30 * time.Minute).Format(time.RFC3339Nano),
				})
			},
		},
		{
			name: "a blocker class that is not a provider refusal",
			seed: func(t *testing.T, store *db.Store) {
				seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
				seedServingJob(t, store, servingJobSpec{
					id: "contention-1", agent: "gm-review-kimi", state: workflow.JobQueued,
					blockerClass: "checkout_contention",
					retryAt:      servingNow.Add(30 * time.Minute).Format(time.RFC3339Nano),
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := servingStatusForStore(t, tc.seed)
			if status == nil {
				t.Fatal("status = nil, want an empty status rather than an omitted check")
			}
			if len(status.Blocks) != 0 {
				t.Fatalf("blocks = %+v, want none", status.Blocks)
			}
			if check := doctor.CheckRuntimeServing(*status); !check.OK {
				t.Fatalf("check = %+v, want OK: %s", check, tc.name)
			}
		})
	}
}

// THE ATTRIBUTION ORDER. No single source covers the population - measured, 107
// of 284 blocked rows carry payload.effective_runtime and 101 have the event -
// so all three are needed, and the priority must be observable rather than
// implied.
func TestRuntimeServingResolvesTheRuntimeInPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec servingJobSpec
		want string
	}{
		{
			name: "payload effective_runtime wins over the agent default",
			spec: servingJobSpec{id: "p-1", agent: "gm-review-kimi", effectiveRuntime: "codex", effectiveEvent: "claude"},
			want: "codex",
		},
		{
			name: "the effective_runtime event is used when the payload has none",
			spec: servingJobSpec{id: "e-1", agent: "gm-review-kimi", effectiveEvent: "claude"},
			want: "claude",
		},
		{
			name: "the agent's registered runtime is the last resort",
			spec: servingJobSpec{id: "a-1", agent: "gm-review-kimi"},
			want: "kimi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.state = workflow.JobQueued
			spec.blockerClass = "runtime_quota"
			spec.retryAt = servingNow.Add(time.Hour).Format(time.RFC3339Nano)
			status := servingStatusForStore(t, func(t *testing.T, store *db.Store) {
				seedDaemonWorkerAgent(t, store, "gm-review-kimi", "kimi", "fresh", []string{"review"}, "owner/repo")
				seedServingJob(t, store, spec)
			})
			if status == nil || len(status.Blocks) != 1 {
				t.Fatalf("status = %+v, want one block", status)
			}
			if status.Blocks[0].Runtime != tc.want {
				t.Fatalf("runtime = %q, want %q", status.Blocks[0].Runtime, tc.want)
			}
		})
	}
}

// A message whose wording no longer matches must degrade to UNATTRIBUTABLE -
// which the check excludes - rather than to a wrong runtime.
func TestEffectiveRuntimeMessageParsingRefusesAGuess(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    string
	}{
		{"job runs on runtime kimi (agent default kimi); session lock runtime:kimi", "kimi"},
		{"job runs on runtime codex", "codex"},
		{"job runs on runtime claude; extra", "claude"},
		{"selected review via agent_review: explicit agent review", ""},
		{"", ""},
		{"job runs on runtime ", ""},
	} {
		if got := runtimeNameFromEffectiveRuntimeMessage(tc.message); got != tc.want {
			t.Fatalf("runtimeNameFromEffectiveRuntimeMessage(%q) = %q, want %q", tc.message, got, tc.want)
		}
	}
}
