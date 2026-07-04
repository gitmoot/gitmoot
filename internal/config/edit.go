package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
)

// SetConfigScalar replaces the value of an existing scalar key in the config
// file, preserving comments, key order, and formatting (tomledit edits the
// lossless AST). keyPath is the full dotted path, e.g.
// {"agents", "planner", "max_background"} or {"feedback", "repo"}. The key must
// already exist — adding/removing keys or whole sections stays an $EDITOR job.
//
// The write is atomic (temp file + rename) and validated: if the resulting
// file fails to re-parse through the Load* parsers, the original is restored
// and the validation error is returned.
func SetConfigScalar(paths Paths, keyPath []string, value ConfigScalar) error {
	if len(keyPath) < 2 {
		return fmt.Errorf("config key path must include a section and a key")
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	doc, err := tomledit.Parse(strings.NewReader(string(original)))
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	entry := doc.First(keyPath...)
	if entry == nil || entry.KeyValue == nil {
		return fmt.Errorf("config key %q not found", strings.Join(keyPath, "."))
	}
	parsed, err := parser.ParseValue(value.toml())
	if err != nil {
		return fmt.Errorf("invalid config value %q: %w", value.toml(), err)
	}
	// Preserve any trailing line-comment on the edited key (ParseValue of the
	// bare new value carries none); block comments live on the KeyValue and
	// are untouched.
	parsed.Trailer = entry.Value.Trailer
	entry.Value = parsed

	var buf strings.Builder
	if err := tomledit.Format(&buf, doc); err != nil {
		return fmt.Errorf("format config: %w", err)
	}

	if err := writeConfigAtomic(paths.ConfigFile, []byte(buf.String())); err != nil {
		return err
	}
	// Validate through the real parsers; restore the original on any failure so
	// a bad write can never wedge the daemon.
	if err := validateConfigFile(paths); err != nil {
		if restoreErr := writeConfigAtomic(paths.ConfigFile, original); restoreErr != nil {
			return fmt.Errorf("config invalid after edit AND restore failed (file left broken: %v): %w", restoreErr, err)
		}
		return fmt.Errorf("config invalid after edit (reverted): %w", err)
	}
	return nil
}

// ConfigScalar is a typed scalar value to write, so the caller need not build
// TOML literals (and get quoting wrong).
type ConfigScalar struct {
	str   *string
	num   *int
	float *float64
	flag  *bool
	list  []string
}

// StringScalar / IntScalar / FloatScalar / BoolScalar / StringListScalar
// construct a ConfigScalar.
func StringScalar(v string) ConfigScalar       { return ConfigScalar{str: &v} }
func IntScalar(v int) ConfigScalar             { return ConfigScalar{num: &v} }
func FloatScalar(v float64) ConfigScalar       { return ConfigScalar{float: &v} }
func BoolScalar(v bool) ConfigScalar           { return ConfigScalar{flag: &v} }
func StringListScalar(v []string) ConfigScalar { return ConfigScalar{list: v} }

func (c ConfigScalar) toml() string {
	switch {
	case c.flag != nil:
		return strconv.FormatBool(*c.flag)
	case c.num != nil:
		return strconv.Itoa(*c.num)
	case c.float != nil:
		return strconv.FormatFloat(*c.float, 'g', -1, 64)
	case c.list != nil:
		quoted := make([]string, len(c.list))
		for i, item := range c.list {
			quoted[i] = strconv.Quote(item)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	case c.str != nil:
		return strconv.Quote(*c.str)
	default:
		return `""`
	}
}

// validateConfigFile re-runs every config parser, returning the first error.
func validateConfigFile(paths Paths) error {
	if _, err := LoadAgentTypes(paths); err != nil {
		return err
	}
	if _, err := LoadParallelSessionPolicy(paths); err != nil {
		return err
	}
	if _, err := LoadDefaultFeedbackRepo(paths); err != nil {
		return err
	}
	if _, err := LoadSkillOptABPolicy(paths); err != nil {
		return err
	}
	if _, err := LoadHeartbeats(paths); err != nil {
		return err
	}
	if _, err := LoadRepoConcurrency(paths); err != nil {
		return err
	}
	if _, err := LoadTemplateRemote(paths); err != nil {
		return err
	}
	if _, err := LoadDaemonRuntimeConfig(paths); err != nil {
		return err
	}
	if _, err := LoadMergeGatePolicy(paths); err != nil {
		return err
	}
	if _, err := LoadMemorySettings(paths); err != nil {
		return err
	}
	return nil
}

func writeConfigAtomic(path string, contents []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
