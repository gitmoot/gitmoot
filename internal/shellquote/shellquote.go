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
//
// SCOPE: ARGUMENT POSITION ONLY. The guarantee is that sh passes the value
// through as one literal word where a WORD is expected — after a command name,
// after a flag, inside a `[ ... ]` test. It is NOT a guarantee about COMMAND
// position, and it cannot be: a bare `if`, `while` or `!` is a reserved word
// there, and a bare `FOO=bar` is a variable assignment, so the shell reinterprets
// them before any quoting question arises. Both are returned unquoted by Posix
// because both are safe as arguments, which is the only position gitmoot uses.
//
// Callers own that boundary. Every current call site builds `<literal command>
// <quoted arguments>` — `gh release view <repo>`, `codex plugin marketplace add
// <root>`, `<gitmootBin> job watch <id>` — so no Posix result ever lands in
// command position, and gitmootBin is deliberately NOT routed through here. A
// future caller that needs a computed COMMAND WORD must not use this function
// and must not assume it protects that position.
//
// Deliberately NOT solved by quoting reserved words and assignment-form tokens:
// that would mean enumerating every shell's reserved list correctly, forever,
// which is precisely the denylist defect this package replaced. Narrowing a
// documented contract to what the code actually guarantees is the cheaper and
// more honest fix (#1759, review of PR #1795).
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
