package daemon

import (
	"fmt"
	"strings"
)

type Command struct {
	Action       string
	Agent        string
	Instructions string
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
	if len(fields) == 0 || fields[0] != "/gitmoot" {
		return Command{}, false
	}
	if len(fields) == 1 {
		return Command{}, false
	}

	switch fields[1] {
	case "status", "merge":
		return Command{Action: fields[1], Instructions: trailing(fields, 2)}, true
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
	case "review", "implement", "ask", "status", "merge":
	default:
		return fmt.Errorf("unsupported command action %q", c.Action)
	}
	if c.Action != "status" && c.Action != "merge" && c.Agent == "" {
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
