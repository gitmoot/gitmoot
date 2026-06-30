package config

import (
	"fmt"
	"os"
	"strings"
)

// TemplateRemotePolicy is the host-level default GitHub repo the agent-template
// publish/pull/add commands fall back to when --repo is omitted, read from the
// [template_remote] section of the gitmoot config (#476). It is OFF BY DEFAULT:
// with no [template_remote] section (or an empty repo) Configured() is false and
// those commands require an explicit --repo, so behavior is byte-identical to a
// config without the section. It follows the LoadEventsPolicy line-parser
// pattern in orchestrate.go.
type TemplateRemotePolicy struct {
	// Repo is the default GitHub owner/repo. Empty (the default) means no default
	// remote is configured.
	Repo string
	// Ref is the default git ref. Empty falls back to DefaultTemplateRemoteRef.
	Ref string
	// Path is the default subdir holding the template .md files. Empty falls back
	// to DefaultTemplateRemotePath.
	Path string
}

const (
	// DefaultTemplateRemoteRef is the ref used when [template_remote].ref is unset.
	DefaultTemplateRemoteRef = "main"
	// DefaultTemplateRemotePath is the subdir used when [template_remote].path is
	// unset: the directory holding the template .md files.
	DefaultTemplateRemotePath = "templates"
)

func DefaultTemplateRemotePolicy() TemplateRemotePolicy {
	return TemplateRemotePolicy{Repo: "", Ref: "", Path: ""}
}

// Configured reports whether a default template remote is set. With no
// [template_remote] section (the default) it is false.
func (p TemplateRemotePolicy) Configured() bool {
	return strings.TrimSpace(p.Repo) != ""
}

// ResolvedRef returns Ref, or DefaultTemplateRemoteRef when unset.
func (p TemplateRemotePolicy) ResolvedRef() string {
	if ref := strings.TrimSpace(p.Ref); ref != "" {
		return ref
	}
	return DefaultTemplateRemoteRef
}

// ResolvedPath returns Path (the subdir holding the .md files), or
// DefaultTemplateRemotePath when unset.
func (p TemplateRemotePolicy) ResolvedPath() string {
	if path := strings.TrimSpace(p.Path); path != "" {
		return path
	}
	return DefaultTemplateRemotePath
}

func LoadTemplateRemote(paths Paths) (TemplateRemotePolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return TemplateRemotePolicy{}, err
	}
	policy := DefaultTemplateRemotePolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "template_remote"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyTemplateRemoteField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return TemplateRemotePolicy{}, fmt.Errorf("parse [template_remote].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateTemplateRemote(policy); err != nil {
		return TemplateRemotePolicy{}, err
	}
	return policy, nil
}

func applyTemplateRemoteField(policy *TemplateRemotePolicy, key string, value string) error {
	switch key {
	case "repo":
		parsed, err := parseConfigString(value)
		policy.Repo = strings.TrimSpace(parsed)
		return err
	case "ref":
		parsed, err := parseConfigString(value)
		policy.Ref = strings.TrimSpace(parsed)
		return err
	case "path":
		parsed, err := parseConfigString(value)
		policy.Path = strings.TrimSpace(parsed)
		return err
	default:
		return nil
	}
}

func validateTemplateRemote(policy TemplateRemotePolicy) error {
	repo := strings.TrimSpace(policy.Repo)
	if repo == "" {
		return nil
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("template_remote.repo %q must be a GitHub owner/repo", repo)
	}
	return nil
}

// EnsureTemplateRemoteSection appends an empty [template_remote] section to the
// config when it is absent, so SetConfigScalar (which only edits keys that
// already exist) can drive `agent template remote set` on a config created
// before this feature. It is idempotent: when the section already exists it is a
// no-op. Fresh configs (DefaultConfig) ship the section, so this only ever fires
// for older configs.
func EnsureTemplateRemoteSection(paths Paths) error {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	for _, raw := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(stripConfigComment(raw)) == "[template_remote]" {
			return nil
		}
	}
	body := string(content)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "\n[template_remote]\nrepo = \"\"\nref = \"\"\npath = \"\"\n"
	return os.WriteFile(paths.ConfigFile, []byte(body), 0o600)
}
