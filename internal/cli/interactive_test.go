package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestInteractiveListShowAndAnswer(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertInteractivePrompt(context.Background(), db.InteractivePrompt{
		ID:            "train-init-template",
		Question:      "Which template should Gitmoot train?",
		Choices:       []string{"planner", "writer"},
		Default:       "planner",
		Required:      true,
		AnswerFormat:  "choice",
		SourceCommand: "gitmoot skillopt train init",
	}); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var listStdout, listStderr bytes.Buffer
	code := Run([]string{"interactive", "list", "--home", home, "--json"}, &listStdout, &listStderr)
	if code != 0 {
		t.Fatalf("interactive list code=%d stderr=%s", code, listStderr.String())
	}
	var prompts []db.InteractivePrompt
	if err := json.Unmarshal(listStdout.Bytes(), &prompts); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, listStdout.String())
	}
	if len(prompts) != 1 || prompts[0].ID != "train-init-template" || prompts[0].State != db.InteractivePromptStatePending {
		t.Fatalf("prompts = %+v", prompts)
	}

	var showStdout, showStderr bytes.Buffer
	code = Run([]string{"interactive", "show", "--home", home, "train-init-template", "--json"}, &showStdout, &showStderr)
	if code != 0 {
		t.Fatalf("interactive show code=%d stderr=%s", code, showStderr.String())
	}
	var shown db.InteractivePrompt
	if err := json.Unmarshal(showStdout.Bytes(), &shown); err != nil {
		t.Fatalf("decode show JSON: %v\n%s", err, showStdout.String())
	}
	if shown.Question == "" || len(shown.Choices) != 2 || shown.Default != "planner" {
		t.Fatalf("shown prompt = %+v", shown)
	}

	var answerStdout, answerStderr bytes.Buffer
	code = Run([]string{"interactive", "answer", "--home", home, "train-init-template", "writer", "--source", "agent"}, &answerStdout, &answerStderr)
	if code != 0 {
		t.Fatalf("interactive answer code=%d stderr=%s", code, answerStderr.String())
	}
	if !strings.Contains(answerStdout.String(), "answered train-init-template: writer") {
		t.Fatalf("answer stdout = %q", answerStdout.String())
	}

	store = openCLIJobStore(t, home)
	resolved, err := store.GetInteractivePrompt(context.Background(), "train-init-template")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if resolved.State != db.InteractivePromptStateResolved || resolved.AnswerValue != "writer" || resolved.AnswerSource != "agent" {
		t.Fatalf("resolved prompt = %+v", resolved)
	}
}

func TestInteractiveAnswerRejectsInvalidChoice(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertInteractivePrompt(context.Background(), db.InteractivePrompt{
		ID:       "mode",
		Question: "Which mode?",
		Choices:  []string{"explore", "refine"},
		Required: true,
	}); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"interactive", "answer", "--home", home, "mode", "validate"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "not one of") {
		t.Fatalf("invalid answer code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	store = openCLIJobStore(t, home)
	prompt, err := store.GetInteractivePrompt(context.Background(), "mode")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if prompt.State != db.InteractivePromptStatePending || prompt.AnswerValue != "" {
		t.Fatalf("prompt after invalid answer = %+v", prompt)
	}
}

func TestInteractiveAnswerAcceptsDashPrefixedTextAfterDelimiter(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	if err := store.UpsertInteractivePrompt(context.Background(), db.InteractivePrompt{
		ID:       "text",
		Question: "What text?",
		Required: true,
	}); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"interactive", "answer", "--home", home, "text", "--", "--force"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dash-prefixed answer code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	store = openCLIJobStore(t, home)
	prompt, err := store.GetInteractivePrompt(context.Background(), "text")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if prompt.AnswerValue != "--force" {
		t.Fatalf("prompt answer = %+v", prompt)
	}
}
