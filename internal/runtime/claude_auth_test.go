package runtime

import (
	"strings"
	"testing"
)

func TestInspectClaudeAuthEnvMasksReadiness(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(name string) (string, bool) {
		switch name {
		case ClaudeOAuthTokenEnv:
			return "secret-token", true
		default:
			return "", false
		}
	})

	if !auth.Ready() {
		t.Fatal("auth env was not ready despite OAuth token")
	}
	detail := auth.MaskedDetail()
	if !strings.Contains(detail, ClaudeOAuthTokenEnv+"=set") || strings.Contains(detail, "secret-token") {
		t.Fatalf("masked detail = %q", detail)
	}
	if warning := auth.Warning(); warning != "" {
		t.Fatalf("warning = %q, want none", warning)
	}
}

func TestInspectClaudeAuthEnvWarnsForMissingCredentials(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(string) (string, bool) { return "", false })

	if auth.Ready() {
		t.Fatal("auth env is ready despite no credentials")
	}
	if !strings.Contains(auth.Warning(), "claude setup-token") {
		t.Fatalf("warning = %q, want setup-token guidance", auth.Warning())
	}
}

func TestInspectClaudeAuthEnvWarnsForAPIKeyPrecedence(t *testing.T) {
	auth := InspectClaudeAuthEnv(func(name string) (string, bool) {
		if name == AnthropicAPIKeyEnv {
			return "secret-key", true
		}
		return "", false
	})

	if !auth.Ready() {
		t.Fatal("auth env was not ready despite API key")
	}
	if !strings.Contains(auth.Warning(), "API-key billing") {
		t.Fatalf("warning = %q, want API key warning", auth.Warning())
	}
}
