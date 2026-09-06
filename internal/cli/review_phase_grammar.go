package cli

import "strings"

// THE DECLARED GRAMMAR IS AN ACCEPTOR, AND ITS PROVENANCE IS PER CHARACTER.
//
// A segment is classified only if EVERY token positively matches one of the
// shapes below; anything else yields phaseBucketUnknown. Round 10 showed that
// one quoted bit PER TOKEN cannot decide that, because bash quotes CHARACTERS,
// not words: `./internal/"cli"*` still globs on its unquoted `*`, `g"\o"` runs
// a command named `g\o`, and `2>"out file"` is an ordinary redirection whose
// operand merely happens to be quoted. Each token therefore carries a mask
// recording which of its characters were quoted or escaped.
//
// ACCEPTED SEGMENT := prefix* commandWord word*
//
//	prefix      := assignment | redirection
//	assignment  := name=value whose NAME characters are all unquoted and match
//	               [A-Za-z_][A-Za-z0-9_]*
//	redirection := operator characters UNQUOTED, exactly one of
//	                 >  >>  <  N>  N>>  N<  N>&M  N<&M  &>
//	               with a non-empty operand carrying no unquoted expansion
//	commandWord := a plain name (letters, digits, '.', '_', '-', '/', ':', '+')
//	word        := no UNQUOTED { } * ? [ ] ~ < > | &
//
// A REJECTED SYNTAX TOKEN NEVER RE-ENTERS ORDINARY-ARGUMENT ACCEPTANCE. That
// fall-through is what admitted partially quoted redirects as arguments, so
// redirect-shaped tokens that fail the redirection rule refuse outright.

// shellToken carries per-character provenance: quoted[i] reports whether rune i
// came from inside quotes or from a backslash escape.
type shellToken struct {
	runes  []rune
	quoted []bool
}

func (t shellToken) text() string { return string(t.runes) }

func (t shellToken) empty() bool { return len(t.runes) == 0 }

// hasUnquoted reports whether any UNQUOTED character of the token is in set.
func (t shellToken) hasUnquoted(set string) bool {
	for i, r := range t.runes {
		if !t.quoted[i] && strings.ContainsRune(set, r) {
			return true
		}
	}
	return false
}

// unquotedPrefixThrough reports whether every character up to and including
// index end is unquoted.
func (t shellToken) unquotedPrefixThrough(end int) bool {
	if end >= len(t.quoted) {
		return false
	}
	for i := 0; i <= end; i++ {
		if t.quoted[i] {
			return false
		}
	}
	return true
}

// shellTokens splits a segment into tokens with per-character provenance.
//
// Backslash handling follows bash rather than "strip it everywhere", which was
// untruthful in two directions: inside DOUBLE quotes a backslash is literal
// unless it precedes $, `, " or \, and an unquoted backslash-newline is a LINE
// CONTINUATION that disappears entirely.
func shellTokens(command string) []shellToken {
	var tokens []shellToken
	var current shellToken
	started := false
	quote := rune(0)
	runes := []rune(command)
	add := func(r rune, quoted bool) {
		current.runes = append(current.runes, r)
		current.quoted = append(current.quoted, quoted)
		started = true
	}
	flush := func() {
		if started {
			tokens = append(tokens, current)
			current = shellToken{}
			started = false
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
				continue
			}
			add(r, true)
		case quote == '"':
			if r == '\\' && i+1 < len(runes) {
				switch next := runes[i+1]; next {
				case '$', '`', '"', '\\':
					i++
					add(next, true)
				case '\n':
					i++
				default:
					// Bash KEEPS the backslash before anything else.
					add('\\', true)
				}
				continue
			}
			if r == '"' {
				quote = 0
				continue
			}
			add(r, true)
		case r == '\\':
			if i+1 < len(runes) {
				if runes[i+1] == '\n' {
					// Unquoted line continuation: both runes disappear, which
					// is why `go te\<newline>st` runs `go test`.
					i++
					continue
				}
				i++
				add(runes[i], true)
				continue
			}
			add('\\', true)
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			flush()
		default:
			add(r, false)
		}
	}
	flush()
	return tokens
}

// acceptedRedirection reports whether a token is one of the redirect forms this
// lexer implements. The OPERATOR characters must be unquoted; the operand may be
// quoted, because `2>"out file"` is an ordinary redirection.
//
// redirectShaped is returned separately so a REJECTED redirect refuses instead
// of being admitted as an argument.
func acceptedRedirection(token shellToken) (accepted bool, takesOperand bool, redirectShaped bool) {
	if token.empty() {
		return false, false, false
	}
	text := token.text()
	digits := 0
	for digits < len(token.runes) && token.runes[digits] >= '0' && token.runes[digits] <= '9' && !token.quoted[digits] {
		digits++
	}
	rest := text[digits:]
	shaped := token.hasUnquoted("<>") || strings.HasPrefix(rest, "&>")
	if !shaped {
		return false, false, false
	}
	if strings.HasPrefix(rest, "&>") {
		if !token.unquotedPrefixThrough(digits + 1) {
			return false, false, true
		}
		operand := rest[2:]
		if strings.ContainsAny(operand, "<>|&") {
			return false, false, true
		}
		return acceptAttachedOperand(token, digits+2, operand)
	}
	for _, form := range []string{">>", ">&", "<&", ">", "<"} {
		if !strings.HasPrefix(rest, form) {
			continue
		}
		if !token.unquotedPrefixThrough(digits + len(form) - 1) {
			// A quoted operator is not an operator. This guard is DEFENCE IN
			// DEPTH and is currently equivalent: every token reaching it with a
			// quoted operator also carries the operator character in its
			// operand, which the ContainsAny check below refuses anyway. It is
			// kept rather than deleted because it states the rule directly,
			// where the operand check states it only incidentally - if that
			// check is ever narrowed, this one still holds the line.
			return false, false, true
		}
		operand := rest[len(form):]
		if form == ">&" || form == "<&" {
			// N>&M duplicates a descriptor and N>&- closes it; the operand may
			// be quoted, so `2>&"1"` is accepted.
			if operand == "-" || allDigits(operand) {
				return true, false, true
			}
			return false, false, true
		}
		if strings.ContainsAny(operand, "<>|&") {
			// `>|` (clobber) and `<>` (read-write) reach here.
			return false, false, true
		}
		return acceptAttachedOperand(token, digits+len(form), operand)
	}
	return false, false, true
}

// acceptAttachedOperand validates an operand carried by the redirect token
// itself. An empty attached operand means the operand is the NEXT token.
func acceptAttachedOperand(token shellToken, start int, operand string) (accepted bool, takesOperand bool, redirectShaped bool) {
	if operand == "" {
		return true, true, true
	}
	for i := start; i < len(token.runes); i++ {
		if !token.quoted[i] && strings.ContainsRune("{}*?[]~", token.runes[i]) {
			// `>redirprobe*` globs; bash decides that filename, not this lexer.
			return false, false, true
		}
	}
	return true, false, true
}

// acceptedOperandToken validates a SEPARATE operand token, which used to be
// skipped unconditionally: `go > "" test` is an ambiguous redirect bash refuses,
// and `go > {one,two} test` brace-expands.
func acceptedOperandToken(token shellToken) bool {
	if token.empty() {
		return false
	}
	return !token.hasUnquoted("{}*?[]~<>|&")
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// acceptedAssignment reports whether a token is a real environment assignment.
// The NAME's characters must be unquoted and form an identifier: bash runs
// `"PROBE=AB"` and `not-an-assignment=x` as commands, both measured.
func acceptedAssignment(token shellToken) bool {
	name, _, found := strings.Cut(token.text(), "=")
	if !found || name == "" {
		return false
	}
	if !token.unquotedPrefixThrough(len([]rune(name)) - 1) {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// acceptedWord reports whether a token needs no expansion this lexer cannot
// perform. The test is PER CHARACTER, because a quoted fragment does not bless
// the unquoted remainder.
func acceptedWord(token shellToken) bool {
	return !token.hasUnquoted("{}*?[]~<>|&")
}

// acceptedCommandWord reports whether a token can name a command this lexer can
// classify. A quoted command word is LITERAL, so `g"\o"` names `g\o` and is
// refused rather than read as `go`.
func acceptedCommandWord(token shellToken) bool {
	if token.empty() {
		return false
	}
	for _, r := range token.runes {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/' || r == ':' || r == '+':
		default:
			return false
		}
	}
	return true
}

// reservedShellWord reports whether a COMMAND WORD is shell syntax or a builtin
// whose effect this lexer cannot model. Checked at the command-word position
// only: `test` is go's subcommand and `.` is a git argument.
func reservedShellWord(word string) bool {
	switch word {
	case "if", "then", "else", "elif", "fi",
		"for", "while", "until", "do", "done", "select",
		"case", "esac",
		"function", "coproc",
		"exec", "command", "eval", "builtin", "source", ".",
		"return", "break", "continue", "trap",
		"declare", "typeset", "local", "readonly", "unset", "alias", "unalias":
		return true
	}
	return strings.HasSuffix(word, "()")
}

// structuralShellToken reports whether a token is shell STRUCTURE wherever it
// appears.
func structuralShellToken(word string) bool {
	switch word {
	case "{", "}", "!", "[[", "]]", "(", ")", "then", "else", "elif", "fi", "do", "done", "esac":
		return true
	}
	return false
}

// consumePrefixes removes assignments and redirections from the head of tokens.
// The classifier calls it AGAIN after recognising an `env` wrapper, because env
// is followed by its own assignments: `env PROBE=1 go test` otherwise left
// `PROBE=1` as the command word.
func consumePrefixes(tokens []shellToken) (rest []shellToken, ok bool) {
	index := 0
	for index < len(tokens) {
		token := tokens[index]
		if acceptedAssignment(token) {
			index++
			continue
		}
		accepted, takesOperand, shaped := acceptedRedirection(token)
		if !accepted {
			if shaped {
				return nil, false
			}
			break
		}
		index++
		if takesOperand {
			if index >= len(tokens) || !acceptedOperandToken(tokens[index]) {
				return nil, false
			}
			index++
		}
	}
	return tokens[index:], true
}

// acceptSimpleCommand validates a segment and returns its words with prefixes
// removed. ok=false means REFUSE. An empty word slice with ok=true means the
// segment ran no command at all.
func acceptSimpleCommand(segment string) (words []shellToken, ok bool) {
	tokens, ok := consumePrefixes(shellTokens(segment))
	if !ok {
		return nil, false
	}
	if len(tokens) == 0 {
		return nil, true
	}
	if structuralShellToken(tokens[0].text()) || !acceptedCommandWord(tokens[0]) {
		return nil, false
	}
	words = append(words, tokens[0])
	index := 1
	for index < len(tokens) {
		token := tokens[index]
		if structuralShellToken(token.text()) {
			return nil, false
		}
		accepted, takesOperand, _ := acceptedRedirection(token)
		if accepted {
			index++
			if takesOperand {
				if index >= len(tokens) || !acceptedOperandToken(tokens[index]) {
					return nil, false
				}
				index++
			}
			continue
		}
		// NO `if shaped { refuse }` HERE, and that is proved rather than
		// assumed: shaped implies the token carries an unquoted '<' or '>',
		// and acceptedWord refuses exactly those, so the branch was
		// unreachable. A search over 26 redirect-shaped candidates found zero
		// tokens that are shaped, rejected, and word-acceptable. Dead code a
		// mutant cannot kill is deleted rather than excused (#1930 round-10).
		if !acceptedWord(token) {
			return nil, false
		}
		words = append(words, token)
		index++
	}
	return words, true
}

// unsupportedShellContext is the character-level half of the boundary, for
// constructs that are not single tokens: substitutions, subshells, heredocs,
// `|&`, and malformed quoting.
func unsupportedShellContext(command string) (string, bool) {
	const (
		backslash   = '\\'
		singleQuote = '\''
		doubleQuote = '"'
	)
	runes := []rune(command)
	quote := rune(0)
	escaped := false
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			escaped = false
			continue
		}
		if r == backslash && quote != singleQuote {
			escaped = true
			continue
		}
		if quote == singleQuote {
			if r == singleQuote {
				quote = 0
			}
			continue
		}
		if quote == doubleQuote {
			switch r {
			case doubleQuote:
				quote = 0
			case '$', '`':
				return "substitution inside double quotes", true
			}
			continue
		}
		switch r {
		case singleQuote, doubleQuote:
			quote = r
		case '$':
			return "parameter, arithmetic or command substitution", true
		case '`':
			return "backquote command substitution", true
		case '(':
			switch {
			case i > 0 && runes[i-1] == '(':
				return "arithmetic command", true
			case i > 0 && (runes[i-1] == '<' || runes[i-1] == '>'):
				return "process substitution", true
			}
			return "subshell", true
		case '<':
			if i+1 < len(runes) && runes[i+1] == '<' {
				return "heredoc or herestring", true
			}
		case '|':
			if i+1 < len(runes) && runes[i+1] == '&' {
				return "pipe with stderr", true
			}
		case ';':
			if i+1 < len(runes) && runes[i+1] == ';' {
				return "case-item terminator", true
			}
		}
	}
	if quote != 0 {
		return "unterminated quote", true
	}
	if escaped {
		return "trailing escape or line continuation", true
	}
	return "", false
}
