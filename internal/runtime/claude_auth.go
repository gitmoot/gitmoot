package runtime

import "strings"

const (
	ClaudeOAuthTokenEnv    = "CLAUDE_CODE_OAUTH_TOKEN"
	AnthropicAPIKeyEnv     = "ANTHROPIC_API_KEY"
	AnthropicAuthTokenEnv  = "ANTHROPIC_AUTH_TOKEN"
	ClaudeAuthSetupMessage = "Claude Code background jobs need non-interactive credentials. Run: claude setup-token; export CLAUDE_CODE_OAUTH_TOKEN=<token>; then restart the Gitmoot daemon so it inherits the token."
)

type ClaudeAuthEnv struct {
	ClaudeOAuthToken   bool
	AnthropicAPIKey    bool
	AnthropicAuthToken bool
}

func InspectClaudeAuthEnv(lookup func(string) (string, bool)) ClaudeAuthEnv {
	if lookup == nil {
		return ClaudeAuthEnv{}
	}
	oauth := hasEnvValue(lookup, ClaudeOAuthTokenEnv)
	apiKey := hasEnvValue(lookup, AnthropicAPIKeyEnv)
	authToken := hasEnvValue(lookup, AnthropicAuthTokenEnv)
	return ClaudeAuthEnv{
		ClaudeOAuthToken:   oauth,
		AnthropicAPIKey:    apiKey,
		AnthropicAuthToken: authToken,
	}
}

func (e ClaudeAuthEnv) Ready() bool {
	return e.ClaudeOAuthToken || e.AnthropicAPIKey || e.AnthropicAuthToken
}

func (e ClaudeAuthEnv) MaskedDetail() string {
	return strings.Join([]string{
		ClaudeOAuthTokenEnv + "=" + setUnset(e.ClaudeOAuthToken),
		AnthropicAPIKeyEnv + "=" + setUnset(e.AnthropicAPIKey),
		AnthropicAuthTokenEnv + "=" + setUnset(e.AnthropicAuthToken),
	}, "; ")
}

func (e ClaudeAuthEnv) Warning() string {
	switch {
	case !e.Ready():
		return ClaudeAuthSetupMessage
	case e.AnthropicAPIKey:
		return "ANTHROPIC_API_KEY is set; Claude Code may use API-key billing and this can override Claude OAuth behavior."
	case e.AnthropicAuthToken:
		return "ANTHROPIC_AUTH_TOKEN is set; it can affect Claude auth precedence."
	default:
		return ""
	}
}

func setUnset(value bool) string {
	if value {
		return "set"
	}
	return "unset"
}

func hasEnvValue(lookup func(string) (string, bool), name string) bool {
	value, ok := lookup(name)
	return ok && strings.TrimSpace(value) != ""
}
