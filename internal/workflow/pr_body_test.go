package workflow

import (
	"strings"
	"testing"
)

func TestRenderPullRequestBodyIncludesRequiredSections(t *testing.T) {
	body, err := RenderPullRequestBody(PullRequestBody{
		TaskID:          "task-8",
		AgentNames:      []string{"lead", "audit"},
		What:            "Add branch rules.",
		Why:             "Keep task work isolated.",
		Changes:         []string{"Added lock release.", "Rendered PR body."},
		Results:         []string{"go test ./..."},
		Risk:            "No skipped checks.",
		RawReviewOutput: "codex exec review is clean",
	})
	if err != nil {
		t.Fatalf("RenderPullRequestBody returned error: %v", err)
	}
	for _, section := range []string{"WHAT:", "WHY:", "CHANGES:", "RESULTS:", "RISK:", "TASK:", "AGENTS:", "RAW FINAL REVIEW OUTPUT:"} {
		if !strings.Contains(body, section) {
			t.Fatalf("body missing section %s:\n%s", section, body)
		}
	}
	for _, value := range []string{"task-8", "lead", "audit", "codex exec review is clean"} {
		if !strings.Contains(body, value) {
			t.Fatalf("body missing %q:\n%s", value, body)
		}
	}
}

func TestRenderPullRequestBodyRequiresTaskAndReviewOutput(t *testing.T) {
	if _, err := RenderPullRequestBody(PullRequestBody{RawReviewOutput: "ok"}); err == nil {
		t.Fatal("RenderPullRequestBody accepted empty task id")
	}
	if _, err := RenderPullRequestBody(PullRequestBody{TaskID: "task-8"}); err == nil {
		t.Fatal("RenderPullRequestBody accepted empty raw review output")
	}
}

func TestRenderPullRequestBodyPreservesRawReviewOutput(t *testing.T) {
	raw := "\n codex exec review is clean \n\n"
	body, err := RenderPullRequestBody(PullRequestBody{
		TaskID:          "task-8",
		RawReviewOutput: raw,
	})
	if err != nil {
		t.Fatalf("RenderPullRequestBody returned error: %v", err)
	}
	want := "````text\n" + raw + "````\n"
	if !strings.Contains(body, want) {
		t.Fatalf("raw review output was not preserved:\n%s", body)
	}
}

func TestRenderPullRequestBodyUsesSafeRawOutputFence(t *testing.T) {
	raw := "review output\n```text\ninside\n```\n"
	body, err := RenderPullRequestBody(PullRequestBody{
		TaskID:          "task-8",
		RawReviewOutput: raw,
	})
	if err != nil {
		t.Fatalf("RenderPullRequestBody returned error: %v", err)
	}
	want := "````text\n" + raw + "````\n"
	if !strings.Contains(body, want) {
		t.Fatalf("raw review output fence was not safe:\n%s", body)
	}
}
