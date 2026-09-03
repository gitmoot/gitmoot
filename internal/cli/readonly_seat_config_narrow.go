package cli

import (
	"fmt"
	"strings"
)

// narrowCodexConfig removes third-party credentials from a codex config.toml
// before it is staged into a read-only seat.
//
// The seat stages config.toml so the codex CLI keeps the operator's model and
// sandbox settings. That file routinely also carries credentials that have
// nothing to do with running the model: [mcp_servers.*.env] holds tokens for
// third-party servers, and [model_providers.*] holds api_key and http_headers.
// The seat's state dir lives inside the one WRITABLE path granted to the
// sandbox, so anything staged there is readable by the reviewer the seat runs.
//
// Two dispositions, chosen for what each costs when it is wrong:
//
//   - mcp_servers is dropped WHOLESALE. Its env table is the token carrier, but
//     args and command can carry a secret just as easily, and a read-only
//     reviewer seat has no business spawning third-party servers. Dropping it
//     cannot break startup: MCP servers are optional to codex.
//   - model_providers keeps its structure and loses only api_key and
//     http_headers. Dropping the section would be safer still and is wrong:
//     when model names a custom provider, removing base_url and wire_api makes
//     the model unresolvable, so a narrowing meant to protect the seat would
//     stop it launching.
//
// Unknown keys are KEPT, because they are the model and sandbox settings this
// file is staged for. An unparseable section header is FAIL-CLOSED: it drops
// through to the end of the file. Once a header cannot be attributed, no
// following key can be classified, and the direction that leaks is the one
// that guesses "probably fine".
func narrowCodexConfig(data []byte) ([]byte, error) {
	var (
		out       []string
		section   []string
		dropping  bool
		unknown   bool
		pending   int
		pendingIn bool
	)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)

		// A value that opened an unbalanced inline table or array continues in
		// the disposition of the key that opened it.
		if pending != 0 {
			pending += bracketBalance(line)
			if !pendingIn {
				out = append(out, raw)
			}
			if pending <= 0 {
				pending = 0
			}
			continue
		}

		if strings.HasPrefix(line, "[") {
			path, ok := parseCodexSectionHeader(line)
			if !ok {
				// Fail closed: an unattributable header means every key after it
				// is unattributable too.
				unknown = true
				dropping = true
				continue
			}
			section = path
			dropping = codexPathCarriesCredentials(path)
			if dropping {
				continue
			}
			out = append(out, raw)
			continue
		}

		if unknown {
			continue
		}

		if line == "" || strings.HasPrefix(line, "#") {
			if !dropping {
				out = append(out, raw)
			}
			continue
		}

		key, _, isPair := strings.Cut(line, "=")
		drop := dropping
		if isPair {
			path, ok := parseCodexKeyPath(strings.TrimSpace(key))
			if !ok {
				// A key whose name cannot be read cannot be cleared.
				drop = true
			} else if !dropping {
				drop = codexPathCarriesCredentials(append(append([]string{}, section...), path...))
			}
		}
		if balance := bracketBalance(line); balance > 0 {
			pending = balance
			pendingIn = drop
		}
		if drop {
			continue
		}
		out = append(out, raw)
	}
	if unknown {
		return nil, fmt.Errorf("codex config.toml has a section header that is not readable, so its credentials cannot be located: fix the file or remove it from the host state dir")
	}
	return []byte(strings.Join(out, "\n")), nil
}

// codexPathCarriesCredentials reports whether a dotted TOML path names a value
// that must never reach a read-only seat.
func codexPathCarriesCredentials(path []string) bool {
	if len(path) == 0 {
		return false
	}
	if path[0] == "mcp_servers" {
		return true
	}
	if path[0] == "model_providers" {
		for _, segment := range path[1:] {
			// env_key and env_http_headers name environment VARIABLES, not
			// values, so they are not secrets and stay.
			if segment == "api_key" || segment == "http_headers" {
				return true
			}
		}
	}
	return false
}

// parseCodexSectionHeader reads [a.b] and [[a.b]] into a dotted path. The
// second result is false for a header that does not close, which is the case
// narrowCodexConfig fails closed on.
func parseCodexSectionHeader(line string) ([]string, bool) {
	body := strings.TrimPrefix(line, "[")
	body = strings.TrimPrefix(body, "[")
	end := strings.LastIndex(body, "]")
	if end < 0 {
		return nil, false
	}
	body = strings.TrimSpace(body[:end])
	body = strings.TrimSuffix(body, "]")
	if strings.TrimSpace(body) == "" {
		return nil, false
	}
	return parseCodexKeyPath(strings.TrimSpace(body))
}

// parseCodexKeyPath splits a dotted TOML key on separators that are not inside
// quotes, so [mcp_servers."my server".env] classifies as mcp_servers.
func parseCodexKeyPath(key string) ([]string, bool) {
	var (
		path    []string
		current strings.Builder
		quote   rune
	)
	for _, r := range key {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == '.':
			path = append(path, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, false
	}
	path = append(path, strings.TrimSpace(current.String()))
	for _, segment := range path {
		if segment == "" {
			return nil, false
		}
	}
	return path, true
}

// bracketBalance counts unclosed inline-table and array brackets on one line,
// ignoring quoted text and comments.
func bracketBalance(line string) int {
	var (
		depth int
		quote rune
	)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return depth
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		}
	}
	return depth
}
