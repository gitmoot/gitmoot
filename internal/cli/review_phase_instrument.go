package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// phaseProfileEventKind is the job-event kind carrying a review's command-phase
// decomposition (#1824). Event kinds are free-form strings, so this needs no
// store schema change and no edit to internal/db.
// The kind is defined in internal/db because the operator-facing SQL that must
// EXCLUDE it lives there (#1824 review F6).
const phaseProfileEventKind = db.PhaseProfileEventKind

// Phase buckets. These classify a command's OWN measured time. They do not
// partition the run on their own - see phaseProfile for the identity that does.
const (
	phaseBucketTest  = "test"
	phaseBucketBuild = "build"
	phaseBucketVCS   = "vcs"
	phaseBucketMixed = "mixed"
	phaseBucketOther = "other"
	// phaseBucketUnknown is CONSERVATIVE REFUSAL, not a residual category.
	// `other` means "a command this lexer understood, matching no phase";
	// `unknown` means "this lexer does not implement the shell context this
	// command uses, so any bucket would be a guess" (ruling 123815, option B).
	// `other` is a measurement; `unknown` is an admission. Its time is still
	// counted in covered_ms, so refusing to classify never demotes the signal.
	phaseBucketUnknown = "unknown"
)

// Coverage tells a reader whether an EMPTY decomposition is a measurement or a
// blind spot. Claude's stream carries one final envelope and no tool events at
// all (internal/transcript/translate.go: claudeTranslator), so a claude run
// yields no commands however long it takes. Without this tag a claude profile
// would read as "residual dominates", which is a claim about the instrument
// rather than about the run - and claude is 26% of review runs at roughly twice
// codex's median, so that misreading would land exactly where it hurts.
const (
	phaseCoverageDecomposed = "decomposed"
	phaseCoverageOpaque     = "opaque_runtime"
)

// runtimeEmitsToolEvents reports whether a runtime's translator emits per-tool
// events at all. Anything else is opaque and is reported as such.
func runtimeEmitsToolEvents(runtimeName string) bool {
	switch strings.ToLower(strings.TrimSpace(runtimeName)) {
	case "codex", "kimi":
		return true
	default:
		return false
	}
}

// shellToolNames are the tool names whose payload is a shell command. Only
// these contribute to the command decomposition; a file_change or a web fetch
// is a tool event but not a command, and counting it as one inflates the
// command count with rows the buckets cannot describe (#1824 review F2).
func isShellToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "bash", "sh", "shell", "zsh", "powershell", "command", "command_execution", "exec", "exec_command", "terminal":
		return true
	default:
		return false
	}
}

// extractToolCommand pulls the shell command out of a tool's input payload.
// Codex sends the command as a bare string; kimi sends function arguments as
// JSON, e.g. {"command":"go test ./..."}, and handing that JSON to a
// leading-token classifier lands every kimi command in `other` (#1824 review
// F2). Unknown shapes return the raw text, which classifies as other honestly.
func extractToolCommand(input string) string {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return trimmed
	}
	for _, key := range []string{"command", "cmd", "script", "shell_command"} {
		switch value := decoded[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		case []any:
			parts := make([]string, 0, len(value))
			for _, item := range value {
				if text, ok := item.(string); ok {
					parts = append(parts, text)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return trimmed
}

// classifyPhaseCommand maps a shell command to a bucket by classifying EVERY
// segment of it. A single leading token cannot describe `go test ./... && go
// build ./...` (which bills build time to test) or `cd repo && go test ./...`
// and `time go test ./...` (which land in other) - all three measured in #1824
// review F3. Segments that disagree yield `mixed`, which says "this command
// spans phases" rather than picking one and being wrong.
func classifyPhaseCommand(command string) string {
	return classifyPhaseCommandDepth(command, 0)
}

// maxWrapperRecursion bounds `sh -c "sh -c ..."` nesting. Three levels is more
// than any real agent command and terminates on hostile input.
const maxWrapperRecursion = 3

// INTERPRETER OPTION STATE, MODELLED PER OPTION (#1930 rounds 13-16, uid f32).
//
// uid #1930-f32 survived two rounds of patching rows because the grammar stored
// option ATTRIBUTES as shared character sets. One `suppressed` boolean cannot
// express what a shell actually does, so `+n` cleared the suppression that `-D`
// had set and a dumped-and-never-executed command was reported as a test. The
// four axes the reviewer named are now DATA on each option: execution effect,
// accepted argument form, value domain, and ordering. Plan note 126177.
//
// Everything here was executed against the installed GNU bash 5.2.21 and
// /usr/bin/dash before it was written down. Anything not executed returns
// unknown - see interpreterGrammars for the zsh entry.
type optionEffect int

const (
	// effectNone: the option does not decide whether the command string runs.
	effectNone optionEffect = iota
	// effectNoexec: CLEARABLE. `-n` sets it and `+n` clears it, and `-o noexec`
	// is the same state spelled differently. Measured: `bash -o noexec -c "go
	// version"` runs no Go, `+o noexec` runs Go, `-o noexec +n -c` RUNS Go.
	effectNoexec
	// effectTerminal: STICKY, and this is the axis that was missing. The shell
	// prints or dumps and never runs the command string, and a later `+n` does
	// NOT restore it. Measured on all six: `-D +n`, `+D +n`, `--dump-strings
	// +n`, `--dump-po-strings +n`, `--help +n`, `--version +n` - each exit 0
	// with zero Go invocations (#1930 round-16).
	effectTerminal
	// effectErrexit: CLEARABLE, and its consequence depends on the COMMAND, not
	// on the option. Measured: `bash -e -c "false; go version"` runs no Go,
	// `bash -e -c "true; go version"` and `bash -e -c "go version"` both run,
	// and `bash -e -c "go version; false"` RUNS Go because the failure comes
	// after it. A single-segment command string is therefore unaffected; a
	// multi-segment one cannot be decided statically and refuses.
	effectErrexit
	// effectRestricted: FAIL CLOSED. Restricted mode blocks a command
	// containing a slash - `bash -r -c "/usr/bin/go version"` exits 1 and runs
	// no Go while `bash -r -c "go version"` runs - and also blocks
	// redirections, cd and PATH assignment, none of which this lexer models.
	// Rather than model a fraction of it, any restricted flag refuses. `+r` is
	// not even valid bash (`bash: +r: invalid option`, exit 2).
	effectRestricted
)

// argumentForm is how an option accepts its value. Inline `=` is rejected by
// every long option measured here: `--rcfile=/dev/null` and
// `--init-file=/dev/null` exit 2, while `--rcfile /dev/null` runs Go, and
// `--noprofile=x` exits 2 for a boolean.
type argumentForm int

const (
	argNone argumentForm = iota
	argSeparate
)

type shortOption struct {
	effect     optionEffect
	takesValue bool
	// valueDomain is the set of values the real shell accepts. An unknown name
	// ABORTS the shell - `-o nonesuch`, `+o nonesuch`, `-O nonesuch` and
	// `+O nonesuch` all exit 2 - so a value outside the domain refuses.
	valueDomain map[string]bool
	// valueEffect maps a value to the state it controls, so `-o noexec` reaches
	// the same clearable bit as `-n`.
	valueEffect map[string]optionEffect
}

type longOption struct {
	effect optionEffect
	form   argumentForm
}

type interpreterGrammar struct {
	shortOptions map[rune]shortOption
	longOptions  map[string]longOption
	// commandStringPlus records whether `+c` also introduces a command string.
	// Measured true for bash and dash; false for zsh, which is unmeasured.
	commandStringPlus bool
	// declared marks an interpreter whose grammar was measured at all.
	declared bool
}

// bashSetOptions and bashShoptOptions are the `-o` and `-O` value domains,
// harvested from the installed bash rather than typed from a manual.
var bashSetOptions = map[string]bool{
	"allexport": true, "braceexpand": true, "emacs": true, "errexit": true,
	"errtrace": true, "functrace": true, "hashall": true, "histexpand": true,
	"history": true, "ignoreeof": true, "interactive-comments": true,
	"keyword": true, "monitor": true, "noclobber": true, "noexec": true,
	"noglob": true, "nolog": true, "notify": true, "nounset": true,
	"onecmd": true, "physical": true, "pipefail": true, "posix": true,
	"privileged": true, "verbose": true, "vi": true, "xtrace": true,
}

var bashShoptOptions = map[string]bool{
	"assoc_expand_once": true, "autocd": true, "cdable_vars": true,
	"cdspell": true, "checkhash": true, "checkjobs": true,
	"checkwinsize": true, "cmdhist": true, "compat31": true,
	"compat32": true, "compat40": true, "compat41": true, "compat42": true,
	"compat43": true, "compat44": true, "complete_fullquote": true,
	"direxpand": true, "dirspell": true, "dotglob": true, "execfail": true,
	"expand_aliases": true, "extdebug": true, "extglob": true,
	"extquote": true, "failglob": true, "force_fignore": true,
	"globasciiranges": true, "globskipdots": true, "globstar": true,
	"gnu_errfmt": true, "histappend": true, "histreedit": true,
	"histverify": true, "hostcomplete": true, "huponexit": true,
	"inherit_errexit": true, "interactive_comments": true, "lastpipe": true,
	"lithist": true, "localvar_inherit": true, "localvar_unset": true,
	"login_shell": true, "mailwarn": true, "no_empty_cmd_completion": true,
	"nocaseglob": true, "nocasematch": true, "noexpand_translation": true,
	"nullglob": true, "patsub_replacement": true, "progcomp": true,
	"progcomp_alias": true, "promptvars": true, "restricted_shell": true,
	"shift_verbose": true, "sourcepath": true, "varredir_close": true,
	"xpg_echo": true,
}

// dashSetOptions is dash's OWN `set -o` domain, which is not bash's: `sh -o
// pipefail` exits 2 because pipefail is not in it, while `sh -o errexit` and
// `sh -o nounset` run Go. dash has no shopt at all, so it declares no `-O`.
var dashSetOptions = map[string]bool{
	"allexport": true, "debug": true, "emacs": true, "errexit": true,
	"ignoreeof": true, "interactive": true, "monitor": true,
	"noclobber": true, "noexec": true, "noglob": true, "nolog": true,
	"notify": true, "nounset": true, "privileged": true, "stdin": true,
	"verbose": true, "vi": true, "xtrace": true,
}

func plainShortOptions(letters string) map[rune]shortOption {
	options := make(map[rune]shortOption, len(letters))
	for _, letter := range letters {
		options[letter] = shortOption{effect: effectNone}
	}
	return options
}

func withShortOption(options map[rune]shortOption, letter rune, option shortOption) map[rune]shortOption {
	options[letter] = option
	return options
}

var interpreterGrammars = map[string]interpreterGrammar{
	"bash": {
		declared:          true,
		commandStringPlus: true,
		// Every letter measured on BOTH signs: all 22 run Go.
		shortOptions: withShortOption(withShortOption(withShortOption(withShortOption(withShortOption(withShortOption(
			plainShortOptions("abfhiklmpstuvxBCEHPT"),
			'n', shortOption{effect: effectNoexec}),
			'e', shortOption{effect: effectErrexit}),
			'r', shortOption{effect: effectRestricted}),
			'D', shortOption{effect: effectTerminal}),
			'o', shortOption{takesValue: true, valueDomain: bashSetOptions,
				valueEffect: map[string]optionEffect{
					"noexec": effectNoexec, "errexit": effectErrexit,
					"restricted": effectRestricted, "privileged": effectNone,
				}}),
			'O', shortOption{takesValue: true, valueDomain: bashShoptOptions}),
		longOptions: map[string]longOption{
			"--login": {}, "--noprofile": {}, "--norc": {}, "--posix": {},
			"--restricted": {effect: effectRestricted},
			"--verbose":    {}, "--noediting": {},
			"--debugger": {}, "--pretty-print": {},
			"--rcfile":    {form: argSeparate},
			"--init-file": {form: argSeparate},
			// Terminal: measured to print and run nothing, even followed by +n.
			"--help": {effect: effectTerminal}, "--version": {effect: effectTerminal},
			"--dump-strings": {effect: effectTerminal}, "--dump-po-strings": {effect: effectTerminal},
		},
	},
	// POSIX sh here is /usr/bin/dash and is NOT bash: it rejects `-h`, has NO
	// long options at all (`--help` and `--version` both exit 2), and carries
	// its own `-o` domain.
	"sh": {
		declared:          true,
		commandStringPlus: true,
		shortOptions: withShortOption(withShortOption(withShortOption(
			plainShortOptions("abCfilmuvx"),
			'n', shortOption{effect: effectNoexec}),
			'e', shortOption{effect: effectErrexit}),
			'o', shortOption{takesValue: true, valueDomain: dashSetOptions,
				valueEffect: map[string]optionEffect{
					"noexec": effectNoexec, "errexit": effectErrexit,
				}}),
		longOptions: map[string]longOption{},
	},
	// zsh IS INSTALLED NOWHERE IN THIS LOOP - not on this box and not in the
	// review seat - so no zsh behaviour has been executed by anybody. Only the
	// two universals are declared, and even `+c` refuses although `+c` is
	// measured to work in bash and dash. An unmeasured grammar is not a
	// grammar.
	"zsh": {declared: true},
}

type interpreterOptionOutcome int

const (
	// interpreterRunsNoCommandString: the shell runs, but no command string
	// does - a script operand, or an option that suppressed execution.
	interpreterRunsNoCommandString interpreterOptionOutcome = iota
	interpreterRunsCommandString
	// interpreterRunsCommandStringUnderErrexit: the command string runs, but a
	// failing segment aborts the rest, so only a SINGLE-segment string can be
	// classified confidently.
	interpreterRunsCommandStringUnderErrexit
	// interpreterUndeclared: outside the measured grammar. The shell aborts on
	// these, and we refuse rather than guess.
	interpreterUndeclared
)

// scanInterpreterOptions walks a shell interpreter's own options and reports
// what it will do, plus the index of its command string.
func scanInterpreterOptions(interpreter string, tokens []shellToken) (interpreterOptionOutcome, int) {
	grammar := interpreterGrammars[interpreter]
	if !grammar.declared {
		return interpreterUndeclared, 0
	}
	noexec, terminal, sawShortCluster := false, false, false
	errexit, restricted := false, false
	for index := 0; index < len(tokens); index++ {
		text := tokens[index].text()
		switch {
		case text == "--":
			// OPTION TERMINATION, and it is exempt from the ordering rule below:
			// `bash -x -- -c` still runs a script named "-c" (exit 127).
			return interpreterRunsNoCommandString, 0
		case strings.HasPrefix(text, "--"):
			if sawShortCluster {
				// ORDERING: a named long option after a short cluster aborts.
				// `bash -x --noprofile -c`, `-i --norc -c`, `-s --noprofile -c`
				// and `-O extglob --noprofile -c` all exit 2 (#1930 round-16).
				return interpreterUndeclared, 0
			}
			// ARGUMENT FORM: every long option measured rejects the inline `=`
			// form, value-taking ones included (`--rcfile=/dev/null` and
			// `--init-file=/dev/null` exit 2, `--noprofile=x` exits 2). NO
			// EXPLICIT `=` GUARD IS NEEDED and one is not kept: the lookup
			// below keys on the WHOLE token, so `--rcfile=/dev/null` is simply
			// not a declared option and refuses. Measured, not assumed - with
			// the guard present, the mutant that disabled it SURVIVED, so it
			// was code no test could kill. TestPhaseInstrumentHonoursArgumentForms
			// pins the behaviour instead.
			option, declared := grammar.longOptions[text]
			if !declared {
				return interpreterUndeclared, 0
			}
			switch option.effect {
			case effectTerminal:
				terminal = true
			case effectRestricted:
				restricted = true
			case effectErrexit:
				errexit = true
			}
			if option.form == argSeparate {
				index++
			}
		case len(text) > 1 && (text[0] == '-' || text[0] == '+'):
			sawShortCluster = true
			enabling := text[0] == '-'
			letters := []rune(text[1:])
			commandString := false
			for position, letter := range letters {
				if letter == 'c' {
					if !enabling && !grammar.commandStringPlus {
						return interpreterUndeclared, 0
					}
					commandString = true
					continue
				}
				option, declared := grammar.shortOptions[letter]
				if !declared {
					return interpreterUndeclared, 0
				}
				if option.takesValue {
					if position != len(letters)-1 || commandString {
						// bash's -O takes an OPTIONAL argument, so a value
						// letter that does not end its cluster is ambiguous.
						return interpreterUndeclared, 0
					}
					if index+1 >= len(tokens) {
						return interpreterUndeclared, 0
					}
					value := tokens[index+1].text()
					if !option.valueDomain[value] {
						// VALUE DOMAIN: an unknown name aborts the shell.
						return interpreterUndeclared, 0
					}
					if effect, controls := option.valueEffect[value]; controls {
						switch effect {
						case effectNoexec:
							noexec = enabling
						case effectTerminal:
							terminal = true
						case effectErrexit:
							errexit = enabling
						case effectRestricted:
							restricted = true
						}
					}
					index++
					continue
				}
				switch option.effect {
				case effectNoexec:
					noexec = enabling
				case effectTerminal:
					// STICKY on either sign, and never cleared by a later +n.
					terminal = true
				case effectErrexit:
					errexit = enabling
				case effectRestricted:
					// `+r` is not valid bash at all, so either sign refuses.
					restricted = true
				}
			}
			if commandString {
				if restricted {
					// FAIL CLOSED: whether the command runs depends on text
					// this lexer does not model.
					return interpreterUndeclared, 0
				}
				if noexec || terminal {
					return interpreterRunsNoCommandString, 0
				}
				if errexit {
					return interpreterRunsCommandStringUnderErrexit, index + 1
				}
				return interpreterRunsCommandString, index + 1
			}
		default:
			// A non-option token: the script file.
			return interpreterRunsNoCommandString, 0
		}
	}
	// No operand at all - an interactive shell, which runs no Go test.
	return interpreterRunsNoCommandString, 0
}

func classifyPhaseCommandDepth(command string, depth int) string {
	text := extractToolCommand(command)
	if _, unsupported := unsupportedShellContext(text); unsupported {
		// CONSERVATIVE REFUSAL (ruling 123815, option B): the lexer's declared
		// grammar lives in review_phase_grammar.go, and anything outside it
		// must not receive a confident bucket.
		return phaseBucketUnknown
	}
	segments := splitCommandSegments(text)
	seen := ""
	for _, segment := range segments {
		bucket := classifyCommandSegment(segment, depth)
		// A pure `cd`/`export` prefix is plumbing, not a phase: it must not
		// turn `cd repo && go test` into mixed.
		if bucket == "" {
			continue
		}
		if bucket == phaseBucketUnknown {
			// One unclassifiable segment makes the whole command
			// unclassifiable: reporting the rest would attribute a run whose
			// shape this lexer does not model.
			return phaseBucketUnknown
		}
		switch {
		case seen == "":
			seen = bucket
		case seen != bucket:
			return phaseBucketMixed
		}
	}
	if seen == "" {
		return phaseBucketOther
	}
	return seen
}

// splitCommandSegments splits on shell sequencing operators OUTSIDE quotes.
// Quote awareness is load-bearing rather than tidy: `bash -c "cd repo && go
// test ./..."` is ONE command whose body happens to contain `&&`, and splitting
// inside the quotes tore the body apart before it could be re-parsed, yielding
// `mixed` for a plain test run (#1930 review F12's fix exposed this).
func splitCommandSegments(command string) []string {
	var segments []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	runes := []rune(command)
	flush := func() {
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			segments = append(segments, trimmed)
		}
		current.Reset()
	}
	// A COMMENT STARTS AFTER WHITESPACE, not at the start of a segment. Asking
	// whether the segment was empty made `go test ./...  # A&B` keep the '#' as
	// an ordinary character, so the '&' inside the comment still split the
	// command and it classified as mixed.
	afterWhitespace := func() bool {
		written := current.String()
		if written == "" {
			return true
		}
		last := []rune(written)[len([]rune(written))-1]
		return last == ' ' || last == '\t'
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			// Escaped data, not syntax: `PROBE=A\&B go test` is one command.
			escaped = true
			current.WriteRune(r)
			continue
		}
		if quote != 0 {
			current.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			current.WriteRune(r)
		case '#':
			if afterWhitespace() {
				// A comment ends at the NEWLINE, not at the command. Returning
				// here discarded every later line, so `go test # note` followed
				// by `git status` reported one phase where two ran (#1930
				// round-8 F2).
				flush()
				for i+1 < len(runes) && runes[i+1] != '\n' {
					i++
				}
				continue
			}
			if false {
				// A COMMENT IS NOT A COMMAND. Everything after an unquoted '#'
				// at a token boundary is text, and an '&' inside it is not an
				// operator (#1930 review f20).
				flush()
				return segments
			}
			current.WriteRune(r)
		case '<':
			if i+1 < len(runes) && runes[i+1] == '<' {
				// HEREDOC DATA IS NOT COMMANDS. Everything from `<<` onward is
				// the document body plus its delimiter; an '&' in it is data.
				// The command up to this point is the segment.
				flush()
				return segments
			}
			current.WriteRune(r)
		case '\n', ';', '|':
			flush()
		case '&':
			previous := rune(0)
			if written := current.String(); written != "" {
				previous = []rune(written)[len([]rune(written))-1]
			}
			switch {
			case i+1 < len(runes) && runes[i+1] == '&':
				i++
				flush()
			case previous == '>' || previous == '<':
				current.WriteRune(r)
			case i+1 < len(runes) && runes[i+1] == '>':
				current.WriteRune(r)
			default:
				flush()
			}
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return segments
}

func shellFields(command string) []string {
	var fields []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	runes := []rune(command)
	quoted := false
	flush := func() {
		// A QUOTED word survives even when empty, so the classifier can refuse
		// it; dropping it made `"" go test` read as a Go test run.
		if current.Len() > 0 || quoted {
			fields = append(fields, current.String())
			current.Reset()
		}
		quoted = false
	}
	for _, r := range runes {
		if escaped {
			// A BACKSLASH-ESCAPED RUNE IS DATA, never syntax. The scanner used
			// to close a quote on any matching rune, so Codex's own
			// `/bin/bash -lc "bash -c \"go test\" ..."` wrapper ended its
			// quoted region early and everything after it was scanned as bare
			// text (#1930 review f20).
			current.WriteRune(r)
			escaped = false
			continue
		}
		switch {
		case r == '\\' && quote != '\'':
			// Single quotes are literal in POSIX shells: a backslash inside
			// them is data, not an escape.
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			quoted = true
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return fields
}

// consumeWrapperArguments drops a wrapper's own flags and, for wrappers that
// take a positional operand of their own, that operand too. It stops at the
// first token that is neither, which is the wrapped command.
// WRAPPER GRAMMARS, DECLARED PER WRAPPER (#1930 round-17 F33). The previous
// consumer treated EVERY dash-prefixed token as a wrapper option and stripped
// it, so an option the real wrapper rejects vanished and whatever followed got
// a confident bucket. Measured against GNU coreutils 9.4 at /usr/bin/timeout:
//
//	/usr/bin/timeout --definitely-invalid 1s go version  exit 125, no Go
//	/usr/bin/timeout -s -999 1s go version               exit 125, no Go
//	/usr/bin/timeout 1s -- go version                    exit 127, no Go
//	/usr/bin/timeout 1s go version                       exit 0,   Go RAN
//	timeout 1s -x go version                             exit 127, no Go
//	timeout 1x / abc go version                          exit 125, no Go
//
// NOTE ON `timeout <dur> --`: the `timeout` FIRST ON PATH here is not GNU - it
// is a clap-based clone that RUNS that form (exit 0) where GNU exits 127. Two
// installed implementations disagree, so no confident bucket exists and it
// refuses. That is a different reason from "unmeasured": it is measured twice
// with different answers.
type wrapperOption struct {
	takesValue bool
	// valueDomain reports whether a value is one the real wrapper accepts.
	// nil means any value is accepted.
	valueDomain func(string) bool
	// terminal marks an option that prints and never runs the command.
	terminal bool
}

type wrapperGrammar struct {
	options map[string]wrapperOption
	// requiresDuration marks a wrapper whose first operand is a duration and
	// whose NEXT token is then the command, dash-prefixed or not.
	requiresDuration bool
	declared         bool
}

func isSignalOperand(value string) bool {
	name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value)), "SIG")
	switch name {
	case "HUP", "INT", "QUIT", "ILL", "TRAP", "ABRT", "BUS", "FPE", "KILL",
		"USR1", "SEGV", "USR2", "PIPE", "ALRM", "TERM", "STKFLT", "CHLD",
		"CONT", "STOP", "TSTP", "TTIN", "TTOU", "URG", "XCPU", "XFSZ",
		"VTALRM", "PROF", "WINCH", "IO", "PWR", "SYS":
		return true
	}
	if name == "" || !isAllDigitsOrDot(name) || strings.Contains(name, ".") {
		return false
	}
	// A numeric signal must be a real one: `-s -999` exits 125.
	number := 0
	for _, digit := range name {
		number = number*10 + int(digit-'0')
		if number > 64 {
			return false
		}
	}
	return number >= 1 && number <= 64
}

var wrapperGrammars = map[string]wrapperGrammar{
	"timeout": {
		declared:         true,
		requiresDuration: true,
		options: map[string]wrapperOption{
			"-s":                {takesValue: true, valueDomain: isSignalOperand},
			"--signal":          {takesValue: true, valueDomain: isSignalOperand},
			"-k":                {takesValue: true, valueDomain: isDurationOperand},
			"--kill-after":      {takesValue: true, valueDomain: isDurationOperand},
			"--preserve-status": {}, "--foreground": {},
			"-v": {}, "--verbose": {},
			"--help": {terminal: true}, "--version": {terminal: true},
		},
	},
	"sudo": {declared: true, options: map[string]wrapperOption{
		"-u": {takesValue: true}, "--user": {takesValue: true},
		"-g": {takesValue: true}, "--group": {takesValue: true},
		"-U": {takesValue: true}, "-E": {}, "--preserve-env": {}, "-n": {},
		"--non-interactive": {}, "-H": {}, "-i": {}, "-s": {},
	}},
	"nice": {declared: true, options: map[string]wrapperOption{
		"-n":           {takesValue: true, valueDomain: isAllDigitsOrDot},
		"--adjustment": {takesValue: true, valueDomain: isAllDigitsOrDot},
	}},
	"ionice": {declared: true, options: map[string]wrapperOption{
		"-c": {takesValue: true, valueDomain: isAllDigitsOrDot},
		"-n": {takesValue: true, valueDomain: isAllDigitsOrDot},
		"-t": {},
	}},
	"xargs": {declared: true, options: map[string]wrapperOption{
		"-n": {takesValue: true}, "-P": {takesValue: true},
		"-I": {takesValue: true}, "-d": {takesValue: true},
		"-0": {}, "--null": {}, "-r": {}, "--no-run-if-empty": {},
	}},
	"stdbuf": {declared: true, options: map[string]wrapperOption{
		"-i": {takesValue: true}, "-o": {takesValue: true}, "-e": {takesValue: true},
	}},
	"time": {declared: true, options: map[string]wrapperOption{
		"-f": {takesValue: true}, "--format": {takesValue: true},
		"-o": {takesValue: true}, "--output": {takesValue: true},
		"-p": {}, "-v": {}, "--verbose": {},
	}},
}

type wrapperOutcome int

const (
	wrapperRunsCommand wrapperOutcome = iota
	// wrapperRunsNothing: a terminal option, or no command at all.
	wrapperRunsNothing
	// wrapperUndeclared: an option, value or operand the real wrapper rejects,
	// or a form on which installed implementations disagree.
	wrapperUndeclared
)

// scanWrapperArguments consumes a wrapper's OWN options and operands and
// returns the index at which its command begins.
func scanWrapperArguments(wrapper string, tokens []shellToken) (wrapperOutcome, int) {
	grammar, declared := wrapperGrammars[wrapper]
	if !declared {
		return wrapperUndeclared, 0
	}
	index := 0
	for index < len(tokens) {
		text := tokens[index].text()
		if !strings.HasPrefix(text, "-") || text == "-" {
			break
		}
		name, inlineValue, hasInline := strings.Cut(text, "=")
		option, known := grammar.options[name]
		if !known {
			// The real wrapper exits rather than running anything.
			return wrapperUndeclared, 0
		}
		if option.terminal {
			return wrapperRunsNothing, 0
		}
		if hasInline {
			if !option.takesValue {
				return wrapperUndeclared, 0
			}
			if option.valueDomain != nil && !option.valueDomain(inlineValue) {
				return wrapperUndeclared, 0
			}
			index++
			continue
		}
		index++
		if option.takesValue {
			if index >= len(tokens) {
				return wrapperUndeclared, 0
			}
			value := tokens[index].text()
			if option.valueDomain != nil && !option.valueDomain(value) {
				return wrapperUndeclared, 0
			}
			index++
		}
	}
	if grammar.requiresDuration {
		if index >= len(tokens) {
			return wrapperRunsNothing, 0
		}
		if !isDurationOperand(tokens[index].text()) {
			// `timeout 1x` and `timeout abc` exit 125.
			return wrapperUndeclared, 0
		}
		index++
		if index < len(tokens) && tokens[index].text() == "--" {
			// GNU makes `--` the command (exit 127); the clone on PATH runs the
			// following command instead. Implementations disagree, so refuse.
			return wrapperUndeclared, 0
		}
	}
	if index >= len(tokens) {
		return wrapperRunsNothing, 0
	}
	// WHATEVER FOLLOWS IS THE COMMAND, dash-prefixed or not: `timeout 1s -x go
	// version` exits 127 because `-x` is the command, so it must not be
	// stripped as though it were a wrapper option.
	return wrapperRunsCommand, index
}

func isDurationOperand(token string) bool {
	if token == "" {
		return false
	}
	body := strings.TrimRight(token, "smhd")
	if body == "" {
		return false
	}
	return isAllDigitsOrDot(body)
}

func isAllDigitsOrDot(token string) bool {
	if token == "" {
		return false
	}
	dots := 0
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
			if dots > 1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// classifyCommandSegment classifies one segment, or returns "" for pure
// plumbing (cd, export, an env-only line) that should not colour the result.
func classifyCommandSegment(segment string, depth int) string {
	normalize := func(token string) string {
		token = strings.Trim(token, "'\"")
		if idx := strings.LastIndex(token, "/"); idx >= 0 && idx+1 < len(token) {
			token = token[idx+1:]
		}
		return token
	}
	// THE ACCEPTOR DECIDES FIRST, and it hands back TOKENS rather than strings.
	// Flattening to strings here is what lost per-character provenance, and it
	// also hid `env`'s own assignments: after the wrapper was stripped,
	// `PROBE=1` became the command word (#1930 round-10 P2 four).
	accepted, ok := acceptSimpleCommand(strings.TrimSpace(segment))
	if !ok {
		return phaseBucketUnknown
	}
	if len(accepted) == 0 {
		// Assignments or redirections only: no command ran.
		return ""
	}
	tokens := accepted
	plumbing := false
	for len(tokens) > 0 {
		head := normalize(tokens[0].text())
		// NO CONTEXT-FREE `-c` BRANCH. Matching a command-string option on
		// shape alone made `nohup -c "go test"` report a test, though nohup has
		// no -c and execs a command literally named "-c" (measured: exit 127,
		// no Go). A -c is only a command string when it belongs to a shell
		// INTERPRETER, so it is honoured inside that arm and nowhere else
		// (#1930 round-13 F1).
		switch head {
		case "env":
			// ONLY env TAKES ASSIGNMENTS. `env PROBE=1 go test` runs go test
			// with PROBE set, so prefix consumption re-runs after this wrapper.
			rest, prefixesOK := consumePrefixes(tokens[1:])
			if !prefixesOK {
				return phaseBucketUnknown
			}
			tokens = rest
			continue
		case "bash", "sh", "zsh":
			// A SHELL INTERPRETER IS NOT AN EXEC WRAPPER. Without a
			// command-string option its operand is a SCRIPT FILE, so `bash go
			// test ./...` runs a script NAMED go and never invokes the Go
			// toolchain (measured: exit 0, the script ran with argv
			// `test ./...`). Option STATE decides which case this is, rather
			// than the shape of any single token (#1930 round-12 F1,
			// round-13 F1).
			outcome, commandStringAt := scanInterpreterOptions(head, tokens[1:])
			underErrexit := outcome == interpreterRunsCommandStringUnderErrexit
			switch outcome {
			case interpreterUndeclared:
				// The shell exits without running anything, and this lexer does
				// not implement the option. Refuse rather than bucket.
				return phaseBucketUnknown
			case interpreterRunsNoCommandString:
				// Every token is understood and what runs is not a Go test.
				return phaseBucketOther
			}
			rest := tokens[1+commandStringAt:]
			if len(rest) == 0 || depth >= maxWrapperRecursion {
				return phaseBucketUnknown
			}
			// ONLY THE FIRST TOKEN IS THE COMMAND STRING. Everything after it
			// is $0 and the positional parameters, so joining them invented a
			// command nobody ran: `bash -c "go version" test ./...` runs Go
			// WITHOUT the test subcommand (measured), and joining turned that
			// into a test bucket (#1930 round-14 F1).
			// NO EXPLICIT EMPTY-STRING GUARD. `bash -c "" go test` runs nothing
			// (measured: exit 0, no Go) and must not be a test - but recursing
			// on "" already yields `other`, so a guard here would be code no
			// mutant can kill. Removing it was checked, not assumed: with the
			// guard present the mutant that disables it SURVIVED the suite.
			// The behaviour is pinned by TestPhaseInstrumentHonoursCommandStringArity
			// instead, which fails if that recursion ever stops returning other.
			commandString := rest[0].text()
			if underErrexit && len(splitCommandSegments(commandString)) > 1 {
				// Measured: `bash -e -c "false; go version"` runs no Go while
				// `bash -e -c "go version; false"` does. Which segments run
				// depends on runtime exit status, so a chain refuses.
				return phaseBucketUnknown
			}
			return classifyPhaseCommandDepth(commandString, depth+1)
		case "nohup":
			// nohup EXECS ITS OPERAND, so it really is an exec wrapper:
			// `nohup go version` invokes Go (measured). It owns no assignments
			// though - `nohup PROBE=1 go test` execs a command named PROBE=1
			// and exits 127 (#1930 round-11 f29).
			tokens = tokens[1:]
			continue
		case "timeout", "time", "nice", "sudo", "xargs", "stdbuf", "ionice":
			// A WRAPPER OWNS ITS OWN ARGUMENTS. Skipping only the wrapper token
			// left `timeout 25m go test ./...` classified as other - and that is
			// this repository's own documented gate shape, so the bucket under
			// investigation was the one being undercounted (#1824 review F8).
			// Consume the wrapper, then its flags, then the one non-flag operand
			// those flags take (timeout's duration, sudo -u's user is already a
			// flag value, nice -n's level likewise).
			outcome, commandAt := scanWrapperArguments(head, tokens[1:])
			switch outcome {
			case wrapperUndeclared:
				return phaseBucketUnknown
			case wrapperRunsNothing:
				return phaseBucketOther
			}
			tokens = tokens[1+commandAt:]
			continue
		case "cd", "export", "pushd", "popd", "mkdir", "rm", "cp", "mv", "echo", "set":
			// `source` is deliberately NOT here: it runs another file, which
			// can change PATH and therefore which binary a later word means,
			// so it belongs to the refusing set (#1930 round-8 f22).
			return ""
		case "grep", "rg", "awk", "sed", "head", "tail", "cut", "tr", "sort", "uniq", "wc", "tee", "jq", "cat":
			// A pipeline consumer is a FILTER on another command's output, not
			// a phase of its own: `go test ./... | grep FAIL` is a test run,
			// and calling it mixed would label most real invocations
			// unclassifiable.
			return ""
		}
		break
	}
	if len(tokens) == 0 {
		if plumbing {
			return ""
		}
		return phaseBucketOther
	}
	if reservedShellWord(normalize(tokens[0].text())) {
		return phaseBucketUnknown
	}
	head := normalize(tokens[0].text())
	sub := ""
	if len(tokens) > 1 {
		sub = normalize(tokens[1].text())
	}
	switch head {
	case "git", "gh":
		return phaseBucketVCS
	case "go":
		switch sub {
		case "test":
			return phaseBucketTest
		case "build", "vet", "generate", "install":
			return phaseBucketBuild
		}
		return phaseBucketOther
	case "gofmt":
		return phaseBucketBuild
	case "gotestsum":
		return phaseBucketTest
	}
	return phaseBucketOther
}

// phaseProfile is the emitted payload. Durations are milliseconds so the row is
// readable without unit guessing.
//
// TWO IDENTITIES, and the difference is the point (#1824 review F1). Command
// intervals can OVERLAP, so their durations do not partition anything:
//
//	CoveredMS + ResidualMS == WallMS          (exact, always)
//	sum(BucketMS)          == CoveredMS + OverlapMS
//
// Covered is the UNION of command intervals, so residual - time with no command
// in flight - is non-negative by construction with nothing clamped. An earlier
// version subtracted the SUM and clamped a negative result to zero, which
// silently absorbed 24ms of a measured 76ms run and made the documented
// partition false exactly when overlap occurred.
type phaseProfile struct {
	Coverage   string `json:"coverage"`
	Runtime    string `json:"runtime"`
	Attempt    int64  `json:"attempt"`
	WallMS     int64  `json:"wall_ms"`
	CoveredMS  int64  `json:"covered_ms"`
	ResidualMS int64  `json:"residual_ms"`
	OverlapMS  int64  `json:"overlap_ms"`
	Commands   int    `json:"commands"`
	// Unpaired counts tool results with no matching call id. They contribute
	// no time; reporting them keeps a stream this instrument cannot follow
	// visible instead of silently short-measuring the run.
	Unpaired int `json:"unpaired"`
	// ToolEvents counts non-shell tool results (file_change and friends).
	ToolEvents int `json:"tool_events"`
	// InFlight counts commands still running when the transcript closed. Their
	// measured-so-far time IS counted, because leaving it out silently moved a
	// killed review's command time into residual and reported the run as idle.
	InFlight int `json:"in_flight"`
	// IDCollisions counts tool calls arriving on an id that was already open.
	// The FIRST call is kept: overwriting it made the first result adopt the
	// second command's text, which is the same misattribution the id pairing
	// was introduced to remove.
	IDCollisions int `json:"id_collisions"`
	// DroppedBytes counts bytes discarded from an over-long unterminated line.
	// Without a cap the pending buffer grows without bound inside the daemon.
	DroppedBytes int64            `json:"dropped_bytes"`
	BucketMS     map[string]int64 `json:"bucket_ms"`
	BucketCount  map[string]int   `json:"bucket_count"`
}

type pendingCommand struct {
	command string
	tool    string
	started time.Time
}

type commandInterval struct {
	start time.Time
	end   time.Time
}

// retainedTranscript is the handle every production caller writes through. It
// is a CONCRETE pointer type on purpose: callers compare it against nil, and
// returning an interface would hand them a non-nil interface holding a nil
// pointer on the capture-disabled path.
type retainedTranscript struct {
	// sink is an io.Writer rather than *os.File so a test can inject a short
	// or failing write. Without that the observe-before-write mutant is
	// unkillable, which is how it survived while being reported as killed
	// (#1824 review F4).
	sink   io.Writer
	closer io.Closer

	store   *db.Store
	jobID   string
	runtime string
	attempt int64

	translator transcript.Translator
	pending    map[string]pendingCommand
	// nonShellCalls remembers ids opened by NON-shell tools, so a result whose
	// id was never opened at all can be told apart from a file_change - the
	// distinction between a lost pairing and ordinary tool activity.
	nonShellCalls map[string]struct{}
	intervals     []commandInterval
	partial       []byte
	// skipToNewline is set after an over-long line is dropped, so the REST of
	// that line is discarded rather than parsed as a line of its own.
	skipToNewline bool
	started       time.Time

	bucketNS     map[string]int64
	bucketCount  map[string]int
	commands     int
	unpaired     int
	toolEvents   int
	inFlight     int
	idCollisions int
	droppedBytes int64
}

// maxPartialLineBytes caps the buffer held for a line that has not yet ended.
// Streams are newline-delimited JSON, so a line longer than this is malformed
// or hostile; retaining it unboundedly is a memory defect in the daemon, and a
// 1 MiB line carries no command the profile can describe anyway.
const maxPartialLineBytes = 1 << 20

// newRetainedTranscript attaches the phase instrument to an open transcript
// file. A job whose runtime is unknown, or whose runtime emits no tool events,
// still gets a profile row - tagged opaque, because a silent absence is
// indistinguishable from a fast run.
func newRetainedTranscript(file *os.File, jobID, jobType, runtimeName string, attempt int64, store *db.Store) *retainedTranscript {
	handle := &retainedTranscript{
		sink:          file,
		closer:        file,
		store:         store,
		jobID:         jobID,
		attempt:       attempt,
		started:       time.Now(),
		pending:       map[string]pendingCommand{},
		nonShellCalls: map[string]struct{}{},
		bucketNS:      map[string]int64{},
		bucketCount:   map[string]int{},
	}
	if file == nil {
		handle.sink = nil
		handle.closer = nil
	}
	// #1824 asks where a REVIEW's wall time goes, and the profile is appended
	// after a job's terminal events. Emitting it for every job type would change
	// the observable event sequence of jobs this work has no business touching -
	// three exec-backend E2Es assert their sequence exactly, as their acceptance
	// contract. Scope the instrument to the population it was built to measure.
	if !strings.EqualFold(strings.TrimSpace(jobType), "review") {
		return handle
	}
	handle.runtime = strings.TrimSpace(runtimeName)
	if store == nil {
		return handle
	}
	if runtimeEmitsToolEvents(handle.runtime) {
		if translator, err := transcript.NewTranslator(handle.runtime); err == nil {
			handle.translator = translator
		}
	}
	if handle.translator == nil {
		// Opaque runtimes still emit a row, so Close must have something to
		// drive the emission. A no-op translator keeps Write on the cheap path
		// while leaving Close's single emit intact.
		handle.translator = opaqueTranslator{}
	}
	return handle
}

// opaqueTranslator yields no events. It exists so a runtime with no tool stream
// still produces a profile tagged opaque_runtime rather than no row at all.
type opaqueTranslator struct{}

func (opaqueTranslator) Translate(string) []transcript.Event { return nil }
func (opaqueTranslator) Flush() []transcript.Event           { return nil }

// Write persists the bytes FIRST and observes only what was actually persisted.
// Instrumentation must never be able to lose, reorder or double-count a
// transcript byte, so the sink write is the only operation whose error is
// returned, and a short write observes p[:n] rather than all of p.
func (r *retainedTranscript) Write(p []byte) (int, error) {
	if r == nil || r.sink == nil {
		return len(p), nil
	}
	n, err := r.sink.Write(p)
	if r.translator != nil && n > 0 {
		r.observe(p[:n])
	}
	return n, err
}

func (r *retainedTranscript) observe(p []byte) {
	if r.skipToNewline {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			r.droppedBytes += int64(len(p))
			return
		}
		r.droppedBytes += int64(idx + 1)
		p = p[idx+1:]
		r.skipToNewline = false
	}
	r.partial = append(r.partial, p...)
	if len(r.partial) > maxPartialLineBytes {
		// Drop the oversized fragment rather than grow forever, and RECORD the
		// loss: a silently dropped fragment would under-count commands while
		// the profile still looked complete.
		r.droppedBytes += int64(len(r.partial))
		r.partial = nil
		// SKIP TO THE NEXT NEWLINE. The rest of that run-on line is not a line
		// of its own, and resuming normal accumulation hands the translator a
		// fragment starting mid-token (#1930 review F13). Inert today because
		// every translator routes a parse failure to KindRaw, which consume
		// ignores - aligned anyway with ScanSnapshot's skip-to-next-newline so
		// it cannot become live the first time a translator grows a lenient
		// path.
		r.skipToNewline = true
		return
	}
	// Scan the BYTES. An earlier form called string(r.partial) once per
	// iteration, copying the whole pending buffer per line - measured at 25x
	// the bare write path. An instrument built to measure cost must not be the
	// cost.
	for {
		idx := bytes.IndexByte(r.partial, '\n')
		if idx < 0 {
			return
		}
		line := string(r.partial[:idx])
		r.partial = r.partial[idx+1:]
		r.consume(r.translator.Translate(line))
	}
}

func (r *retainedTranscript) consume(events []transcript.Event) {
	for _, event := range events {
		switch event.Kind {
		case transcript.KindToolCall:
			if !isShellToolName(event.Name) {
				// Remember the id anyway. At result time an unknown id must
				// mean "a pairing was lost", and without this a file_change
				// result would be indistinguishable from one (#1824 review F9
				// wanted the opposite error fixed; both need the id, not a
				// guess from the result's name).
				if id := strings.TrimSpace(event.ToolID); id != "" {
					r.nonShellCalls[id] = struct{}{}
				}
				continue
			}
			// Pair by TOOL ID. Arrival order is wrong the moment two tools
			// overlap: a probe where vcs finished first swapped the two
			// durations outright (#1824 review F1).
			if _, open := r.pending[event.ToolID]; open {
				// Keep the FIRST call. Overwriting made the next result adopt
				// this command's text and bill one command's time to the other
				// bucket entirely.
				r.idCollisions++
				continue
			}
			r.pending[event.ToolID] = pendingCommand{
				command: extractToolCommand(event.InputDigest),
				tool:    event.Name,
				started: time.Now(),
			}
		case transcript.KindToolResult:
			// THE ID DECIDES FIRST. Kimi names a result whose call it never saw
			// "tool" (internal/transcript/translate.go), so a shell-ness test
			// ahead of the id lookup filed a genuinely unpaired result as a
			// non-shell tool event and left unpaired at zero - contradicting
			// the documented meaning of both fields (#1824 review F9).
			call, ok := r.pending[event.ToolID]
			if !ok {
				if id := strings.TrimSpace(event.ToolID); id != "" {
					if _, seen := r.nonShellCalls[id]; seen {
						delete(r.nonShellCalls, id)
						r.toolEvents++
						continue
					}
				} else if !isShellToolName(event.Name) {
					r.toolEvents++
					continue
				}
				// The id was never opened by any call, so a pairing is
				// genuinely lost. Kimi names such a result "tool", which is why
				// this decision must not consult the name (#1824 review F9).
				r.unpaired++
				continue
			}
			delete(r.pending, event.ToolID)
			end := time.Now()
			// Time the interval HERE rather than reading event.Duration: the
			// duration cannot be attributed to a command without the pairing
			// this branch just did, and owning both ends keeps the interval
			// available for the union that residual needs.
			bucket := classifyPhaseCommand(call.command)
			r.bucketNS[bucket] += end.Sub(call.started).Nanoseconds()
			r.bucketCount[bucket]++
			r.intervals = append(r.intervals, commandInterval{start: call.started, end: end})
			r.commands++
		}
	}
}

// Close flushes the translator, emits one bounded profile row, and closes the
// file. One row per ATTEMPT rather than per job: RetryJob preserves prior
// job_events and re-delivers the same job id, so a retried review would
// otherwise double-count against a one-row-per-job contract (#1824 review F5).
func (r *retainedTranscript) Close() error {
	if r == nil {
		return nil
	}
	if r.translator != nil {
		if len(r.partial) > 0 {
			// The final line of a stream that ends without a newline is a real
			// command result; dropping it silently short-measures the run.
			r.consume(r.translator.Translate(string(r.partial)))
			r.partial = nil
		}
		r.consume(r.translator.Flush())
		r.closeInFlight(time.Now())
		r.emit()
	}
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// unionMS returns the total wall time during which at least one command was in
// flight, merging overlapping intervals.
func unionNS(intervals []commandInterval) int64 {
	if len(intervals) == 0 {
		return 0
	}
	sorted := make([]commandInterval, len(intervals))
	copy(sorted, intervals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	var total time.Duration
	current := sorted[0]
	for _, interval := range sorted[1:] {
		if interval.start.After(current.end) {
			total += current.end.Sub(current.start)
			current = interval
			continue
		}
		if interval.end.After(current.end) {
			current.end = interval.end
		}
	}
	total += current.end.Sub(current.start)
	return total.Nanoseconds()
}

// closeInFlight attributes commands that were still running when the transcript
// closed. Their time is real - a review killed or timing out mid-`go test` spent
// it - and leaving the interval out moved it into residual, reporting the run as
// idle exactly when it was busiest.
func (r *retainedTranscript) closeInFlight(now time.Time) {
	for id, call := range r.pending {
		delete(r.pending, id)
		bucket := classifyPhaseCommand(call.command)
		r.bucketNS[bucket] += now.Sub(call.started).Nanoseconds()
		r.bucketCount[bucket]++
		r.intervals = append(r.intervals, commandInterval{start: call.started, end: now})
		r.commands++
		r.inFlight++
	}
}

func (r *retainedTranscript) emit() {
	if r.store == nil || strings.TrimSpace(r.jobID) == "" {
		return
	}
	// ARITHMETIC IN NANOSECONDS, rounded once at the edge. Truncating each
	// interval to milliseconds first loses sub-millisecond remainders, and the
	// bucket sum then disagrees with the union by a millisecond per command -
	// small, but it makes a documented identity false, which is the same defect
	// class as the clamp this rewrite removed.
	wallNS := time.Since(r.started).Nanoseconds()
	var summedNS int64
	for _, ns := range r.bucketNS {
		summedNS += ns
	}
	coveredNS := unionNS(r.intervals)
	if coveredNS > wallNS {
		// Cannot happen while both ends come from the same monotonic clock, but
		// a covered span longer than the run would make residual negative, and
		// a negative residual is exactly what this rewrite removes.
		coveredNS = wallNS
	}
	bucketMS, wall, covered, overlap := phaseProfileMilliseconds(r.bucketNS, wallNS, coveredNS, summedNS)
	coverage := phaseCoverageDecomposed
	if !runtimeEmitsToolEvents(r.runtime) {
		coverage = phaseCoverageOpaque
	}
	profile := phaseProfile{
		Coverage:     coverage,
		Runtime:      strings.TrimSpace(r.runtime),
		Attempt:      r.attempt,
		WallMS:       wall,
		CoveredMS:    covered,
		ResidualMS:   wall - covered,
		OverlapMS:    overlap,
		Commands:     r.commands,
		Unpaired:     r.unpaired,
		ToolEvents:   r.toolEvents,
		InFlight:     r.inFlight,
		IDCollisions: r.idCollisions,
		DroppedBytes: r.droppedBytes,
		BucketMS:     bucketMS,
		BucketCount:  r.bucketCount,
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return
	}
	// Best-effort: a profile is diagnostic, so a store error must never fail the
	// job whose transcript this is.
	_ = r.store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   r.jobID,
		Kind:    phaseProfileEventKind,
		Message: string(encoded),
	})
}

// phaseProfileMilliseconds converts the nanosecond accounting into the emitted
// millisecond fields. It is a PURE FUNCTION so the arithmetic can be tested on
// exact inputs: a +1ms per-bucket bias survived the end-to-end tests, whose
// tolerance was necessarily loose because real durations are not deterministic
// (#1824 review F10).
func phaseProfileMilliseconds(bucketNS map[string]int64, wallNS, coveredNS, summedNS int64) (map[string]int64, int64, int64, int64) {
	roundMS := func(ns int64) int64 {
		if ns <= 0 {
			return 0
		}
		return (ns + int64(time.Millisecond)/2) / int64(time.Millisecond)
	}
	bucketMS := make(map[string]int64, len(bucketNS))
	for bucket, ns := range bucketNS {
		bucketMS[bucket] = roundMS(ns)
	}
	wall := roundMS(wallNS)
	covered := roundMS(coveredNS)
	if covered > wall {
		covered = wall
	}
	overlap := roundMS(summedNS - coveredNS)
	return bucketMS, wall, covered, overlap
}

// sortedBuckets is used by tests and by any reader that wants deterministic
// ordering out of the map.
func sortedBuckets(buckets map[string]int64) []string {
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectiveTranscriptRuntime mirrors resolveTranscriptRuntime's precedence -
// payload override first, then the agent's registered runtime - without the
// store lookup that function needs when it starts from only a job id. Callers
// on the dispatch path already hold both, so the answer is free there.
func effectiveTranscriptRuntime(payload workflow.JobPayload, agent runtime.Agent) string {
	if override := strings.TrimSpace(payload.RuntimeOverride); override != "" {
		return override
	}
	return strings.TrimSpace(agent.Runtime)
}

var _ io.Writer = (*retainedTranscript)(nil)
