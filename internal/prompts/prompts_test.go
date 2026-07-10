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

func TestRenderJobIncludesDelegationArtifacts(t *testing.T) {
	prompt := RenderJob(JobPrompt{
		Repo:                  "jerryfane/gitmoot",
		Branch:                "task-005",
		Action:                "implement",
		Instructions:          "build the api",
		DelegationArtifactDir: "/home/user/.gitmoot/delegations/parent-job",
	})

	for _, want := range []string{
		"Delegation artifacts:",
		"/home/user/.gitmoot/delegations/parent-job/brief.md",
		"/home/user/.gitmoot/delegations/parent-job/context-manifest.json",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderJobOmitsDelegationArtifactsWhenUnset(t *testing.T) {
	prompt := RenderJob(JobPrompt{
		Repo:         "jerryfane/gitmoot",
		Branch:       "task-005",
		Action:       "implement",
		Instructions: "build the api",
	})
	if strings.Contains(prompt, "Delegation artifacts:") {
		t.Fatalf("prompt unexpectedly includes delegation artifacts section:\n%s", prompt)
	}
}

// TestRenderJobCommitContractLine pins the #805 commit contract: an implement
// prompt carries the one deterministic sentence telling the worker that Gitmoot
// owns commit+delivery, and ask/review prompts never carry it (they stay
// byte-identical to before #805 — the sentence is the only conditional write).
func TestRenderJobCommitContractLine(t *testing.T) {
	const contract = "Gitmoot commits and delivers your changes after you finish; do not run git commit or git push."
	base := JobPrompt{
		Repo:         "jerryfane/gitmoot",
		Branch:       "task-005",
		Instructions: "build the api",
	}

	implement := base
	implement.Action = "implement"
	if got := RenderJob(implement); !strings.Contains(got, contract) {
		t.Fatalf("implement prompt missing commit contract line:\n%s", got)
	}

	for _, action := range []string{"ask", "review"} {
		other := base
		other.Action = action
		if got := RenderJob(other); strings.Contains(got, contract) {
			t.Fatalf("%s prompt unexpectedly carries the commit contract line:\n%s", action, got)
		}
	}
}

func TestRenderRepairPromptIncludesErrorAndRawOutput(t *testing.T) {
	prompt := RenderRepairPrompt("not json", errors.New("missing gitmoot_result"))

	for _, want := range []string{
		"did not contain a valid gitmoot_result",
		"Validation errors (fix every line):",
		"- missing gitmoot_result",
		"Return only one JSON object",
		"not json",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Parse error:") {
		t.Fatalf("repair prompt unexpectedly includes the old Parse error label:\n%s", prompt)
	}
}

func TestRenderRepairPromptSurfacesJoinedFieldErrors(t *testing.T) {
	// Simulate an errors.Join'd validation failure against the frozen contract:
	// each per-field error renders as one line, joined with newlines.
	joined := errors.New(`delegations[0] (id "<missing>"): id is required
delegations[1] (id "build-api"): action is required`)
	prompt := RenderRepairPrompt("bad output", joined)

	for _, want := range []string{
		"Validation errors (fix every line):",
		`- delegations[0] (id "<missing>"): id is required`,
		`- delegations[1] (id "build-api"): action is required`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderRepairPromptIncludesSchemaHelpHint(t *testing.T) {
	prompt := RenderRepairPrompt("bad output", errors.New("missing gitmoot_result"))
	want := `delegations[<index>] (id "<id>")`
	if !strings.Contains(prompt, want) {
		t.Fatalf("repair prompt missing schema-help hint %q:\n%s", want, prompt)
	}
}

func TestRenderJobIncludesDelegationValidationHint(t *testing.T) {
	prompt := RenderJob(JobPrompt{
		Repo:         "jerryfane/gitmoot",
		Branch:       "task-005",
		Action:       "implement",
		Instructions: "build the api",
	})
	want := `delegations[<index>] (id "<id>")`
	if !strings.Contains(prompt, want) {
		t.Fatalf("job prompt missing delegation validation hint %q:\n%s", want, prompt)
	}
}
