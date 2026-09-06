package cli

import "strings"

// THE DECLARED GRAMMAR IS AN ACCEPTOR. Nothing here enumerates rejected syntax:
// a segment is classified only if EVERY token positively matches one of the
// shapes below, and anything else yields phaseBucketUnknown.
//
// That inversion is the whole correction, and it took three rounds to accept.
// A blacklist of punctuation cannot express "everything else refuses", because
// the shell grammar is open-ended - each round named more valid syntax that
// walked past the list: compound commands, then <>, >|, &>>, |&, brace,
// pathname and tilde expansion, and dynamic-file-descriptor redirection. An
// acceptor over a finite declared grammar is not a shell parser; it recognises
// a simple command and refuses everything else.
//
// ACCEPTED SEGMENT := prefix* commandWord word*
//
//	prefix      := assignment | redirection
//	assignment  := UNQUOTED name=value, name matching [A-Za-z_][A-Za-z0-9_]*
//	redirection := UNQUOTED, exactly one of  >  >>  <  N>  N>>  N<  N>&M
//	               N<&M  &>  - carrying its operand or taking the next word
//	commandWord := a plain name (letters, digits, '.', '_', '-', '/', ':', '+'),
//	               quoted or not, because a quoted word is LITERAL
//	word        := any token needing no expansion this lexer cannot perform; an
//	               UNQUOTED word containing { } * ? [ ] ~ is refused, since
//	               brace, pathname and tilde expansion decide what it becomes

// shellToken carries the provenance the classifier needs. Discarding it caused a
// whole class of false measurements: with quoting erased, a QUOTED
// ">not-a-command" is indistinguishable from a redirection, and an ESCAPED
// \>out from an operator - so one was deleted and the other believed.
type shellToken struct {
	text   string
	quoted bool
	// nameQuoted records whether quoting or escaping occurred BEFORE the first
	// '=' in this token. Bash recognises an assignment by its NAME, so
	// `PROBE=A\&B` and `PROBE="A B"` ARE assignments while `"PROBE=AB"` and
	// `"PROBE"=AB` are commands - all four measured. Tracking one flag for the
	// whole token refused the first two (#1930 round-9).
	nameQuoted bool
}

// shellTokens splits a segment into tokens, KEEPING quote provenance. Single
// quotes are literal (backslashes included); double quotes honour backslash
// escapes; an unquoted backslash escapes the next rune.
func shellTokens(command string) []shellToken {
	var tokens []shellToken
	var current strings.Builder
	quote := rune(0)
	quoted := false
	nameQuoted := false
	escaped := false
	flush := func() {
		if current.Len() > 0 || quoted {
			tokens = append(tokens, shellToken{text: current.String(), quoted: quoted, nameQuoted: nameQuoted})
			current.Reset()
		}
		quoted = false
		nameQuoted = false
	}
	markQuoted := func() {
		quoted = true
		if !strings.Contains(current.String(), "=") {
			nameQuoted = true
		}
	}
	for _, r := range command {
		if escaped {
			// An escaped rune is DATA, so this token is no longer a bare word:
			// \>out is a literal filename, not a redirection. Mark BEFORE
			// writing, so the '=' test sees the name as it stood.
			markQuoted()
			current.WriteRune(r)
			escaped = false
			continue
		}
		switch {
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			markQuoted()
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return tokens
}

// acceptedRedirection reports whether an UNQUOTED token is one of the redirect
// forms this lexer implements. There is deliberately no {fd}> guard: such a
// token matches none of the forms below and is refused here anyway, and the
// unquoted-word arm refuses it again for its braces - a mutation showed the
// guard could not change any outcome, so it is deleted rather than kept as
// unkillable code (#1930 round-9).
//
// acceptedRedirection reports whether an UNQUOTED token is one of the redirect
// forms this lexer implements, and whether its operand is the next word. Any
// other redirect-looking token is REFUSED rather than guessed at, which is what
// keeps <>, >|, &>> and {fd}> out of a confident bucket.
func acceptedRedirection(token shellToken) (accepted bool, takesOperand bool) {
	if token.quoted || token.text == "" {
		return false, false
	}
	text := token.text
	if strings.HasPrefix(text, "&>") {
		operand := text[2:]
		if strings.ContainsAny(operand, "<>|&") {
			// &>> appends; unimplemented.
			return false, false
		}
		return true, operand == ""
	}
	digits := 0
	for digits < len(text) && text[digits] >= '0' && text[digits] <= '9' {
		digits++
	}
	rest := text[digits:]
	for _, form := range []string{">>", ">&", "<&", ">", "<"} {
		if !strings.HasPrefix(rest, form) {
			continue
		}
		operand := rest[len(form):]
		if form == ">&" || form == "<&" {
			// N>&M duplicates a descriptor; N>&- closes it.
			if operand == "-" || allDigits(operand) {
				return true, false
			}
			return false, false
		}
		if strings.ContainsAny(operand, "<>|&") {
			// >| (clobber) and <> (read-write) reach here.
			return false, false
		}
		return true, operand == ""
	}
	return false, false
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

// acceptedAssignment reports whether an UNQUOTED token is a real environment
// assignment. Treating any word containing '=' as one was wrong: bash runs
// not-an-assignment=x as a COMMAND, because the text before '=' is not a valid
// identifier.
func acceptedAssignment(token shellToken) bool {
	if token.nameQuoted {
		// A quoted NAME is not an assignment: bash reports
		// `"PROBE=AB": command not found`. A quoted or escaped VALUE is fine.
		return false
	}
	name, _, found := strings.Cut(token.text, "=")
	if !found || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
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
// perform. A QUOTED word is literal and always acceptable; an unquoted word
// carrying brace, pathname or tilde expansion is refused, because what it
// becomes is decided by the shell and the filesystem rather than by its text.
func acceptedWord(token shellToken) bool {
	if token.quoted {
		return true
	}
	// An UNQUOTED word carrying redirect or list operators is not a word at
	// all: the shell would have split there. `<>io.log` and `&>>out.log` are
	// redirect forms this lexer does not implement, and acceptedRedirection
	// correctly refuses them - so they must not then be admitted through the
	// argument arm, which is how both reached a confident bucket (#1930
	// round-9 class one).
	return !strings.ContainsAny(token.text, "{}*?[]~<>|&")
}

// acceptedCommandWord reports whether a token can name a command this lexer can
// classify. Deliberately narrow: a plain name, quoted or not.
func acceptedCommandWord(token shellToken) bool {
	if token.text == "" {
		return false
	}
	for _, r := range token.text {
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
// only: test is go's subcommand and . is a git argument, so scanning every word
// would refuse the commands this instrument exists to measure.
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
// appears, so a compound command is refused even when its keyword is not the
// first word: ! go test, { go test; }, [[ -f x ]] && go test.
func structuralShellToken(word string) bool {
	switch word {
	case "{", "}", "!", "[[", "]]", "(", ")", "then", "else", "elif", "fi", "do", "done", "esac":
		return true
	}
	return false
}

// acceptSimpleCommand validates a segment against the declared grammar and
// returns its words with prefixes removed. ok=false means REFUSE: the caller
// must yield phaseBucketUnknown rather than classify anything. An empty word
// slice with ok=true means the segment ran no command at all (assignments or
// redirections only), which is plumbing rather than a phase.
//
// ORDER IS LOAD-BEARING: prefixes are consumed BEFORE the command word is
// examined, because checking for an empty command word first read the
// assignment as the command.
func acceptSimpleCommand(segment string) (words []string, ok bool) {
	tokens := shellTokens(segment)
	if len(tokens) == 0 {
		return nil, true
	}
	index := 0
	for index < len(tokens) {
		token := tokens[index]
		if acceptedAssignment(token) {
			index++
			continue
		}
		accepted, takesOperand := acceptedRedirection(token)
		if !accepted {
			break
		}
		index++
		if takesOperand {
			if index >= len(tokens) {
				return nil, false
			}
			index++
		}
	}
	if index >= len(tokens) {
		return nil, true
	}
	if structuralShellToken(tokens[index].text) || !acceptedCommandWord(tokens[index]) {
		return nil, false
	}
	words = append(words, tokens[index].text)
	index++
	for index < len(tokens) {
		token := tokens[index]
		if structuralShellToken(token.text) {
			return nil, false
		}
		if accepted, takesOperand := acceptedRedirection(token); accepted {
			index++
			if takesOperand {
				if index >= len(tokens) {
					return nil, false
				}
				index++
			}
			continue
		}
		if !acceptedWord(token) {
			return nil, false
		}
		words = append(words, token.text)
		index++
	}
	return words, true
}

// unsupportedShellContext is the character-level half of the boundary, kept for
// constructs that are not single tokens at all: substitutions, subshells,
// heredocs, |&, and malformed quoting. The acceptor above decides everything
// token-shaped.
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
				// |& pipes stderr too; the segment splitter would treat the '&'
				// as a separator and confidently classify both halves.
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
