// Package shellquote holds the single POSIX shell quoting implementation.
//
// Before #1759 there were four, with four different safe-character policies:
// an allowlist in internal/update, a slightly wider allowlist in
// internal/plugininstall (it also admitted ',' and '%'), an unconditional
// quoter in internal/cockpit, and a DENYLIST in internal/cli's posixQuote that
// passed '~', '#' and every non-ASCII rune through unquoted. Four policies over
// twenty-odd call sites is not duplication that can be deleted by picking one
// at random: each choice is a different answer to "which bytes reach a shell".
//
// This package answers it once, with the STRICTEST of the four: quote unless
// every rune is provably safe. Over-quoting is always correctness-preserving —
// a quoted string means exactly itself to sh — whereas under-quoting is how a
// path with a '#' or a '~' becomes a comment or a home-directory expansion.
package shellquote

import "strings"

// Posix returns value as a single POSIX shell word.
//
// The empty string becomes ” because a bare empty word would vanish from the
// command line rather than be passed as an empty argument.
//
// The allowlist is deliberately narrower than "characters sh happens to treat
// literally today": it is the set that is safe in every position of every
// POSIX-compatible shell, so a caller never has to reason about whether its
// value lands in a command position, an argument, or inside a `[ ... ]` test.
// Anything else is single-quoted, with embedded single quotes closed, escaped
// and reopened via the '"'"' idiom.
func Posix(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if !safe(r) {
			return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
		}
	}
	return value
}

// safe reports whether r needs no quoting anywhere in a POSIX command line.
//
// ',' and '%' are NOT here even though internal/plugininstall's allowlist
// admitted them and they are in fact literal to sh: the strictest of the four
// merged policies wins, and quoting them costs two bytes rather than a class of
// argument nobody re-derives when this list is next edited.
func safe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '/', '.', '-', '_', ':', '@', '+', '=':
		return true
	default:
		return false
	}
}
