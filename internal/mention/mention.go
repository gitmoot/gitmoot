// Package mention normalizes a single @agent mention token. It is deliberately
// dependency-free (only the standard library) so the daemon command parser
// (issue/PR comment routing, #389) and any future consumer can share one
// implementation WITHOUT creating an import cycle.
package mention

import "strings"

// Clean strips a leading "@" and surrounding whitespace from a single mention
// token, e.g. "@codex-b" -> "codex-b". It is the exact normalization the daemon
// command parser has always applied to an agent field, shared so every mention
// consumer agrees byte-for-byte.
func Clean(token string) string {
	return strings.TrimPrefix(strings.TrimSpace(token), "@")
}
