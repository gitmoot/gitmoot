// Package mention parses @agent mentions out of free text and normalizes a
// single mention token. It is deliberately dependency-free (only the standard
// library) so both the daemon command parser (issue/PR comment routing, #389)
// and the CLI chat layer (#534) can share one implementation WITHOUT creating an
// import cycle between internal/daemon and internal/cli.
package mention

import "strings"

// mentionTrailing is the set of prose punctuation commonly adjacent to an inline
// mention (e.g. "ping @codex-b, please") that is not part of the agent name.
const mentionTrailing = ".,;:!?)]}"

// Clean strips a leading "@" and surrounding whitespace from a single mention
// token, e.g. "@codex-b" -> "codex-b". It is the exact normalization the daemon
// command parser has always applied to an agent field; the chat send path uses
// it too so both layers agree byte-for-byte.
func Clean(token string) string {
	return strings.TrimPrefix(strings.TrimSpace(token), "@")
}

// Parse extracts every distinct @agent mention from a free-text body, in
// first-appearance order. A mention is a whitespace-delimited token that begins
// with "@" and has at least one following character after trailing prose
// punctuation is trimmed; a lone "@" is not a mention. Duplicates are collapsed.
//
// Parse never resolves a name against any registry — resolution (known vs
// unknown agent) is the caller's responsibility. This keeps the package free of
// a db dependency and usable from both the daemon and the CLI.
func Parse(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.Fields(body) {
		if !strings.HasPrefix(field, "@") {
			continue
		}
		// Trim trailing prose punctuation ("@a," -> "@a") but keep interior
		// characters (hyphens/underscores are valid in agent names).
		token := strings.TrimRight(field, mentionTrailing)
		name := Clean(token)
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
