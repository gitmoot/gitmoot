package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// RemoteExecConfig is the optional [remote_exec] section (#1535 P0 contract,
// #1536 P1): it selects the execution backend — WHERE a job's runtime
// subprocess executes. It is distinct from the Landlock local-confinement
// surface (internal/sandbox, the `sandbox` CLI, agent path grants), which is
// unrelated and untouched by this seam.
//
// The section is OFF by default: a config file with no [remote_exec] section
// loads the DefaultRemoteExecConfig ("local"), which is a byte-for-byte
// passthrough to the pre-#1536 runner composition.
type RemoteExecConfig struct {
	// Backend is the [remote_exec].backend selection. "local" is the default;
	// GITMOOT-IMPL: "remote" requires a validated E2B provider configuration.
	Backend string
	// LocalUID and LocalGID opt local-backend commands into an OS-level
	// privilege drop. They are a pair: omitting both preserves the daemon
	// identity, while setting only one is invalid.
	LocalUID *uint32
	LocalGID *uint32
	// LocalRoot optionally relocates local instances beneath a parent the
	// configured identity can traverse (required when the Gitmoot home itself is
	// below a root-only directory such as /root).
	LocalRoot string
	// GITMOOT-IMPL: E2BAPIKeyFile is read only while validating or constructing the remote
	// provider. Secret bytes are deliberately never retained in this config.
	E2BAPIKeyFile string
	E2BTemplate   string
	// E2BOMPTemplate is a purpose-built template with enough memory for the OMP
	// runtime. The public base template has 512 MiB, and a 1 GiB proof sandbox
	// was OOM-killed during OMP startup.
	E2BOMPTemplate string
	E2BBaseURL     string
	E2BDomain      string
	// Provider names the remote E2B-protocol endpoint the fields above describe,
	// for the ledger, admission and reconciliation. The [remote_exec] section
	// itself always describes cloud E2B; ForProvider returns the view of an
	// opted-in job, whose provider is chosen per job, never home-wide.
	Provider string
	// OMPLinuxFile and OMPGuestArch are set only in a sandboxd view: the
	// host-side Linux OMP executable uploaded into that provider's guests and
	// the guest architecture its key declares (omp_linux_arm64_file or
	// omp_linux_amd64_file). E2BEnvdBaseURL is likewise sandboxd-only: one envd
	// origin with sandbox routing headers in place of wildcard hosts.
	E2BEnvdBaseURL string
	OMPLinuxFile   string
	OMPGuestArch   string
	// SandboxdProviders holds the optional sandboxd provider sections, keyed by
	// provider name: [remote_exec.sandboxd] and [remote_exec.sandboxd-linux].
	// Each declares a sandboxd gateway beside E2B; a job runs there only when
	// its payload names it (exec_provider = "sandboxd" or "sandboxd-linux").
	SandboxdProviders map[string]*SandboxdProviderConfig
	// CredentialGatewayListen is the daemon bind address; URL is the HTTPS
	// origin reachable from a sandbox. They are configured together and are
	// used only for opt-in broker material, never for the provider control key.
	CredentialGatewayListen string
	CredentialGatewayURL    string
	// ExecBackendCost is the compute-dollar cap (#1540). Its zero value denies
	// every cloud provision, which is why it needs no default: a [remote_exec]
	// section that sets backend = "remote" without cost keys provisions nothing.
	ExecBackendCost ExecBackendCostConfig
}

// Remote execution providers. E2B is the default for every remote job; a
// sandboxd provider is used only by a job that opts in explicitly.
// RemoteExecProviderSandboxd is the Mac gateway; RemoteExecProviderSandboxdLinux
// is a separate Linux (Firecracker) gateway with its own endpoint, key,
// templates, caps and ledger rows.
const (
	RemoteExecProviderE2B           = "e2b"
	RemoteExecProviderSandboxd      = "sandboxd"
	RemoteExecProviderSandboxdLinux = "sandboxd-linux"
)

// remoteExecSandboxdProviders lists the sandboxd providers in their stable
// order: validation, gateway origins and reconciliation follow it.
var remoteExecSandboxdProviders = []string{RemoteExecProviderSandboxd, RemoteExecProviderSandboxdLinux}

// RemoteExecSandboxdProviders returns the sandboxd provider names.
func RemoteExecSandboxdProviders() []string {
	return append([]string(nil), remoteExecSandboxdProviders...)
}

// IsSandboxdProvider reports whether a canonical provider name is a sandboxd
// gateway, admitted by reported capacity rather than dollars.
func IsSandboxdProvider(name string) bool {
	for _, provider := range remoteExecSandboxdProviders {
		if name == provider {
			return true
		}
	}
	return false
}

// IsRemoteExecProvider reports whether a canonical provider name is known.
func IsRemoteExecProvider(name string) bool {
	return name == RemoteExecProviderE2B || IsSandboxdProvider(name)
}

// RemoteExecProviderChoices renders the allowed provider names for errors.
func RemoteExecProviderChoices() string {
	return fmt.Sprintf("%q, %q and %q", RemoteExecProviderE2B, RemoteExecProviderSandboxd, RemoteExecProviderSandboxdLinux)
}

// SandboxdProviderSection names a sandboxd provider's config.toml section.
func SandboxdProviderSection(provider string) string {
	return "[remote_exec." + provider + "]"
}

// removedMacProvider is the sandboxd provider's name before #2328 renamed it.
// It is no longer accepted anywhere; every refusal names the replacement.
const removedMacProvider = "mac"

// UnknownRemoteExecProviderError refuses name given as field (for example
// "--exec-provider" or "checks_provider"), listing the allowed providers and,
// for the removed "mac", the provider it was renamed to.
func UnknownRemoteExecProviderError(field, name string) error {
	if name == removedMacProvider {
		return fmt.Errorf("unknown %s %q: the provider was renamed %q; allowed providers are %s", field, name, RemoteExecProviderSandboxd, RemoteExecProviderChoices())
	}
	return fmt.Errorf("unknown %s %q: allowed providers are %s", field, name, RemoteExecProviderChoices())
}

// OMPLinuxFileKey is the provider-section key naming the OMP executable for a
// guest architecture (execbackend.GuestArchARM64 or GuestArchAMD64): the key
// that names the file declares the architecture, omp_linux_arm64_file or
// omp_linux_amd64_file.
func OMPLinuxFileKey(arch string) string {
	return "omp_linux_" + arch + "_file"
}

// SandboxdProviderConfig is one sandboxd provider section
// ([remote_exec.sandboxd] or [remote_exec.sandboxd-linux]): a sandboxd
// gateway, an E2B-compatible API, as an opt-in remote provider. It is
// capacity-limited on-prem compute with no dollar cost: concurrency follows the
// capacity the gateway reports at GET /sandboxd/capacity.
type SandboxdProviderConfig struct {
	APIKeyFile  string
	Template    string
	OMPTemplate string
	BaseURL     string
	EnvdBaseURL string
	// OMPLinuxARM64File and OMPLinuxAMD64File name the host-side OMP
	// executable uploaded into the provider's guests; which one is set declares
	// the guest architecture, so at most one may be.
	OMPLinuxARM64File string
	OMPLinuxAMD64File string
	// CredentialGatewayURL is the gateway origin sandboxd guests dial (the
	// sandboxd relay). The daemon's one gateway listener also advertises it,
	// beside [remote_exec].credential_gateway_url. Empty means sandboxd guests
	// use that URL.
	CredentialGatewayURL string
	// MaxConcurrent is an optional operator ceiling on the reported capacity;
	// 0 means no extra ceiling. Against a sandboxd without the capacity
	// endpoint it is the whole cap.
	MaxConcurrent int
}

// DefaultRemoteExecConfig preserves today's local backend and cloud identity.
func DefaultRemoteExecConfig() RemoteExecConfig {
	return RemoteExecConfig{Backend: string(execbackend.Local), Provider: RemoteExecProviderE2B}
}

// LoadRemoteExecConfig parses the optional [remote_exec] section. A missing
// section returns the default (local). An unknown backend value is a hard
// error — via execbackend.Parse it names the offending value AND the allowed
// set, and dispatch surfaces it as a job failure rather than ever falling
// back silently.
func LoadRemoteExecConfig(paths Paths) (RemoteExecConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return RemoteExecConfig{}, err
	}
	cfg := DefaultRemoteExecConfig()
	current := false
	var provider *SandboxdProviderConfig
	providerSection := ""
	// declaredBy remembers the header that declared each sandboxd provider.
	// TOML refuses a table defined twice; any two spellings of one provider
	// would otherwise merge silently, later keys winning.
	declaredBy := map[string]string{}
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			current = section == "remote_exec"
			provider = nil
			if remoteExecSubsection(section) == removedMacProvider {
				return RemoteExecConfig{}, fmt.Errorf("config.toml section [remote_exec.%s] is no longer read: the provider was renamed %q, so rename the section to %s (same keys)", removedMacProvider, RemoteExecProviderSandboxd, SandboxdProviderSection(RemoteExecProviderSandboxd))
			}
			name, declared := sandboxdProviderSectionName(section)
			if declared {
				providerSection = "[" + section + "]"
				if previous, seen := declaredBy[name]; seen {
					return RemoteExecConfig{}, fmt.Errorf("config.toml declares the %s provider twice, as %s and [%s]: keep one section", name, previous, section)
				}
				declaredBy[name] = "[" + section + "]"
				if cfg.SandboxdProviders == nil {
					cfg.SandboxdProviders = map[string]*SandboxdProviderConfig{}
				}
				if cfg.SandboxdProviders[name] == nil {
					cfg.SandboxdProviders[name] = &SandboxdProviderConfig{}
				}
				provider = cfg.SandboxdProviders[name]
			}
			continue
		}
		if !current && provider == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if provider != nil {
			if err := parseSandboxdProviderKey(provider, providerSection, key, value); err != nil {
				return RemoteExecConfig{}, err
			}
			continue
		}
		switch key {
		case "provider", "e2b_envd_base_url", "omp_linux_arm64_file", "omp_linux_amd64_file":
			// These once selected the sandboxd provider for the whole home. The
			// provider is now chosen per job, so a home-wide value is refused
			// rather than ignored.
			return RemoteExecConfig{}, fmt.Errorf("[remote_exec].%s is not supported: declare the sandboxd provider in [remote_exec.sandboxd] or [remote_exec.sandboxd-linux] and opt a job in with --exec-provider", key)
		case "backend":
			parsed, err := parseConfigString(value)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].backend: %w", err)
			}
			cfg.Backend = strings.TrimSpace(parsed)
		case "local_uid", "local_gid":
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].%s: expected an unsigned integer: %w", key, err)
			}
			converted := uint32(parsed)
			if key == "local_uid" {
				cfg.LocalUID = &converted
			} else {
				cfg.LocalGID = &converted
			}
		case "local_root":
			parsed, err := parseConfigString(value)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].local_root: %w", err)
			}
			cfg.LocalRoot = strings.TrimSpace(parsed)
		case "e2b_api_key_file", "e2b_template", "e2b_omp_template", "e2b_base_url", "e2b_domain", "credential_gateway_listen", "credential_gateway_url":
			parsed, err := parseConfigString(value)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].%s: %w", key, err)
			}
			parsed = strings.TrimSpace(parsed)
			switch key {
			case "e2b_api_key_file":
				cfg.E2BAPIKeyFile = parsed
			case "e2b_template":
				cfg.E2BTemplate = parsed
			case "e2b_omp_template":
				cfg.E2BOMPTemplate = parsed
			case "e2b_base_url":
				cfg.E2BBaseURL = parsed
			case "e2b_domain":
				cfg.E2BDomain = parsed
			case "credential_gateway_listen":
				cfg.CredentialGatewayListen = parsed
			case "credential_gateway_url":
				cfg.CredentialGatewayURL = parsed
			}
		case "cost_max_reserved_usd", "cost_per_attempt_usd", "cost_per_hour_usd":
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].%s: expected a number: %w", key, err)
			}
			switch key {
			case "cost_max_reserved_usd":
				cfg.ExecBackendCost.MaxReservedUSD = parsed
			case "cost_per_attempt_usd":
				cfg.ExecBackendCost.PerAttemptUSD = parsed
			default:
				cfg.ExecBackendCost.PerHourUSD = parsed
			}
		case "cost_max_concurrent":
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].cost_max_concurrent: expected an integer: %w", err)
			}
			cfg.ExecBackendCost.MaxConcurrent = parsed
		default:
			// Ignore unknown keys so the section remains forward-compatible.
		}
	}
	if err := validateRemoteExecConfig(cfg); err != nil {
		return RemoteExecConfig{}, err
	}
	return cfg, nil
}

func validateRemoteExecConfig(cfg RemoteExecConfig) error {
	if err := cfg.ExecBackendCost.Validate(); err != nil {
		return err
	}
	for _, name := range remoteExecSandboxdProviders {
		if sandboxd := cfg.SandboxdProviders[name]; sandboxd != nil {
			if err := sandboxd.validate(cfg, name); err != nil {
				return err
			}
		}
	}
	backend, err := execbackend.ParseImplemented(cfg.Backend)
	if err != nil {
		return fmt.Errorf("unsupported [remote_exec].backend: %w", err)
	}
	if (cfg.LocalUID == nil) != (cfg.LocalGID == nil) {
		return fmt.Errorf("[remote_exec].local_uid and [remote_exec].local_gid must be configured together")
	}
	if cfg.LocalUID != nil {
		if *cfg.LocalUID == 0 || *cfg.LocalUID == ^uint32(0) {
			return fmt.Errorf("[remote_exec].local_uid must be a non-root usable uid, got %d", *cfg.LocalUID)
		}
		if *cfg.LocalGID == 0 || *cfg.LocalGID == ^uint32(0) {
			return fmt.Errorf("[remote_exec].local_gid must be a non-root usable gid, got %d", *cfg.LocalGID)
		}
	}
	if cfg.LocalRoot != "" && !filepath.IsAbs(cfg.LocalRoot) {
		return fmt.Errorf("[remote_exec].local_root must be an absolute path, got %q", cfg.LocalRoot)
	}
	if cfg.LocalRoot != "" && filepath.Dir(filepath.Clean(cfg.LocalRoot)) == filepath.Clean(cfg.LocalRoot) {
		return fmt.Errorf("[remote_exec].local_root must not be a filesystem root, got %q", cfg.LocalRoot)
	}
	if backend == execbackend.Remote {
		if err := cfg.ValidateE2BProvider(); err != nil {
			return err
		}
	}
	return nil
}

// GITMOOT-IMPL: ValidateE2BProvider preflights every value needed to construct the remote
// provider. It performs no network calls and does not retain credential bytes.
func (cfg RemoteExecConfig) ValidateE2BProvider() error {
	apiKey, err := cfg.LoadE2BAPIKey()
	if err != nil {
		return err
	}
	if len(apiKey) < 8 {
		return fmt.Errorf("[remote_exec].e2b_api_key_file must contain a key of at least 8 characters")
	}
	if strings.TrimSpace(cfg.E2BTemplate) == "" {
		return fmt.Errorf("[remote_exec].e2b_template is required when backend=remote")
	}
	if err := validateE2BDomain(cfg.E2BDomain); err != nil {
		return fmt.Errorf("invalid [remote_exec].e2b_domain: %w", err)
	}
	if err := validateE2BBaseURL(cfg.E2BBaseURL); err != nil {
		return fmt.Errorf("invalid [remote_exec].e2b_base_url: %w", err)
	}
	if IsSandboxdProvider(cfg.Provider) {
		sandboxd := cfg.SandboxdProviders[cfg.Provider]
		if sandboxd == nil {
			return fmt.Errorf("the %s remote provider is not configured: add a %s section", cfg.Provider, SandboxdProviderSection(cfg.Provider))
		}
		if err := sandboxd.validate(cfg, cfg.Provider); err != nil {
			return err
		}
	}
	if err := cfg.ValidateCredentialGateway(); err != nil {
		return err
	}
	return nil
}

// ValidateOMPExecutable checks a sandboxd view's configured OMP executable:
// it must exist, be an executable regular file, and be a Linux ELF for the
// guest architecture its key declares. A view without one passes; an omp
// review on it is refused at provision. Requests for the provider and doctor
// run it; backend construction does not, so reconciliation never depends on
// the upload file.
func (cfg RemoteExecConfig) ValidateOMPExecutable() error {
	if cfg.OMPLinuxFile == "" {
		return nil
	}
	file, err := execbackend.OpenLinuxExecutable(cfg.OMPLinuxFile, cfg.OMPGuestArch)
	if err != nil {
		return fmt.Errorf("%s.%s: %w", SandboxdProviderSection(cfg.Provider), OMPLinuxFileKey(cfg.OMPGuestArch), err)
	}
	return file.Close()
}

// ForProvider returns the configuration one remote job runs with. An empty
// name is the default, cloud E2B. "sandboxd" and "sandboxd-linux" substitute
// their own section for the E2B endpoint, credentials, templates and caps;
// the gateway listener and the rest of the section are shared. Any other
// name, or a sandboxd provider without its section, is refused rather than
// falling back to E2B.
func (cfg RemoteExecConfig) ForProvider(name string) (RemoteExecConfig, error) {
	canonical := strings.TrimSpace(name)
	switch {
	case canonical == "" || canonical == RemoteExecProviderE2B:
		cfg.Provider = RemoteExecProviderE2B
		return cfg, nil
	case IsSandboxdProvider(canonical):
		section := cfg.SandboxdProviders[canonical]
		if section == nil {
			return RemoteExecConfig{}, fmt.Errorf("remote execution provider %q is not configured: add a %s section to config.toml", canonical, SandboxdProviderSection(canonical))
		}
		sandboxd := *section
		cfg.Provider = canonical
		cfg.E2BAPIKeyFile = sandboxd.APIKeyFile
		cfg.E2BTemplate = sandboxd.Template
		cfg.E2BOMPTemplate = sandboxd.OMPTemplate
		cfg.E2BBaseURL = sandboxd.BaseURL
		cfg.E2BDomain = ""
		cfg.E2BEnvdBaseURL = sandboxd.EnvdBaseURL
		cfg.OMPGuestArch, cfg.OMPLinuxFile = sandboxd.GuestArch()
		// The ceiling only: the effective cap is read from the gateway's
		// reported capacity at admission (#30).
		cfg.ExecBackendCost = ExecBackendCostConfig{MaxConcurrent: sandboxd.MaxConcurrent}
		return cfg, nil
	default:
		return RemoteExecConfig{}, UnknownRemoteExecProviderError("remote execution provider", canonical)
	}
}

// ValidateProvider preflights the named provider for a job that requests it,
// including its API key file and, for a sandboxd provider, its OMP
// executable, without any network call.
func (cfg RemoteExecConfig) ValidateProvider(name string) error {
	view, err := cfg.ForProvider(name)
	if err != nil {
		return err
	}
	if err := view.ValidateE2BProvider(); err != nil {
		return err
	}
	return view.ValidateOMPExecutable()
}

// reviewChecksProviderConfigured reports whether a repository's checks_*
// routing (#2316) names a provider this home configures, without reading the
// API key file: the view must exist, name a key file, and have a template once
// checks_template is applied. Which template a review provisions depends on
// its runtime, which only dispatch knows; ReviewChecksTemplateError refuses
// the runtime a view with one of the two templates cannot serve.
func (cfg RemoteExecConfig) reviewChecksProviderConfigured(route ReviewChecksRoute) error {
	view, err := cfg.ForProvider(route.Provider)
	if err != nil {
		return err
	}
	section := view.reviewChecksSection()
	if strings.TrimSpace(view.E2BAPIKeyFile) == "" {
		return fmt.Errorf("checks_provider %q is not configured: %s has no API key file", route.Provider, section)
	}
	if route.Template == "" && strings.TrimSpace(view.E2BTemplate) == "" && strings.TrimSpace(view.E2BOMPTemplate) == "" {
		return fmt.Errorf("checks_provider %q is not configured: %s has no template and checks_template is unset", route.Provider, section)
	}
	return nil
}

// ReviewChecksTemplateError reports whether a review on an omp (omp=true) or
// other runtime has a template under route. With checks_template set every
// runtime uses it. Without it an omp review provisions the provider's
// e2b_omp_template and every other runtime its e2b_template, so a view that
// sets only one of them serves only those runtimes.
func (cfg RemoteExecConfig) ReviewChecksTemplateError(route ReviewChecksRoute, omp bool) error {
	if route.Template != "" {
		return nil
	}
	view, err := cfg.ForProvider(route.Provider)
	if err != nil {
		return err
	}
	section := view.reviewChecksSection()
	if omp && strings.TrimSpace(view.E2BOMPTemplate) == "" {
		return fmt.Errorf("checks_provider %q has no template for an omp review: %s sets no e2b_omp_template, the template omp reviews provision, and checks_template is unset; set either", route.Provider, section)
	}
	if !omp && strings.TrimSpace(view.E2BTemplate) == "" {
		return fmt.Errorf("checks_provider %q has no template for a non-omp review: %s sets only e2b_omp_template, which only omp reviews provision, and checks_template is unset; set e2b_template or checks_template", route.Provider, section)
	}
	return nil
}

func (cfg RemoteExecConfig) reviewChecksSection() string {
	if IsSandboxdProvider(cfg.Provider) {
		return SandboxdProviderSection(cfg.Provider)
	}
	return "[remote_exec]"
}

// ProviderCredentialGatewayURL is the gateway origin guests of this view's
// provider dial.
func (cfg RemoteExecConfig) ProviderCredentialGatewayURL() string {
	if sandboxd := cfg.SandboxdProviders[cfg.Provider]; IsSandboxdProvider(cfg.Provider) && sandboxd != nil && sandboxd.CredentialGatewayURL != "" {
		return sandboxd.CredentialGatewayURL
	}
	return cfg.CredentialGatewayURL
}

// CredentialGatewayURLs lists every origin the one gateway listener serves,
// [remote_exec].credential_gateway_url first, then each sandboxd provider's
// credential_gateway_url in provider order. The listener's certificate covers
// exactly these hosts. The list does not depend on which provider a job uses,
// so every job shares one listener.
func (cfg RemoteExecConfig) CredentialGatewayURLs() []string {
	urls := []string{cfg.CredentialGatewayURL}
	for _, name := range remoteExecSandboxdProviders {
		sandboxd := cfg.SandboxdProviders[name]
		if sandboxd != nil && sandboxd.CredentialGatewayURL != "" && !slices.Contains(urls, sandboxd.CredentialGatewayURL) {
			urls = append(urls, sandboxd.CredentialGatewayURL)
		}
	}
	return urls
}

// parseSandboxdProviderKey parses one key of the provider section, named in
// errors as it was written.
func parseSandboxdProviderKey(sandboxd *SandboxdProviderConfig, section, key, value string) error {
	if key == "max_concurrent" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse %s.max_concurrent: expected an integer: %w", section, err)
		}
		sandboxd.MaxConcurrent = parsed
		return nil
	}
	fields := map[string]*string{
		"api_key_file": &sandboxd.APIKeyFile, "template": &sandboxd.Template, "omp_template": &sandboxd.OMPTemplate,
		"base_url": &sandboxd.BaseURL, "envd_base_url": &sandboxd.EnvdBaseURL,
		OMPLinuxFileKey(execbackend.GuestArchARM64): &sandboxd.OMPLinuxARM64File, OMPLinuxFileKey(execbackend.GuestArchAMD64): &sandboxd.OMPLinuxAMD64File,
		"credential_gateway_url": &sandboxd.CredentialGatewayURL,
	}
	field, ok := fields[key]
	if !ok {
		// Ignore unknown keys so the section remains forward-compatible.
		return nil
	}
	parsed, err := parseConfigString(value)
	if err != nil {
		return fmt.Errorf("parse %s.%s: %w", section, key, err)
	}
	*field = strings.TrimSpace(parsed)
	return nil
}

// sandboxdProviderSectionName maps a config.toml section to the sandboxd
// provider it declares: [remote_exec.sandboxd] and
// [remote_exec.sandboxd-linux] (TOML also allows the quoted spelling
// [remote_exec."sandboxd-linux"]).
func sandboxdProviderSectionName(section string) (string, bool) {
	switch remoteExecSubsection(section) {
	case RemoteExecProviderSandboxd:
		return RemoteExecProviderSandboxd, true
	case RemoteExecProviderSandboxdLinux:
		return RemoteExecProviderSandboxdLinux, true
	}
	return "", false
}

// remoteExecSubsection returns the key under remote_exec in a section header,
// with TOML basic or literal quotes removed, so [remote_exec.x],
// [remote_exec."x"] and [remote_exec.'x'] all name x. Any other header yields "".
func remoteExecSubsection(section string) string {
	key, ok := strings.CutPrefix(section, "remote_exec.")
	if !ok {
		return ""
	}
	key = strings.TrimSpace(key)
	if len(key) >= 2 && (key[0] == '"' && key[len(key)-1] == '"' || key[0] == '\'' && key[len(key)-1] == '\'') {
		key = key[1 : len(key)-1]
	}
	return key
}

// GuestArch is the guest architecture this section's OMP upload targets and
// the configured file. The key that names the file declares the
// architecture; empty means no OMP file is configured.
func (sandboxd SandboxdProviderConfig) GuestArch() (arch, file string) {
	switch {
	case sandboxd.OMPLinuxAMD64File != "":
		return execbackend.GuestArchAMD64, sandboxd.OMPLinuxAMD64File
	case sandboxd.OMPLinuxARM64File != "":
		return execbackend.GuestArchARM64, sandboxd.OMPLinuxARM64File
	}
	return "", ""
}

// validate checks one sandboxd provider section without reading its API key
// or OMP executable, so a declared sandboxd provider cannot break a home that
// never uses it. ValidateOMPExecutable checks the executable itself; doctor
// and every request for the provider run it.
func (sandboxd SandboxdProviderConfig) validate(cfg RemoteExecConfig, provider string) error {
	section := SandboxdProviderSection(provider)
	if sandboxd.APIKeyFile == "" || !filepath.IsAbs(sandboxd.APIKeyFile) {
		return fmt.Errorf("%s.api_key_file must be an absolute path", section)
	}
	if sandboxd.Template == "" {
		return fmt.Errorf("%s.template is required", section)
	}
	for _, key := range []string{"base_url", "envd_base_url"} {
		value := sandboxd.BaseURL
		if key == "envd_base_url" {
			value = sandboxd.EnvdBaseURL
		}
		if err := validateSandboxdOrigin(value); err != nil {
			return fmt.Errorf("%s.%s %w", section, key, err)
		}
	}
	if sandboxd.OMPLinuxARM64File != "" && sandboxd.OMPLinuxAMD64File != "" {
		return fmt.Errorf("%s sets both omp_linux_arm64_file and omp_linux_amd64_file: set only the one matching the provider's guest architecture", section)
	}
	if arch, file := sandboxd.GuestArch(); file != "" && !filepath.IsAbs(file) {
		return fmt.Errorf("%s.%s must be an absolute path", section, OMPLinuxFileKey(arch))
	}
	// max_concurrent is an optional ceiling: the gateway reports the real
	// capacity. Absent or 0 adds no ceiling; negative is a typo, not a policy.
	if sandboxd.MaxConcurrent < 0 {
		return fmt.Errorf("%s.max_concurrent must not be negative: it is an optional ceiling on the capacity sandboxd reports (0 or absent means no extra ceiling)", section)
	}
	if sandboxd.CredentialGatewayURL != "" {
		if strings.TrimSpace(cfg.CredentialGatewayListen) == "" {
			return fmt.Errorf("%s.credential_gateway_url requires [remote_exec].credential_gateway_listen and credential_gateway_url", section)
		}
		if err := (RemoteExecConfig{CredentialGatewayListen: cfg.CredentialGatewayListen, CredentialGatewayURL: sandboxd.CredentialGatewayURL}).ValidateCredentialGateway(); err != nil {
			return fmt.Errorf("%s.credential_gateway_url: %w", section, err)
		}
	}
	return nil
}

// validateSandboxdOrigin requires an HTTPS origin, or a plain-HTTP origin on a
// loopback address: a gateway on the daemon's own host (the Linux Firecracker
// gateway listens on 127.0.0.1) has no network hop to protect.
func validateSandboxdOrigin(value string) error {
	const shape = "must be an HTTPS origin (or HTTP on a loopback address) without path, query, credentials, or fragment"
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New(shape)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New(shape)
}

// ValidateCredentialGateway validates only transport coordinates. The mTLS CA
// and all job credentials are generated in memory by credgw.
func (cfg RemoteExecConfig) ValidateCredentialGateway() error {
	listenAddress := strings.TrimSpace(cfg.CredentialGatewayListen)
	advertiseURL := strings.TrimSpace(cfg.CredentialGatewayURL)
	if listenAddress == "" && advertiseURL == "" {
		return nil
	}
	if listenAddress == "" || advertiseURL == "" {
		return fmt.Errorf("[remote_exec].credential_gateway_listen and [remote_exec].credential_gateway_url must be configured together")
	}
	_, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return fmt.Errorf("invalid [remote_exec].credential_gateway_listen: %w", err)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return fmt.Errorf("invalid [remote_exec].credential_gateway_listen: require a non-zero TCP port")
	}
	parsed, err := url.Parse(advertiseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid [remote_exec].credential_gateway_url: require an HTTPS origin without path, query, credentials, or fragment")
	}
	return nil
}

// GITMOOT-IMPL: LoadE2BAPIKey follows secret-mount symlinks, then requires a
// regular credential file with no group or other permissions. Callers must keep
// the returned value in memory only and must never render it.
func (cfg RemoteExecConfig) LoadE2BAPIKey() (string, error) {
	path := strings.TrimSpace(cfg.E2BAPIKeyFile)
	if path == "" {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file is required when backend=remote")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file must be an absolute path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read [remote_exec].e2b_api_key_file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file %s must be a regular file", path)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file %s has permissions %04o; group and other permissions must be zero", path, mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read [remote_exec].e2b_api_key_file %s: %w", path, err)
	}
	apiKey := strings.TrimSpace(string(data))
	if apiKey == "" {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file %s is empty", path)
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return "", fmt.Errorf("[remote_exec].e2b_api_key_file %s must contain exactly one key", path)
	}
	return apiKey, nil
}

func validateE2BDomain(domain string) error {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	if len(domain) > 253 || strings.Trim(domain, ".") != domain {
		return fmt.Errorf("must be a DNS name without a trailing dot")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("must be a DNS name")
		}
		for _, char := range label {
			if char != '-' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
				return fmt.Errorf("must be an ASCII DNS name")
			}
		}
	}
	return nil
}

func validateE2BBaseURL(base string) error {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an absolute HTTP(S) URL without query or fragment")
	}
	return nil
}

// LocalIdentity returns nil unless the operator configured both identity
// fields. No account name or numeric identity is inferred from the host.
func (cfg RemoteExecConfig) LocalIdentity() *execbackend.LocalIdentity {
	if cfg.LocalUID == nil || cfg.LocalGID == nil {
		return nil
	}
	return &execbackend.LocalIdentity{UID: *cfg.LocalUID, GID: *cfg.LocalGID}
}
