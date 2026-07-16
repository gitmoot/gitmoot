package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// ClaudeHookResource is an absolute filesystem path referenced by an
// operator-configured Claude command hook. Command text is deliberately not
// retained: dispatch only needs the path and its settings origin.
type ClaudeHookResource struct {
	Path         string
	SettingsPath string
	Event        string
}

// ClaudeHookWarning describes settings that could not be converted into a safe
// absolute resource path. The warning contains no hook command body.
type ClaudeHookWarning struct {
	SettingsPath string
	Event        string
	Reason       string
}

type claudeHookSettings struct {
	Hooks map[string][]claudeHookGroup `json:"hooks"`
}

type claudeHookGroup struct {
	Hooks []claudeHook `json:"hooks"`
}

type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// DiscoverClaudeHookResources reads Claude's user-level settings files and
// returns absolute paths referenced by command hooks. Missing files are a
// no-op. Malformed settings and relative/unparseable commands become warnings
// so dispatch can surface them without guessing at filesystem grants.
func DiscoverClaudeHookResources(home, configDir string) ([]ClaudeHookResource, []ClaudeHookWarning) {
	home = filepath.Clean(strings.TrimSpace(home))
	configDir = strings.TrimSpace(configDir)
	if configDir == "" && home != "" && filepath.IsAbs(home) {
		configDir = filepath.Join(home, ".claude")
	}
	settingsPaths := []string{}
	if configDir != "" {
		settingsPaths = append(settingsPaths, filepath.Join(filepath.Clean(configDir), "settings.json"))
	}
	if home != "" && filepath.IsAbs(home) {
		settingsPaths = append(settingsPaths, filepath.Join(home, ".claude.json"))
	}

	seenSettings := make(map[string]struct{}, len(settingsPaths))
	seenResources := make(map[string]struct{})
	var resources []ClaudeHookResource
	var warnings []ClaudeHookWarning
	for _, settingsPath := range settingsPaths {
		settingsPath = filepath.Clean(settingsPath)
		if _, ok := seenSettings[settingsPath]; ok {
			continue
		}
		seenSettings[settingsPath] = struct{}{}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			warnings = append(warnings, ClaudeHookWarning{SettingsPath: settingsPath, Reason: fmt.Sprintf("cannot read settings: %v", err)})
			continue
		}
		var settings claudeHookSettings
		if err := json.Unmarshal(data, &settings); err != nil {
			warnings = append(warnings, ClaudeHookWarning{SettingsPath: settingsPath, Reason: fmt.Sprintf("invalid JSON: %v", err)})
			continue
		}
		events := make([]string, 0, len(settings.Hooks))
		for event := range settings.Hooks {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			for _, group := range settings.Hooks[event] {
				for _, hook := range group.Hooks {
					if hook.Type != "" && hook.Type != "command" {
						continue
					}
					command := strings.TrimSpace(hook.Command)
					if command == "" {
						warnings = append(warnings, ClaudeHookWarning{SettingsPath: settingsPath, Event: event, Reason: "command hook is empty"})
						continue
					}
					paths, warning := claudeHookCommandPaths(command)
					if warning != "" {
						warnings = append(warnings, ClaudeHookWarning{SettingsPath: settingsPath, Event: event, Reason: warning})
					}
					for _, path := range paths {
						path = filepath.Clean(path)
						key := settingsPath + "\x00" + event + "\x00" + path
						if _, ok := seenResources[key]; ok {
							continue
						}
						seenResources[key] = struct{}{}
						resources = append(resources, ClaudeHookResource{Path: path, SettingsPath: settingsPath, Event: event})
					}
				}
			}
		}
	}
	return resources, warnings
}

func claudeHookCommandPaths(command string) ([]string, string) {
	words, err := splitHookCommand(command)
	if err != nil {
		return nil, err.Error()
	}
	seen := make(map[string]struct{})
	paths := make([]string, 0, len(words))
	var relative string
	for _, word := range words {
		candidate := hookPathCandidate(word)
		if candidate == "" {
			continue
		}
		if filepath.IsAbs(candidate) {
			candidate = filepath.Clean(candidate)
			if _, ok := seen[candidate]; !ok {
				seen[candidate] = struct{}{}
				paths = append(paths, candidate)
			}
			continue
		}
		if relative == "" && looksLikeRelativeHookPath(candidate) {
			relative = candidate
		}
	}
	if relative == "" {
		for i, word := range words {
			if !isHookInterpreter(word) {
				continue
			}
			for _, candidate := range words[i+1:] {
				if strings.HasPrefix(candidate, "-") {
					continue
				}
				candidate = hookPathCandidate(candidate)
				if candidate != "" && !filepath.IsAbs(candidate) {
					relative = candidate
				}
				break
			}
			if relative != "" {
				break
			}
		}
	}
	if relative != "" {
		return paths, fmt.Sprintf("hook command references relative path %q; use an absolute path", relative)
	}
	return paths, ""
}

func splitHookCommand(command string) ([]string, error) {
	var words []string
	var current strings.Builder
	var quote rune
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}
	for _, r := range command {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		switch quote {
		case '\'':
			if r == '\'' {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		case '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				current.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
		case r == '\\':
			escaped = true
		case unicode.IsSpace(r) || strings.ContainsRune(";|&()<>", r):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("hook command has an unterminated quote")
	}
	if escaped {
		return nil, fmt.Errorf("hook command has a trailing escape")
	}
	flush()
	return words, nil
}

func hookPathCandidate(word string) string {
	word = strings.Trim(strings.TrimSpace(word), "[]")
	if idx := strings.LastIndex(word, "="); idx >= 0 && idx+1 < len(word) {
		value := word[idx+1:]
		if filepath.IsAbs(value) || looksLikeRelativeHookPath(value) {
			return value
		}
	}
	return strings.TrimPrefix(word, "@")
}

func looksLikeRelativeHookPath(value string) bool {
	return strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") ||
		strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "$HOME/") ||
		strings.HasPrefix(value, "${HOME}/")
}

func isHookInterpreter(word string) bool {
	word = filepath.Base(hookPathCandidate(word))
	switch word {
	case "sh", "bash", "dash", "zsh", "python", "python3", "node", "ruby", "perl":
		return true
	default:
		return false
	}
}
