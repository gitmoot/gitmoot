package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// RequireWorkflowPolicy is the resolved per-repository workflow-label policy.
// It is read at enqueue time, so daemon config changes take effect on its next
// construction without restart plumbing.
type RequireWorkflowPolicy struct {
	Enabled bool
	Mode    string
}

func defaultRequireWorkflowPolicy() RequireWorkflowPolicy { return RequireWorkflowPolicy{Mode: "auto"} }

// RequireWorkflowConfig holds global [workflow] defaults and flat
// [repos."owner/repo"] overrides. Pointer fields preserve inheritance when a
// repository omits one of the two keys.
type RequireWorkflowConfig struct {
	global requireWorkflowOverride
	repos  map[string]requireWorkflowOverride
}

type requireWorkflowOverride struct {
	enabled *bool
	mode    *string
}

func (c RequireWorkflowConfig) For(repo string) RequireWorkflowPolicy {
	p := defaultRequireWorkflowPolicy()
	applyRequireWorkflowOverride(&p, c.global)
	applyRequireWorkflowOverride(&p, c.repos[strings.TrimSpace(repo)])
	return p
}

func applyRequireWorkflowOverride(p *RequireWorkflowPolicy, o requireWorkflowOverride) {
	if o.enabled != nil {
		p.Enabled = *o.enabled
	}
	if o.mode != nil {
		p.Mode = *o.mode
	}
}

// LoadRequireWorkflow parses [workflow] and flat [repos."owner/repo"] keys.
// The loader is deliberately independent from other [workflow] scanners.
func LoadRequireWorkflow(paths Paths) (RequireWorkflowConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return RequireWorkflowConfig{}, err
	}
	cfg := RequireWorkflowConfig{repos: map[string]requireWorkflowOverride{}}
	var repo string
	inSection := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			repo, inSection = parseRequireWorkflowSection(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			if inSection && repo != "" {
				if _, ok := cfg.repos[repo]; !ok {
					cfg.repos[repo] = requireWorkflowOverride{}
				}
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if repo == "" {
			if err := applyRequireWorkflowField(&cfg.global, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
				return RequireWorkflowConfig{}, fmt.Errorf("parse [workflow].%s: %w", strings.TrimSpace(key), err)
			}
		} else {
			o := cfg.repos[repo]
			if err := applyRequireWorkflowField(&o, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
				return RequireWorkflowConfig{}, fmt.Errorf("parse [repos.%q].%s: %w", repo, strings.TrimSpace(key), err)
			}
			cfg.repos[repo] = o
		}
	}
	if err := validateRequireWorkflowPolicy("[workflow]", cfg.For("")); err != nil {
		return RequireWorkflowConfig{}, err
	}
	for repo := range cfg.repos {
		if err := validateRequireWorkflowPolicy(fmt.Sprintf("[repos.%q]", repo), cfg.For(repo)); err != nil {
			return RequireWorkflowConfig{}, err
		}
	}
	return cfg, nil
}

func parseRequireWorkflowSection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if section == "workflow" {
		return "", true
	}
	if !strings.HasPrefix(section, "repos.") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(section, "repos."))
	if strings.HasPrefix(rest, "\"") {
		unquoted, err := strconv.Unquote(rest)
		if err != nil || strings.TrimSpace(unquoted) == "" {
			return "", false
		}
		return strings.TrimSpace(unquoted), true
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

func applyRequireWorkflowField(o *requireWorkflowOverride, key, value string) error {
	switch key {
	case "require_workflow":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		o.enabled = &v
	case "require_workflow_mode":
		v, err := strconv.Unquote(value)
		if err != nil {
			return err
		}
		o.mode = &v
	}
	return nil
}

func validateRequireWorkflowPolicy(section string, p RequireWorkflowPolicy) error {
	if p.Mode != "auto" && p.Mode != "strict" {
		return fmt.Errorf("%s.require_workflow_mode must be \"auto\" or \"strict\"", section)
	}
	return nil
}
