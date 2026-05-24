package plugininstall

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/pluginpack"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

func TestInstallCodexCommandOrder(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	runner := &fakeRunner{paths: map[string]string{"codex": "/bin/codex"}}

	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderCodex,
		Home:     home,
		Runner:   runner,
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !result.Installed || result.RuntimeMissing {
		t.Fatalf("unexpected result: %+v", result)
	}
	wantCalls := []fakeCall{
		{command: "codex", args: []string{"plugin", "marketplace", "add", pluginpack.DefaultMarketplaceDir(home, pluginpack.ProviderCodex)}},
		{command: "codex", args: []string{"plugin", "add", "gitmoot@gitmoot-local"}},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("calls = %+v, want %+v", runner.calls, wantCalls)
	}
	wantPackagePath := pluginpack.DefaultPackageDir(home, pluginpack.ProviderCodex)
	if result.PackagePath != wantPackagePath {
		t.Fatalf("PackagePath = %q, want %q", result.PackagePath, wantPackagePath)
	}
	if _, err := os.Stat(filepath.Join(result.MarketplaceRoot, "plugins", pluginpack.PluginName, ".codex-plugin", "plugin.json")); err != nil {
		t.Fatalf("expected marketplace package copy: %v", err)
	}
	assertCodexMarketplace(t, result.MarketplaceManifest)
}

func TestInstallClaudeCommandOrder(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	runner := &fakeRunner{paths: map[string]string{"claude": "/bin/claude"}}

	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderClaude,
		Home:     home,
		Scope:    "project",
		Runner:   runner,
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	want := []fakeCall{
		{command: "claude", args: []string{"plugin", "validate", result.PackagePath}},
		{command: "claude", args: []string{"plugin", "marketplace", "add", result.MarketplaceRoot, "--scope", "project"}},
		{command: "claude", args: []string{"plugin", "uninstall", "gitmoot@gitmoot-local", "--scope", "project", "--keep-data"}},
		{command: "claude", args: []string{"plugin", "install", "gitmoot@gitmoot-local", "--scope", "project"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %+v, want %+v", runner.calls, want)
	}
	wantPackagePath := pluginpack.DefaultPackageDir(home, pluginpack.ProviderClaude)
	if result.PackagePath != wantPackagePath {
		t.Fatalf("PackagePath = %q, want %q", result.PackagePath, wantPackagePath)
	}
	if _, err := os.Stat(filepath.Join(result.MarketplaceRoot, "plugins", pluginpack.PluginName, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("expected marketplace package copy: %v", err)
	}
	assertClaudeMarketplace(t, result.MarketplaceManifest)
}

func TestInstallClaudeIgnoresMissingPriorInstall(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	runner := &fakeRunner{
		paths:                 map[string]string{"claude": "/bin/claude"},
		failUninstallNotFound: true,
	}

	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderClaude,
		Home:     home,
		Runner:   runner,
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !result.Installed {
		t.Fatalf("expected installed result: %+v", result)
	}
}

func TestInstallMissingRuntimeKeepsGeneratedFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderCodex,
		Home:     home,
		Runner:   &fakeRunner{},
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !result.RuntimeMissing || result.Installed {
		t.Fatalf("unexpected missing runtime result: %+v", result)
	}
	for _, path := range []string{
		filepath.Join(result.PackagePath, ".codex-plugin", "plugin.json"),
		result.MarketplaceManifest,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected generated file %s: %v", path, err)
		}
	}
	if len(result.ManualCommands) != 2 {
		t.Fatalf("manual commands = %+v", result.ManualCommands)
	}
}

func TestInstallMarketplaceIsIdempotent(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	runner := &fakeRunner{paths: map[string]string{"codex": "/bin/codex"}}
	if _, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderCodex,
		Home:     home,
		Runner:   runner,
	}); err != nil {
		t.Fatalf("first Install() error = %v", err)
	}
	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderCodex,
		Home:     home,
		Runner:   runner,
	})
	if err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	assertCodexMarketplace(t, result.MarketplaceManifest)
}

func TestInstallRefusesExistingNonGeneratedPackageWithoutForce(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gitmoot")
	packagePath := pluginpack.DefaultPackageDir(home, pluginpack.ProviderCodex)
	if err := os.MkdirAll(packagePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packagePath, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderCodex,
		Home:     home,
		Runner:   &fakeRunner{},
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Install() error = %v, want already exists", err)
	}
	if _, err := os.Stat(filepath.Join(packagePath, "keep.txt")); err != nil {
		t.Fatalf("existing file was removed: %v", err)
	}
}

func TestMissingRuntimeManualCommandsUseShellSafeQuotes(t *testing.T) {
	home := filepath.Join(t.TempDir(), "gitmoot '$HOME';next")
	result, err := Install(context.Background(), Options{
		Provider: pluginpack.ProviderClaude,
		Home:     home,
		Runner:   &fakeRunner{},
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if len(result.ManualCommands) != 4 {
		t.Fatalf("manual commands = %+v", result.ManualCommands)
	}
	for _, command := range result.ManualCommands[:2] {
		if !strings.Contains(command, "$HOME") || !strings.Contains(command, "'\"'\"'") {
			t.Fatalf("manual command is not shell-safe: %s", command)
		}
	}
}

type fakeRunner struct {
	paths                 map[string]string
	calls                 []fakeCall
	failUninstallNotFound bool
}

type fakeCall struct {
	command string
	args    []string
}

func (r *fakeRunner) LookPath(file string) (string, error) {
	if path, ok := r.paths[file]; ok {
		return path, nil
	}
	return "", errors.New("not found")
}

func (r *fakeRunner) Run(ctx context.Context, dir string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, fakeCall{command: command, args: append([]string(nil), args...)})
	if r.failUninstallNotFound && command == "claude" && len(args) >= 2 && args[0] == "plugin" && args[1] == "uninstall" {
		return subprocess.Result{Command: command, Args: args, Stderr: `Plugin "gitmoot@gitmoot-local" not found in installed plugins`}, errors.New("exit status 1")
	}
	return subprocess.Result{Command: command, Args: args}, nil
}

func assertCodexMarketplace(t *testing.T, path string) {
	t.Helper()
	var payload struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Source string `json:"source"`
				Path   string `json:"path"`
			} `json:"source"`
		} `json:"plugins"`
	}
	readJSONFile(t, path, &payload)
	if payload.Name != pluginpack.MarketplaceName || len(payload.Plugins) != 1 {
		t.Fatalf("unexpected codex marketplace: %+v", payload)
	}
	if payload.Plugins[0].Name != pluginpack.PluginName || payload.Plugins[0].Source.Path != marketplacePluginRelPath {
		t.Fatalf("unexpected codex plugin entry: %+v", payload.Plugins[0])
	}
}

func assertClaudeMarketplace(t *testing.T, path string) {
	t.Helper()
	var payload struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	readJSONFile(t, path, &payload)
	if payload.Name != pluginpack.MarketplaceName || len(payload.Plugins) != 1 {
		t.Fatalf("unexpected claude marketplace: %+v", payload)
	}
	if payload.Plugins[0].Name != pluginpack.PluginName || payload.Plugins[0].Source != marketplacePluginRelPath {
		t.Fatalf("unexpected claude plugin entry: %+v", payload.Plugins[0])
	}
}

func readJSONFile(t *testing.T, path string, out any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(content, out); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", path, err, content)
	}
}
