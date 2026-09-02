package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/creachadair/tomledit"
)

type editableConfig struct {
	doc *tomledit.Document
}

// parseEditableConfig parses the config with tomledit's strict TOML parser,
// mapping a parse failure onto the section being edited so the error names the
// offending block.
//
// It used to carry a compatibility rewrite for the one legacy scalar grammar
// Gitmoot documented — the bare comma list accepted for
// [skillopt].deterministic_checkers — restoring the operator's exact line on
// format. That section went with the SkillOpt loop (#1752), so the only grammar
// left is strict TOML.
func parseEditableConfig(original []byte, editedSection string) (editableConfig, error) {
	doc, err := tomledit.Parse(strings.NewReader(string(original)))
	if err != nil {
		return editableConfig{}, configMutationParseError(string(original), editedSection, err)
	}
	return editableConfig{doc: doc}, nil
}

func (c editableConfig) format() ([]byte, error) {
	var formatted strings.Builder
	if err := tomledit.Format(&formatted, c.doc); err != nil {
		return nil, fmt.Errorf("format config: %w", err)
	}
	return []byte(formatted.String()), nil
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
