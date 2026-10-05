package config

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

func remoteExecTestPaths(t *testing.T, content string) Paths {
	t.Helper()
	dir := t.TempDir()
	paths := PathsForHome(dir)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestLoadRemoteExecConfigDefaultsToLocal(t *testing.T) {
	// A config file with no [remote_exec] section at all.
	paths := remoteExecTestPaths(t, "[workflow]\nimplement_base = \"main\"\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	if cfg.Backend != "local" {
		t.Fatalf("Backend = %q, want the local default", cfg.Backend)
	}
	if cfg.Provider != "e2b" {
		t.Fatalf("default remote provider = %q, want cloud E2B", cfg.Provider)
	}
	if cfg.LocalIdentity() != nil || cfg.LocalRoot != "" {
		t.Fatalf("default local privilege config = uid %v gid %v root %q, want unset", cfg.LocalUID, cfg.LocalGID, cfg.LocalRoot)
	}
}

func TestLoadRemoteExecConfigExplicitImplementedBackend(t *testing.T) {
	for _, backend := range []string{"local", "remote"} {
		t.Run(backend, func(t *testing.T) {
			content := "[remote_exec]\nbackend = \"" + backend + "\"\n"
			if backend == "remote" {
				keyFile := filepath.Join(t.TempDir(), "e2b-api-key")
				if err := os.WriteFile(keyFile, []byte("api-key-GITMOOT-IMPL\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				content += fmt.Sprintf("e2b_api_key_file = %q\ne2b_template = \"template-test\"\ne2b_omp_template = \"omp-template-test\"\ne2b_base_url = \"https://control.example\"\ne2b_domain = \"sandboxes.example\"\ncredential_gateway_listen = \"127.0.0.1:8443\"\ncredential_gateway_url = \"https://broker.example:8443\"\n", keyFile)
			}
			paths := remoteExecTestPaths(t, content)
			cfg, err := LoadRemoteExecConfig(paths)
			if err != nil {
				t.Fatalf("LoadRemoteExecConfig: %v", err)
			}
			if cfg.Backend != backend {
				t.Fatalf("Backend = %q, want %q", cfg.Backend, backend)
			}
			if backend == "remote" && (cfg.E2BTemplate != "template-test" || cfg.E2BOMPTemplate != "omp-template-test" || cfg.E2BBaseURL != "https://control.example" || cfg.E2BDomain != "sandboxes.example" || cfg.CredentialGatewayListen != "127.0.0.1:8443" || cfg.CredentialGatewayURL != "https://broker.example:8443") {
				t.Fatalf("remote provider config = %+v", cfg)
			}
		})
	}
}

// The sandboxd provider is declared beside E2B and selected only per job: the
// home's remote view stays E2B, the sandboxd view swaps in every
// provider-specific value, and an unknown or undeclared provider is refused
// instead of quietly running on E2B.
func TestRemoteExecSandboxdProviderIsAnOptInViewBesideE2B(t *testing.T) {
	keyDir := t.TempDir()
	e2bKey := filepath.Join(keyDir, "e2b-api-key")
	sandboxdKey := filepath.Join(keyDir, "sandboxd-api-key")
	for _, file := range []string{e2bKey, sandboxdKey} {
		if err := os.WriteFile(file, []byte("private-"+filepath.Base(file)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ompARM64 := writeTestLinuxELF(t, keyDir, "omp-linux-arm64", elfMachineAARCH64)
	e2bSection := fmt.Sprintf(`[remote_exec]
backend = "remote"
e2b_api_key_file = %q
e2b_template = "base"
e2b_omp_template = "omp-x86"
credential_gateway_listen = "0.0.0.0:8443"
credential_gateway_url = "https://203.0.113.7:8443"
cost_max_reserved_usd = 9
cost_per_attempt_usd = 4.5
cost_max_concurrent = 2
`, e2bKey)
	sandboxdSection := fmt.Sprintf(`
[remote_exec.sandboxd]
api_key_file = %q
template = "review-arm64"
omp_template = "review-arm64"
base_url = "https://sandboxd.example:8443"
envd_base_url = "https://sandboxd.example:8443"
omp_linux_arm64_file = %q
credential_gateway_url = "https://192.168.128.1:43181"
max_concurrent = 1
`, sandboxdKey, ompARM64)
	cfg, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2bSection+sandboxdSection))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != RemoteExecProviderE2B || cfg.E2BTemplate != "base" || cfg.E2BEnvdBaseURL != "" {
		t.Fatalf("declaring sandboxd changed the home's default remote view: %+v", cfg)
	}
	defaultView, err := cfg.ForProvider("")
	if err != nil || defaultView.Provider != RemoteExecProviderE2B || defaultView.E2BAPIKeyFile != e2bKey || defaultView.ExecBackendCost.PerAttemptUSD != 4.5 {
		t.Fatalf("default provider view = %+v, %v; want E2B", defaultView, err)
	}
	sandboxd, err := cfg.ForProvider(RemoteExecProviderSandboxd)
	if err != nil {
		t.Fatal(err)
	}
	if sandboxd.Provider != RemoteExecProviderSandboxd || sandboxd.E2BAPIKeyFile != sandboxdKey || sandboxd.E2BBaseURL != "https://sandboxd.example:8443" ||
		sandboxd.E2BEnvdBaseURL != "https://sandboxd.example:8443" || sandboxd.E2BTemplate != "review-arm64" || sandboxd.E2BOMPTemplate != "review-arm64" ||
		sandboxd.OMPLinuxFile != ompARM64 || sandboxd.OMPGuestArch != "arm64" || sandboxd.E2BDomain != "" ||
		sandboxd.ExecBackendCost != (ExecBackendCostConfig{MaxConcurrent: 1}) {
		t.Fatalf("sandboxd view kept E2B values: %+v", sandboxd)
	}
	if sandboxd.ProviderCredentialGatewayURL() != "https://192.168.128.1:43181" || defaultView.ProviderCredentialGatewayURL() != "https://203.0.113.7:8443" {
		t.Fatalf("gateway origins: sandboxd %q e2b %q", sandboxd.ProviderCredentialGatewayURL(), defaultView.ProviderCredentialGatewayURL())
	}
	if err := cfg.ValidateProvider(RemoteExecProviderSandboxd); err != nil {
		t.Fatalf("configured sandboxd provider refused: %v", err)
	}
	if len(cfg.Deprecations) != 0 {
		t.Fatalf("[remote_exec.sandboxd] reported deprecations %q", cfg.Deprecations)
	}
	err = cfg.ValidateProvider("mac-studio")
	if err == nil || !strings.Contains(err.Error(), "unknown remote execution provider") ||
		!strings.Contains(err.Error(), `"sandboxd"`) || strings.Contains(err.Error(), `"mac"`) {
		t.Fatalf("unknown provider = %v; want a refusal listing e2b and sandboxd, not mac", err)
	}

	e2bOnly, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2bSection))
	if err != nil {
		t.Fatal(err)
	}
	if err := e2bOnly.ValidateProvider(RemoteExecProviderSandboxd); err == nil || !strings.Contains(err.Error(), "[remote_exec.sandboxd]") {
		t.Fatalf("undeclared sandboxd provider accepted: %v", err)
	}

	for name, broken := range map[string]string{
		"negative ceiling":       strings.Replace(sandboxdSection, "max_concurrent = 1", "max_concurrent = -1", 1),
		"plain HTTP control":     strings.Replace(sandboxdSection, `base_url = "https://sandboxd.example:8443"`, `base_url = "http://sandboxd.example:8443"`, 1),
		"envd with a path":       strings.Replace(sandboxdSection, `envd_base_url = "https://sandboxd.example:8443"`, `envd_base_url = "https://sandboxd.example:8443/envd"`, 1),
		"relative ARM64 runtime": strings.Replace(sandboxdSection, fmt.Sprintf("%q", ompARM64), `"omp-linux-arm64"`, 1),
	} {
		if _, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2bSection+broken)); err == nil {
			t.Errorf("%s: invalid [remote_exec.sandboxd] accepted", name)
		}
	}
	// max_concurrent is an optional ceiling: absent or 0 adds none.
	for name, section := range map[string]string{
		"absent ceiling": strings.Replace(sandboxdSection, "max_concurrent = 1\n", "", 1),
		"zero ceiling":   strings.Replace(sandboxdSection, "max_concurrent = 1", "max_concurrent = 0", 1),
	} {
		loaded, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2bSection+section))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		view, err := loaded.ForProvider(RemoteExecProviderSandboxd)
		if err != nil || view.ExecBackendCost.MaxConcurrent != 0 || view.ValidateProvider(RemoteExecProviderSandboxd) != nil {
			t.Fatalf("%s: view ceiling %d, %v; want a valid sandboxd view without a ceiling", name, view.ExecBackendCost.MaxConcurrent, err)
		}
	}
	if _, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2bSection+"provider = \"sandboxd\"\n")); err == nil || !strings.Contains(err.Error(), "[remote_exec.sandboxd]") {
		t.Fatalf("home-wide provider selection accepted: %v", err)
	}
}

// The provider was renamed from "mac". Production config still declares
// [remote_exec.mac], so for one release it loads as [remote_exec.sandboxd]
// with one deprecation naming the new section, and "mac" resolves to the
// sandboxd view. Declaring both spellings is ambiguous and refused.
func TestRemoteExecMacSectionIsADeprecatedSandboxdAlias(t *testing.T) {
	key := filepath.Join(t.TempDir(), "sandboxd-api-key")
	if err := os.WriteFile(key, []byte("private-sandboxd-api-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("\napi_key_file = %q\ntemplate = \"review-arm64\"\nbase_url = \"https://sandboxd.example:8443\"\nenvd_base_url = \"https://sandboxd.example:8443\"\nmax_concurrent = 2\n", key)
	cfg, err := LoadRemoteExecConfig(remoteExecTestPaths(t, "[remote_exec.mac]"+body))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Deprecations) != 1 || !strings.Contains(cfg.Deprecations[0], "[remote_exec.mac]") || !strings.Contains(cfg.Deprecations[0], "[remote_exec.sandboxd]") {
		t.Fatalf("deprecations = %q; want one naming [remote_exec.sandboxd]", cfg.Deprecations)
	}
	for _, name := range []string{"mac", "sandboxd"} {
		view, err := cfg.ForProvider(name)
		if err != nil || view.Provider != RemoteExecProviderSandboxd || view.E2BTemplate != "review-arm64" || view.ExecBackendCost.MaxConcurrent != 2 {
			t.Fatalf("ForProvider(%q) = %+v, %v; want the sandboxd view", name, view, err)
		}
	}
	if canonical, alias := NormalizeRemoteExecProvider(" mac "); canonical != RemoteExecProviderSandboxd || !alias {
		t.Fatalf("NormalizeRemoteExecProvider(mac) = %q, %v", canonical, alias)
	}
	if canonical, alias := NormalizeRemoteExecProvider("sandboxd"); canonical != RemoteExecProviderSandboxd || alias {
		t.Fatalf("NormalizeRemoteExecProvider(sandboxd) = %q, %v", canonical, alias)
	}
	_, err = LoadRemoteExecConfig(remoteExecTestPaths(t, "[remote_exec.sandboxd]"+body+"\n[remote_exec.mac]"+body))
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("both sections loaded: %v", err)
	}
}

const (
	elfMachineAARCH64 = 183
	elfMachineX86_64  = 62
)

// writeTestLinuxELF writes a minimal executable ELF64 header for machine; the
// OMP verifier reads it and never runs it.
func writeTestLinuxELF(t *testing.T, dir, name string, machine uint16) string {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4], header[5], header[6], header[7] = 2, 1, 1, 3
	binary.LittleEndian.PutUint16(header[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(header[18:], machine)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[52:], 64)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, header, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A home declares the Mac gateway and the Linux Firecracker gateway side by
// side. Each provider is its own view: endpoint (plain HTTP on loopback for
// the Linux gateway), key, templates, ceiling, gateway origin, and an OMP
// upload whose key declares the guest architecture. The listener advertises
// both gateway origins, and a binary for the wrong architecture is refused
// before any review runs.
func TestRemoteExecLoadsSandboxdAndSandboxdLinuxProviders(t *testing.T) {
	dir := t.TempDir()
	keys := map[string]string{}
	for _, name := range []string{"e2b", "sandboxd", "sandboxd-linux"} {
		keys[name] = filepath.Join(dir, name+"-api-key")
		if err := os.WriteFile(keys[name], []byte("private-"+name+"-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ompARM64 := writeTestLinuxELF(t, dir, "omp-linux-arm64", elfMachineAARCH64)
	ompAMD64 := writeTestLinuxELF(t, dir, "omp-linux-amd64", elfMachineX86_64)
	e2b := fmt.Sprintf(`[remote_exec]
backend = "local"
e2b_api_key_file = %q
e2b_template = "base"
credential_gateway_listen = "0.0.0.0:8443"
credential_gateway_url = "https://203.0.113.7:8443"
`, keys["e2b"])
	mac := fmt.Sprintf(`
[remote_exec.sandboxd]
api_key_file = %q
template = "review-arm64"
omp_template = "review-arm64"
base_url = "https://sandboxd.example:8443"
envd_base_url = "https://sandboxd.example:8443"
omp_linux_arm64_file = %q
credential_gateway_url = "https://192.168.128.1:43181"
max_concurrent = 1
`, keys["sandboxd"], ompARM64)
	linux := fmt.Sprintf(`
[remote_exec."sandboxd-linux"]
api_key_file = %q
template = "review-amd64"
omp_template = "review-amd64"
base_url = "http://127.0.0.1:43190"
envd_base_url = "http://127.0.0.1:43190"
omp_linux_amd64_file = %q
credential_gateway_url = "https://10.0.2.2:8443"
max_concurrent = 2
`, keys["sandboxd-linux"], ompAMD64)
	cfg, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2b+mac+linux))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		provider, key, template, base, arch, omp, gateway string
		ceiling                                           int
	}{
		{"sandboxd", keys["sandboxd"], "review-arm64", "https://sandboxd.example:8443", "arm64", ompARM64, "https://192.168.128.1:43181", 1},
		{"sandboxd-linux", keys["sandboxd-linux"], "review-amd64", "http://127.0.0.1:43190", "amd64", ompAMD64, "https://10.0.2.2:8443", 2},
	} {
		view, err := cfg.ForProvider(want.provider)
		if err != nil {
			t.Fatalf("ForProvider(%q): %v", want.provider, err)
		}
		if view.Provider != want.provider || view.E2BAPIKeyFile != want.key || view.E2BTemplate != want.template || view.E2BOMPTemplate != want.template ||
			view.E2BBaseURL != want.base || view.E2BEnvdBaseURL != want.base || view.OMPGuestArch != want.arch || view.OMPLinuxFile != want.omp ||
			view.ProviderCredentialGatewayURL() != want.gateway || view.ExecBackendCost != (ExecBackendCostConfig{MaxConcurrent: want.ceiling}) {
			t.Fatalf("ForProvider(%q) = %+v; want %+v", want.provider, view, want)
		}
		if err := cfg.ValidateProvider(want.provider); err != nil {
			t.Fatalf("configured provider %q refused: %v", want.provider, err)
		}
	}
	if got, want := cfg.CredentialGatewayURLs(), []string{"https://203.0.113.7:8443", "https://192.168.128.1:43181", "https://10.0.2.2:8443"}; !slices.Equal(got, want) {
		t.Fatalf("CredentialGatewayURLs = %q, want %q", got, want)
	}
	if len(cfg.Deprecations) != 0 {
		t.Fatalf("deprecations = %q", cfg.Deprecations)
	}

	// The unquoted section spelling declares the same provider.
	unquoted, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2b+strings.Replace(linux, `[remote_exec."sandboxd-linux"]`, "[remote_exec.sandboxd-linux]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if view, err := unquoted.ForProvider("sandboxd-linux"); err != nil || view.E2BTemplate != "review-amd64" {
		t.Fatalf("[remote_exec.sandboxd-linux] = %+v, %v", view, err)
	}
	// Declaring only the Mac gateway leaves sandboxd-linux undeclared.
	macOnly, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2b+mac))
	if err != nil {
		t.Fatal(err)
	}
	if err := macOnly.ValidateProvider("sandboxd-linux"); err == nil || !strings.Contains(err.Error(), "[remote_exec.sandboxd-linux]") {
		t.Fatalf("undeclared sandboxd-linux accepted: %v", err)
	}

	// The key declares the architecture: an ARM64 binary under the AMD64 key,
	// or a missing file, is refused for that provider alone.
	for name, section := range map[string]string{
		"arm64 binary for amd64 guests": strings.Replace(linux, fmt.Sprintf("%q", ompAMD64), fmt.Sprintf("%q", ompARM64), 1),
		"missing amd64 binary":          strings.Replace(linux, fmt.Sprintf("%q", ompAMD64), fmt.Sprintf("%q", filepath.Join(dir, "absent")), 1),
	} {
		loaded, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2b+mac+section))
		if err != nil {
			t.Fatalf("%s: load: %v", name, err)
		}
		if err := loaded.ValidateProvider("sandboxd-linux"); err == nil || !strings.Contains(err.Error(), "[remote_exec.sandboxd-linux].omp_linux_amd64_file") {
			t.Fatalf("%s: ValidateProvider = %v; want a refusal naming omp_linux_amd64_file", name, err)
		}
		if err := loaded.ValidateProvider("sandboxd"); err != nil {
			t.Fatalf("%s: the Mac provider was refused too: %v", name, err)
		}
	}
	for name, broken := range map[string]string{
		"both architectures":     strings.Replace(linux, "omp_linux_amd64_file", fmt.Sprintf("omp_linux_arm64_file = %q\nomp_linux_amd64_file", ompARM64), 1),
		"plain HTTP off-host":    strings.ReplaceAll(linux, "http://127.0.0.1:43190", "http://198.51.100.4:43190"),
		"relative amd64 runtime": strings.Replace(linux, fmt.Sprintf("%q", ompAMD64), `"omp-linux-amd64"`, 1),
	} {
		if _, err := LoadRemoteExecConfig(remoteExecTestPaths(t, e2b+broken)); err == nil || !strings.Contains(err.Error(), "[remote_exec.sandboxd-linux]") {
			t.Errorf("%s: invalid [remote_exec.sandboxd-linux] = %v; want a refusal naming the section", name, err)
		}
	}
}

func TestLoadE2BAPIKeyAcceptsSecureSecretDelivery(t *testing.T) {
	const secret = "api-key-GITMOOT-IMPL"
	dir := t.TempDir()
	target := filepath.Join(dir, "e2b-api-key-target")
	if err := os.WriteFile(target, []byte(secret+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "e2b-api-key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "owner read-only regular file", path: target},
		{name: "symlinked secret mount", path: link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (RemoteExecConfig{E2BAPIKeyFile: tc.path}).LoadE2BAPIKey()
			if err != nil {
				t.Fatalf("LoadE2BAPIKey: %v", err)
			}
			if got != secret {
				t.Fatalf("LoadE2BAPIKey = %q, want configured key", got)
			}
		})
	}
}

func TestLoadRemoteExecConfigRejectsUnusableE2BCredentials(t *testing.T) {
	const secret = "api-key-must-never-be-rendered-GITMOOT-IMPL"
	writeKey := func(t *testing.T, mode os.FileMode, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "e2b-api-key")
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name    string
		keyFile func(*testing.T) string
		extra   string
		want    string
	}{
		{name: "missing key setting", want: "e2b_api_key_file is required"},
		{name: "relative key path", keyFile: func(*testing.T) string { return "relative.key" }, want: "must be an absolute path"},
		{name: "missing key file", keyFile: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }, want: "no such file"},
		{name: "permissive key mode", keyFile: func(t *testing.T) string { return writeKey(t, 0o644, secret) }, want: "group and other permissions must be zero"},
		{name: "empty key", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, " \n") }, want: "is empty"},
		{name: "multiline key", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, secret+"\nsecond") }, want: "exactly one key"},
		{name: "short key", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, "short") }, want: "at least"},
		{name: "missing template", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, secret) }, extra: "e2b_template = \"\"\n", want: "e2b_template is required"},
		{name: "invalid base URL", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, secret) }, extra: "e2b_template = \"template\"\ne2b_base_url = \"relative\"\n", want: "e2b_base_url"},
		{name: "invalid domain", keyFile: func(t *testing.T) string { return writeKey(t, 0o600, secret) }, extra: "e2b_template = \"template\"\ne2b_domain = \"https://bad.example/path\"\n", want: "e2b_domain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keyFile := ""
			if tc.keyFile != nil {
				keyFile = tc.keyFile(t)
			}
			body := "[remote_exec]\nbackend = \"remote\"\n"
			if keyFile != "" {
				body += fmt.Sprintf("e2b_api_key_file = %q\n", keyFile)
			}
			if tc.extra != "" {
				body += tc.extra
			} else {
				body += "e2b_template = \"template\"\n"
			}
			_, err := LoadRemoteExecConfig(remoteExecTestPaths(t, body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadRemoteExecConfig error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("LoadRemoteExecConfig leaked API key: %v", err)
			}
		})
	}
}

func TestValidateCredentialGatewayRequiresReachableHTTPSPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		url    string
		want   string
	}{
		{name: "valid", listen: "0.0.0.0:8443", url: "https://broker.example:8443"},
		{name: "missing URL", listen: "0.0.0.0:8443", want: "configured together"},
		{name: "missing listen", url: "https://broker.example:8443", want: "configured together"},
		{name: "zero port", listen: "0.0.0.0:0", url: "https://broker.example", want: "non-zero TCP port"},
		{name: "HTTP URL", listen: "0.0.0.0:8443", url: "http://broker.example:8443", want: "HTTPS origin"},
		{name: "URL path", listen: "0.0.0.0:8443", url: "https://broker.example:8443/path", want: "HTTPS origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (RemoteExecConfig{CredentialGatewayListen: tc.listen, CredentialGatewayURL: tc.url}).ValidateCredentialGateway()
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("ValidateCredentialGateway error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadRemoteExecConfigLocalIdentity(t *testing.T) {
	paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"local\"\nlocal_uid = 1234\nlocal_gid = 5678\nlocal_root = \"/var/tmp/gitmoot-local\"\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	identity := cfg.LocalIdentity()
	if identity == nil || identity.UID != 1234 || identity.GID != 5678 {
		t.Fatalf("LocalIdentity = %+v, want uid 1234 gid 5678", identity)
	}
	if cfg.LocalRoot != "/var/tmp/gitmoot-local" {
		t.Fatalf("LocalRoot = %q", cfg.LocalRoot)
	}
}

func TestLoadRemoteExecConfigRejectsIncompleteOrRootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "uid only", content: "local_uid = 1234\n", want: "configured together"},
		{name: "gid only", content: "local_gid = 1234\n", want: "configured together"},
		{name: "root uid", content: "local_uid = 0\nlocal_gid = 1234\n", want: "local_uid must be a non-root"},
		{name: "root gid", content: "local_uid = 1234\nlocal_gid = 0\n", want: "local_gid must be a non-root"},
		{name: "relative root", content: "local_root = \"relative\"\n", want: "must be an absolute path"},
		{name: "filesystem root", content: "local_root = \"/\"\n", want: "must not be a filesystem root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"local\"\n"+tc.content)
			_, err := LoadRemoteExecConfig(paths)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadRemoteExecConfig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadRemoteExecConfigUnknownFailsLoud(t *testing.T) {
	for _, value := range []string{"e2b", "loca"} {
		paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \""+value+"\"\n")
		_, err := LoadRemoteExecConfig(paths)
		if err == nil {
			t.Fatalf("backend %q loaded, want a loud error", value)
		}
		if !strings.Contains(err.Error(), "[remote_exec].backend") {
			t.Fatalf("backend %q error = %q, want the config key named", value, err)
		}
		if !strings.Contains(err.Error(), `"`+value+`"`) || !strings.Contains(err.Error(), "allowed: local, remote") {
			t.Fatalf("backend %q error = %q, want the value AND the allowed set", value, err)
		}
	}
}

func TestLoadRemoteExecConfigExplicitBlankFailsLoud(t *testing.T) {
	for _, value := range []string{"", "   "} {
		paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \""+value+"\"\n")
		_, err := LoadRemoteExecConfig(paths)
		if err == nil {
			t.Fatalf("explicit blank backend %q loaded, want a loud error", value)
		}
		if !strings.Contains(err.Error(), "[remote_exec].backend") || !strings.Contains(err.Error(), `unknown execution backend ""`) || !strings.Contains(err.Error(), "allowed: local, remote") {
			t.Fatalf("explicit blank backend %q error = %q, want key + blank value + allowed set", value, err)
		}
	}
}

func TestLoadRemoteExecConfigAdvertisedWithoutImplementationFailsLoud(t *testing.T) {
	original := append([]string(nil), execbackend.AllowedNames...)
	defer func() { execbackend.AllowedNames = original }()
	execbackend.AllowedNames = append(execbackend.AllowedNames, "future-remote")

	paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"future-remote\"\n")
	_, err := LoadRemoteExecConfig(paths)
	if err == nil || !strings.Contains(err.Error(), `"future-remote"`) || !strings.Contains(err.Error(), "advertised but not implemented") {
		t.Fatalf("advertised future backend error = %v, want loud missing-implementation error", err)
	}
}

func TestLoadRemoteExecConfigIgnoresUnknownKeysAndOtherSections(t *testing.T) {
	// A foreign "backend" key outside [remote_exec] must NOT be picked up, and
	// unknown keys inside the section stay forward-compatible.
	paths := remoteExecTestPaths(t, "[other]\nbackend = \"e2b\"\n\n[remote_exec]\nfuture_key = true\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	if cfg.Backend != "local" {
		t.Fatalf("Backend = %q, want local (foreign section key must not leak)", cfg.Backend)
	}
}
