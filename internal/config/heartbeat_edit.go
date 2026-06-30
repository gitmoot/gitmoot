package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
)

// SaveHeartbeat writes (creates or updates) one [agents.<agent>.heartbeats.<name>]
// section programmatically through the tomledit AST (#533 write-side), the same
// lossless edit seam SetConfigScalar uses. It is the structural cousin of
// SetConfigScalar: that one edits an existing scalar, this one upserts a whole
// heartbeat subsection while leaving every OTHER section — agent-type blocks and
// sibling heartbeats — byte-for-byte untouched.
//
// CRITICAL no-clobber invariant: unlike SaveAgentType (which rewrites and
// re-appends all agent-type blocks), this never rewrites agent-type config, so a
// heartbeat write can never drop an agent type. The companion guard in
// SaveAgentType (removeAgentTypeBlocks scopes to top-level blocks only) keeps the
// reverse true. The write is atomic and re-validated through validateConfigFile;
// the original is restored on any validation failure so a bad write cannot wedge
// the daemon.
func SaveHeartbeat(paths Paths, entry Heartbeat) error {
	applyHeartbeatDefaults(&entry)
	if err := validateHeartbeat(entry); err != nil {
		return err
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	doc, err := tomledit.Parse(strings.NewReader(string(original)))
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	tableName := parser.Key{"agents", entry.Agent, "heartbeats", entry.Name}
	section := findSection(doc, tableName)
	// Field order mirrors the documented config example so a freshly written block
	// reads top-to-bottom as enabled→repo→interval→jitter→action→prompt→concurrency.
	fields := []struct {
		key   string
		value ConfigScalar
	}{
		{"enabled", BoolScalar(entry.Enabled)},
		{"repo", StringScalar(entry.Repo)},
		{"interval", StringScalar(entry.Interval)},
		{"jitter", StringScalar(entry.Jitter)},
		{"action", StringScalar(entry.Action)},
		{"prompt", StringScalar(entry.Prompt)},
		{"max_concurrent", IntScalar(entry.MaxConcurrent)},
	}
	if section == nil {
		section = &tomledit.Section{Heading: &parser.Heading{Name: tableName}}
		for _, field := range fields {
			kv, err := newKeyValue(field.key, field.value)
			if err != nil {
				return err
			}
			section.Items = append(section.Items, kv)
		}
		doc.Sections = append(doc.Sections, section)
	} else {
		for _, field := range fields {
			if err := setSectionScalar(section, field.key, field.value); err != nil {
				return err
			}
		}
	}
	return formatAndValidateConfig(paths, doc, original)
}

// SetHeartbeatEnabled flips just the `enabled` flag of an existing heartbeat via
// the lossless scalar seam (preserving comments and the rest of the block). It
// errors if no such heartbeat section exists, so `enable`/`disable` cannot
// silently create a half-formed entry — use `heartbeat add` for that.
func SetHeartbeatEnabled(paths Paths, agent, name string, enabled bool) error {
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	if agent == "" || name == "" {
		return fmt.Errorf("heartbeat agent and name are required")
	}
	keyPath := []string{"agents", agent, "heartbeats", name, "enabled"}
	// The `enabled` key is always written by SaveHeartbeat, so the common path is a
	// pure scalar edit. A hand-written config may have omitted it (defaulting to
	// false); in that case fall back to a full upsert that reads the existing block,
	// flips enabled, and rewrites the section through the same AST seam.
	if err := SetConfigScalar(paths, keyPath, BoolScalar(enabled)); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return err
		}
		existing, ok, loadErr := loadHeartbeat(paths, agent, name)
		if loadErr != nil {
			return loadErr
		}
		if !ok {
			return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s] not found", agent, name)
		}
		existing.Enabled = enabled
		return SaveHeartbeat(paths, existing)
	}
	return nil
}

// RemoveHeartbeat deletes one [agents.<agent>.heartbeats.<name>] section via the
// AST seam, leaving all other config untouched. It reports whether a section was
// actually removed (false ⇒ the heartbeat did not exist).
func RemoveHeartbeat(paths Paths, agent, name string) (bool, error) {
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	if agent == "" || name == "" {
		return false, fmt.Errorf("heartbeat agent and name are required")
	}
	original, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return false, err
	}
	doc, err := tomledit.Parse(strings.NewReader(string(original)))
	if err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	entry := doc.First("agents", agent, "heartbeats", name)
	if entry == nil || !entry.IsSection() {
		return false, nil
	}
	if !entry.Remove() {
		return false, nil
	}
	if err := formatAndValidateConfig(paths, doc, original); err != nil {
		return false, err
	}
	return true, nil
}

// loadHeartbeat returns one named heartbeat from the parsed config.
func loadHeartbeat(paths Paths, agent, name string) (Heartbeat, bool, error) {
	heartbeats, err := LoadHeartbeats(paths)
	if err != nil {
		return Heartbeat{}, false, err
	}
	for _, hb := range heartbeats {
		if hb.Agent == agent && hb.Name == name {
			return hb, true, nil
		}
	}
	return Heartbeat{}, false, nil
}

// findSection returns the named-table section whose heading equals name, or nil.
func findSection(doc *tomledit.Document, name parser.Key) *tomledit.Section {
	for _, section := range doc.Sections {
		if section.TableName().Equals(name) {
			return section
		}
	}
	return nil
}

// newKeyValue builds a parser.KeyValue for a single scalar key.
func newKeyValue(key string, value ConfigScalar) (*parser.KeyValue, error) {
	parsed, err := parser.ParseValue(value.toml())
	if err != nil {
		return nil, fmt.Errorf("invalid config value %q: %w", value.toml(), err)
	}
	return &parser.KeyValue{Name: parser.Key{key}, Value: parsed}, nil
}

// setSectionScalar sets key=value inside section, updating the existing mapping
// (preserving its trailing comment) when present or appending a new one. It is
// the section-scoped analogue of SetConfigScalar's value swap.
func setSectionScalar(section *tomledit.Section, key string, value ConfigScalar) error {
	parsed, err := parser.ParseValue(value.toml())
	if err != nil {
		return fmt.Errorf("invalid config value %q: %w", value.toml(), err)
	}
	for _, item := range section.Items {
		kv, ok := item.(*parser.KeyValue)
		if !ok || len(kv.Name) != 1 || kv.Name[0] != key {
			continue
		}
		parsed.Trailer = kv.Value.Trailer
		kv.Value = parsed
		return nil
	}
	section.Items = append(section.Items, &parser.KeyValue{Name: parser.Key{key}, Value: parsed})
	return nil
}

// formatAndValidateConfig serializes doc, writes it atomically, then re-validates
// through every Load* parser; on any validation failure it restores original so a
// malformed write can never wedge the daemon. It mirrors SetConfigScalar's tail.
func formatAndValidateConfig(paths Paths, doc *tomledit.Document, original []byte) error {
	var buf strings.Builder
	if err := tomledit.Format(&buf, doc); err != nil {
		return fmt.Errorf("format config: %w", err)
	}
	if err := writeConfigAtomic(paths.ConfigFile, []byte(buf.String())); err != nil {
		return err
	}
	if err := validateConfigFile(paths); err != nil {
		if restoreErr := writeConfigAtomic(paths.ConfigFile, original); restoreErr != nil {
			return fmt.Errorf("config invalid after edit AND restore failed (file left broken: %v): %w", restoreErr, err)
		}
		return fmt.Errorf("config invalid after edit (reverted): %w", err)
	}
	return nil
}
