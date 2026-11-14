// Package clidoc walks the gitmoot command tree through its own `--help`
// output. The skill's generated CLI reference (skills/gitmoot/references/CLI.md)
// and the skill command-line test both use it, so the reference and the test
// see exactly the command paths and flags the binary prints.
//
// The CLI has no declarative command table below the root: each family parses
// its own subcommand. Discovery therefore reads the help text itself. The root
// help lists families under "Commands:"; a family's help lists its subcommands
// as "gitmoot <family> <sub> ..." synopsis lines, and every plain word (or
// "|"-separated alternative) that follows the parent path on such a line is a
// command path. A path whose `--help` prints its parent's help again (a
// subcommand without help of its own, or a literal argument such as the `codex`
// in `gitmoot plugin build codex|claude`) is still a known path; it just gets no
// section of its own in the rendered reference.
package clidoc

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Runner runs the CLI with args and returns its combined stdout+stderr and
// exit code.
type Runner func(args []string) (output string, code int)

// Command is one known command path, the output and exit status of
// `gitmoot <path> --help`, and whether that output is the path's own help
// (non-empty and different from its parent's).
type Command struct {
	Path []string
	Help string
	Code int
	Own  bool
}

// Name is the command path as typed after `gitmoot`.
func (c Command) Name() string { return strings.Join(c.Path, " ") }

// Isolation returns the environment changes that keep a help run away from
// the operator's gitmoot home and session identity: HOME points at home, the
// Herdr socket at a path inside it that nothing listens on, PATH at an empty
// directory inside it so no external program (gh, git, omp, herdr) can run,
// and every other GITMOOT_* and HERDR_* variable in environ is cleared.
func Isolation(home string, environ []string) (set map[string]string, unset []string) {
	set = map[string]string{
		"HOME":              home,
		"HERDR_SOCKET_PATH": home + "/herdr.sock",
		"PATH":              home + "/empty-path",
	}
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := set[key]; ok {
			continue
		}
		if strings.HasPrefix(key, "GITMOOT_") || strings.HasPrefix(key, "HERDR_") {
			unset = append(unset, key)
		}
	}
	sort.Strings(unset)
	return set, unset
}

var wordPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Tree is every known command path keyed by Name.
type Tree map[string]Command

// Discover returns every known command path, the root included under "".
func Discover(run Runner) (Tree, error) {
	rootHelp, code := run([]string{"--help"})
	if code != 0 {
		return nil, fmt.Errorf("gitmoot --help exited %d", code)
	}
	names := rootCommandNames(rootHelp)
	if len(names) == 0 {
		return nil, fmt.Errorf("gitmoot --help lists no commands:\n%s", rootHelp)
	}
	tree := Tree{"": {Help: rootHelp, Own: true}}
	queue := []Command{}
	visit := func(path []string) {
		key := strings.Join(path, " ")
		if _, ok := tree[key]; ok {
			return
		}
		help, code := run(append(append([]string{}, path...), "--help"))
		parent := tree[strings.Join(path[:len(path)-1], " ")]
		command := Command{
			Path: append([]string{}, path...),
			Help: help,
			Code: code,
			Own:  strings.TrimSpace(help) != "" && help != parent.Help,
		}
		tree[key] = command
		queue = append(queue, command)
	}
	for _, name := range names {
		visit([]string{name})
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, path := range childPaths(parent) {
			visit(path)
		}
	}
	return tree, nil
}

// Sorted returns the tree's commands ordered by path.
func (t Tree) Sorted() []Command {
	commands := make([]Command, 0, len(t))
	for _, command := range t {
		commands = append(commands, command)
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name() < commands[j].Name() })
	return commands
}

// Resolve returns the longest known command path that prefixes words, and
// how many words it consumed. The root ("", 0) always matches.
func (t Tree) Resolve(words []string) (Command, int) {
	for n := len(words); n > 0; n-- {
		if command, ok := t[strings.Join(words[:n], " ")]; ok {
			return command, n
		}
	}
	return t[""], 0
}

// HasChildren reports whether any known path extends command's path.
func (t Tree) HasChildren(command Command) bool {
	for _, other := range t {
		if len(other.Path) == len(command.Path)+1 && equalWords(other.Path[:len(command.Path)], command.Path) {
			return true
		}
	}
	return false
}

// Flag is one flag the help documents for a command path. Type is the value
// type flag.PrintDefaults printed for it ("string", "int", "duration", ...,
// or a backquoted usage name), "" for a boolean; Defaults is false when only a
// synopsis line names the flag, which happens for commands that print their own
// usage text instead of the flag set's.
type Flag struct {
	Type     string
	Defaults bool
}

// Flags returns every flag (keyed by name, without dashes) the help text
// documents for command: the flag-set lines of its own help (`  -name type`,
// written by flag.PrintDefaults from the real flag set) plus every -name or
// --name on a synopsis line for this path in its own help or any ancestor's.
func (t Tree) Flags(command Command) map[string]Flag {
	flags := map[string]Flag{"h": {}, "help": {}}
	prefix := append([]string{"gitmoot"}, command.Path...)
	for n := len(command.Path); n >= 0; n-- {
		help := t[strings.Join(command.Path[:n], " ")].Help
		for _, line := range strings.Split(help, "\n") {
			fields := strings.Fields(line)
			if len(fields) < len(prefix) || !equalWords(fields[:len(prefix)], prefix) {
				continue
			}
			for _, match := range synopsisFlagPattern.FindAllStringSubmatch(line, -1) {
				flags[match[1]] = Flag{}
			}
		}
	}
	if command.Own {
		for _, match := range defaultsFlagPattern.FindAllStringSubmatch(command.Help, -1) {
			flags[match[1]] = Flag{Type: strings.TrimSpace(match[2]), Defaults: true}
		}
	}
	return flags
}

var (
	defaultsFlagPattern = regexp.MustCompile(`(?m)^  -([A-Za-z0-9][A-Za-z0-9-]*)(?: (.*))?$`)
	synopsisFlagPattern = regexp.MustCompile(`(?:^|[\s\[(|])--?([A-Za-z0-9][A-Za-z0-9-]*)`)
)

// rootCommandNames reads the "Commands:" table of the root help.
func rootCommandNames(help string) []string {
	var names []string
	inCommands := false
	for _, line := range strings.Split(help, "\n") {
		if strings.TrimSpace(line) == "Commands:" {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			break
		}
		if wordPattern.MatchString(fields[0]) {
			names = append(names, fields[0])
		}
	}
	return names
}

// childPaths returns the command paths named by parent's synopsis lines:
// every run of plain words (alternatives split on "|") that follows
// "gitmoot <parent path>" on a line, shortest first.
func childPaths(parent Command) [][]string {
	prefix := append([]string{"gitmoot"}, parent.Path...)
	var paths [][]string
	for _, line := range strings.Split(parent.Help, "\n") {
		fields := strings.Fields(line)
		if len(fields) <= len(prefix) || !equalWords(fields[:len(prefix)], prefix) {
			continue
		}
		bases := [][]string{parent.Path}
		for _, field := range fields[len(prefix):] {
			var next [][]string
			for _, alt := range strings.Split(field, "|") {
				if !wordPattern.MatchString(alt) {
					continue
				}
				for _, base := range bases {
					next = append(next, append(append([]string{}, base...), alt))
				}
			}
			if len(next) == 0 {
				break
			}
			paths = append(paths, next...)
			bases = next
		}
	}
	return paths
}

func equalWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Render writes the generated CLI reference: one section per command path
// with help of its own, each holding that path's verbatim `--help` output.
func Render(tree Tree) []byte {
	var sections []Command
	for _, command := range tree.Sorted() {
		if command.Own {
			sections = append(sections, command)
		}
	}
	var b bytes.Buffer
	b.WriteString("# Gitmoot CLI Reference\n\n")
	b.WriteString("<!-- Code generated by internal/clidoc/clidocgen from `gitmoot <command> --help`. DO NOT EDIT. -->\n")
	b.WriteString("<!-- Regenerate with `go generate ./skills/`; CI fails when this file is stale. -->\n\n")
	b.WriteString("Each section is the verbatim `--help` output of one command path of the\n")
	b.WriteString("gitmoot binary this skill ships with. A subcommand without a section prints\n")
	b.WriteString("its family's help; read its synopsis line there. If the installed binary\n")
	b.WriteString("disagrees, trust its live `--help`.\n\n")
	b.WriteString("## Commands\n\n")
	for _, command := range sections {
		b.WriteString("- [" + heading(command) + "](#" + anchor(command) + ")\n")
	}
	for _, command := range sections {
		b.WriteString("\n## " + heading(command) + "\n\n")
		if command.Code != 0 {
			fmt.Fprintf(&b, "`--help` exits %d here and prints:\n\n", command.Code)
		}
		b.WriteString("```text\n")
		b.WriteString(strings.TrimRight(command.Help, "\n"))
		b.WriteString("\n```\n")
	}
	return b.Bytes()
}

func heading(command Command) string {
	return strings.TrimSpace("gitmoot " + command.Name())
}

// anchor is the GitHub-style fragment for heading(command).
func anchor(command Command) string {
	return strings.ReplaceAll(heading(command), " ", "-")
}
