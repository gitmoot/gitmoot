package prompts

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderJobIncludesContextAndContract(t *testing.T) {
	prompt := RenderJob(JobPrompt{
		Repo:                   "jerryfane/gitmoot",
		Branch:                 "task-005",
		PullRequest:            5,
		Task:                   "task-5: Job Mailbox",
		Sender:                 "octocat",
		Action:                 "review",
		Instructions:           "focus on state transitions",
		Constraints:            []string{"Preserve existing behavior", "", "Return JSON"},
		TemplateID:             "thermo",
		TemplateResolvedCommit: "abc123",
		TemplateInstructions:   "Review deeply.",
	})

	for _, want := range []string{
		"Template: thermo",
		"Template source commit: abc123",
		"Template instructions:\nReview deeply.",
		"Job context:",
		"Repo: jerryfane/gitmoot",
		"Branch: task-005",
		"Pull request: #5",
		"Task: task-5: Job Mailbox",
		"Sender: octocat",
		"Requested action: review",
		"- Preserve existing behavior",
		"gitmoot_result",
		`"decision":"approved|changes_requested|blocked|implemented|failed"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderRepairPromptIncludesErrorAndRawOutput(t *testing.T) {
	prompt := RenderRepairPrompt("not json", errors.New("missing gitmoot_result"))

	for _, want := range []string{
		"did not contain a valid gitmoot_result",
		"Parse error: missing gitmoot_result",
		"Return only one JSON object",
		"not json",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
}
