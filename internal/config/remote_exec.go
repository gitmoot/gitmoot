package config

import (
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
	E2BBaseURL    string
	E2BDomain     string
	// CredentialGatewayListen is the daemon bind address; URL is the HTTPS
	// origin reachable from a sandbox. They are configured together and are
	// used only for opt-in broker material, never for the provider control key.
	CredentialGatewayListen string
	CredentialGatewayURL    string
}

// DefaultRemoteExecConfig preserves today's behaviour: the local backend.
func DefaultRemoteExecConfig() RemoteExecConfig {
	return RemoteExecConfig{Backend: string(execbackend.Local)}
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
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "remote_exec"
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
		value = strings.TrimSpace(value)
		switch key {
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
		case "e2b_api_key_file", "e2b_template", "e2b_base_url", "e2b_domain", "credential_gateway_listen", "credential_gateway_url":
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
			case "e2b_base_url":
				cfg.E2BBaseURL = parsed
			case "e2b_domain":
				cfg.E2BDomain = parsed
			case "credential_gateway_listen":
				cfg.CredentialGatewayListen = parsed
			case "credential_gateway_url":
				cfg.CredentialGatewayURL = parsed
			}
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
	if err := cfg.ValidateCredentialGateway(); err != nil {
		return err
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
