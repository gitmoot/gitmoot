package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestDeriveStuckReason exercises the pure why-stuck derivation (#552): the
// authoritative reason for each stuck signal, and silence for healthy jobs.
func TestDeriveStuckReason(t *testing.T) {
	queued := db.Job{ID: "j", State: string(workflow.JobQueued)}
	blocked := db.Job{ID: "j", State: string(workflow.JobBlocked)}
	locks := []db.ResourceLock{{
		ResourceKey: "runtime:codex:agent-x",
		OwnerJobID:  "other-job",
		ExpiresAt:   "2026-07-01T03:00:00Z",
	}}

	for _, tt := range []struct {
		name        string
		job         db.Job
		event       db.JobEvent
		hasEvent    bool
		wantReason  string // substring; "" means expect empty
		wantNoMatch string
		wantNext    string
	}{
		{
			name:       "queued waiting on runtime session lock surfaces owner and lease",
			job:        queued,
			event:      db.JobEvent{Kind: "runtime_lock_wait", Message: "runtime session runtime:codex:agent-x is busy"},
			hasEvent:   true,
			wantReason: "waiting on runtime session lock runtime:codex:agent-x (held by job other-job)",
			wantNext:   "2026-07-01T03:00:00Z",
		},
		{
			name:       "blocked awaiting human",
			job:        blocked,
			event:      db.JobEvent{Kind: "advance_awaiting_human", Message: "need a decision"},
			hasEvent:   true,
			wantReason: "blocked: awaiting human",
		},
		{
			name:       "auth failure is labeled auth failing",
			job:        blocked,
			event:      db.JobEvent{Kind: "advance_blocked", Message: "GitHub CLI authentication for account jerryfane is invalid"},
			hasEvent:   true,
			wantReason: "auth failing:",
		},
		{
			name:       "usage limit is labeled throttled",
			job:        queued,
			event:      db.JobEvent{Kind: "advance_blocked", Message: "You've hit your usage limit; resets Jul 1, 3am"},
			hasEvent:   true,
			wantReason: "throttled:",
		},
		{
			name:        "failed-check test name containing 'resets' is not mislabeled throttled",
			job:         blocked,
			event:       db.JobEvent{Kind: "deterministic_checkers_failed", Message: "--- FAIL: TestConfigResets (0.01s)"},
			hasEvent:    true,
			wantReason:  "blocked:",
			wantNoMatch: "throttled",
		},
		{
			name:        "git 'invalid author email' is not mislabeled auth failing",
			job:         blocked,
			event:       db.JobEvent{Kind: "advance_blocked", Message: "commit failed: invalid author email"},
			hasEvent:    true,
			wantReason:  "blocked:",
			wantNoMatch: "auth failing",
		},
		{
			name:        "bare 401 digit run without http context is not mislabeled auth failing",
			job:         queued,
			event:       db.JobEvent{Kind: "advance_blocked", Message: "delegation to PR 401 exceeded budget"},
			hasEvent:    true,
			wantReason:  "waiting:",
			wantNoMatch: "auth failing",
		},
		{
			name:       "http 401 status is labeled auth failing",
			job:        blocked,
			event:      db.JobEvent{Kind: "advance_blocked", Message: "gh api returned http 401"},
			hasEvent:   true,
			wantReason: "auth failing:",
		},
		{
			name:       "retry event surfaces retrying",
			job:        queued,
			event:      db.JobEvent{Kind: "advance_retry", Message: "attempt 2 of 3"},
			hasEvent:   true,
			wantReason: "retrying: attempt 2 of 3",
		},
		{
			name:       "review loop event surfaces blocked reason",
			job:        blocked,
			event:      db.JobEvent{Kind: workflow.ReviewLoopDetectedEventKind, Message: "same verdict at unchanged head"},
			hasEvent:   true,
			wantReason: "blocked: same verdict at unchanged head",
		},
		{
			name:       "blocked with no reason event still explained",
			job:        blocked,
			hasEvent:   false,
			wantReason: "blocked",
		},
		{
			name:       "healthy queued job stays silent",
			job:        queued,
			hasEvent:   false,
			wantReason: "",
		},
		{
			name:       "running job is never stuck",
			job:        db.Job{ID: "j", State: string(workflow.JobRunning)},
			event:      db.JobEvent{Kind: "runtime_lock_wait", Message: "runtime session runtime:codex:agent-x is busy"},
			hasEvent:   true,
			wantReason: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveStuckReason(tt.job, tt.event, tt.hasEvent, locks)
			if tt.wantReason == "" {
				if !got.empty() {
					t.Fatalf("deriveStuckReason = %+v, want empty", got)
				}
				return
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want substring %q", got.Reason, tt.wantReason)
			}
			if tt.wantNoMatch != "" && strings.Contains(got.Reason, tt.wantNoMatch) {
				t.Fatalf("reason = %q, must not contain %q", got.Reason, tt.wantNoMatch)
			}
			if tt.wantNext != "" && got.NextRetryAt != tt.wantNext {
				t.Fatalf("next retry = %q, want %q", got.NextRetryAt, tt.wantNext)
			}
		})
	}
}

// TestRunJobListShowSurfaceWhyStuck is the end-to-end surface: a queued job
// waiting on a runtime session lock reports WHY in `job list` and why_stuck in
// `job show`, while a healthy queued job keeps its output unchanged (no WHY
// column, exactly six columns).
func TestRunJobListShowSurfaceWhyStuck(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	seedCLIJob(t, store, db.Job{
		ID:      "stuck-job",
		Agent:   "impl",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 4}),
	}, "queued")
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "stuck-job",
		Kind:    "runtime_lock_wait",
		Message: "runtime session runtime:codex:agent-x is busy",
	}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	if _, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:agent-x",
		OwnerJobID:  "other-running-job",
		OwnerToken:  "tok",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	// A healthy queued job with only lifecycle events must stay unchanged.
	seedCLIJob(t, store, db.Job{
		ID:      "healthy-job",
		Agent:   "impl",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 5}),
	}, "queued")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	var stuckLine, healthyLine string
	for _, line := range strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "stuck-job\t") {
			stuckLine = line
		}
		if strings.HasPrefix(line, "healthy-job\t") {
			healthyLine = line
		}
	}
	if stuckLine == "" || healthyLine == "" {
		t.Fatalf("job list output = %q", stdout.String())
	}
	if !strings.Contains(stuckLine, "WHY: waiting on runtime session lock runtime:codex:agent-x") {
		t.Fatalf("stuck line missing WHY column: %q", stuckLine)
	}
	if !strings.Contains(stuckLine, "held by job other-running-job") || !strings.Contains(stuckLine, "next retry") {
		t.Fatalf("stuck line missing lock owner / next retry: %q", stuckLine)
	}
	if strings.Contains(healthyLine, "WHY:") {
		t.Fatalf("healthy job must not carry a WHY column: %q", healthyLine)
	}
	if got := strings.Count(healthyLine, "\t"); got != 5 {
		t.Fatalf("healthy job line changed shape (%d tabs, want 5): %q", got, healthyLine)
	}

	// job show surfaces the reason as a detail line.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "stuck-job", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "why_stuck: waiting on runtime session lock") {
		t.Fatalf("job show missing why_stuck line:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "next_retry_at:") {
		t.Fatalf("job show missing next_retry_at line:\n%s", stdout.String())
	}

	// A healthy job's show output carries no why_stuck line.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "healthy-job", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "why_stuck:") {
		t.Fatalf("healthy job show must not carry why_stuck:\n%s", stdout.String())
	}

	// --json carries the fields for the stuck job and omits them for the healthy one.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "stuck-job", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show --json exit = %d, stderr=%s", code, stderr.String())
	}
	var decoded jobShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("job show --json did not decode: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(decoded.WhyStuck, "waiting on runtime session lock") || decoded.NextRetryAt == "" {
		t.Fatalf("job show --json missing stuck fields: %+v", decoded)
	}
}

// authFakeRunner is a minimal subprocess.Runner for the doctor auth checks: every
// binary looks present and Run returns the result/err configured per command line.
type authFakeRunner struct {
	runs map[string]subprocess.Result
	errs map[string]error
}

func (f authFakeRunner) LookPath(string) (string, error) { return "/bin/x", nil }

func (f authFakeRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	key := command + " " + strings.Join(args, " ")
	return f.runs[key], f.errs[key]
}

// TestDoctorGHAuthValidVsInvalid is the proactive auth-validation report (#552
// slice B): `gh auth status` success reports OK; failure reports not-OK with an
// actionable remediation message. Exercised through doctor.GlobalChecks so the
// same code the `gitmoot doctor` command runs is validated.
func TestDoctorGHAuthValidVsInvalid(t *testing.T) {
	find := func(checks []doctor.Check) doctor.Check {
		for _, c := range checks {
			if c.Name == "gh auth" {
				return c
			}
		}
		t.Fatalf("no gh auth check in %+v", checks)
		return doctor.Check{}
	}

	valid := doctor.Checker{Runner: authFakeRunner{
		runs: map[string]subprocess.Result{"gh auth status": {Stdout: "Logged in to github.com account jerryfane\n"}},
	}}.GlobalChecks(context.Background())
	if got := find(valid); !got.OK || !got.Required {
		t.Fatalf("valid gh auth = %+v, want required OK", got)
	}

	invalid := doctor.Checker{Runner: authFakeRunner{
		runs: map[string]subprocess.Result{"gh auth status": {Stderr: "You are not logged into any GitHub hosts\n"}},
		errs: map[string]error{"gh auth status": context.DeadlineExceeded},
	}}.GlobalChecks(context.Background())
	got := find(invalid)
	if got.OK {
		t.Fatalf("invalid gh auth = %+v, want not OK", got)
	}
	if !strings.Contains(got.Detail, doctor.GHAuthRemediation) {
		t.Fatalf("invalid gh auth detail = %q, want actionable remediation %q", got.Detail, doctor.GHAuthRemediation)
	}
	if !strings.Contains(got.Detail, "not logged into") {
		t.Fatalf("invalid gh auth detail = %q, want the underlying gh error", got.Detail)
	}
}
