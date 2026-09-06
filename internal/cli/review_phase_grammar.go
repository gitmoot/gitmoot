package cli

import "strings"

// SUPPORTED GRAMMAR - the explicit boundary ruling 123815 required.
//
// The phase classifier is a cheap lexer, deliberately NOT a shell parser. It
// implements: whitespace tokenisation; single quotes (literal, including
// backslashes) and double quotes with backslash escapes; backslash escapes
// outside quotes; the sequencing operators newline, ';', '|', '||', '&&' and a
// lone '&' including the redirect forms N>&M, N<&M and &>file; simple
// redirections '>', '>>' and '<' to a filename; comments introduced by an
// unquoted '#' after whitespace; and wrapper prefixes (env assignments,
// bash/sh/env/nohup, timeout, time, nice, sudo, xargs, stdbuf, ionice) plus
// -c-style command strings.
//
// EVERYTHING ELSE IS OUT OF SCOPE AND YIELDS phaseBucketUnknown.
//
// Round 7's finding was not that one more token was mishandled. It was that a
// partial lexer which implements selected tokens WITHOUT rejecting what it
// cannot parse emits confidently wrong buckets, and that token-by-token patches
// cannot converge without becoming a shell parser. f19 (a lone '&') and f20
// (escapes, comments, heredoc data) were two symptoms of one class, so the
// boundary is declared here and enforced rather than extended a token per round.
//
// The refusal is deliberately CHEAP AND OVER-BROAD: a construct that merely
// MIGHT change which word is the command yields unknown, because for an
// instrument whose purpose is attribution a wrong bucket is worse than an
// absent one. Unknown time is still counted in covered_ms, so refusing to
// classify narrows the claim without demoting the signal.
// reservedShellWord reports whether a COMMAND WORD is shell syntax or a
// builtin whose effect this lexer cannot model, rather than a command whose
// phase can be read from its name.
//
// This is the ACCEPTOR half of the boundary, and it is why the first version
// was unsound: unsupportedShellContext is a character-level blacklist, so a
// construct spelled entirely in ordinary words - a brace group, if/for/while,
// negation, a function definition, [[...]], exec, command - slid past it and
// received a confident bucket. "Everything else yields unknown" cannot be
// enforced by enumerating punctuation (#1930 round-8 F1).
//
// IT IS CHECKED AT THE COMMAND-WORD POSITION ONLY. Several of these words are
// ordinary arguments elsewhere - `go test` has `test` as its subcommand, and
// `git add .` ends in a dot - so scanning every word for them would refuse the
// commands this instrument exists to measure.
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
	// A function definition reads `name() { ... }`, with the parenthesis
	// attached to the name.
	return strings.HasSuffix(word, "()")
}

// structuralShellToken reports whether a token is shell STRUCTURE wherever it
// appears, so a compound command is refused even when its keyword is not the
// first word: `! go test`, `{ go test; }` and `[[ -f x ]] && go test` are all
// outside the declared simple-command grammar.
func structuralShellToken(word string) bool {
	switch word {
	case "{", "}", "!", "[[", "]]", "(", ")", "then", "else", "elif", "fi", "do", "done", "esac":
		return true
	}
	return false
}

// redirectionWord reports whether a token is a redirection rather than an
// argument, and whether its operand is a SEPARATE following token. The declared
// grammar permits redirections anywhere in a simple command - `>out.log go
// test` and `go >out.log test` are both valid - so they must be removed
// wherever they appear, not only after the command word (#1930 round-8 F2).
func redirectionWord(word string) (redirection bool, takesOperand bool) {
	// A REDIRECTION IS A SINGLE WORD. Codex delivers the wrapped command as one
	// QUOTED field, so `bash -lc ">out.log go test ./..."` arrives as a token
	// that begins with '>' and contains the whole command - treating it as a
	// redirection deleted the command outright (#1930 round-8 F2, found by my
	// own fix rather than by the reviewer).
	if strings.ContainsAny(word, " \t\n") {
		return false, false
	}
	trimmed := strings.TrimLeft(word, "0123456789")
	switch {
	case strings.HasPrefix(trimmed, ">>"), strings.HasPrefix(trimmed, ">"),
		strings.HasPrefix(trimmed, "<"), strings.HasPrefix(word, "&>"):
	default:
		return false, false
	}
	body := strings.TrimLeft(trimmed, "<>&")
	if strings.HasPrefix(word, "&>") {
		body = strings.TrimLeft(strings.TrimPrefix(word, "&>"), ">")
	}
	// `>out.log` carries its own operand; a bare `>` takes the next token.
	return true, strings.TrimSpace(body) == ""
}

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
			// Single quotes are literal: nothing inside them is a construct,
			// which is why `go test -run 'A$B'` stays classifiable.
			if r == singleQuote {
				quote = 0
			}
			continue
		}
		if quote == doubleQuote {
			// Expansions DO occur inside double quotes, so they remain
			// unsupported there; only the closing quote ends the region.
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
			// Parameter, arithmetic and command substitution all start here,
			// and any of them can decide which word is the command.
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
				// Heredoc and herestring: the delimiter and body are DATA, and
				// this lexer does not track where the body ends.
				return "heredoc or herestring", true
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
