package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

func claudeCredential(t *testing.T, accessToken, refreshToken string, expiresAt int64) string {
	t.Helper()
	oauth := map[string]any{"accessToken": accessToken}
	if refreshToken != "" {
		oauth["refreshToken"] = refreshToken
	}
	if expiresAt != 0 {
		oauth["expiresAt"] = expiresAt
	}
	encoded, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestClaudeOAuthUsabilityIsJudgedOnlyWhenProvable pins BOTH directions.
//
// The policy previously validated presence and JSON shape and never usability,
// so an expired-and-unrefreshable profile staged cleanly and the seat died on
// the runtime's own opaque error. The check must catch that - and must NOT
// invent a verdict where none is provable, which is how every bound added in
// this campaign has previously broken valid input.
func TestClaudeOAuthUsabilityIsJudgedOnlyWhenProvable(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UnixMilli()
	future := time.Now().Add(2 * time.Hour).UnixMilli()

	usable := map[string]string{
		"unexpired":                 claudeCredential(t, "token", "", future),
		"expired but refreshable":   claudeCredential(t, "token", "refresh", past),
		"no expiry recorded":        claudeCredential(t, "token", "", 0),
		"unexpired and refreshable": claudeCredential(t, "token", "refresh", future),
	}
	for name, credential := range usable {
		t.Run("usable/"+name, func(t *testing.T) {
			if err := claudeOAuthUsable([]byte(credential)); err != nil {
				t.Fatalf("rejected a credential it cannot prove is broken: %v", err)
			}
		})
	}

	unusable := map[string]struct{ credential, wants string }{
		"expired with no refresh token": {claudeCredential(t, "token", "", past), "refreshToken"},
		"no access token":               {claudeCredential(t, "", "refresh", future), "accessToken"},
	}
	for name, tc := range unusable {
		t.Run("unusable/"+name, func(t *testing.T) {
			err := claudeOAuthUsable([]byte(tc.credential))
			if err == nil {
				t.Fatal("accepted a credential that cannot authenticate")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v, want it to name %q so an operator knows what to fix", err, tc.wants)
			}
		})
	}
}

// TestReadOnlySeatRefusesAnUnusableClaudeProfileByName drives the check through
// the real staging path: the failure must arrive at STAGING time and name the
// file, rather than later as the runtime's own message.
func TestReadOnlySeatRefusesAnUnusableClaudeProfileByName(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	expired := claudeCredential(t, "token", "", time.Now().Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: source}
	_, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err == nil {
		t.Fatal("an expired, unrefreshable claude profile staged cleanly; the seat would fail later and opaquely")
	}
	for _, want := range []string{".credentials.json", "expired"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// A working profile must still stage, and the narrowed section must survive.
	good := claudeCredential(t, "token", "refresh", time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir, _, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), false)
	if err != nil {
		t.Fatalf("a working claude profile must stage, got: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(stateDir, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(staged), "claudeAiOauth") {
		t.Errorf("staged credential lost its section: %s", staged)
	}
}

// TestReadOnlySeatGatewayModeStagesNoCredential closes the reviewer's P3 on
// gateway mode, which the coverage test never probed: in gateway mode the
// credential is supplied by the gateway, so the seat must stage none - and the
// usability check must therefore not run at all.
func TestReadOnlySeatGatewayModeStagesNoCredential(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(home, ".claude")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	// Deliberately unusable: gateway mode must not read it, let alone judge it.
	expired := claudeCredential(t, "", "", time.Now().Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(source, ".credentials.json"), []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Runtime: runtime.ClaudeRuntime, RuntimeConfigDir: source}
	stateDir, stateEnv, _, err := prepareReadOnlyRuntimeState(agent, t.TempDir(), true)
	if err != nil {
		t.Fatalf("gateway mode must not stage or judge the host credential, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, ".credentials.json")); !os.IsNotExist(statErr) {
		t.Errorf("gateway mode staged a credential into the seat: %v", statErr)
	}
	if len(stateEnv) == 0 || !strings.Contains(fmt.Sprint(stateEnv), "CLAUDE_CONFIG_DIR") {
		t.Errorf("gateway mode state env = %v, want CLAUDE_CONFIG_DIR pointed at the isolated dir", stateEnv)
	}
}

// TestClaudeOAuthToleratesFormatDrift is the round-2 P3: expiresAt decoded as a
// strict int64, so a third party writing it as a STRING or in exponent
// notation aborted the entire seat with a raw Go unmarshal error - a hard
// outage caused by how gitmoot READS a file it does not own. Both staged fine
// at merge base.
func TestClaudeOAuthToleratesFormatDrift(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UnixMilli()
	noVerdict := map[string]string{
		"expiresAt as a string":          `{"claudeAiOauth":{"accessToken":"t","expiresAt":"32503680000000"}}`,
		"expiresAt in exponent form":     `{"claudeAiOauth":{"accessToken":"t","expiresAt":1.9e12}}`,
		"expiresAt in SECONDS":           `{"claudeAiOauth":{"accessToken":"t","expiresAt":1756000000}}`,
		"expiresAt as a bool":            `{"claudeAiOauth":{"accessToken":"t","expiresAt":true}}`,
		"claudeAiOauth is not an object": `{"claudeAiOauth":"opaque"}`,
		"not our format at all":          `{"somethingElse":{}}`,
	}
	for name, credential := range noVerdict {
		t.Run("no verdict/"+name, func(t *testing.T) {
			if err := claudeOAuthUsable([]byte(credential)); err != nil {
				t.Fatalf("format drift became a hard seat outage: %v", err)
			}
		})
	}

	// Judgements it CAN prove are unchanged.
	if err := claudeOAuthUsable([]byte(`{"claudeAiOauth":{"accessToken":"t","expiresAt":` + fmt.Sprint(future) + `}}`)); err != nil {
		t.Errorf("an unexpired token was rejected: %v", err)
	}
	expired := time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := claudeOAuthUsable([]byte(`{"claudeAiOauth":{"accessToken":"t","expiresAt":` + fmt.Sprint(expired) + `}}`)); err == nil {
		t.Error("an expired token with no refreshToken was accepted")
	}

	// A PAST exponent value decodes and yields the ordinary verdict - the
	// round-2 finding was the raw unmarshal error, not the judgement.
	err := claudeOAuthUsable([]byte(`{"claudeAiOauth":{"accessToken":"t","expiresAt":1.75e12}}`))
	if err == nil {
		t.Error("a past exponent-form expiry produced no verdict")
	} else if strings.Contains(err.Error(), "cannot unmarshal") {
		t.Errorf("exponent form still aborts with a raw Go json error: %v", err)
	}

	// Clock skew must not condemn a seat: a token that just expired is inside
	// the grace window.
	justExpired := time.Now().Add(-time.Minute).UnixMilli()
	if err := claudeOAuthUsable([]byte(`{"claudeAiOauth":{"accessToken":"t","expiresAt":` + fmt.Sprint(justExpired) + `}}`)); err != nil {
		t.Errorf("a token one minute past expiry was refused, which is a clock-skew verdict: %v", err)
	}
}
