package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ClientConfig is the [client] section (#2710): this gitmoot home is a client
// of a hub on another machine. With Hub set, the commands the hub serves call
// it, and every other command that needs the org database refuses, so a
// client never falls back to an empty local store.
type ClientConfig struct {
	// Hub is the hub's https origin, e.g. "https://100.106.218.88:7711".
	Hub string
	// Certs is the directory holding ca.pem, NAME.crt and NAME.key from
	// `gitmoot hub machine add`. Relative paths are relative to the home.
	Certs string
	// Name is the machine name the certificate was issued for (NAME).
	Name string
	// Role is the default acting role when a machine holds several and no
	// --role or GITMOOT_ORG_ROLE names one. Optional.
	Role string
	// Agent is this machine's local omp agent name that `gitmoot client
	// deliver` notifies (agent:NAME). Optional; only omp seats need it.
	Agent string
}

// Enabled reports whether this home is a hub client.
func (c ClientConfig) Enabled() bool { return c.Hub != "" }

// LoadClientConfig reads [client]. A missing file or section is not a client.
func LoadClientConfig(paths Paths) (ClientConfig, error) {
	var cfg ClientConfig
	content, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return ClientConfig{}, err
	}
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "client") {
				return ClientConfig{}, fmt.Errorf("malformed section header %q", line)
			}
			current = section == "client"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return ClientConfig{}, fmt.Errorf("parse [client]: expected key = value, got %q", line)
		}
		key = strings.TrimSpace(key)
		parsed, err := parseConfigString(strings.TrimSpace(value))
		if err != nil {
			return ClientConfig{}, fmt.Errorf("parse [client].%s: %w", key, err)
		}
		switch key {
		case "hub":
			cfg.Hub = strings.TrimSpace(parsed)
		case "certs":
			cfg.Certs = strings.TrimSpace(parsed)
		case "name":
			cfg.Name = strings.ToLower(strings.TrimSpace(parsed))
		case "role":
			cfg.Role = strings.ToLower(strings.TrimSpace(parsed))
		case "agent":
			cfg.Agent = strings.TrimSpace(parsed)
		default:
			return ClientConfig{}, fmt.Errorf("parse [client]: unknown key %q", key)
		}
	}
	if !cfg.Enabled() {
		if cfg != (ClientConfig{}) {
			return ClientConfig{}, errors.New("[client] needs hub")
		}
		return cfg, nil
	}
	hub, err := url.Parse(cfg.Hub)
	if err != nil || hub.Scheme != "https" || hub.Host == "" || hub.Port() == "" || (hub.Path != "" && hub.Path != "/") || hub.RawQuery != "" || hub.User != nil {
		return ClientConfig{}, fmt.Errorf("[client].hub %q: want https://HOST:PORT", cfg.Hub)
	}
	cfg.Hub = "https://" + hub.Host
	if cfg.Certs == "" || cfg.Name == "" {
		return ClientConfig{}, errors.New("[client] needs certs and name with hub")
	}
	if !filepath.IsAbs(cfg.Certs) {
		cfg.Certs = filepath.Join(paths.Home, cfg.Certs)
	}
	return cfg, nil
}
