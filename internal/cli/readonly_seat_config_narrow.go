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
//
// ONE SCANNER, ONE SET OF RULES. Earlier versions grew three independent line
// scanners over the same file - the narrower plus two selector helpers - each
// with its own idea of what a quote, a comment and a multi-line string are.
// Hardening one of them left the others naive, and every round of review found
// the same class of defect in whichever scanner had not been touched. So the
// file is now tokenized ONCE by scanTOMLLines, and narrowing, provider
// selection and section listing all read that single result.

// narrowedConfig is the result of narrowing one config file.
type narrowedConfig struct {
	data []byte
	// dropped names the sections and keys withheld from the seat. Silence was
	// a review finding: a seat started without its MCP servers gave the
	// reviewer no way to learn why a tool it expected was missing.
	dropped []string
}

// tomlLineKind classifies one physical line of a config file.
type tomlLineKind int

const (
	// tomlLineOther is blank, a comment, or a continuation of the value opened
	// on an earlier line (a multi-line string, array or inline table).
	tomlLineOther tomlLineKind = iota
	tomlLineHeader
	tomlLinePair
)

// tomlLine is one physical line plus the state needed to classify it.
type tomlLine struct {
	raw  string
	kind tomlLineKind
	// path is the FULL dotted path: section-qualified for a pair, absolute for
	// a header. Empty for tomlLineOther.
	path []string
	// section is the header in force when this line was read, so a top-level
	// key is exactly one whose section is empty.
	section []string
	// opensRun is true when this line opens a value that continues onto
	// following lines; those lines inherit this line's disposition.
	opensRun bool
	// readable is false for a pair whose key could not be parsed. Such a line
	// is dropped rather than guessed at.
	readable bool
}

// scanTOMLLines tokenizes a config file into classified lines.
//
// It is a line scanner rather than a full TOML parser, and every place that
// distinction has bitten is handled HERE, once, instead of in each consumer:
//
//   - BACKSLASH ESCAPES inside basic strings. `x = "say \"[hi\""` is valid
//     TOML; reading the escaped quote as a real terminator made the following
//     "[" look unclosed, which opened a continuation run that swallowed - and
//     therefore staged verbatim - the entire rest of the file.
//   - COMMENTS and SINGLE-LINE STRINGS that merely CONTAIN `"""` or `”'`.
//     Counting raw substrings turned `note = 1 # see """ docs` into a
//     never-closing multi-line string (refusing a valid file) and a second
//     stray occurrence into a leak of everything between them.
//   - MULTI-LINE STRINGS, whose interior lines are neither headers nor keys.
//   - UNBALANCED arrays and inline tables, whose continuation lines belong to
//     the key that opened them.
//   - A UTF-8 BOM, which strings.TrimSpace does not remove.
//
// Both open states FAIL CLOSED at end of file: an unterminated string or an
// unbalanced bracket means the tail of the file was never classified, and
// staging an unclassified tail is exactly the leak this function exists to
// prevent. An unreadable section header fails closed for the same reason -
// once a header cannot be attributed, no key after it can be either.
func scanTOMLLines(data []byte) ([]tomlLine, error) {
	var (
		lines     []tomlLine
		section   []string
		multiline string
		pending   int
	)
	for _, raw := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		// A line inside an open run is a continuation, whatever it looks like.
		if multiline != "" || pending != 0 {
			multiline, pending = advanceTOMLState(raw, multiline, pending)
			lines = append(lines, tomlLine{raw: raw, kind: tomlLineOther})
			continue
		}

		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			lines = append(lines, tomlLine{raw: raw, kind: tomlLineOther, section: section})
		case strings.HasPrefix(line, "["):
			path, ok := parseTOMLSectionHeader(line)
			if !ok {
				return nil, fmt.Errorf("config has a section header that is not readable, so its credentials cannot be located: fix the file or remove it from the host state dir")
			}
			section = path
			lines = append(lines, tomlLine{raw: raw, kind: tomlLineHeader, path: path, section: path, readable: true})
		default:
			key, value, isPair := strings.Cut(line, "=")
			if !isPair {
				lines = append(lines, tomlLine{raw: raw, kind: tomlLineOther, section: section})
				continue
			}
			path, ok := parseTOMLKeyPath(strings.TrimSpace(key))
			entry := tomlLine{raw: raw, kind: tomlLinePair, section: section, readable: ok}
			if ok {
				entry.path = append(append([]string{}, section...), path...)
			}
			nextMultiline, nextPending := advanceTOMLState(value, "", 0)
			entry.opensRun = nextMultiline != "" || nextPending > 0
			multiline, pending = nextMultiline, nextPending
			lines = append(lines, entry)
		}
	}
	if multiline != "" {
		return nil, fmt.Errorf("config has a multi-line string that never closes, so narrowing it would corrupt the file: fix the file or remove it from the host state dir")
	}
	if pending != 0 {
		return nil, fmt.Errorf("config has an array or inline table that never closes, so the rest of the file cannot be classified: fix the file or remove it from the host state dir")
	}
	return lines, nil
}

// advanceTOMLState walks one line of TOML text and returns the string and
// bracket state left open at its end.
//
// This is the single place quotes, escapes and comments are interpreted. A
// comment ends the line only OUTSIDE a string; a basic string honours
// backslash escapes; a literal string does not; and a triple delimiter opens a
// multi-line string that a later occurrence on the same line can close again.
func advanceTOMLState(text, multiline string, pending int) (string, int) {
	runes := []rune(text)
	for index := 0; index < len(runes); {
		if multiline != "" {
			if strings.HasPrefix(string(runes[index:]), multiline) {
				index += len([]rune(multiline))
				multiline = ""
				continue
			}
			index++
			continue
		}
		rest := string(runes[index:])
		switch {
		case strings.HasPrefix(rest, `"""`):
			multiline = `"""`
			index += 3
		case strings.HasPrefix(rest, `'''`):
			multiline = `'''`
			index += 3
		case runes[index] == '"' || runes[index] == '\'':
			quote := runes[index]
			index++
			for index < len(runes) {
				if quote == '"' && runes[index] == '\\' {
					// An escaped character - including an escaped quote - is
					// part of the string, not its terminator.
					index += 2
					continue
				}
				if runes[index] == quote {
					index++
					break
				}
				index++
			}
		case runes[index] == '#':
			// Outside a string, the rest of the line is a comment.
			return multiline, pending
		case runes[index] == '[' || runes[index] == '{':
			pending++
			index++
		case runes[index] == ']' || runes[index] == '}':
			pending--
			if pending < 0 {
				pending = 0
			}
			index++
		default:
			index++
		}
	}
	return multiline, pending
}

// narrowTOML rewrites a config file, dropping every path the classifier
// rejects. The classifier receives the FULL dotted path, so a section header, a
// dotted key at top level and a key inside a section are judged identically.
func narrowTOML(lines []tomlLine, drop func(path []string) (bool, string)) narrowedConfig {
	var (
		out        []string
		dropping   bool
		inRun      bool
		runDrop    bool
		droppedSet = map[string]struct{}{}
	)
	note := func(label string) {
		if label != "" {
			droppedSet[label] = struct{}{}
		}
	}
	for _, line := range lines {
		// A continuation line inherits the disposition of the key that opened
		// the run. The scanner classifies every continuation as tomlLineOther,
		// so the run ends at the next header or pair.
		if inRun && line.kind == tomlLineOther {
			if !runDrop {
				out = append(out, line.raw)
			}
			continue
		}
		inRun = false
		switch line.kind {
		case tomlLineHeader:
			var label string
			dropping, label = drop(line.path)
			if dropping {
				note(label)
				continue
			}
			out = append(out, line.raw)
		case tomlLinePair:
			dropLine := dropping
			if !dropping {
				if !line.readable {
					// A key whose name cannot be read cannot be cleared.
					dropLine = true
					note("unreadable key")
				} else {
					var label string
					dropLine, label = drop(line.path)
					note(label)
				}
			}
			if line.opensRun {
				inRun, runDrop = true, dropLine
			}
			if dropLine {
				continue
			}
			out = append(out, line.raw)
		default:
			if !dropping {
				out = append(out, line.raw)
			}
		}
	}

	result := narrowedConfig{data: []byte(strings.Join(out, "\n"))}
	for label := range droppedSet {
		result.dropped = append(result.dropped, label)
	}
	sort.Strings(result.dropped)
	return result
}

// narrowCodexConfig strips third-party credentials from a codex config.toml.
//
//   - mcp_servers is dropped WHOLESALE. env holds tokens, args can hold them
//     just as easily, and a read-only reviewer seat has no business spawning
//     third-party servers. MCP servers are optional to codex, so this cannot
//     stop a launch.
//   - model_providers keeps its STRUCTURE - base_url and wire_api must survive
//     or a custom model becomes unresolvable - and keeps api_key/http_headers
//     ONLY for the provider that model_provider selects.
func narrowCodexConfig(data []byte) ([]byte, error) {
	result, err := narrowCodexConfigDetailed(data)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func narrowCodexConfigDetailed(data []byte) (narrowedConfig, error) {
	lines, err := scanTOMLLines(data)
	if err != nil {
		return narrowedConfig{}, err
	}
	selected := tomlTopLevelScalar(lines, "model_provider")
	result := narrowTOML(lines, func(path []string) (bool, string) {
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
			// than hold values, so they are not secrets.
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
	if err := codexSelectedProviderIsReachable(lines, selected); err != nil {
		return narrowedConfig{}, err
	}
	return result, nil
}

// codexSelectedProviderIsReachable refuses a seat that could not authenticate
// anyway, NAMING the reason.
//
// A custom provider can authenticate with an inline api_key or with env_key,
// which names an environment variable. The seat's environment is a strict
// allowlist (readOnlyRuntimeBaseEnv) that does not pass an arbitrary provider
// variable through, so an env_key-only provider leaves the seat with no
// credential at all. Staging succeeded silently and the runtime failed later
// for a reason of its own choosing - the exact failure mode this whole policy
// exists to remove.
func codexSelectedProviderIsReachable(lines []tomlLine, selected string) error {
	if selected == "" {
		return nil
	}
	var hasAPIKey, hasEnvKey, exists bool
	for _, line := range lines {
		if len(line.path) < 2 || line.path[0] != "model_providers" || line.path[1] != selected {
			continue
		}
		exists = true
		if line.kind != tomlLinePair {
			continue
		}
		switch line.path[len(line.path)-1] {
		case "api_key":
			hasAPIKey = true
		case "env_key":
			hasEnvKey = true
		}
	}
	if !exists || hasAPIKey || !hasEnvKey {
		return nil
	}
	return fmt.Errorf("codex model_provider %q authenticates only through env_key, and a read-only seat's environment allowlist does not pass that variable through: set an inline api_key for it, or point model_provider at a provider the seat can reach", selected)
}

// narrowKimiConfig strips third-party credentials from a kimi config.toml.
//
// This file is REQUIRED for a kimi seat and carries more than codex's does.
// Measured on a live host: api_key under [services.*], a key under
// [services.*.oauth], alongside [providers.*].
//
//   - services is dropped WHOLESALE, for the same reason as codex's
//     mcp_servers: it configures optional tooling and carries those tools'
//     credentials. Dropping the section beats enumerating which of its keys is
//     a secret, and it fails closed when a new one appears.
//   - providers keeps its structure and keeps api_key / env only for the
//     provider default_model resolves to. Dropping every provider credential
//     is not an option: kimi refuses to start when both api_key and the env
//     sub-table are absent, so the seat legitimately needs exactly one.
func narrowKimiConfig(data []byte) ([]byte, error) {
	result, err := narrowKimiConfigDetailed(data)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func narrowKimiConfigDetailed(data []byte) (narrowedConfig, error) {
	lines, err := scanTOMLLines(data)
	if err != nil {
		return narrowedConfig{}, err
	}
	selected := kimiSelectedProvider(lines)
	return narrowTOML(lines, func(path []string) (bool, string) {
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
	}), nil
}

// kimiSelectedProvider resolves default_model to the provider that serves it.
//
// The repo's own fixture pins the shape: default_model = "kimi-code/k3" is
// served by [providers."managed:kimi-code"], so the model's segment before the
// first "/" matches the provider name's segment after the last ":".
//
// An empty result means NO provider was matched, and every provider credential
// is then withheld: a credential nobody can prove is needed is not staged, and
// the withholding is reported so the resulting startup failure names what
// gitmoot removed.
func kimiSelectedProvider(lines []tomlLine) string {
	model := tomlTopLevelScalar(lines, "default_model")
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
	for _, provider := range tomlSectionNames(lines, "providers") {
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

// tomlTopLevelScalar reads a TOP-LEVEL scalar string key (model_provider,
// default_model) from an already-scanned file, so it inherits the scanner's
// multi-line-string and comment handling rather than reimplementing it. Keys
// inside sections are ignored deliberately: both keys are documented as top
// level, and a same-named key in a section must not be read as the selection.
func tomlTopLevelScalar(lines []tomlLine, wanted string) string {
	for _, line := range lines {
		if line.kind != tomlLinePair || !line.readable {
			continue
		}
		// path is section-qualified, so a length of exactly one IS the
		// top-level test: any key inside a section has its header prepended
		// and cannot match. An earlier version also checked line.section
		// explicitly, which was redundant - and unkillable by mutation, which
		// is how the redundancy came to light.
		if len(line.path) != 1 || line.path[0] != wanted {
			continue
		}
		_, value, ok := strings.Cut(strings.TrimSpace(line.raw), "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// tomlSectionNames lists the second path segment of every section under root,
// so [providers."managed:kimi-code"] yields `managed:kimi-code`.
func tomlSectionNames(lines []tomlLine, root string) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, line := range lines {
		if line.kind != tomlLineHeader || len(line.path) < 2 || line.path[0] != root {
			continue
		}
		if _, done := seen[line.path[1]]; done {
			continue
		}
		seen[line.path[1]] = struct{}{}
		names = append(names, line.path[1])
	}
	return names
}

// parseTOMLSectionHeader reads [a.b] and [[a.b]] into a dotted path. The second
// result is false for a header that does not close, which the scanner fails
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
