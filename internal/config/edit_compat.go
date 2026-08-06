package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/creachadair/tomledit"
)

type legacyConfigLine struct {
	occurrence int
	original   string
}

type editableConfig struct {
	doc         *tomledit.Document
	legacyLines []legacyConfigLine
}

// parseEditableConfig keeps tomledit's strict TOML parser while admitting the
// one legacy scalar grammar Gitmoot previously documented for
// deterministic_checkers. The compatibility rewrite exists only in memory; the
// formatter restores the operator's exact legacy line unless that field itself
// is the edit target.
func parseEditableConfig(original []byte, editedSection string, preserveLegacy bool) (editableConfig, error) {
	normalized, legacyLines, err := normalizeLegacyDeterministicCheckerLines(string(original))
	if err != nil {
		return editableConfig{}, err
	}
	doc, err := tomledit.Parse(strings.NewReader(normalized))
	if err != nil {
		return editableConfig{}, configMutationParseError(string(original), editedSection, err)
	}
	if !preserveLegacy {
		legacyLines = nil
	}
	return editableConfig{doc: doc, legacyLines: legacyLines}, nil
}

func normalizeLegacyDeterministicCheckerLines(contents string) (string, []legacyConfigLine, error) {
	lines := strings.Split(contents, "\n")
	section := ""
	occurrence := 0
	var legacyLines []legacyConfigLine
	for i, raw := range lines {
		line := strings.TrimSpace(stripConfigComment(raw))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "skillopt" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "deterministic_checkers" {
			continue
		}
		names, err := parseDeterministicCheckerList(strings.TrimSpace(value))
		if err != nil {
			return "", nil, fmt.Errorf("parse [skillopt].deterministic_checkers: %w", err)
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "[") {
			occurrence++
			continue
		}
		equals := strings.IndexByte(raw, '=')
		if equals < 0 {
			continue
		}
		comment := ""
		if hash := strings.IndexByte(raw[equals+1:], '#'); hash >= 0 {
			comment = strings.TrimSpace(raw[equals+1+hash:])
		}
		normalized := raw[:equals+1] + " " + StringListScalar(names).toml()
		if comment != "" {
			normalized += " " + comment
		}
		legacyLines = append(legacyLines, legacyConfigLine{occurrence: occurrence, original: raw})
		lines[i] = normalized
		occurrence++
	}
	return strings.Join(lines, "\n"), legacyLines, nil
}

func (c editableConfig) format() ([]byte, error) {
	var formatted strings.Builder
	if err := tomledit.Format(&formatted, c.doc); err != nil {
		return nil, fmt.Errorf("format config: %w", err)
	}
	if len(c.legacyLines) == 0 {
		return []byte(formatted.String()), nil
	}

	restore := make(map[int]string, len(c.legacyLines))
	for _, line := range c.legacyLines {
		restore[line.occurrence] = line.original
	}
	lines := strings.Split(formatted.String(), "\n")
	section := ""
	occurrence := 0
	for i, raw := range lines {
		line := strings.TrimSpace(stripConfigComment(raw))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "skillopt" {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "deterministic_checkers" {
			continue
		}
		if original, ok := restore[occurrence]; ok {
			lines[i] = original
		}
		occurrence++
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func configMutationParseError(contents, editedSection string, parseErr error) error {
	offending := configSectionAtParseError(contents, parseErr)
	if offending != "" && offending != editedSection {
		return fmt.Errorf(
			"parse config: invalid TOML in [%s], not the [%s] section being edited: %w; remedy: correct [%s] syntax (list values use TOML arrays such as [\"a\", \"b\"])",
			offending, editedSection, parseErr, offending,
		)
	}
	return fmt.Errorf(
		"parse config while editing [%s]: %w; remedy: correct the named section's TOML (list values use arrays such as [\"a\", \"b\"])",
		editedSection, parseErr,
	)
}

func configSectionAtParseError(contents string, parseErr error) string {
	message := strings.TrimSpace(parseErr.Error())
	location, _, ok := strings.Cut(strings.TrimPrefix(message, "at "), ":")
	if !ok {
		return ""
	}
	lineNumber, err := strconv.Atoi(location)
	if err != nil || lineNumber <= 0 {
		return ""
	}
	lines := strings.Split(contents, "\n")
	if lineNumber > len(lines) {
		lineNumber = len(lines)
	}
	section := ""
	for _, raw := range lines[:lineNumber] {
		line := strings.TrimSpace(stripConfigComment(raw))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
		}
	}
	return section
}
