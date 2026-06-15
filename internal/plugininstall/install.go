package plugininstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/pluginpack"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const (
	defaultScope             = "user"
	marketplacePluginRelPath = "./plugins/" + pluginpack.PluginName
)

type Options struct {
	Provider      pluginpack.Provider
	Home          string
	Scope         string
	Force         bool
	Info          buildinfo.Info
	Runner        subprocess.Runner
	GitmootBinary string
}

type Result struct {
	Provider            pluginpack.Provider
	PackagePath         string
	MarketplaceRoot     string
	MarketplaceManifest string
	Installed           bool
	RuntimeMissing      bool
	ManualCommands      []string
	Commands            []string
}

func Install(ctx context.Context, opts Options) (Result, error) {
	provider, err := pluginpack.ParseProvider(string(opts.Provider))
	if err != nil {
		return Result{}, err
	}
	if opts.Home == "" {
		return Result{}, errors.New("home is required")
	}
	scope := opts.Scope
	if scope == "" {
		scope = defaultScope
	}
	if provider == pluginpack.ProviderClaude && !validClaudeScope(scope) {
		return Result{}, fmt.Errorf("unknown claude plugin scope %q", scope)
	}
	runner := opts.Runner
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}

	marketplaceRoot := pluginpack.DefaultMarketplaceDir(opts.Home, provider)
	packagePath := pluginpack.DefaultPackageDir(opts.Home, provider)
	marketplacePackagePath := filepath.Join(marketplaceRoot, "plugins", pluginpack.PluginName)
	build, err := buildPackage(provider, opts.Home, packagePath, opts.Force, opts.Info, opts.GitmootBinary)
	if err != nil {
		return Result{}, err
	}
	if _, err := buildPackage(provider, opts.Home, marketplacePackagePath, opts.Force, opts.Info, opts.GitmootBinary); err != nil {
		return Result{}, err
	}

	result := Result{
		Provider:            provider,
		PackagePath:         build.Path,
		MarketplaceRoot:     marketplaceRoot,
		MarketplaceManifest: marketplaceManifestPath(opts.Home, provider),
	}
	if err := writeMarketplace(result.MarketplaceRoot, provider); err != nil {
		return Result{}, err
	}

	binary := string(provider)
	if _, err := runner.LookPath(binary); err != nil {
		result.RuntimeMissing = true
		result.ManualCommands = manualCommands(provider, result.MarketplaceRoot, result.PackagePath, scope)
		return result, nil
	}

	switch provider {
	case pluginpack.ProviderCodex:
		if err := runCommand(ctx, runner, &result, "codex", "plugin", "marketplace", "add", result.MarketplaceRoot); err != nil {
			return result, err
		}
		if err := runCommand(ctx, runner, &result, "codex", "plugin", "add", pluginpack.PluginName+"@"+pluginpack.MarketplaceName); err != nil {
			return result, err
		}
	case pluginpack.ProviderClaude:
		if err := runCommand(ctx, runner, &result, "claude", "plugin", "validate", build.Path); err != nil {
			return result, err
		}
		if err := runCommand(ctx, runner, &result, "claude", "plugin", "marketplace", "add", result.MarketplaceRoot, "--scope", scope); err != nil {
			return result, err
		}
		if err := runOptionalClaudeUninstall(ctx, runner, &result, pluginpack.PluginName+"@"+pluginpack.MarketplaceName, scope); err != nil {
			return result, err
		}
		if err := runCommand(ctx, runner, &result, "claude", "plugin", "install", pluginpack.PluginName+"@"+pluginpack.MarketplaceName, "--scope", scope); err != nil {
			return result, err
		}
	}
	result.Installed = true
	return result, nil
}

func buildPackage(provider pluginpack.Provider, home string, outDir string, force bool, info buildinfo.Info, gitmootBinary string) (pluginpack.BuildResult, error) {
	buildForce := force
	if !buildForce {
		if exists, err := pathExists(outDir); err != nil {
			return pluginpack.BuildResult{}, err
		} else if exists && pluginpack.IsGeneratedPackageDir(outDir, provider) {
			buildForce = true
		}
	}
	return pluginpack.Build(pluginpack.BuildOptions{
		Provider:      provider,
		Home:          home,
		OutDir:        outDir,
		Force:         buildForce,
		Info:          info,
		GitmootBinary: gitmootBinary,
	})
}

func validClaudeScope(scope string) bool {
	switch scope {
	case "user", "project", "local":
		return true
	default:
		return false
	}
}

func runCommand(ctx context.Context, runner subprocess.Runner, result *Result, command string, args ...string) error {
	result.Commands = append(result.Commands, command+" "+joinArgs(args))
	run, err := runner.Run(ctx, "", command, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w\nstdout: %s\nstderr: %s", command, joinArgs(args), err, run.Stdout, run.Stderr)
	}
	return nil
}

func runOptionalClaudeUninstall(ctx context.Context, runner subprocess.Runner, result *Result, selector string, scope string) error {
	args := []string{"plugin", "uninstall", selector, "--scope", scope, "--keep-data"}
	result.Commands = append(result.Commands, "claude "+joinArgs(args))
	run, err := runner.Run(ctx, "", "claude", args...)
	if err == nil {
		return nil
	}
	output := run.Stdout + "\n" + run.Stderr
	if strings.Contains(output, "not found in installed plugins") {
		return nil
	}
	return fmt.Errorf("claude %s: %w\nstdout: %s\nstderr: %s", joinArgs(args), err, run.Stdout, run.Stderr)
}

func marketplaceManifestPath(home string, provider pluginpack.Provider) string {
	root := pluginpack.DefaultMarketplaceDir(home, provider)
	switch provider {
	case pluginpack.ProviderCodex:
		return filepath.Join(root, ".agents", "plugins", "marketplace.json")
	case pluginpack.ProviderClaude:
		return filepath.Join(root, ".claude-plugin", "marketplace.json")
	default:
		return filepath.Join(root, "marketplace.json")
	}
}

func writeMarketplace(root string, provider pluginpack.Provider) error {
	path := filepath.Join(root, "marketplace.json")
	switch provider {
	case pluginpack.ProviderCodex:
		path = filepath.Join(root, ".agents", "plugins", "marketplace.json")
	case pluginpack.ProviderClaude:
		path = filepath.Join(root, ".claude-plugin", "marketplace.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create marketplace dir: %w", err)
	}
	payload := marketplacePayload(provider)
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marketplace: %w", err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write marketplace: %w", err)
	}
	return nil
}

func marketplacePayload(provider pluginpack.Provider) any {
	switch provider {
	case pluginpack.ProviderCodex:
		return codexMarketplace{
			Name: pluginpack.MarketplaceName,
			Interface: marketplaceInterface{
				DisplayName: "Gitmoot Local",
			},
			Plugins: []codexMarketplacePlugin{{
				Name: pluginpack.PluginName,
				Source: codexSource{
					Source: "local",
					Path:   marketplacePluginRelPath,
				},
				Policy: codexPolicy{
					Installation:   "AVAILABLE",
					Authentication: "ON_INSTALL",
				},
				Category: "Productivity",
			}},
		}
	case pluginpack.ProviderClaude:
		return claudeMarketplace{
			Schema:      "https://anthropic.com/claude-code/marketplace.schema.json",
			Name:        pluginpack.MarketplaceName,
			Description: "Local Gitmoot plugin marketplace.",
			Owner:       claudeOwner{Name: "Gitmoot"},
			Plugins: []claudeMarketplacePlugin{{
				Name:        pluginpack.PluginName,
				Description: pluginpack.Description,
				Author:      claudeOwner{Name: "Gitmoot"},
				Category:    "productivity",
				Source:      marketplacePluginRelPath,
				Homepage:    pluginpack.HomepageURL,
			}},
		}
	default:
		return map[string]any{"name": pluginpack.MarketplaceName}
	}
}

type marketplaceInterface struct {
	DisplayName string `json:"displayName"`
}

type codexMarketplace struct {
	Name      string                   `json:"name"`
	Interface marketplaceInterface     `json:"interface"`
	Plugins   []codexMarketplacePlugin `json:"plugins"`
}

type codexMarketplacePlugin struct {
	Name     string      `json:"name"`
	Source   codexSource `json:"source"`
	Policy   codexPolicy `json:"policy"`
	Category string      `json:"category"`
}

type codexSource struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

type codexPolicy struct {
	Installation   string `json:"installation"`
	Authentication string `json:"authentication"`
}

type claudeMarketplace struct {
	Schema      string                    `json:"$schema"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Owner       claudeOwner               `json:"owner"`
	Plugins     []claudeMarketplacePlugin `json:"plugins"`
}

type claudeOwner struct {
	Name string `json:"name"`
}

type claudeMarketplacePlugin struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Author      claudeOwner `json:"author"`
	Category    string      `json:"category"`
	Source      string      `json:"source"`
	Homepage    string      `json:"homepage"`
}

func manualCommands(provider pluginpack.Provider, marketplaceRoot string, packagePath string, scope string) []string {
	switch provider {
	case pluginpack.ProviderCodex:
		return []string{
			"codex plugin marketplace add " + shellQuote(marketplaceRoot),
			"codex plugin add " + pluginpack.PluginName + "@" + pluginpack.MarketplaceName,
		}
	case pluginpack.ProviderClaude:
		return []string{
			"claude plugin validate " + shellQuote(packagePath),
			"claude plugin marketplace add " + shellQuote(marketplaceRoot) + " --scope " + scope,
			"claude plugin uninstall " + pluginpack.PluginName + "@" + pluginpack.MarketplaceName + " --scope " + scope + " --keep-data 2>/dev/null || true",
			"claude plugin install " + pluginpack.PluginName + "@" + pluginpack.MarketplaceName + " --scope " + scope,
		}
	default:
		return nil
	}
}

func joinArgs(args []string) string {
	out := ""
	for i, arg := range args {
		if i > 0 {
			out += " "
		}
		out += arg
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if !isShellSafe(r) {
			return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
	}
	return value
}

func isShellSafe(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '/', '.', '_', '-', '+', '=', ':', ',', '@', '%':
		return true
	default:
		return false
	}
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
