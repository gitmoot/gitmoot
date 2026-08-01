package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/mention"
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

type parsedCommentCommand struct {
	command Command
	err     error
}

type commentCommandLine struct {
	text      string
	addressed bool
}

type commentCommandInput struct {
	lines     []commentCommandLine
	addressed bool
}

// ParseCommandsWithoutAuthorization sanitizes and parses addressed command
// lines without checking repository permission. Production comment handlers
// must authorize the author before calling the underlying parser.
func ParseCommandsWithoutAuthorization(body string) []Command {
	input := prepareCommentCommandInput(body)
	var commands []Command
	for _, parsed := range parseCommentCommands(input, ParseCommand) {
		if parsed.err == nil {
			commands = append(commands, parsed.command)
		}
	}
	return commands
}

// prepareCommentCommandInput removes Markdown code before any command parser is
// called. It also records whether an outside-code line was actually addressed
// to Gitmoot, preserving the original first-token position so removing a leading
// inline span cannot expose a later /gitmoot token as a new command.
func prepareCommentCommandInput(body string) commentCommandInput {
	lines := strings.Split(body, "\n")
	input := commentCommandInput{lines: make([]commentCommandLine, 0, len(lines))}
	var fence markdownFence
	for _, line := range lines {
		if isMarkdownIndentedCodeLine(line) {
			input.lines = append(input.lines, commentCommandLine{})
			continue
		}
		if fence.marker != 0 {
			if isMarkdownFenceClose(line, fence) {
				fence = markdownFence{}
			}
			input.lines = append(input.lines, commentCommandLine{})
			continue
		}
		if opened, ok := markdownFenceOpen(line); ok {
			fence = opened
			input.lines = append(input.lines, commentCommandLine{})
			continue
		}
		addressed := isCommandAddressedLine(line)
		input.addressed = input.addressed || addressed
		input.lines = append(input.lines, commentCommandLine{
			text:      stripInlineCodeSpans(line),
			addressed: addressed,
		})
	}
	return input
}

// isMarkdownIndentedCodeLine recognizes CommonMark's older indented-code form.
// Tabs advance to four-column stops, so a leading tab or spaces followed by a
// tab can establish the same code indentation as four literal spaces.
func isMarkdownIndentedCodeLine(line string) bool {
	column := 0
	for offset := 0; offset < len(line); offset++ {
		switch line[offset] {
		case ' ':
			column++
		case '\t':
			column += 4 - column%4
		default:
			return false
		}
		if column >= 4 {
			return true
		}
	}
	return false
}

func parseCommentCommands(input commentCommandInput, parse func(string) (Command, bool)) []parsedCommentCommand {
	commands := make([]parsedCommentCommand, 0)
	for _, line := range input.lines {
		if !line.addressed {
			continue
		}
		command, ok := parse(line.text)
		if !ok {
			commands = append(commands, parsedCommentCommand{err: malformedCommentCommand(line.text)})
			continue
		}
		commands = append(commands, parsedCommentCommand{command: command})
	}
	return commands
}

func malformedCommentCommand(line string) error {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) > 0 && fields[0] == "/gitmoot" {
		return errors.New("malformed /gitmoot command")
	}
	return errors.New("malformed agent mention command")
}

func isCommandAddressedLine(line string) bool {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "/gitmoot" || (strings.HasPrefix(fields[0], "@") && len(fields[0]) > 1)
}

type markdownFence struct {
	marker byte
	length int
}

func markdownFenceOpen(line string) (markdownFence, bool) {
	content := markdownFenceContent(line)
	if len(content) < 3 || (content[0] != '`' && content[0] != '~') {
		return markdownFence{}, false
	}
	marker := content[0]
	length := markerRunLength(content, marker)
	if length < 3 {
		return markdownFence{}, false
	}
	// CommonMark does not allow a backtick in a backtick fence's info string.
	if marker == '`' && strings.Contains(content[length:], "`") {
		return markdownFence{}, false
	}
	return markdownFence{marker: marker, length: length}, true
}

func isMarkdownFenceClose(line string, fence markdownFence) bool {
	content := markdownFenceContent(line)
	if len(content) < fence.length || content[0] != fence.marker {
		return false
	}
	length := markerRunLength(content, fence.marker)
	return length >= fence.length && strings.TrimSpace(content[length:]) == ""
}

// markdownFenceContent removes CommonMark's blockquote container prefixes and
// up to three spaces of fence indentation. Four-space-indented fence-looking
// text is an indented code block rather than a fenced block and is deliberately
// not treated as an opener here.
func markdownFenceContent(line string) string {
	content := strings.TrimSuffix(line, "\r")
	for {
		offset := leadingSpaces(content, 3)
		if offset >= len(content) || content[offset] != '>' {
			break
		}
		content = content[offset+1:]
		if len(content) > 0 && (content[0] == ' ' || content[0] == '\t') {
			content = content[1:]
		}
	}
	return content[leadingSpaces(content, 3):]
}

func leadingSpaces(value string, limit int) int {
	count := 0
	for count < len(value) && count < limit && value[count] == ' ' {
		count++
	}
	return count
}

func markerRunLength(value string, marker byte) int {
	length := 0
	for length < len(value) && value[length] == marker {
		length++
	}
	return length
}

// stripInlineCodeSpans strips closed CommonMark-style backtick spans, including
// the requested single-backtick form. Unmatched delimiter runs remain literal,
// as CommonMark specifies. Spaces preserve token boundaries without retaining
// code content.
func stripInlineCodeSpans(line string) string {
	var out strings.Builder
	for offset := 0; offset < len(line); {
		if line[offset] != '`' {
			out.WriteByte(line[offset])
			offset++
			continue
		}
		run := markerRunLength(line[offset:], '`')
		closeAt := matchingBacktickRun(line, offset+run, run)
		if closeAt < 0 {
			out.WriteString(line[offset : offset+run])
			offset += run
			continue
		}
		out.WriteByte(' ')
		offset = closeAt + run
	}
	return out.String()
}

func matchingBacktickRun(line string, start, want int) int {
	for offset := start; offset < len(line); {
		if line[offset] != '`' {
			offset++
			continue
		}
		run := markerRunLength(line[offset:], '`')
		if run == want {
			return offset
		}
		offset += run
	}
	return -1
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
		return Command{Action: fields[1], Agent: mention.Clean(fields[0]), Instructions: trailing(fields, 2)}, true
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
		return Command{Action: "ask", Agent: mention.Clean(fields[2]), Instructions: trailing(fields, 3)}, true
	default:
		if len(fields) < 3 {
			return Command{}, false
		}
		return Command{Action: fields[2], Agent: mention.Clean(fields[1]), Instructions: trailing(fields, 3)}, true
	}
}

// ErrUnsupportedAction reports a line that parsed structurally as an addressed
// command but names an action Gitmoot does not implement. Ordinary source code
// reaches this path routinely: a decorator or attribute line such as
// `@Published private(set) var state = .uninitialized` parses as the mention
// form `@<agent> <action>`, yielding the action "private(set)". Callers must
// log it and stay silent rather than reply on the thread — replying posts
// Gitmoot's parser errors onto unrelated repositories (#1355).
var ErrUnsupportedAction = errors.New("unsupported command action")

func (c Command) Validate() error {
	switch c.Action {
	case "review", "implement", "ask", "status", "merge", "retry", "cancel", "help", "resume":
	default:
		return fmt.Errorf("%w %q", ErrUnsupportedAction, c.Action)
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

func trailing(fields []string, start int) string {
	if len(fields) <= start {
		return ""
	}
	return strings.Join(fields[start:], " ")
}
