package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewEventContractShape(t *testing.T) {
	ts := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	ev := NewEvent(EventJobFinished, "job-1", "root-1", "jerryfane/gitmoot", "succeeded", "all good", ts, nil)

	if ev.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", ev.SchemaVersion)
	}
	if SchemaVersion != 1 {
		t.Fatalf("SchemaVersion const = %d, want 1", SchemaVersion)
	}
	if ev.Type != EventJobFinished {
		t.Fatalf("Type = %q, want %q", ev.Type, EventJobFinished)
	}
	if ev.JobID != "job-1" || ev.RootID != "root-1" || ev.Repo != "jerryfane/gitmoot" || ev.Status != "succeeded" || ev.Detail != "all good" {
		t.Fatalf("event fields = %+v", ev)
	}
	if ev.Timestamp != "2026-06-16T12:00:00Z" {
		t.Fatalf("Timestamp = %q, want RFC3339 2026-06-16T12:00:00Z", ev.Timestamp)
	}

	// The JSON wire shape is the documented contract; pin the key names.
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	for _, key := range []string{"schema_version", "event_type", "job_id", "root_id", "repo", "status", "ts", "detail"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("event JSON missing key %q; got %s", key, raw)
		}
	}
	if decoded["schema_version"].(float64) != 1 {
		t.Fatalf("schema_version = %v, want 1", decoded["schema_version"])
	}
	if decoded["event_type"].(string) != "job.finished" {
		t.Fatalf("event_type = %v, want job.finished", decoded["event_type"])
	}
}

func TestEventTypeEnumValues(t *testing.T) {
	cases := map[EventType]string{
		EventJobFinished:                "job.finished",
		EventJobFailed:                  "job.failed",
		EventJobBlocked:                 "job.blocked",
		EventJobNeedsAttention:          "job.needs_attention",
		EventCandidateAwaitingPromotion: "candidate.awaiting_promotion",
		EventCandidateAutoPromoted:      "candidate.auto_promoted",
		EventJobStarted:                 "job.started",
		EventDelegationEscalation:       "delegation.escalation",
		EventDelegationFinalized:        "delegation.finalized",
		EventOrchestrationFinished:      "orchestration.finished",
	}
	for typ, want := range cases {
		if string(typ) != want {
			t.Fatalf("event type %q != %q", typ, want)
		}
	}
}

func TestNewEventRedactsEveryStringField(t *testing.T) {
	// A redactor that uppercases proves the func is applied to detail (the only
	// free-text field); the real RedactCommentText is exercised by the workflow
	// tests. Secrets must not survive into detail.
	redact := func(s string) string {
		return strings.ReplaceAll(s, "ghp_secretsecretsecret", "[REDACTED]")
	}
	ev := NewEvent(EventJobFailed, "job-1", "root-1", "jerryfane/gitmoot", "failed", "token ghp_secretsecretsecret leaked", time.Now(), redact)
	if strings.Contains(ev.Detail, "ghp_secretsecretsecret") {
		t.Fatalf("detail not redacted: %q", ev.Detail)
	}
	if !strings.Contains(ev.Detail, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %q", ev.Detail)
	}
}

func TestNewEventScrubsAbsolutePathsFromDetail(t *testing.T) {
	// The injected redact func (workflow.RedactCommentText) only strips secrets;
	// absolute checkout/worktree paths in a pre-flight failure detail must not
	// leave the box (#446 frozen criterion). NewEvent collapses them to <path>
	// even when redact is nil, so the scrub is unconditional.
	cases := map[string]struct {
		mustNotContain []string
		mustContain    []string
	}{
		"checkout /root/.gitmoot/repos/owner__repo/main is dirty": {
			mustNotContain: []string{"/root/.gitmoot", "/root"},
			mustContain:    []string{"<path>", "is dirty"},
		},
		"worktree add /root/.gitmoot/worktrees/some-job failed": {
			mustNotContain: []string{"/root/.gitmoot/worktrees", "/root"},
			mustContain:    []string{"<path>", "failed"},
		},
	}
	for detail, want := range cases {
		ev := NewEvent(EventJobFailed, "j", "r", "o/r", "failed", detail, time.Now(), nil)
		for _, frag := range want.mustNotContain {
			if strings.Contains(ev.Detail, frag) {
				t.Fatalf("detail %q still contains absolute path fragment %q: %q", detail, frag, ev.Detail)
			}
		}
		for _, frag := range want.mustContain {
			if !strings.Contains(ev.Detail, frag) {
				t.Fatalf("detail %q missing expected fragment %q: %q", detail, frag, ev.Detail)
			}
		}
	}

	// A URL is not an absolute filesystem path and must survive intact.
	ev := NewEvent(EventJobFinished, "j", "r", "o/r", "succeeded", "see https://github.com/owner/repo for details", time.Now(), nil)
	if !strings.Contains(ev.Detail, "https://github.com/owner/repo") {
		t.Fatalf("URL was mangled by the path scrubber: %q", ev.Detail)
	}

	// Scrubbing runs AFTER redaction: a secret and a path in the same detail are
	// both removed.
	redact := func(s string) string { return strings.ReplaceAll(s, "ghp_secretsecretsecret00", "[REDACTED]") }
	ev = NewEvent(EventJobFailed, "j", "r", "o/r", "failed", "token ghp_secretsecretsecret00 at /root/.gitmoot/x/y", time.Now(), redact)
	if strings.Contains(ev.Detail, "ghp_secretsecretsecret00") || strings.Contains(ev.Detail, "/root/.gitmoot") {
		t.Fatalf("secret or path leaked: %q", ev.Detail)
	}
	if !strings.Contains(ev.Detail, "[REDACTED]") || !strings.Contains(ev.Detail, "<path>") {
		t.Fatalf("expected both redaction marker and path placeholder: %q", ev.Detail)
	}
}

func TestNewEventRepoIsOwnerRepoOnly(t *testing.T) {
	cases := map[string]string{
		"jerryfane/gitmoot":            "jerryfane/gitmoot",
		"github.com/jerryfane/gitmoot": "jerryfane/gitmoot",
		"/abs/path/to/checkout":        "", // absolute path must never leak
		"":                             "",
		"singletoken":                  "singletoken",
	}
	for in, want := range cases {
		ev := NewEvent(EventJobFinished, "j", "r", in, "succeeded", "", time.Now(), nil)
		if ev.Repo != want {
			t.Fatalf("ownerRepoOnly(%q) -> %q, want %q", in, ev.Repo, want)
		}
	}
}

// TestNewEventCandidateDetailRedactedAndScrubbed proves a candidate.* event's
// Detail is redacted (secret) and path-scrubbed (no absolute path) like every
// other emit site, and SchemaVersion stays 1 for the additive enum values (#471).
func TestNewEventCandidateDetailRedactedAndScrubbed(t *testing.T) {
	redact := func(s string) string {
		return strings.ReplaceAll(s, "ghp_secretsecretsecret", "[REDACTED]")
	}
	detail := "candidate agent-template:foo:v7 promoted from /root/.gitmoot/evals/run; token ghp_secretsecretsecret"
	ev := NewEvent(EventCandidateAutoPromoted, "agent-template:foo:v7", "agent-template:foo", "", "auto_promoted", detail, time.Now(), redact)
	if ev.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1 (additive enum, no bump)", ev.SchemaVersion)
	}
	if strings.Contains(ev.Detail, "ghp_secretsecretsecret") {
		t.Fatalf("candidate detail not redacted: %q", ev.Detail)
	}
	if strings.Contains(ev.Detail, "/root/.gitmoot/evals/run") {
		t.Fatalf("candidate detail absolute path not scrubbed: %q", ev.Detail)
	}
	if ev.JobID != "agent-template:foo:v7" || ev.RootID != "agent-template:foo" {
		t.Fatalf("candidate event ids = %q/%q, want version/template id", ev.JobID, ev.RootID)
	}
}

func TestNewEventDefaultsTimestamp(t *testing.T) {
	ev := NewEvent(EventJobFinished, "j", "r", "o/r", "succeeded", "", time.Time{}, nil)
	if ev.Timestamp == "" {
		t.Fatal("zero ts should default to now, got empty Timestamp")
	}
	if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
		t.Fatalf("Timestamp %q is not RFC3339: %v", ev.Timestamp, err)
	}
}

// recordingSink captures Emit calls for the engine/daemon best-effort tests.
type recordingSink struct {
	events []Event
}

func (r *recordingSink) Emit(_ context.Context, event Event) {
	r.events = append(r.events, event)
}

func TestEmitEventNilSinkIsNoOp(t *testing.T) {
	// A nil Sink must be a no-op (the off-by-default guarantee): no panic, nothing
	// dispatched.
	EmitEvent(context.Background(), nil, NewEvent(EventJobFinished, "j", "r", "o/r", "succeeded", "", time.Now(), nil))

	// A real sink does dispatch, proving the nil-guard is the only short circuit.
	rec := &recordingSink{}
	EmitEvent(context.Background(), rec, NewEvent(EventJobFinished, "j", "r", "o/r", "succeeded", "", time.Now(), nil))
	if len(rec.events) != 1 {
		t.Fatalf("recording sink got %d events, want 1", len(rec.events))
	}
}
