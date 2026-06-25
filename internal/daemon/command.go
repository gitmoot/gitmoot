package daemon

import (
	"fmt"
	"strings"
)

type Command struct {
	Action       string
	Agent        string
	JobID        string
	Instructions string
	// Decision is the resume verb (retry|continue|abort) for the `resume` action
	// (#340); empty for every other action.
	Decision string
}

func ParseCommands(body string) []Command {
	var commands []Command
	for _, line := range strings.Split(body, "\n") {
		command, ok := ParseCommand(line)
		if ok {
			commands = append(commands, command)
		}
	}
	return commands
}

func ParseCommand(line string) (Command, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return Command{}, false
	}
	// Issue/PR mention form (#389): a bare `@<agent> <action> …` line routes to
	// that agent, exactly like the `/gitmoot <agent> <action> …` agent-first form.
	// This is the natural way a user summons an agent on an issue, so it is the
	// trigger the issue-comment watcher actually receives.
	if strings.HasPrefix(fields[0], "@") && len(fields[0]) > 1 {
		if len(fields) < 2 {
			return Command{}, false
		}
		return Command{Action: fields[1], Agent: cleanAgent(fields[0]), Instructions: trailing(fields, 2)}, true
	}
	if fields[0] != "/gitmoot" {
		return Command{}, false
	}
	if len(fields) == 1 {
		return Command{}, false
	}

	switch fields[1] {
	case "status", "merge", "help":
		return Command{Action: fields[1], Instructions: trailing(fields, 2)}, true
	case "retry", "cancel":
		if len(fields) < 3 {
			return Command{}, false
		}
		return Command{Action: fields[1], JobID: fields[2], Instructions: trailing(fields, 3)}, true
	case "resume":
		// `/gitmoot resume <jobID> <retry|continue|abort|answer> [instructions]`
		// (#340; `answer` added by #445). For `answer`, Instructions is the trailing
		// `<id>: text` payload (multi-line for several questions).
		if len(fields) < 4 {
			return Command{}, false
		}
		return Command{Action: "resume", JobID: fields[2], Decision: strings.ToLower(fields[3]), Instructions: trailing(fields, 4)}, true
	case "ask":
		if len(fields) < 3 {
			return Command{}, false
		}
		return Command{Action: "ask", Agent: cleanAgent(fields[2]), Instructions: trailing(fields, 3)}, true
	default:
		if len(fields) < 3 {
			return Command{}, false
		}
		return Command{Action: fields[2], Agent: cleanAgent(fields[1]), Instructions: trailing(fields, 3)}, true
	}
}

func (c Command) Validate() error {
	switch c.Action {
	case "review", "implement", "ask", "status", "merge", "retry", "cancel", "help", "resume":
	default:
		return fmt.Errorf("unsupported command action %q", c.Action)
	}
	if (c.Action == "retry" || c.Action == "cancel" || c.Action == "resume") && c.JobID == "" {
		return fmt.Errorf("command %q requires a job id", c.Action)
	}
	if c.Action == "resume" {
		switch c.Decision {
		case "retry", "continue", "abort", "answer":
		default:
			return fmt.Errorf("command resume requires a decision of retry, continue, abort, or answer")
		}
	}
	if c.Action != "status" && c.Action != "merge" && c.Action != "retry" && c.Action != "cancel" && c.Action != "help" && c.Action != "resume" && c.Agent == "" {
		return fmt.Errorf("command %q requires an agent", c.Action)
	}
	return nil
}

func cleanAgent(agent string) string {
	return strings.TrimPrefix(strings.TrimSpace(agent), "@")
}

func trailing(fields []string, start int) string {
	if len(fields) <= start {
		return ""
	}
	return strings.Join(fields[start:], " ")
}
