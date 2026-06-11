package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/creachadair/tomledit"
)

// ConfiguredCodexModel returns the model the codex CLI is configured to use: the
// top-level `model` key in <codex-home>/config.toml, resolving the codex home the
// same way the adapter does (CODEX_HOME, else ~/.codex). It returns "" with no
// error when the config file or the key is absent, so callers treat an empty
// result as "no override".
func ConfiguredCodexModel() (string, error) {
	path, err := codexConfigPath()
	if err != nil || path == "" {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	doc, err := tomledit.Parse(strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	entry := doc.First("model")
	if entry == nil || entry.KeyValue == nil {
		return "", nil
	}
	raw := strings.TrimSpace(entry.KeyValue.Value.String())
	// The value is the TOML source form (e.g. `"gpt-5.5"`); unquote a basic
	// string, falling back to trimming quotes for a literal string.
	if unq, err := strconv.Unquote(raw); err == nil {
		return strings.TrimSpace(unq), nil
	}
	return strings.TrimSpace(strings.Trim(raw, "\"'")), nil
}

func codexConfigPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Join(home, "config.toml"), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".codex", "config.toml"), nil
}
