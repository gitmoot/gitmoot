package cli

import (
	"fmt"
	"sort"
	"strings"
)

// Narrowing a runtime's config file before it is staged into a read-only seat.
//
// The seat's state dir lives inside the ONE writable path granted to the
// sandbox, so anything staged there is readable by the reviewer the seat runs.
// Both codex and kimi keep model settings in the same file as credentials for
// unrelated third parties, so the file cannot be copied through.
//
// The rule both runtimes share: a seat needs the credential for the provider it
// actually runs on, and no others. Everything else that carries a secret is
// withheld, and WHAT WAS WITHHELD IS REPORTED, so a seat that then cannot
// authenticate says so rather than leaving the runtime to explain.

// narrowedConfig is the result of narrowing one config file.
type narrowedConfig struct {
	data []byte
	// dropped names the sections and keys withheld from the seat. Silence was
	// a review finding: a seat started without its MCP servers gave the
	// reviewer no way to learn why a tool it expected was missing.
	dropped []string
}

// narrowCodexConfig strips third-party credentials from a codex config.toml.
//
//   - mcp_servers is dropped WHOLESALE. env holds tokens, args can hold them
//     just as easily, and a read-only reviewer seat has no business spawning
//     third-party servers. MCP servers are optional to codex, so this cannot
//     stop a launch.
//   - model_providers keeps its STRUCTURE - base_url and wire_api must survive
//     or a custom model becomes unresolvable - and keeps api_key/http_headers
//     ONLY for the provider that model_provider selects. Stripping the selected
//     provider's key too was a review finding: env_key survives narrowing but
//     names a variable readOnlyRuntimeBaseEnv's allowlist does not pass into
//     the seat, so the seat was left with no way to authenticate at all.
//
// Everything else is kept: those are the model and sandbox settings the file is
// staged for.
func narrowCodexConfig(data []byte) ([]byte, error) {
	result, err := narrowCodexConfigDetailed(data)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func narrowCodexConfigDetailed(data []byte) (narrowedConfig, error) {
	selected := tomlTopLevelScalar(data, "model_provider")
	return narrowTOML(data, func(path []string) (bool, string) {
		if len(path) == 0 {
			return false, ""
		}
		if path[0] == "mcp_servers" {
			return true, "mcp_servers." + pathTail(path, 1)
		}
		if path[0] != "model_providers" || len(path) < 2 {
			return false, ""
		}
		provider := path[1]
		credential := false
		for _, segment := range path[2:] {
			// env_key and env_http_headers name environment VARIABLES rather
			// than hold values, so they are not secrets and stay.
			if segment == "api_key" || segment == "http_headers" {
				credential = true
			}
		}
		if !credential {
			return false, ""
		}
		if selected != "" && provider == selected {
			// The provider this seat will actually use: the one credential it
			// legitimately needs.
			return false, ""
		}
		return true, "model_providers." + provider + " credential"
	})
}

// narrowKimiConfig strips third-party credentials from a kimi config.toml.
//
// This file is REQUIRED for a kimi seat and carries more than codex's does.
// Measured on a live host: api_key under [services.*], a key under
// [services.*.oauth], alongside [providers.*].
//
//   - services is dropped WHOLESALE, for the same reason as codex's
//     mcp_servers: it configures optional tooling (moonshot search and fetch)
//     and carries those tools' credentials. Dropping the section beats
//     enumerating which of its keys is a secret, and it fails closed when a
//     new one appears.
//   - providers keeps its structure and keeps api_key / env only for the
//     provider default_model resolves to. Dropping every provider credential
//     is not an option: kimi fails at startup when both api_key and the env
//     sub-table are absent, so the seat legitimately needs exactly one.
func narrowKimiConfig(data []byte) ([]byte, error) {
	result, err := narrowKimiConfigDetailed(data)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func narrowKimiConfigDetailed(data []byte) (narrowedConfig, error) {
	selected := kimiSelectedProvider(data)
	return narrowTOML(data, func(path []string) (bool, string) {
		if len(path) == 0 {
			return false, ""
		}
		if path[0] == "services" {
			return true, "services." + pathTail(path, 1)
		}
		if path[0] != "providers" || len(path) < 2 {
			return false, ""
		}
		provider := path[1]
		credential := false
		for _, segment := range path[2:] {
			if segment == "api_key" || segment == "env" {
				credential = true
			}
		}
		if !credential {
			return false, ""
		}
		if selected != "" && provider == selected {
			return false, ""
		}
		return true, "providers." + provider + " credential"
	})
}

// kimiSelectedProvider resolves default_model to the provider that serves it.
//
// The repo's own fixture pins the shape: default_model = "kimi-code/k3" is
// served by [providers."managed:kimi-code"], so the model's segment before the
// first "/" matches the provider name's segment after the last ":".
//
// An empty result means NO provider was matched, and every provider credential
// is then withheld: a credential nobody can prove is needed is not staged. The
// withholding is reported, so the resulting startup failure names what gitmoot
// removed instead of arriving as the runtime's own message.
func kimiSelectedProvider(data []byte) string {
	model := tomlTopLevelScalar(data, "default_model")
	if model == "" {
		return ""
	}
	wanted := model
	if index := strings.Index(wanted, "/"); index >= 0 {
		wanted = wanted[:index]
	}
	wanted = strings.TrimSpace(wanted)
	if wanted == "" {
		return ""
	}
	var match string
	for _, provider := range tomlSectionNames(data, "providers") {
		short := provider
		if index := strings.LastIndex(short, ":"); index >= 0 {
			short = short[index+1:]
		}
		if short != wanted && provider != wanted {
			continue
		}
		if match != "" && match != provider {
			// Ambiguous: withhold rather than guess which one is live.
			return ""
		}
		match = provider
	}
	return match
}

func pathTail(path []string, from int) string {
	if from >= len(path) {
		return "*"
	}
	return strings.Join(path[from:], ".")
}

// narrowTOML rewrites a TOML file line by line, dropping every path the
// classifier rejects. The classifier receives the FULL dotted path, so a
// section header, a dotted key at top level, and a key inside a section are all
// judged the same way.
//
// It is a line scanner, not a TOML parser, and the places that distinction
// bites are handled explicitly:
//
//   - MULTI-LINE STRINGS. A line inside """ or ”' that begins with "[" is not
//     a section header. Treating it as one both LEAKED (the fake header reset
//     the section, so a following api_key was attributed to nothing) and
//     CORRUPTED (it deleted lines out of a valid file and left an unterminated
//     string, so codex reported a TOML error about a file gitmoot wrote).
//   - UNBALANCED inline tables and arrays, which continue in the disposition of
//     the key that opened them.
//   - A UTF-8 BOM, which strings.TrimSpace does not remove, so a leading BOM
//     hid the first header and classified everything under it against an empty
//     path.
//
// An unreadable section header, and a multi-line string that never closes, both
// FAIL CLOSED with an error naming the file. Once a header cannot be attributed
// no following key can be classified, and the direction that leaks is the one
// that guesses "probably fine".
func narrowTOML(data []byte, drop func(path []string) (bool, string)) (narrowedConfig, error) {
	var (
		out        []string
		section    []string
		dropping   bool
		pending    int
		pendingIn  bool
		multiline  string
		multiIn    bool
		droppedSet = map[string]struct{}{}
	)
	note := func(label string) {
		if label != "" {
			droppedSet[label] = struct{}{}
		}
	}

	for _, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)

		// Inside a multi-line string nothing is a header and nothing is a key.
		if multiline != "" {
			if !multiIn {
				out = append(out, raw)
			}
			if strings.Contains(raw, multiline) {
				multiline = ""
			}
			continue
		}

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
			path, ok := parseTOMLSectionHeader(line)
			if !ok {
				return narrowedConfig{}, fmt.Errorf("config has a section header that is not readable, so its credentials cannot be located: fix the file or remove it from the host state dir")
			}
			section = path
			var label string
			dropping, label = drop(path)
			if dropping {
				note(label)
				continue
			}
			out = append(out, raw)
			continue
		}

		if line == "" || strings.HasPrefix(line, "#") {
			if !dropping {
				out = append(out, raw)
			}
			continue
		}

		key, value, isPair := strings.Cut(line, "=")
		dropLine := dropping
		if isPair && !dropping {
			path, ok := parseTOMLKeyPath(strings.TrimSpace(key))
			if !ok {
				// A key whose name cannot be read cannot be cleared.
				dropLine = true
				note("unreadable key")
			} else {
				var label string
				dropLine, label = drop(append(append([]string{}, section...), path...))
				note(label)
			}
		}
		if delimiter := openMultilineDelimiter(value); delimiter != "" {
			multiline, multiIn = delimiter, dropLine
		} else if balance := bracketBalance(line); balance > 0 {
			pending, pendingIn = balance, dropLine
		}
		if dropLine {
			continue
		}
		out = append(out, raw)
	}
	if multiline != "" {
		return narrowedConfig{}, fmt.Errorf("config has a multi-line string that never closes, so narrowing it would corrupt the file: fix the file or remove it from the host state dir")
	}

	result := narrowedConfig{data: []byte(strings.Join(out, "\n"))}
	for label := range droppedSet {
		result.dropped = append(result.dropped, label)
	}
	sort.Strings(result.dropped)
	return result, nil
}

// openMultilineDelimiter reports the delimiter of a multi-line string opened on
// this line and not closed on it: `"""a"""` opens and closes, `"""` stays open.
func openMultilineDelimiter(value string) string {
	for _, delimiter := range []string{`"""`, `'''`} {
		if count := strings.Count(value, delimiter); count > 0 && count%2 == 1 {
			return delimiter
		}
	}
	return ""
}

// tomlTopLevelScalar reads a TOP-LEVEL scalar string key (model_provider,
// default_model). Keys inside sections are ignored deliberately: both are
// documented as top level, and a same-named key in a section must not be
// mistaken for the selection.
func tomlTopLevelScalar(data []byte, wanted string) string {
	inSection := false
	for _, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") {
			inSection = true
			continue
		}
		if inSection || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		path, parsed := parseTOMLKeyPath(strings.TrimSpace(key))
		if !parsed || len(path) != 1 || path[0] != wanted {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// tomlSectionNames lists the second path segment of every section under root,
// so [providers."managed:kimi-code"] yields `managed:kimi-code`.
func tomlSectionNames(data []byte, root string) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		path, ok := parseTOMLSectionHeader(line)
		if !ok || len(path) < 2 || path[0] != root {
			continue
		}
		if _, done := seen[path[1]]; done {
			continue
		}
		seen[path[1]] = struct{}{}
		names = append(names, path[1])
	}
	return names
}

// parseTOMLSectionHeader reads [a.b] and [[a.b]] into a dotted path. The second
// result is false for a header that does not close, which narrowTOML fails
// closed on.
func parseTOMLSectionHeader(line string) ([]string, bool) {
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
	return parseTOMLKeyPath(strings.TrimSpace(body))
}

// parseTOMLKeyPath splits a dotted TOML key on separators outside quotes, so
// [mcp_servers."my server".env] classifies as mcp_servers. A backslash escape
// inside a basic string is honoured: ["a\"b".c] is valid TOML, and failing it
// closed would refuse a whole seat over a file the runtime accepts.
func parseTOMLKeyPath(key string) ([]string, bool) {
	var (
		path    []string
		current strings.Builder
		quote   rune
		escaped bool
	)
	for _, r := range key {
		switch {
		case quote != 0:
			if escaped {
				escaped = false
				current.WriteRune(r)
				continue
			}
			if r == '\\' && quote == '"' {
				escaped = true
				continue
			}
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
