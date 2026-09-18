package cli

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	dashboard "github.com/gitmoot/gitmoot-dashboard"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/pipeline"
)

type dashboardConfigSettings struct {
	orchestrate config.OrchestratePolicy
	github      config.GitHubLimiterPolicy
}

type dashboardConfigProjectionRow struct {
	section      string
	key          string
	kind         string
	doc          string
	value        func(dashboardConfigSettings) any
	defaultValue func(dashboardConfigSettings) any
}

// dashboardConfigProjection is the sole value-bearing allowlist for Config.
// Keep it in section/key order: the dashboard polls this payload and expects a
// byte-stable projection when neither the config nor the agents have changed.
var dashboardConfigProjection = []dashboardConfigProjectionRow{
	{section: "github", key: "max_concurrent", kind: "int", doc: "Maximum number of concurrent GitHub calls; zero is unlimited.", value: func(s dashboardConfigSettings) any { return s.github.MaxConcurrent }, defaultValue: func(s dashboardConfigSettings) any { return s.github.MaxConcurrent }},
	{section: "github", key: "min_interval", kind: "duration", doc: "Minimum spacing between GitHub call starts.", value: func(s dashboardConfigSettings) any { return s.github.MinInterval.String() }, defaultValue: func(s dashboardConfigSettings) any { return s.github.MinInterval.String() }},
	{section: "orchestrate", key: "blocked_ttl", kind: "duration", doc: "Maximum time a blocked job remains awaiting a human; empty disables expiry.", value: func(s dashboardConfigSettings) any { return s.orchestrate.BlockedTTL }, defaultValue: func(s dashboardConfigSettings) any { return s.orchestrate.BlockedTTL }},
}

// Config returns the effective, sanitized dashboard configuration. Every value
// comes from a canonical loader and every default comes from its canonical
// constructor; arbitrary TOML values are never copied into the response.
func (d *webDataSource) Config(ctx context.Context) (dashboard.ConfigSnapshot, error) {
	out := dashboard.ConfigSnapshot{
		ContractVersion: 1,
		Sections:        []dashboard.ConfigSection{},
		Agents:          []dashboard.ConfigAgent{},
		UnknownKeys:     []string{},
		Keychain:        dashboard.KeychainView{Keys: []dashboard.KeychainKeyView{}},
	}
	err := withStoreAndPaths(d.home, func(paths config.Paths, store *db.Store) error {
		out.Path = paths.ConfigFile
		if info, err := os.Stat(paths.ConfigFile); err == nil {
			out.Exists = true
			out.ModifiedAt = info.ModTime().UnixMilli()
		} else if !os.IsNotExist(err) {
			return err
		}

		values, defaults, err := loadDashboardConfigSettings(paths)
		if err != nil {
			return err
		}
		out.Sections = projectDashboardConfig(values, defaults)

		agentTypes, err := config.LoadAgentTypes(paths)
		if err != nil {
			return fmt.Errorf("load agent config: %w", err)
		}
		agents, err := store.ListAgents(ctx)
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}
		out.Agents = projectDashboardConfigAgents(agents, agentTypes)
		out.Keychain, err = projectDashboardConfigKeychain(ctx, store, d.home)
		if err != nil {
			return fmt.Errorf("project keychain config: %w", err)
		}
		// Unknown-key discovery re-parses the file with a strict TOML reader,
		// which can reject values gitmoot's lenient loader accepts (e.g. the
		// live box's bare-word list `deterministic_checkers = diff_size,...`).
		// The names are decoration, never worth failing the snapshot: degrade
		// to an empty list on any parse error.
		if unknown, unknownErr := dashboardUnknownConfigKeys(paths.ConfigFile); unknownErr == nil {
			out.UnknownKeys = unknown
		}
		return nil
	})
	if err != nil {
		return dashboard.ConfigSnapshot{}, err
	}
	return out, nil
}

// projectDashboardConfigKeychain exposes registry metadata only. The live file
// check reuses the names-only pipeline classifier; credential values have no
// representation in this projection or in the dashboard contract.
func projectDashboardConfigKeychain(ctx context.Context, store *db.Store, home string) (dashboard.KeychainView, error) {
	path, err := pipeline.ResolveKeychainPath(store, home)
	if err != nil {
		return dashboard.KeychainView{}, err
	}
	inspection := pipeline.ClassifyPipelineEnvFile(ctx, store, home, path)
	out := dashboard.KeychainView{
		File: dashboard.KeychainFileStatus{Path: path, Status: inspection.Status},
		Keys: []dashboard.KeychainKeyView{},
	}

	keys, err := store.ListKeychainKeys(ctx)
	if err != nil {
		return dashboard.KeychainView{}, fmt.Errorf("list keychain keys: %w", err)
	}
	grants, err := store.ListKeychainGrants(ctx, "")
	if err != nil {
		return dashboard.KeychainView{}, fmt.Errorf("list keychain grants: %w", err)
	}
	grantsByKey := make(map[string][]dashboard.KeychainGrantView, len(keys))
	for _, grant := range grants {
		grantsByKey[grant.KeyName] = append(grantsByKey[grant.KeyName], dashboard.KeychainGrantView{
			ConsumerKind: grant.ConsumerKind,
			ConsumerID:   grant.ConsumerID,
		})
	}
	for _, key := range keys {
		row := dashboard.KeychainKeyView{
			Name:      key.Name,
			Mode:      key.Mode,
			Grants:    grantsByKey[key.Name],
			CreatedAt: key.CreatedAt,
		}
		if row.Grants == nil {
			row.Grants = []dashboard.KeychainGrantView{}
		}
		if key.ProxyConfigured() {
			row.ProxyUpstream = key.ProxyUpstream
			row.ProxyAuth = keyProxyAuthLabel(key)
		}
		out.Keys = append(out.Keys, row)
	}
	return out, nil
}

func loadDashboardConfigSettings(paths config.Paths) (dashboardConfigSettings, dashboardConfigSettings, error) {
	values := dashboardConfigSettings{}
	defaults := dashboardConfigSettings{
		orchestrate: config.DefaultOrchestratePolicy(),
		github:      config.DefaultGitHubLimiterPolicy(),
	}
	var err error
	if values.orchestrate, err = config.LoadOrchestratePolicy(paths); err != nil {
		return values, defaults, fmt.Errorf("load orchestrate config: %w", err)
	}
	if values.github, err = config.LoadGitHubLimiterPolicy(paths); err != nil {
		return values, defaults, fmt.Errorf("load github config: %w", err)
	}
	return values, defaults, nil
}

func projectDashboardConfig(values, defaults dashboardConfigSettings) []dashboard.ConfigSection {
	sections := make([]dashboard.ConfigSection, 0)
	for _, row := range dashboardConfigProjection {
		if len(sections) == 0 || sections[len(sections)-1].Name != row.section {
			sections = append(sections, dashboard.ConfigSection{Name: row.section, Knobs: []dashboard.ConfigKnob{}})
		}
		value, defaultValue := row.value(values), row.defaultValue(defaults)
		sections[len(sections)-1].Knobs = append(sections[len(sections)-1].Knobs, dashboard.ConfigKnob{
			Key: row.key, Value: value, Default: defaultValue, IsDefault: reflect.DeepEqual(value, defaultValue), Kind: row.kind, Doc: row.doc,
		})
	}
	return sections
}

func projectDashboardConfigAgents(registered []db.Agent, configured map[string]config.AgentType) []dashboard.ConfigAgent {
	byName := make(map[string]dashboard.ConfigAgent, len(registered)+len(configured))
	for _, agent := range registered {
		byName[agent.Name] = dashboard.ConfigAgent{
			Name: agent.Name, Runtime: strings.TrimSpace(agent.Runtime), Model: strings.TrimSpace(agent.Model),
			Capabilities: append([]string(nil), agent.Capabilities...), AutonomyPolicy: strings.TrimSpace(agent.AutonomyPolicy),
		}
	}
	for name, agentType := range configured {
		row := byName[name]
		row.Name = name
		if runtime := strings.TrimSpace(agentType.Runtime); runtime != "" {
			row.Runtime = runtime
		}
		if model := strings.TrimSpace(agentType.Model); model != "" {
			row.Model = model
		}
		row.Capabilities = append([]string(nil), agentType.Capabilities...)
		row.AutonomyPolicy = strings.TrimSpace(agentType.AutonomyPolicy)
		row.MaxBackground = agentType.MaxBackground
		byName[name] = row
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]dashboard.ConfigAgent, 0, len(names))
	for _, name := range names {
		row := byName[name]
		if row.Capabilities == nil {
			row.Capabilities = []string{}
		}
		out = append(out, row)
	}
	return out
}

func dashboardUnknownConfigKeys(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	doc, err := tomledit.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse config key names: %w", err)
	}

	known := make(map[string]struct{}, len(dashboardConfigProjection))
	for _, row := range dashboardConfigProjection {
		known[row.section+"."+row.key] = struct{}{}
	}
	agentKeys := map[string]struct{}{
		"runtime": {}, "model": {}, "capabilities": {}, "autonomy_policy": {}, "max_background": {},
	}
	unknown := map[string]struct{}{}
	doc.Scan(func(key parser.Key, entry *tomledit.Entry) bool {
		if entry.KeyValue == nil {
			return true
		}
		plain := strings.Join(key, ".")
		if _, ok := known[plain]; ok {
			return true
		}
		if len(key) == 3 && key[0] == "agents" {
			if _, ok := agentKeys[key[2]]; ok {
				return true
			}
		}
		unknown[plain] = struct{}{}
		return true
	})
	out := make([]string, 0, len(unknown))
	for key := range unknown {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
