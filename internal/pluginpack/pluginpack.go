package pluginpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/gitmoot/gitmoot/internal/buildinfo"
	"github.com/gitmoot/gitmoot/skills"
)

const (
	PluginName      = "gitmoot"
	DisplayName     = "Gitmoot"
	Description     = "Local-first GitHub PR agent coordination."
	RepositoryURL   = "https://github.com/gitmoot/gitmoot"
	HomepageURL     = "https://gitmoot.io"
	PrivacyURL      = "https://gitmoot.io/privacy"
	TermsURL        = "https://gitmoot.io/terms"
	License         = "Apache-2.0"
	MarketplaceName = "gitmoot-local"

	skillRoot = "gitmoot"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
)

type BuildOptions struct {
	Provider      Provider
	Home          string
	OutDir        string
	Force         bool
	Info          buildinfo.Info
	SourceFS      fs.FS
	GitmootBinary string
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

func HooksPath(root string) string {
	return filepath.Join(root, "hooks", "hooks.json")
}

func ValidateHooksManifest(root string, provider Provider) error {
	if _, err := validateProvider(provider); err != nil {
		return err
	}
	path := HooksPath(root)
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var hooks hooksFile
	if err := json.Unmarshal(content, &hooks); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return validateHooksManifest(hooks, provider)
}

func IsGeneratedPackageDir(path string, provider Provider) bool {
	return isGeneratedPackageDir(path, provider)
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

	hooksPath := HooksPath(outDir)
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("create hooks dir: %w", err)
	}
	hooks, err := hooksManifest(provider, opts.GitmootBinary, runtime.GOOS)
	if err != nil {
		return BuildResult{}, err
	}
	if err := writeJSON(hooksPath, hooks); err != nil {
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

const (
	sessionStartMatcher = "startup|resume|clear|compact"
	hookStatusMessage   = "Loading Gitmoot context"
	hookTimeoutSeconds  = 5
	// turnHookTimeoutSeconds exceeds every budget inside `gitmoot message
	// pending` (claim by 5s, receipts by 8s), so the runtime never cancels the
	// process after a claim committed and discards the output that carried it.
	turnHookTimeoutSeconds = 15
	// postToolUseMatcher is the documented match-every-tool value for both
	// runtimes; UserPromptSubmit and Stop ignore matchers and omit it.
	postToolUseMatcher = "*"
)

// hookSpec is one generated hook event. Turn hooks (#2302) carry no status
// message: PostToolUse runs after every tool call, and a spinner there is noise.
type hookSpec struct {
	event   string
	matcher string
	args    string
	timeout int
	status  string
}

func hookSpecs(provider Provider) []hookSpec {
	specs := []hookSpec{{
		event:   "SessionStart",
		matcher: sessionStartMatcher,
		args:    "plugin hook-context",
		timeout: hookTimeoutSeconds,
		status:  hookStatusMessage,
	}}
	for _, event := range []string{"UserPromptSubmit", "PostToolUse", "Stop"} {
		spec := hookSpec{
			event:   event,
			args:    "message pending --claim --hook " + event + " --runtime " + string(provider),
			timeout: turnHookTimeoutSeconds,
		}
		if event == "PostToolUse" {
			spec.matcher = postToolUseMatcher
		}
		specs = append(specs, spec)
	}
	return specs
}

type hooksFile struct {
	Hooks map[string][]hookMatcher `json:"hooks"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []commandHook `json:"hooks"`
}

type commandHook struct {
	Type           string `json:"type"`
	Command        string `json:"command"`
	CommandWindows string `json:"commandWindows,omitempty"`
	Shell          string `json:"shell,omitempty"`
	Timeout        int    `json:"timeout"`
	StatusMessage  string `json:"statusMessage,omitempty"`
}

func hooksManifest(provider Provider, gitmootBinary string, goos string) (hooksFile, error) {
	if _, err := validateProvider(provider); err != nil {
		return hooksFile{}, err
	}
	hooks := hooksFile{Hooks: map[string][]hookMatcher{}}
	for _, spec := range hookSpecs(provider) {
		handler := commandHook{
			Type:          "command",
			Command:       posixHookCommand(gitmootBinary, spec.args),
			Timeout:       spec.timeout,
			StatusMessage: spec.status,
		}
		switch provider {
		case ProviderCodex:
			handler.CommandWindows = powershellHookCommand(gitmootBinary, spec.args)
		case ProviderClaude:
			if goos == "windows" {
				handler.Command = powershellHookCommand(gitmootBinary, spec.args)
				handler.Shell = "powershell"
			}
		}
		hooks.Hooks[spec.event] = []hookMatcher{{
			Matcher: spec.matcher,
			Hooks:   []commandHook{handler},
		}}
	}
	return hooks, nil
}

func validateHooksManifest(hooks hooksFile, provider Provider) error {
	specs := hookSpecs(provider)
	if len(hooks.Hooks) != len(specs) {
		return fmt.Errorf("hook manifest has %d hook events, want exactly %d (%s)", len(hooks.Hooks), len(specs), hookEventNames(specs))
	}
	for _, spec := range specs {
		groups, ok := hooks.Hooks[spec.event]
		if !ok {
			return fmt.Errorf("hook manifest is missing %s; want exactly %s", spec.event, hookEventNames(specs))
		}
		if len(groups) != 1 {
			return fmt.Errorf("%s hook groups = %d, want one", spec.event, len(groups))
		}
		group := groups[0]
		if group.Matcher != spec.matcher {
			return fmt.Errorf("%s matcher = %q, want %q", spec.event, group.Matcher, spec.matcher)
		}
		if len(group.Hooks) != 1 {
			return fmt.Errorf("%s command hooks = %d, want one", spec.event, len(group.Hooks))
		}
		if err := validateHookCommand(group.Hooks[0], provider, spec); err != nil {
			return fmt.Errorf("%s %w", spec.event, err)
		}
	}
	return nil
}

func hookEventNames(specs []hookSpec) string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.event)
	}
	return strings.Join(names, ", ")
}

func validateHookCommand(hook commandHook, provider Provider, spec hookSpec) error {
	if hook.Type != "command" {
		return fmt.Errorf("hook type = %q, want command", hook.Type)
	}
	if hook.Timeout != spec.timeout {
		return fmt.Errorf("hook timeout = %d, want %d", hook.Timeout, spec.timeout)
	}
	if hook.StatusMessage != spec.status {
		return fmt.Errorf("hook statusMessage = %q, want %q", hook.StatusMessage, spec.status)
	}
	switch provider {
	case ProviderCodex:
		if err := validatePOSIXHookCommand("command", hook.Command, spec.args); err != nil {
			return err
		}
		return validatePowerShellHookCommand("commandWindows", hook.CommandWindows, spec.args)
	case ProviderClaude:
		if hook.Shell == "powershell" {
			return validatePowerShellHookCommand("command", hook.Command, spec.args)
		}
		if strings.TrimSpace(hook.Shell) != "" {
			return fmt.Errorf("hook shell = %q, want empty or powershell", hook.Shell)
		}
		return validatePOSIXHookCommand("command", hook.Command, spec.args)
	default:
		return fmt.Errorf("unknown plugin runtime %q", provider)
	}
}

func validatePOSIXHookCommand(label string, command string, args string) error {
	command = strings.TrimSpace(command)
	suffix := " " + args + " || true"
	if !strings.HasSuffix(command, suffix) {
		return fmt.Errorf("%s has unexpected shape; want <gitmoot-binary>%s", label, suffix)
	}
	binary, ok := parsePOSIXCommandWord(strings.TrimSuffix(command, suffix))
	if !ok {
		return fmt.Errorf("%s gitmoot binary is not a single POSIX shell word", label)
	}
	return validateHookBinary(label, binary)
}

func validatePowerShellHookCommand(label string, command string, args string) error {
	command = strings.TrimSpace(command)
	const prefix = `& "`
	suffix := `" ` + args + `; exit 0`
	if !strings.HasPrefix(command, prefix) {
		return fmt.Errorf("%s has unexpected shape; want & \"<gitmoot-binary>%s", label, suffix)
	}
	end := len(prefix)
	var binary strings.Builder
	for end < len(command) {
		switch command[end] {
		case '`':
			if end+1 >= len(command) {
				return fmt.Errorf("%s has dangling PowerShell escape", label)
			}
			switch command[end+1] {
			case '`', '"', '$':
			default:
				return fmt.Errorf("%s has unsupported PowerShell escape", label)
			}
			binary.WriteByte(command[end+1])
			end += 2
		case '$':
			return fmt.Errorf("%s gitmoot binary contains unescaped PowerShell expansion", label)
		case '"':
			if end == len(prefix) {
				return fmt.Errorf("%s gitmoot binary is empty", label)
			}
			if command[end:] != suffix {
				return fmt.Errorf("%s has unexpected shape; want & \"<gitmoot-binary>%s", label, suffix)
			}
			return validateHookBinary(label, binary.String())
		default:
			binary.WriteByte(command[end])
			end++
		}
	}
	return fmt.Errorf("%s has unterminated PowerShell string", label)
}

func parsePOSIXCommandWord(word string) (string, bool) {
	if word == "" {
		return "", false
	}
	var binary strings.Builder
	for i := 0; i < len(word); {
		switch word[i] {
		case '\'':
			i++
			for i < len(word) && word[i] != '\'' {
				binary.WriteByte(word[i])
				i++
			}
			if i == len(word) {
				return "", false
			}
			i++
		case '"':
			if i+3 > len(word) || word[i:i+3] != `"'"` {
				return "", false
			}
			binary.WriteByte('\'')
			i += 3
		default:
			if !isPOSIXShellSafe(rune(word[i])) {
				return "", false
			}
			binary.WriteByte(word[i])
			i++
		}
	}
	return binary.String(), true
}

func validateHookBinary(label string, binary string) error {
	binary = strings.TrimSpace(binary)
	if !hookBinaryHasPath(binary) {
		if binary != "gitmoot" && binary != "gitmoot.exe" {
			return fmt.Errorf("%s gitmoot binary = %q, want gitmoot executable on PATH or a path to an existing executable", label, binary)
		}
		return nil
	}
	if !filepath.IsAbs(binary) {
		return fmt.Errorf("%s gitmoot binary %q must be absolute", label, binary)
	}
	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("%s gitmoot binary %q is unavailable: %w", label, binary, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s gitmoot binary %q is a directory", label, binary)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s gitmoot binary %q is not executable", label, binary)
	}
	return nil
}

func hookBinaryHasPath(binary string) bool {
	return filepath.IsAbs(binary) || strings.ContainsAny(binary, `/\`)
}

func posixHookCommand(gitmootBinary string, args string) string {
	return posixShellQuote(hookBinary(gitmootBinary)) + " " + args + " || true"
}

func powershellHookCommand(gitmootBinary string, args string) string {
	return "& " + powershellDoubleQuote(hookBinary(gitmootBinary)) + " " + args + "; exit 0"
}

func hookBinary(gitmootBinary string) string {
	if trimmed := strings.TrimSpace(gitmootBinary); trimmed != "" {
		return trimmed
	}
	return "gitmoot"
}

func posixShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if !isPOSIXShellSafe(r) {
			return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		}
	}
	return value
}

func isPOSIXShellSafe(r rune) bool {
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

func powershellDoubleQuote(value string) string {
	escaped := strings.NewReplacer(
		"`", "``",
		`"`, "`\"",
		"$", "`$",
	).Replace(value)
	return `"` + escaped + `"`
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
