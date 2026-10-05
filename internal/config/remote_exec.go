package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
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
	// E2BEnvdBaseURL and OMPLinuxARM64File are set only in the sandboxd view
	// from [remote_exec.sandboxd]: one HTTPS envd origin with sandbox routing
	// headers in place of wildcard hosts, and the host-side Linux ARM64 OMP
	// executable.
	E2BEnvdBaseURL    string
	OMPLinuxARM64File string
	// Sandboxd is the optional [remote_exec.sandboxd] section. It declares a
	// local sandboxd provider beside E2B; a job runs there only when its
	// payload names it (exec_provider = "sandboxd").
	Sandboxd *SandboxdProviderConfig
	// Deprecations lists deprecated spellings this config was loaded with,
	// each naming its replacement. The daemon prints them once at start.
	Deprecations []string
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

// Remote execution providers. E2B is the default for every remote job; the
// sandboxd provider is used only by a job that opts in explicitly.
const (
	RemoteExecProviderE2B      = "e2b"
	RemoteExecProviderSandboxd = "sandboxd"
)

// remoteExecProviderMacAlias is the provider's pre-rename name. Production
// configs and queued payloads still carry it, so it is accepted for one
// release, with a deprecation warning, and then removed.
const remoteExecProviderMacAlias = "mac"

// NormalizeRemoteExecProvider maps a provider name to its canonical spelling.
// deprecatedAlias reports that name was the "mac" alias for "sandboxd". Every
// other name, including unknown ones, is returned trimmed and unchanged.
func NormalizeRemoteExecProvider(name string) (canonical string, deprecatedAlias bool) {
	name = strings.TrimSpace(name)
	if name == remoteExecProviderMacAlias {
		return RemoteExecProviderSandboxd, true
	}
	return name, false
}

// RemoteExecProviderMacDeprecation is the one warning every "mac" entry point
// prints, completed with where the alias appeared.
func RemoteExecProviderMacDeprecation(where string) string {
	return fmt.Sprintf("%s: remote execution provider %q is deprecated and will be removed in the next release; use %q", where, remoteExecProviderMacAlias, RemoteExecProviderSandboxd)
}

// SandboxdProviderConfig is the [remote_exec.sandboxd] section: a sandboxd
// gateway, an E2B-compatible API, as an opt-in remote provider. It is
// capacity-limited on-prem compute with no dollar cost: concurrency follows the
// capacity the gateway reports at GET /sandboxd/capacity.
type SandboxdProviderConfig struct {
	APIKeyFile        string
	Template          string
	OMPTemplate       string
	BaseURL           string
	EnvdBaseURL       string
	OMPLinuxARM64File string
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
	current, provider := false, false
	providerSection, sawSandboxd, sawMac := "", false, false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			current = section == "remote_exec"
			provider = section == "remote_exec.sandboxd" || section == "remote_exec.mac"
			if provider {
				providerSection = "[" + section + "]"
				if section == "remote_exec.mac" {
					sawMac = true
				} else {
					sawSandboxd = true
				}
				if sawMac && sawSandboxd {
					return RemoteExecConfig{}, errors.New("config.toml declares both [remote_exec.sandboxd] and its deprecated alias [remote_exec.mac]: keep only [remote_exec.sandboxd]")
				}
				if cfg.Sandboxd == nil {
					cfg.Sandboxd = &SandboxdProviderConfig{}
				}
			}
			continue
		}
		if !current && !provider {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if provider {
			if err := parseSandboxdProviderKey(cfg.Sandboxd, providerSection, key, value); err != nil {
				return RemoteExecConfig{}, err
			}
			continue
		}
		switch key {
		case "provider", "e2b_envd_base_url", "omp_linux_arm64_file":
			// These once selected the sandboxd provider for the whole home. The
			// provider is now chosen per job, so a home-wide value is refused
			// rather than ignored.
			return RemoteExecConfig{}, fmt.Errorf("[remote_exec].%s is not supported: declare the sandboxd provider in [remote_exec.sandboxd] and opt a job in with --exec-provider sandboxd", key)
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
	if sawMac {
		cfg.Deprecations = append(cfg.Deprecations, "config.toml: section [remote_exec.mac] is deprecated and will be removed in the next release; rename it to [remote_exec.sandboxd] (same keys)")
	}
	return cfg, nil
}

func validateRemoteExecConfig(cfg RemoteExecConfig) error {
	if err := cfg.ExecBackendCost.Validate(); err != nil {
		return err
	}
	if cfg.Sandboxd != nil {
		if err := cfg.Sandboxd.validate(cfg); err != nil {
			return err
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
	if cfg.Provider == RemoteExecProviderSandboxd {
		if cfg.Sandboxd == nil {
			return fmt.Errorf("the sandboxd remote provider is not configured: add a [remote_exec.sandboxd] section")
		}
		if err := cfg.Sandboxd.validate(cfg); err != nil {
			return err
		}
	}
	if err := cfg.ValidateCredentialGateway(); err != nil {
		return err
	}
	return nil
}

// ForProvider returns the configuration one remote job runs with. An empty
// name is the default, cloud E2B. "sandboxd" (or its deprecated alias "mac")
// substitutes [remote_exec.sandboxd] for the E2B endpoint, credentials,
// templates and caps; the gateway listener and the rest of the section are
// shared. Any other name, or "sandboxd" without its section, is refused rather
// than falling back to E2B.
func (cfg RemoteExecConfig) ForProvider(name string) (RemoteExecConfig, error) {
	canonical, _ := NormalizeRemoteExecProvider(name)
	switch canonical {
	case "", RemoteExecProviderE2B:
		cfg.Provider = RemoteExecProviderE2B
		return cfg, nil
	case RemoteExecProviderSandboxd:
		if cfg.Sandboxd == nil {
			return RemoteExecConfig{}, fmt.Errorf("remote execution provider %q is not configured: add a [remote_exec.sandboxd] section to config.toml", RemoteExecProviderSandboxd)
		}
		sandboxd := *cfg.Sandboxd
		cfg.Provider = RemoteExecProviderSandboxd
		cfg.E2BAPIKeyFile = sandboxd.APIKeyFile
		cfg.E2BTemplate = sandboxd.Template
		cfg.E2BOMPTemplate = sandboxd.OMPTemplate
		cfg.E2BBaseURL = sandboxd.BaseURL
		cfg.E2BDomain = ""
		cfg.E2BEnvdBaseURL = sandboxd.EnvdBaseURL
		cfg.OMPLinuxARM64File = sandboxd.OMPLinuxARM64File
		// The ceiling only: the effective cap is read from the gateway's
		// reported capacity at admission (#30).
		cfg.ExecBackendCost = ExecBackendCostConfig{MaxConcurrent: sandboxd.MaxConcurrent}
		return cfg, nil
	default:
		return RemoteExecConfig{}, fmt.Errorf("unknown remote execution provider %q: allowed providers are %q and %q", strings.TrimSpace(name), RemoteExecProviderE2B, RemoteExecProviderSandboxd)
	}
}

// ValidateProvider preflights the named provider for a job that requests it,
// including its API key file, without any network call.
func (cfg RemoteExecConfig) ValidateProvider(name string) error {
	view, err := cfg.ForProvider(name)
	if err != nil {
		return err
	}
	return view.ValidateE2BProvider()
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
	if cfg.Provider == RemoteExecProviderSandboxd {
		return "[remote_exec.sandboxd]"
	}
	return "[remote_exec]"
}

// ProviderCredentialGatewayURL is the gateway origin guests of this view's
// provider dial.
func (cfg RemoteExecConfig) ProviderCredentialGatewayURL() string {
	if cfg.Provider == RemoteExecProviderSandboxd && cfg.Sandboxd != nil && cfg.Sandboxd.CredentialGatewayURL != "" {
		return cfg.Sandboxd.CredentialGatewayURL
	}
	return cfg.CredentialGatewayURL
}

// CredentialGatewayURLs lists every origin the one gateway listener serves,
// [remote_exec].credential_gateway_url first. The list does not depend on which
// provider a job uses, so every job shares one listener.
func (cfg RemoteExecConfig) CredentialGatewayURLs() []string {
	urls := []string{cfg.CredentialGatewayURL}
	if cfg.Sandboxd != nil && cfg.Sandboxd.CredentialGatewayURL != "" && cfg.Sandboxd.CredentialGatewayURL != cfg.CredentialGatewayURL {
		urls = append(urls, cfg.Sandboxd.CredentialGatewayURL)
	}
	return urls
}

// parseSandboxdProviderKey parses one key of the provider section, named in
// errors as it was written ([remote_exec.sandboxd] or the deprecated
// [remote_exec.mac]).
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
		"omp_linux_arm64_file": &sandboxd.OMPLinuxARM64File, "credential_gateway_url": &sandboxd.CredentialGatewayURL,
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

// validate checks [remote_exec.sandboxd] without reading its API key, so a
// declared sandboxd provider cannot break a home that never uses it.
func (sandboxd SandboxdProviderConfig) validate(cfg RemoteExecConfig) error {
	if sandboxd.APIKeyFile == "" || !filepath.IsAbs(sandboxd.APIKeyFile) {
		return fmt.Errorf("[remote_exec.sandboxd].api_key_file must be an absolute path")
	}
	if sandboxd.Template == "" {
		return fmt.Errorf("[remote_exec.sandboxd].template is required")
	}
	for key, value := range map[string]string{"base_url": sandboxd.BaseURL, "envd_base_url": sandboxd.EnvdBaseURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("[remote_exec.sandboxd].%s must be an HTTPS origin without path, query, credentials, or fragment", key)
		}
	}
	if sandboxd.OMPLinuxARM64File != "" && !filepath.IsAbs(sandboxd.OMPLinuxARM64File) {
		return fmt.Errorf("[remote_exec.sandboxd].omp_linux_arm64_file must be an absolute path")
	}
	// max_concurrent is an optional ceiling: the gateway reports the real
	// capacity. Absent or 0 adds no ceiling; negative is a typo, not a policy.
	if sandboxd.MaxConcurrent < 0 {
		return fmt.Errorf("[remote_exec.sandboxd].max_concurrent must not be negative: it is an optional ceiling on the capacity sandboxd reports (0 or absent means no extra ceiling)")
	}
	if sandboxd.CredentialGatewayURL != "" {
		if strings.TrimSpace(cfg.CredentialGatewayListen) == "" {
			return fmt.Errorf("[remote_exec.sandboxd].credential_gateway_url requires [remote_exec].credential_gateway_listen and credential_gateway_url")
		}
		if err := (RemoteExecConfig{CredentialGatewayListen: cfg.CredentialGatewayListen, CredentialGatewayURL: sandboxd.CredentialGatewayURL}).ValidateCredentialGateway(); err != nil {
			return fmt.Errorf("[remote_exec.sandboxd].credential_gateway_url: %w", err)
		}
	}
	return nil
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
