package daemon

import "testing"

func TestParseCommandAgentFirstActions(t *testing.T) {
	command, ok := ParseCommand("/gitmoot audit review check the branch")
	if !ok {
		t.Fatal("ParseCommand did not parse review command")
	}
	if command.Agent != "audit" || command.Action != "review" || command.Instructions != "check the branch" {
		t.Fatalf("command = %+v", command)
	}

	command, ok = ParseCommand("/gitmoot @builder implement fix tests")
	if !ok {
		t.Fatal("ParseCommand did not parse implement command")
	}
	if command.Agent != "builder" || command.Action != "implement" || command.Instructions != "fix tests" {
		t.Fatalf("command = %+v", command)
	}
}

func TestParseCommandAskAgentShape(t *testing.T) {
	command, ok := ParseCommand("/gitmoot ask reviewer what failed?")
	if !ok {
		t.Fatal("ParseCommand did not parse ask command")
	}
	if command.Agent != "reviewer" || command.Action != "ask" || command.Instructions != "what failed?" {
		t.Fatalf("command = %+v", command)
	}
}

// TestParseCommandMentionForm is the regression guard for the #389 live bug: a
// real user summons an agent on an issue with a bare `@<agent> ask …` mention,
// not `/gitmoot <agent> ask …`. The parser previously required the line to start
// with `/gitmoot`, so the mention was silently dropped — PollIssuesOnce saw zero
// commands, routed no job, and posted no reply. ParseCommand must now treat a
// leading `@<agent>` as the same agent-first command, with the `@` stripped.
func TestParseCommandMentionForm(t *testing.T) {
	command, ok := ParseCommand("@helper ask Reply with exactly: ok")
	if !ok {
		t.Fatal("ParseCommand did not parse @<agent> ask mention")
	}
	if command.Agent != "helper" || command.Action != "ask" || command.Instructions != "Reply with exactly: ok" {
		t.Fatalf("command = %+v", command)
	}

	// The mention form is general agent-first, mirroring the /gitmoot form.
	command, ok = ParseCommand("@builder implement fix the flaky test")
	if !ok {
		t.Fatal("ParseCommand did not parse @<agent> implement mention")
	}
	if command.Agent != "builder" || command.Action != "implement" || command.Instructions != "fix the flaky test" {
		t.Fatalf("command = %+v", command)
	}

	// A bare `@agent` with no action is not a command.
	if _, ok := ParseCommand("@helper"); ok {
		t.Fatal("ParseCommand accepted a bare @agent with no action")
	}

	// A lone `@` is not a mention.
	if _, ok := ParseCommand("@ ask something"); ok {
		t.Fatal("ParseCommand accepted a lone @ as a mention")
	}

	// Plain prose that merely contains an @mention mid-line is not a command.
	if _, ok := ParseCommand("thanks @helper for the help"); ok {
		t.Fatal("ParseCommand accepted mid-line prose as a mention command")
	}
}

func TestParseCommandsOnlyReturnsGitmootLines(t *testing.T) {
	commands := ParseCommands("hello\n/gitmoot status\n/gitmoot merge when ready\nthanks")
	if len(commands) != 2 {
		t.Fatalf("commands length = %d, want 2", len(commands))
	}
	if commands[0].Action != "status" || commands[1].Action != "merge" || commands[1].Instructions != "when ready" {
		t.Fatalf("commands = %+v", commands)
	}
}

func TestParseJobRecoveryCommands(t *testing.T) {
	command, ok := ParseCommand("/gitmoot retry job-123")
	if !ok {
		t.Fatal("ParseCommand did not parse retry command")
	}
	if command.Action != "retry" || command.JobID != "job-123" {
		t.Fatalf("retry command = %+v", command)
	}
	command, ok = ParseCommand("/gitmoot cancel job-456")
	if !ok {
		t.Fatal("ParseCommand did not parse cancel command")
	}
	if command.Action != "cancel" || command.JobID != "job-456" {
		t.Fatalf("cancel command = %+v", command)
	}
}

func TestParseHelpCommand(t *testing.T) {
	command, ok := ParseCommand("/gitmoot help")
	if !ok {
		t.Fatal("ParseCommand did not parse help command")
	}
	if command.Action != "help" || command.Agent != "" {
		t.Fatalf("help command = %+v", command)
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateRejectsUnsupportedCommand(t *testing.T) {
	if err := (Command{Action: "deploy", Agent: "audit"}).Validate(); err == nil {
		t.Fatal("Validate accepted unsupported action")
	}
}

// TestParseCommandResume pins the #340 resume grammar:
// `/gitmoot resume <jobID> <retry|continue|abort> [instructions]`.
func TestParseCommandResume(t *testing.T) {
	command, ok := ParseCommand("/gitmoot resume job-1 retry use the staging endpoint")
	if !ok {
		t.Fatal("ParseCommand did not parse resume command")
	}
	if command.Action != "resume" || command.JobID != "job-1" || command.Decision != "retry" || command.Instructions != "use the staging endpoint" {
		t.Fatalf("command = %+v", command)
	}

	// Decision normalizes case; instructions are optional.
	command, ok = ParseCommand("/gitmoot resume job-2 CONTINUE")
	if !ok {
		t.Fatal("ParseCommand did not parse resume continue command")
	}
	if command.Decision != "continue" || command.Instructions != "" {
		t.Fatalf("command = %+v", command)
	}

	// Missing the decision verb is not a valid resume command.
	if _, ok := ParseCommand("/gitmoot resume job-3"); ok {
		t.Fatal("ParseCommand accepted resume without a decision")
	}
}

func TestValidateResume(t *testing.T) {
	if err := (Command{Action: "resume", JobID: "job-1", Decision: "retry"}).Validate(); err != nil {
		t.Fatalf("Validate rejected a valid resume: %v", err)
	}
	if err := (Command{Action: "resume", Decision: "retry"}).Validate(); err == nil {
		t.Fatal("Validate accepted resume without a job id")
	}
	if err := (Command{Action: "resume", JobID: "job-1", Decision: "bogus"}).Validate(); err == nil {
		t.Fatal("Validate accepted resume with an invalid decision")
	}
	if err := (Command{Action: "resume", JobID: "job-1"}).Validate(); err == nil {
		t.Fatal("Validate accepted resume without a decision")
	}
}

// TestParseCommandResumeAnswer pins the #445 ask-gate answer grammar:
// `/gitmoot resume <jobID> answer "<id>: ..."` — Decision=answer and Instructions
// is the trailing `<id>: text` payload.
func TestParseCommandResumeAnswer(t *testing.T) {
	command, ok := ParseCommand(`/gitmoot resume job-1 answer q1: v3`)
	if !ok {
		t.Fatal("ParseCommand did not parse resume answer command")
	}
	if command.Action != "resume" || command.JobID != "job-1" || command.Decision != "answer" || command.Instructions != "q1: v3" {
		t.Fatalf("command = %+v", command)
	}
}

func TestValidateResumeAnswer(t *testing.T) {
	if err := (Command{Action: "resume", JobID: "job-1", Decision: "answer", Instructions: "q1: v3"}).Validate(); err != nil {
		t.Fatalf("Validate rejected a valid answer resume: %v", err)
	}
}
