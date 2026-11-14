package skills_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/cli"
	"github.com/gitmoot/gitmoot/internal/clidoc"
	"github.com/gitmoot/gitmoot/skills"
)

// TestSkillCommandLinesParse checks every `gitmoot ...` command line the
// hand-written skill files show (fenced code blocks and inline code spans)
// against the binary's own command tree, on a throwaway home (#2595):
//
//   - the command path exists (`gitmoot <path> --help` lists it);
//   - every flag the line uses is one that path's help documents;
//   - running the path with those flags and `--help` prints exactly what the
//     bare `--help` prints, so the real flag set accepts them (an unknown or
//     removed flag makes the flag package refuse the line instead).
//
// references/CLI.md is skipped: it IS the binary's --help output, kept current
// by the generate step in CI.
func TestSkillCommandLinesParse(t *testing.T) {
	run := isolatedRunner(t)
	tree, err := clidoc.Discover(run)
	if err != nil {
		t.Fatal(err)
	}
	lines := skillCommandLines(t)
	if len(lines) < 40 {
		t.Fatalf("found only %d command lines in the skill; the extractor is broken", len(lines))
	}
	for _, line := range lines {
		if problem := checkCommandLine(tree, run, line.text); problem != "" {
			t.Errorf("%s:%d: %s\n\t%s", line.file, line.line, problem, line.text)
		}
	}
}

func isolatedRunner(t *testing.T) clidoc.Runner {
	t.Helper()
	set, unset := clidoc.Isolation(t.TempDir(), os.Environ())
	for key, value := range set {
		t.Setenv(key, value)
	}
	for _, key := range unset {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	return func(args []string) (string, int) {
		var buf bytes.Buffer
		code := cli.Run(args, &buf, &buf)
		return buf.String(), code
	}
}

type commandLine struct {
	file string
	line int
	text string
}

var (
	fencePattern      = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*\n(.*?)^[ \t]*```[ \t]*$")
	inlineCodePattern = regexp.MustCompile("`([^`]+)`")
)

// skillCommandLines extracts every command line from the hand-written skill
// files: lines of fenced code blocks that start with `gitmoot `, and inline
// code spans (which may wrap across lines) that start with `gitmoot `.
func skillCommandLines(t *testing.T) []commandLine {
	t.Helper()
	var lines []commandLine
	err := fs.WalkDir(skills.FS, "gitmoot", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".md") || name == "gitmoot/references/CLI.md" {
			return err
		}
		data, err := fs.ReadFile(skills.FS, name)
		if err != nil {
			return err
		}
		text := string(data)
		lineOf := func(offset int) int { return strings.Count(text[:offset], "\n") + 1 }
		prose := []byte(text)
		for _, fence := range fencePattern.FindAllStringSubmatchIndex(text, -1) {
			body := text[fence[2]:fence[3]]
			for i, raw := range strings.Split(body, "\n") {
				command := strings.TrimPrefix(strings.TrimSpace(raw), "$ ")
				if strings.HasPrefix(command, "gitmoot ") {
					lines = append(lines, commandLine{file: name, line: lineOf(fence[2]) + i, text: command})
				}
			}
			// Blank the fence out of the prose so its backticks cannot pair
			// with an inline span; newlines keep line numbers.
			for i := fence[0]; i < fence[1]; i++ {
				if prose[i] != '\n' {
					prose[i] = ' '
				}
			}
		}
		for _, span := range inlineCodePattern.FindAllSubmatchIndex(prose, -1) {
			command := strings.Join(strings.Fields(string(prose[span[2]:span[3]])), " ")
			if command == "gitmoot" || strings.HasPrefix(command, "gitmoot ") {
				lines = append(lines, commandLine{file: name, line: lineOf(span[2]), text: command})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

var wordPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// checkCommandLine returns "" when line parses, else what is wrong with it.
func checkCommandLine(tree clidoc.Tree, run clidoc.Runner, line string) string {
	tokens, err := shellWords(line)
	if err != nil {
		return err.Error()
	}
	args := tokens[1:]
	words := 0
	for words < len(args) && wordPattern.MatchString(args[words]) {
		words++
	}
	command, consumed := tree.Resolve(args[:words])
	if consumed < words && tree.HasChildren(command) {
		return fmt.Sprintf("unknown command %q", "gitmoot "+strings.Join(args[:consumed+1], " "))
	}
	if consumed == 0 && words == 0 && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return fmt.Sprintf("unknown command %q", args[0])
	}
	known := tree.Flags(command)
	probe := append([]string{}, command.Path...)
	rest := args[consumed:]
	lastFlag := -1
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		if token == "--" {
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			// Positionals stay in place: some commands read their id first.
			probe = append(probe, token)
			continue
		}
		name, inline, hasInline := strings.Cut(strings.TrimLeft(token, "-"), "=")
		flag, ok := known[name]
		if !ok {
			return fmt.Sprintf("flag %s is not documented by `gitmoot %s --help`", token, command.Name())
		}
		if name == "h" || name == "help" {
			continue
		}
		takesValue := flag.Type != "" || (!flag.Defaults && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-"))
		switch {
		case name == "home":
			// Never let a documented --home reach a real directory.
			if !hasInline && takesValue {
				i++
			}
			probe = append(probe, "--home", os.Getenv("HOME"))
		case hasInline:
			probe = append(probe, "--"+name+"="+probeValue(flag.Type, inline))
		case takesValue && i+1 < len(rest):
			i++
			probe = append(probe, "--"+name, probeValue(flag.Type, rest[i]))
		case takesValue:
			return fmt.Sprintf("flag %s needs a value", token)
		default:
			probe = append(probe, "--"+name)
		}
		lastFlag = len(probe)
	}
	if lastFlag < 0 {
		// No flags: the resolved path is the whole claim, and it exists.
		return ""
	}
	// `--help` goes right after the last flag, before trailing positionals: a
	// flag set stops at its first positional, so a later `--help` would never
	// be parsed. Compare output only: a command that reads its id first exits 2
	// on `<id> --help` while printing the same usage as a bare `--help`.
	probe = append(append(append([]string{}, probe[:lastFlag]...), "--help"), probe[lastFlag:]...)
	wantHelp, _ := run(append(append([]string{}, command.Path...), "--help"))
	gotHelp, gotCode := run(probe)
	if gotHelp != wantHelp {
		return fmt.Sprintf("`gitmoot %s` does not print the command's help (exit %d), so its flag set refuses the line:\n%s", strings.Join(probe, " "), gotCode, firstLine(gotHelp))
	}
	return ""
}

// probeValue stands in for a placeholder value (`<n>`, `<sha>`) with one the
// flag's type parses, so the probe tests the flag name, not the example.
func probeValue(flagType, value string) string {
	switch flagType {
	case "int", "int64", "uint", "uint64", "float64":
		return "1"
	case "duration":
		return "1h"
	default:
		return value
	}
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// shellWords splits a command line the way sh would for the quoting the skill
// uses: single quotes, double quotes with backslash escapes, and whitespace.
func shellWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			word.WriteString(line[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				word.WriteByte(line[i])
			}
			if i >= len(line) {
				return nil, fmt.Errorf("unterminated double quote")
			}
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}
