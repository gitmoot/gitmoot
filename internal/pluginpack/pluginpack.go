package pluginpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/skills"
)

const (
	PluginName      = "gitmoot"
	DisplayName     = "Gitmoot"
	Description     = "Local-first GitHub PR agent coordination."
	RepositoryURL   = "https://github.com/jerryfane/gitmoot"
	HomepageURL     = "https://gitmoot.io"
	PrivacyURL      = "https://gitmoot.io/privacy"
	TermsURL        = "https://gitmoot.io/terms"
	License         = "MIT"
	MarketplaceName = "gitmoot-local"

	skillRoot = "gitmoot"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
)

type BuildOptions struct {
	Provider Provider
	Home     string
	OutDir   string
	Force    bool
	Info     buildinfo.Info
	SourceFS fs.FS
}

type BuildResult struct {
	Provider Provider
	Path     string
	Manifest string
	SkillDir string
}

func ParseProvider(value string) (Provider, error) {
	switch Provider(strings.ToLower(strings.TrimSpace(value))) {
	case ProviderCodex:
		return ProviderCodex, nil
	case ProviderClaude:
		return ProviderClaude, nil
	default:
		return "", fmt.Errorf("unknown plugin runtime %q", value)
	}
}

func DefaultPackageDir(home string, provider Provider) string {
	return filepath.Join(home, "plugins", "build", string(provider), PluginName)
}

func DefaultMarketplaceDir(home string, provider Provider) string {
	return filepath.Join(home, "plugins", "marketplaces", string(provider))
}

func ManifestPath(root string, provider Provider) string {
	switch provider {
	case ProviderCodex:
		return filepath.Join(root, ".codex-plugin", "plugin.json")
	case ProviderClaude:
		return filepath.Join(root, ".claude-plugin", "plugin.json")
	default:
		return filepath.Join(root, "plugin.json")
	}
}

func Build(opts BuildOptions) (BuildResult, error) {
	provider, err := validateProvider(opts.Provider)
	if err != nil {
		return BuildResult{}, err
	}
	source := opts.SourceFS
	if source == nil {
		source = skills.FS
	}
	if err := validateSkill(source); err != nil {
		return BuildResult{}, err
	}

	outDir := strings.TrimSpace(opts.OutDir)
	defaultOutDir := ""
	if strings.TrimSpace(opts.Home) != "" {
		defaultOutDir = filepath.Clean(DefaultPackageDir(opts.Home, provider))
	}
	if outDir == "" {
		if defaultOutDir == "" {
			return BuildResult{}, errors.New("home is required when out dir is omitted")
		}
		outDir = defaultOutDir
	}
	outDir = filepath.Clean(outDir)
	if exists, err := pathExists(outDir); err != nil {
		return BuildResult{}, err
	} else if exists {
		if !opts.Force {
			return BuildResult{}, fmt.Errorf("%s already exists; pass --force to replace it", outDir)
		}
		if outDir != defaultOutDir && !isGeneratedPackageDir(outDir, provider) {
			return BuildResult{}, fmt.Errorf("%s does not look like a generated %s plugin package; refusing forced replacement", outDir, provider)
		}
		if err := os.RemoveAll(outDir); err != nil {
			return BuildResult{}, fmt.Errorf("remove existing package: %w", err)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create package dir: %w", err)
	}

	skillDir := filepath.Join(outDir, "skills", PluginName)
	if err := copyDir(source, skillRoot, skillDir); err != nil {
		return BuildResult{}, err
	}

	manifestPath := ManifestPath(outDir, provider)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create manifest dir: %w", err)
	}
	manifest, err := manifest(provider, opts.Info)
	if err != nil {
		return BuildResult{}, err
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return BuildResult{}, err
	}

	return BuildResult{
		Provider: provider,
		Path:     outDir,
		Manifest: manifestPath,
		SkillDir: skillDir,
	}, nil
}

func validateProvider(provider Provider) (Provider, error) {
	switch provider {
	case ProviderCodex, ProviderClaude:
		return provider, nil
	case "":
		return "", errors.New("plugin runtime is required")
	default:
		return "", fmt.Errorf("unknown plugin runtime %q", provider)
	}
}

func validateSkill(source fs.FS) error {
	info, err := fs.Stat(source, skillRoot)
	if err != nil {
		return fmt.Errorf("canonical skill %q is unavailable: %w", skillRoot, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("canonical skill %q is not a directory", skillRoot)
	}
	skillFile, err := fs.Stat(source, skillRoot+"/SKILL.md")
	if err != nil {
		return fmt.Errorf("canonical skill %q is missing SKILL.md: %w", skillRoot, err)
	}
	if skillFile.IsDir() {
		return fmt.Errorf("canonical skill %q SKILL.md is a directory", skillRoot)
	}
	entries, err := fs.ReadDir(source, skillRoot)
	if err != nil {
		return fmt.Errorf("read canonical skill %q: %w", skillRoot, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("canonical skill %q is empty", skillRoot)
	}
	return nil
}

func copyDir(source fs.FS, sourceRoot, targetRoot string) error {
	return fs.WalkDir(source, sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := strings.TrimPrefix(path, sourceRoot)
		rel = strings.TrimPrefix(rel, "/")
		target := targetRoot
		if rel != "" {
			target = filepath.Join(targetRoot, filepath.FromSlash(rel))
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular skill file %q", path)
		}
		content, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := info.Mode().Perm() | 0o600
		return os.WriteFile(target, content, perm)
	})
}

func manifest(provider Provider, info buildinfo.Info) (any, error) {
	version := manifestVersion(info.Version)
	switch provider {
	case ProviderCodex:
		return codexManifest{
			Name:        PluginName,
			Version:     version,
			Description: Description,
			Author: author{
				Name: "Gitmoot",
				URL:  RepositoryURL,
			},
			Homepage:   HomepageURL,
			Repository: RepositoryURL,
			License:    License,
			Keywords:   []string{"gitmoot", "github", "agents", "codex", "claude"},
			Skills:     "./skills/",
			Interface: codexInterface{
				DisplayName:       DisplayName,
				ShortDescription:  "Coordinate AI agents through GitHub PRs.",
				LongDescription:   "Gitmoot coordinates local Codex and Claude Code agents through GitHub pull request comments, branch locks, jobs, and review result contracts.",
				DeveloperName:     "Gitmoot",
				Category:          "Productivity",
				Capabilities:      []string{"Read", "Write"},
				WebsiteURL:        HomepageURL,
				PrivacyPolicyURL:  PrivacyURL,
				TermsOfServiceURL: TermsURL,
				DefaultPrompt:     []string{"Use $gitmoot to check agent status."},
			},
		}, nil
	case ProviderClaude:
		return claudeManifest{
			Name:        PluginName,
			Description: Description,
			Version:     version,
			Author: author{
				Name: "Gitmoot",
				URL:  RepositoryURL,
			},
			Homepage:   HomepageURL,
			Repository: RepositoryURL,
			License:    License,
		}, nil
	default:
		return nil, fmt.Errorf("unknown plugin runtime %q", provider)
	}
}

type author struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type codexManifest struct {
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Description string         `json:"description"`
	Author      author         `json:"author"`
	Homepage    string         `json:"homepage"`
	Repository  string         `json:"repository"`
	License     string         `json:"license"`
	Keywords    []string       `json:"keywords"`
	Skills      string         `json:"skills"`
	Interface   codexInterface `json:"interface"`
}

type codexInterface struct {
	DisplayName       string   `json:"displayName"`
	ShortDescription  string   `json:"shortDescription"`
	LongDescription   string   `json:"longDescription"`
	DeveloperName     string   `json:"developerName"`
	Category          string   `json:"category"`
	Capabilities      []string `json:"capabilities"`
	WebsiteURL        string   `json:"websiteURL"`
	PrivacyPolicyURL  string   `json:"privacyPolicyURL"`
	TermsOfServiceURL string   `json:"termsOfServiceURL"`
	DefaultPrompt     []string `json:"defaultPrompt"`
}

type claudeManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      author `json:"author"`
	Homepage    string `json:"homepage"`
	Repository  string `json:"repository"`
	License     string `json:"license"`
}

var semverish = regexp.MustCompile(`^\d+\.\d+\.\d+([-.+][0-9A-Za-z.-]+)?$`)

func manifestVersion(version string) string {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" || version == "dev" || version == "unknown" {
		return "0.0.0-dev"
	}
	if semverish.MatchString(version) {
		return version
	}
	return "0.0.0-dev"
}

func writeJSON(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
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

func isGeneratedPackageDir(path string, provider Provider) bool {
	manifestBytes, err := os.ReadFile(ManifestPath(path, provider))
	if err != nil {
		return false
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return false
	}
	if manifest.Name != PluginName {
		return false
	}
	info, err := os.Stat(filepath.Join(path, "skills", PluginName, "SKILL.md"))
	return err == nil && !info.IsDir()
}
