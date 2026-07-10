package config

import (
	"fmt"
	"os"
	"strings"
)

// RuntimeOverride is a parsed [runtimes.<name>] config section: an operator's
// partial override of a built-in runtime's declarative metadata (issue #652).
// Each field carries a companion *Set bool reporting whether the key was PRESENT
// in the file, so an override applies EXACTLY the keys the operator wrote and an
// absent-or-empty section is a pure no-op (byte-identical default). The values
// here are neutral data — the runtime package (which owns the metadata registry)
// consumes them; config deliberately does not import runtime, so a new first-class
// runtime is a code change, not a config edit.
type RuntimeOverride struct {
	// Name is the runtime id from the section header ([runtimes.<name>]).
	Name string
	// DefaultModel overrides the runtime's default model (empty = the CLI's own
	// default). DefaultModelSet reports the key was present.
	DefaultModel    string
	DefaultModelSet bool
	// DefaultEffort overrides the runtime's default reasoning effort (empty = no
	// explicit override). DefaultEffortSet reports the key was present.
	DefaultEffort    string
	DefaultEffortSet bool
	// Models replaces the advisory known-valid model list. ModelsSet reports the
	// key was present (an empty array is a valid, explicit "clear the list").
	Models    []string
	ModelsSet bool
	// Capabilities replaces the advertised capability set. CapabilitiesSet reports
	// the key was present.
	Capabilities    []string
	CapabilitiesSet bool
	// UsageSource overrides the human-readable usage descriptor. UsageSourceSet
	// reports the key was present.
	UsageSource    string
	UsageSourceSet bool
}

// LoadRuntimeOverrides parses every [runtimes.<name>] section from the config
// file. A file with no such section returns nil (nothing to apply), which the
// runtime package treats as "use the built-in registry unchanged" — so behavior
// is byte-identical for configs that never write the section. Sections are
// returned in first-seen order; a repeated [runtimes.<name>] merges into the same
// override (last key wins), matching how the other loaders treat duplicate keys.
func LoadRuntimeOverrides(paths Paths) ([]RuntimeOverride, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	collected := map[string]*RuntimeOverride{}
	order := make([]string, 0)
	var current *RuntimeOverride
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = nil
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			name, ok := parseRuntimeRegistrySection(section)
			if !ok {
				continue
			}
			if collected[name] == nil {
				collected[name] = &RuntimeOverride{Name: name}
				order = append(order, name)
			}
			current = collected[name]
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyRuntimeOverrideField(current, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return nil, fmt.Errorf("parse [runtimes.%s].%s: %w", current.Name, strings.TrimSpace(key), err)
		}
	}
	overrides := make([]RuntimeOverride, 0, len(order))
	for _, name := range order {
		overrides = append(overrides, *collected[name])
	}
	return overrides, nil
}

// parseRuntimeRegistrySection extracts the runtime name from a section of the
// form runtimes.<name>. It returns ok=false for any other section (including a
// deeper sub-subsection under <name>), so unrelated sections are ignored.
func parseRuntimeRegistrySection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	const prefix = "runtimes."
	if !strings.HasPrefix(section, prefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(section, prefix))
	if name == "" {
		return "", false
	}
	// The name must be a leaf: a further '.' would be a deeper subsection this
	// section does not define, so reject it rather than silently truncating.
	if strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

func applyRuntimeOverrideField(override *RuntimeOverride, key string, value string) error {
	switch key {
	case "default_model":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		override.DefaultModel = strings.TrimSpace(parsed)
		override.DefaultModelSet = true
		return nil
	case "default_effort":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		override.DefaultEffort = strings.TrimSpace(parsed)
		override.DefaultEffortSet = true
		return nil
	case "models":
		parsed, err := parseConfigStringArray(value)
		if err != nil {
			return err
		}
		override.Models = parsed
		override.ModelsSet = true
		return nil
	case "capabilities":
		parsed, err := parseConfigStringArray(value)
		if err != nil {
			return err
		}
		override.Capabilities = parsed
		override.CapabilitiesSet = true
		return nil
	case "usage_source":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		override.UsageSource = strings.TrimSpace(parsed)
		override.UsageSourceSet = true
		return nil
	default:
		// Unknown keys are ignored so the section can grow without breaking older
		// binaries, mirroring the other config loaders.
		return nil
	}
}
