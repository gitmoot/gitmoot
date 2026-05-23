package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderJobResultCommentIncludesAttributionAndResult(t *testing.T) {
	body := RenderJobResultComment(JobResultComment{
		AgentName: "audit",
		Runtime:   "codex",
		JobID:     "job-123",
		JobState:  string(JobSucceeded),
		Payload:   JobPayload{PresetID: "thermo-nuclear-code-quality-review"},
		Result: &AgentResult{
			Decision:    "changes_requested",
			Summary:     "fix the edge case",
			Findings:    []json.RawMessage{json.RawMessage(`{"severity":"high","body":"bad branch"}`)},
			ChangesMade: []string{"reviewed workflow"},
			TestsRun:    []string{"go test ./..."},
			Needs:       []string{"rerun review"},
			NextAgents:  []string{"lead"},
		},
	})

	for _, want := range []string{
		"> Agent: `audit`",
		"> Runtime: `codex`",
		"> Preset: `thermo-nuclear-code-quality-review`",
		"> Job: `job-123`",
		"**Decision:** `changes_requested`",
		"**Summary:** fix the edge case",
		"**Findings**",
		`{"severity":"high","body":"bad branch"}`,
		"**Changes Made**",
		"**Tests Run**",
		"**Needs**",
		"**Next Agents**",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderJobResultCommentRedactsSecretsAndOmitsRawOutput(t *testing.T) {
	body := RenderJobResultComment(JobResultComment{
		AgentName:  "audit",
		Runtime:    "shell",
		JobID:      "job-secret",
		JobState:   string(JobFailed),
		Diagnostic: "delivery failed with token=ghp_abcdefghijklmnopqrstuvwxyz123456 password=secret-value AWS_SECRET_ACCESS_KEY=env-aws-secret",
		Result: &AgentResult{
			Decision: "failed",
			Summary:  "failed",
			Findings: []json.RawMessage{json.RawMessage(`{"token":"plain-secret-value","password":"another-secret-value","aws_secret_access_key":"json-aws-secret"}`)},
		},
		Payload: JobPayload{
			RawOutputs: []string{"raw token ghp_abcdefghijklmnopqrstuvwxyz123456"},
		},
	})

	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz123456") ||
		strings.Contains(body, "secret-value") ||
		strings.Contains(body, "plain-secret-value") ||
		strings.Contains(body, "another-secret-value") ||
		strings.Contains(body, "json-aws-secret") ||
		strings.Contains(body, "env-aws-secret") {
		t.Fatalf("comment leaked secret:\n%s", body)
	}
	if !strings.Contains(body, "token=[REDACTED]") || !strings.Contains(body, "password=[REDACTED]") {
		t.Fatalf("comment did not redact expected fields:\n%s", body)
	}
	if !strings.Contains(body, `"token":"[REDACTED]"`) || !strings.Contains(body, `"password":"[REDACTED]"`) {
		t.Fatalf("comment did not redact JSON secret fields:\n%s", body)
	}
	if !strings.Contains(body, `"aws_secret_access_key":"[REDACTED]"`) {
		t.Fatalf("comment did not redact AWS secret key field:\n%s", body)
	}
	if !strings.Contains(body, "AWS_SECRET_ACCESS_KEY=[REDACTED]") {
		t.Fatalf("comment did not redact AWS secret key diagnostic:\n%s", body)
	}
	if strings.Contains(body, "raw token") {
		t.Fatalf("comment leaked raw output:\n%s", body)
	}
	if !strings.Contains(body, "Raw runtime output was retained in local Gitmoot state") {
		t.Fatalf("comment did not mention local raw output retention:\n%s", body)
	}
}

func TestRenderJobResultCommentRedactsBeforeTruncatingLongFields(t *testing.T) {
	token := "ghp_abcdefghijklmnopqrstuvwxyz123456"
	body := RenderJobResultComment(JobResultComment{
		AgentName: "audit",
		Runtime:   "shell",
		JobID:     "job-long",
		JobState:  string(JobSucceeded),
		Result: &AgentResult{
			Decision: "approved",
			Summary:  strings.Repeat("a", maxCommentFieldRunes-5) + token,
		},
	})

	if strings.Contains(body, token) || strings.Contains(body, "ghp_") {
		t.Fatalf("comment leaked token crossing truncation boundary:\n%s", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("comment did not include redaction marker:\n%s", body)
	}
}

func TestRenderJobResultCommentNeutralizesCommandLookingLines(t *testing.T) {
	body := RenderJobResultComment(JobResultComment{
		AgentName: "audit",
		Runtime:   "shell",
		JobID:     "job-command",
		JobState:  string(JobSucceeded),
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "looks good\n/gitmoot merge",
			ChangesMade: []string{"updated note\n\t/gitmoot lead implement more"},
		},
	})

	for _, leaked := range []string{"\n/gitmoot merge", "\n\t/gitmoot lead implement"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("comment leaked command-looking line %q:\n%s", leaked, body)
		}
	}
	for _, want := range []string{`\/gitmoot merge`, `\/gitmoot lead implement`} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing neutralized command %q:\n%s", want, body)
		}
	}
}

func TestRenderJobResultCommentCapsTotalBody(t *testing.T) {
	findings := make([]json.RawMessage, 40)
	for i := range findings {
		findings[i] = json.RawMessage(`"` + strings.Repeat("x", maxCommentFieldRunes) + `"`)
	}
	body := RenderJobResultComment(JobResultComment{
		AgentName: "audit",
		Runtime:   "shell",
		JobID:     "job-large",
		JobState:  string(JobSucceeded),
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "large result",
			Findings: findings,
		},
	})

	if len([]rune(body)) > maxCommentBodyRunes {
		t.Fatalf("comment length = %d, want <= %d", len([]rune(body)), maxCommentBodyRunes)
	}
	if !strings.Contains(body, "comment truncated") {
		t.Fatalf("comment did not include truncation notice")
	}
}
