package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/credgw"
)

var (
	modelGatewayUpstreamURL = credgw.DefaultAnthropicUpstream
)

// modelGatewayCredential picks the Anthropic credential the remote model
// gateway injects when no model_gateway_key names a keychain key. When more than
// one managed value is present the API key wins, then the Anthropic bearer
// token, then the Claude OAuth token.
func modelGatewayCredential(auth runtimeAuthFile) (credgw.Credential, error) {
	if !auth.Exists || len(auth.Values) == 0 {
		return credgw.Credential{}, fmt.Errorf("model gateway requires a populated %s", runtimeAuthFileName)
	}
	if value := strings.TrimSpace(auth.Values[anthropicAPIKeyEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialAPIKey, Value: value}, nil
	}
	if value := strings.TrimSpace(auth.Values[anthropicAuthTokenEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialBearer, Value: value}, nil
	}
	if value := strings.TrimSpace(auth.Values[claudeOAuthTokenEnv]); value != "" {
		return credgw.Credential{Kind: credgw.CredentialBearer, Value: value}, nil
	}
	return credgw.Credential{}, fmt.Errorf("model gateway requires a populated %s", runtimeAuthFileName)
}

func closeModelGatewayHome(home string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = credgw.DefaultRegistry.CloseHome(ctx, home)
}
