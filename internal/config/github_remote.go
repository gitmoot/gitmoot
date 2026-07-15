package config

import (
	"fmt"
	"os"
	"strings"
)

// GitHubRemotePolicy is the shared config shape for GitHub-backed artifact
// remotes. Feature-specific policy types choose their own defaults.
type GitHubRemotePolicy struct {
	Repo string
	Ref  string
	Path string
}

// Configured reports whether the remote has an owner/repo configured.
func (p GitHubRemotePolicy) Configured() bool {
	return strings.TrimSpace(p.Repo) != ""
}

// ResolvedRef returns the configured ref or the feature's default.
func (p GitHubRemotePolicy) ResolvedRef(defaultRef string) string {
	if ref := strings.TrimSpace(p.Ref); ref != "" {
		return ref
	}
	return defaultRef
}

// ResolvedPath returns the configured subdirectory or the feature's default.
func (p GitHubRemotePolicy) ResolvedPath(defaultPath string) string {
	if path := strings.TrimSpace(p.Path); path != "" {
		return path
	}
	return defaultPath
}

func loadGitHubRemote(paths Paths, section string) (GitHubRemotePolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return GitHubRemotePolicy{}, err
	}
	policy := GitHubRemotePolicy{}
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(name) == section
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if err := applyGitHubRemoteField(&policy, key, strings.TrimSpace(value)); err != nil {
			return GitHubRemotePolicy{}, fmt.Errorf("parse [%s].%s: %w", section, key, err)
		}
	}
	if err := validateGitHubRemote(section, policy); err != nil {
		return GitHubRemotePolicy{}, err
	}
	return policy, nil
}

func applyGitHubRemoteField(policy *GitHubRemotePolicy, key, value string) error {
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

func validateGitHubRemote(section string, policy GitHubRemotePolicy) error {
	repo := strings.TrimSpace(policy.Repo)
	if repo == "" {
		return nil
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("%s.repo %q must be a GitHub owner/repo", section, repo)
	}
	return nil
}

// ensureGitHubRemoteSection appends the three shared scalar keys when an older
// config predates a feature's remote section. It is idempotent.
func ensureGitHubRemoteSection(paths Paths, section string) error {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	want := "[" + section + "]"
	for _, raw := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(stripConfigComment(raw)) == want {
			return nil
		}
	}
	body := string(content)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += "\n" + want + "\nrepo = \"\"\nref = \"\"\npath = \"\"\n"
	return os.WriteFile(paths.ConfigFile, []byte(body), 0o600)
}
