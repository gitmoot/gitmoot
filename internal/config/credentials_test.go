package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func writeCredentialsConfig(t *testing.T, body string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

func TestLoadCredentialsConfigDefaults(t *testing.T) {
	paths := writeCredentialsConfig(t, "")
	got, err := LoadCredentialsConfig(paths)
	if err != nil {
		t.Fatalf("LoadCredentialsConfig: %v", err)
	}
	want := CredentialsConfig{GitHub: CredentialsGitHubDeny, ModelGatewayAllowHosts: []string{DefaultModelGatewayHost}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %#v, want %#v", got, want)
	}
}

func TestLoadCredentialsConfigParsesSection(t *testing.T) {
	paths := writeCredentialsConfig(t, `
[credentials]
env_curation = true
env_passthrough = ["GOCACHE", "NPM_*"]
github = "inherit"
model_gateway = true
model_gateway_allow_hosts = ["api.anthropic.com", "localhost"]
`)
	got, err := LoadCredentialsConfig(paths)
	if err != nil {
		t.Fatalf("LoadCredentialsConfig: %v", err)
	}
	if !got.EnvCuration || got.GitHub != CredentialsGitHubInherit || !got.ModelGateway || !reflect.DeepEqual(got.EnvPassthrough, []string{"GOCACHE", "NPM_*"}) || !reflect.DeepEqual(got.ModelGatewayAllowHosts, []string{"api.anthropic.com", "localhost"}) {
		t.Fatalf("unexpected config: %#v", got)
	}
}

func TestLoadCredentialsConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "github", body: "github = \"prompt\"", want: "use deny or inherit"},
		{name: "equals", body: "env_passthrough = [\"A=B\"]", want: "must not contain"},
		{name: "nul", body: "env_passthrough = [\"A\\u0000B\"]", want: "must not contain"},
		{name: "middle glob", body: "env_passthrough = [\"N*M\"]", want: "trailing '*'"},
		{name: "multiple globs", body: "env_passthrough = [\"N**\"]", want: "trailing '*'"},
		{name: "empty gateway allowlist", body: "model_gateway = true\nmodel_gateway_allow_hosts = []", want: "must not be empty"},
		{name: "gateway scheme", body: "model_gateway_allow_hosts = [\"https://api.anthropic.com\"]", want: "without scheme"},
		{name: "gateway port", body: "model_gateway_allow_hosts = [\"api.anthropic.com:443\"]", want: "without a port"},
		{name: "gateway wildcard", body: "model_gateway_allow_hosts = [\"*.anthropic.com\"]", want: "unsupported character"},
		{name: "gateway space", body: "model_gateway_allow_hosts = [\"api anthropic.com\"]", want: "unsupported character"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := writeCredentialsConfig(t, "\n[credentials]\n"+test.body+"\n")
			_, err := LoadCredentialsConfig(paths)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDefaultConfigCredentialsExampleRoundTrips(t *testing.T) {
	paths := writeCredentialsConfig(t, "")
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, line := range []string{"# [credentials]", "# env_curation = false", "# env_passthrough = []", "# github = \"deny\"", "# model_gateway = false", "# model_gateway_allow_hosts = [\"api.anthropic.com\"]"} {
		if !strings.Contains(string(content), line) {
			t.Fatalf("DefaultConfig missing %q", line)
		}
	}
	got, err := LoadCredentialsConfig(paths)
	if err != nil {
		t.Fatalf("LoadCredentialsConfig(DefaultConfig): %v", err)
	}
	if !reflect.DeepEqual(got, DefaultCredentialsConfig()) {
		t.Fatalf("round-trip config = %#v", got)
	}
}
