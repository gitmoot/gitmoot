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

func TestValidateRejectsUnsupportedCommand(t *testing.T) {
	if err := (Command{Action: "deploy", Agent: "audit"}).Validate(); err == nil {
		t.Fatal("Validate accepted unsupported action")
	}
}
