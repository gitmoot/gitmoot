package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeLine(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// emptyText renders a value for a human-readable listing, substituting "-" for an
// empty or whitespace-only one so a column never collapses. It moved here from the
// `gitmoot skillopt` root command deleted in #1752; the agent and dashboard plain
// renderers still use it.
func emptyText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}

// writeJSONFile marshals v as indented JSON to path (with a trailing newline). It
// moved here from the SkillOpt rubric command deleted in #1752; the memory cluster
// and groom exports still use it.
func writeJSONFile(path string, v any) error {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o644)
}

// repeatedStringFlag collects a flag that may be passed more than once, in order.
// It moved here from the SkillOpt review command deleted in #1752; `gitmoot
// pipeline` still uses it for --payload.
type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(*f, ",")
}

func (f *repeatedStringFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
