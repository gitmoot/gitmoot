package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

const (
	ClaudeOAuthTokenEnv   = "CLAUDE_CODE_OAUTH_TOKEN"
	AnthropicAPIKeyEnv    = "ANTHROPIC_API_KEY"
	AnthropicAuthTokenEnv = "ANTHROPIC_AUTH_TOKEN"
	// ClaudeBackgroundTokenMessage is the warn-path caveat: foreground Claude
	// authenticates fine via cached credentials, but daemon background jobs run
	// non-interactively and need an explicit token in the daemon env.
	ClaudeBackgroundTokenMessage = "Foreground Claude works via cached credentials, but daemon background jobs run non-interactively and need a token in the daemon env. Run: claude setup-token; export CLAUDE_CODE_OAUTH_TOKEN=<token>; then restart the Gitmoot daemon from that same shell so it inherits the token. export is per-shell, so persist the token in ~/.bashrc (interactive non-login terminals read it; on Debian/Raspberry Pi OS ~/.profile is login-only and itself sources ~/.bashrc). For a daemon that does not depend on which shell launched it, run it under `systemd --user` with an EnvironmentFile."
	// ClaudeSessionAuthFailedMessage is the failure-path message: a genuine
	// authentication/session failure (the cached session is expired or the
	// --resume target is dead) that needs re-authentication and a fresh bind.
	ClaudeSessionAuthFailedMessage = "Claude authentication/session failed; the cached session may be expired or the --resume target is dead. Re-authenticate (claude setup-token) and rebind the session."
	// ClaudeSessionMissingMessage is the remediation guidance for an unrecoverable
	// dead pinned session (#443): the pinned conversation is gone and self-heal
	// could not start a fresh one. The caller formats in the agent name; this
	// constant carries the generic remediation so it stays reusable.
	ClaudeSessionMissingMessage = "Re-point the agent with `gitmoot agent restart <name>` or `gitmoot agent subscribe … --session last`."
	ClaudeLiveCheckPrompt       = "Gitmoot Claude live check. Return OK only."
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
		return ClaudeBackgroundTokenMessage
	case e.AnthropicAPIKey:
		return "ANTHROPIC_API_KEY is set; Claude Code may use API-key billing and this can override Claude OAuth behavior."
	case e.AnthropicAuthToken:
		return "ANTHROPIC_AUTH_TOKEN is set; it can affect Claude auth precedence."
	default:
		return ""
	}
}

func ClaudeLiveCheck(ctx context.Context, runner subprocess.Runner, dir string) error {
	return ClaudeLiveCheckEnv(ctx, runner, dir, nil)
}

// ClaudeLiveCheckEnv is ClaudeLiveCheck with extraEnv (KEY=VALUE entries)
// appended to the probe's environment. The doctor uses it to validate a SPECIFIC
// credential — e.g. the running daemon's CLAUDE_CODE_OAUTH_TOKEN read from /proc —
// rather than whatever the doctor process happens to carry. extraEnv is honored
// only when runner implements subprocess.EnvRunner; otherwise (fakes, plain
// runners) it falls back to the same command+args so the override is invisible to
// arg-keyed test doubles. A nil extraEnv is exactly the prior behavior.
func ClaudeLiveCheckEnv(ctx context.Context, runner subprocess.Runner, dir string, extraEnv []string) error {
	if runner == nil {
		runner = subprocess.ExecRunner{}
	}
	run := func(args ...string) (subprocess.Result, error) {
		if env, ok := runner.(subprocess.EnvRunner); ok && len(extraEnv) > 0 {
			return env.RunEnv(ctx, dir, extraEnv, "claude", args...)
		}
		return runner.Run(ctx, dir, "claude", args...)
	}
	result, err := run("-p", "--output-format", "json", "--", ClaudeLiveCheckPrompt)
	if err != nil && isClaudeJSONUnsupported(result) {
		result, err = run("-p", "--", ClaudeLiveCheckPrompt)
		if err != nil {
			return ClassifyClaudeCommandError(result, err)
		}
		return validateClaudeLiveText(result.Stdout)
	}
	if err != nil {
		return ClassifyClaudeCommandError(result, err)
	}
	return validateClaudeLiveJSON(result.Stdout)
}

// ClaudeTokenStatus is the tri-state verdict of validating a Claude credential
// with a live probe: it either authenticated (Valid), was rejected (Invalid), or
// could not be determined (Unknown — a transient/network error, or the probe
// could not run at all). A merely-SET token is not Valid until a probe says so.
type ClaudeTokenStatus int

const (
	ClaudeTokenUnknown ClaudeTokenStatus = iota
	ClaudeTokenValid
	ClaudeTokenInvalid
)

func (s ClaudeTokenStatus) String() string {
	switch s {
	case ClaudeTokenValid:
		return "valid"
	case ClaudeTokenInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// ClaudeClassifyProbe maps a ClaudeLiveCheck/ClaudeLiveCheckEnv error to a
// tri-state status. A nil error is Valid; a classified credential rejection
// (errors.Is ErrClaudeAuthFailed) is Invalid; everything else — a missing/
// unrunnable binary or a transient/network/timeout error — is Unknown and MUST
// NOT be reported as Invalid, so a network blip can never flip doctor red or
// claim the token is bad.
func ClaudeClassifyProbe(err error) ClaudeTokenStatus {
	switch {
	case err == nil:
		return ClaudeTokenValid
	case errors.Is(err, ErrClaudeAuthFailed):
		return ClaudeTokenInvalid
	default:
		return ClaudeTokenUnknown
	}
}

// ClaudeProbeUnavailable reports whether a ClaudeLiveCheck error means the probe
// could not run at all — the `claude` binary is missing or otherwise not
// executable — as opposed to running and returning an auth/session failure. A
// missing binary must never be classified as an auth failure: the operator may
// simply lack the CLI on this box, which is not a new health regression. The
// underlying exec error is wrapped with %w by commandError/ClassifyClaudeCommandError,
// so errors.Is/As still reach it through the chain.
func ClaudeProbeUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

func validateClaudeLiveJSON(stdout string) error {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return fmt.Errorf("Claude Code live check returned no stdout response")
	}
	var payload struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return fmt.Errorf("Claude Code live check returned invalid JSON: %w", err)
	}
	if strings.TrimSpace(payload.Result) == "" {
		return fmt.Errorf("Claude Code live check returned no result")
	}
	return nil
}

func validateClaudeLiveText(stdout string) error {
	if strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("Claude Code live check returned no stdout response")
	}
	return nil
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
