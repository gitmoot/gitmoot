package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// buildLocalAgentJobOutput must render a terminally-succeeded implement job into
// the same populated output the success path returns, so the advance-error
// recovery branch can surface the persisted result instead of discarding it.
func TestBuildLocalAgentJobOutputRendersSucceededJob(t *testing.T) {
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        "owner/repo",
		PullRequest: 7,
		Result:      &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
		RawOutputs:  []string{`{"gitmoot_result":{}}`},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job := db.Job{
		ID:      "local-implement-lead-abc",
		Agent:   "lead",
		Type:    "implement",
		State:   string(workflow.JobSucceeded),
		Payload: string(payload),
	}
	out, err := buildLocalAgentJobOutput(job, localAgentDispatchRequest{
		SelectedAction:       "implement",
		SelectedActionReason: "explicit agent implement",
		ExecutionPath:        "agent_implement",
	})
	if err != nil {
		t.Fatalf("buildLocalAgentJobOutput returned error: %v", err)
	}
	if out.JobID != job.ID || out.State != string(workflow.JobSucceeded) || out.Repo != "owner/repo" {
		t.Fatalf("output = %+v", out)
	}
	if out.Result == nil || out.Result.Summary != "opened PR" || out.RawOutputCount != 1 {
		t.Fatalf("output result = %+v (raw=%d)", out.Result, out.RawOutputCount)
	}
	if out.AdvanceError != "" {
		t.Fatalf("AdvanceError = %q, want empty by default", out.AdvanceError)
	}
}

// The terminal-success output, when an advance error is attached, must serialize
// the result AND the advance_error in --json mode, and render an advance_error
// line in human mode. This is the #387 surface: exit 0 with the result on
// stdout and the advance warning carried alongside.
func TestLocalAgentJobOutputSurfacesAdvanceError(t *testing.T) {
	out := localAgentJobOutput{
		JobID:        "local-implement-lead-abc",
		State:        string(workflow.JobSucceeded),
		Repo:         "owner/repo",
		Agent:        "lead",
		Action:       "implement",
		Result:       &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
		AdvanceError: "workflow advance failed: workflow blocked: ci is pending",
	}

	t.Run("json includes advance_error and result", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeJSON(&buf, out); err != nil {
			t.Fatalf("writeJSON returned error: %v", err)
		}
		var decoded localAgentJobOutput
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatalf("decode: %v\n%s", err, buf.String())
		}
		if decoded.AdvanceError != out.AdvanceError {
			t.Fatalf("decoded advance_error = %q", decoded.AdvanceError)
		}
		if decoded.Result == nil || decoded.Result.Summary != "opened PR" {
			t.Fatalf("decoded result = %+v", decoded.Result)
		}
		if !strings.Contains(buf.String(), `"advance_error"`) {
			t.Fatalf("json missing advance_error key:\n%s", buf.String())
		}
	})

	t.Run("human mode prints advance_error line", func(t *testing.T) {
		var buf bytes.Buffer
		printLocalAgentJobOutput(&buf, out)
		if !strings.Contains(buf.String(), "advance_error: workflow advance failed: workflow blocked: ci is pending") {
			t.Fatalf("human output missing advance_error line:\n%s", buf.String())
		}
		// The result still renders.
		if !strings.Contains(buf.String(), "summary: opened PR") {
			t.Fatalf("human output missing result summary:\n%s", buf.String())
		}
	})
}

// By default (no advance error) the JSON output must NOT carry an advance_error
// key — the field is omitempty, so the normal success path stays byte-identical.
func TestLocalAgentJobOutputOmitsAdvanceErrorByDefault(t *testing.T) {
	out := localAgentJobOutput{
		JobID:  "local-implement-lead-abc",
		State:  string(workflow.JobSucceeded),
		Repo:   "owner/repo",
		Agent:  "lead",
		Action: "implement",
		Result: &workflow.AgentResult{Decision: "implemented", Summary: "opened PR"},
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, out); err != nil {
		t.Fatalf("writeJSON returned error: %v", err)
	}
	if strings.Contains(buf.String(), "advance_error") {
		t.Fatalf("default json should omit advance_error:\n%s", buf.String())
	}
	var human bytes.Buffer
	printLocalAgentJobOutput(&human, out)
	if strings.Contains(human.String(), "advance_error") {
		t.Fatalf("default human output should omit advance_error:\n%s", human.String())
	}
}
