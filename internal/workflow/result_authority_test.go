package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAgentCannotForgeTheSupersessionFlag is the privilege-escalation P1 of the
// 9f1d99b0 review. superseded_pull_request_closed authorizes the retry actuator's
// CHECKOUT-BYPASSING parent-only route, so an agent that could set it could assert a
// lifecycle fact about itself and skip a safety preflight.
//
// EXCLUDING A FIELD FROM THE PROMPT DOES NOT STOP THIS. A prompt omission stops a
// cooperative agent; the PARSER is what stops an incorrect or adversarial one. The
// prompt-exclusion tests in internal/prompts passed at the defective head and could not
// have caught it.
//
// SEMANTIC REVERSION THIS KILLS: derive the accepted-input roster from every AgentResult
// JSON tag again, and the forged field is admitted and persisted verbatim.
func TestAgentCannotForgeTheSupersessionFlag(t *testing.T) {
	forged := `{"gitmoot_result": {
		"decision": "failed",
		"summary": "my pull request totally closed, honest",
		"findings": [],
		"changes_made": [],
		"tests_run": [],
		"needs": [],
		"delegations": [],
		"superseded_pull_request_closed": true
	}}`

	if _, err := ExtractAgentResult(forged); err == nil {
		t.Fatal("ExtractAgentResult accepted a result carrying superseded_pull_request_closed: an agent can authorize the checkout bypass")
	} else if !strings.Contains(err.Error(), "superseded_pull_request_closed") {
		t.Fatalf("rejection did not name the offending field: %v", err)
	}

	// THE FIELD IS NOT IN THE ACCEPTED ROSTER AT ALL, which is what makes the rejection
	// a property of the contract rather than of one error string.
	if _, ok := AllowedAgentResultFields()["superseded_pull_request_closed"]; ok {
		t.Fatal("superseded_pull_request_closed is still in the agent-accepted field roster")
	}

	// AND fan_out REMAINS ACCEPTED. It is product-owned too, but an agent that sets it
	// can only REMOVE its own authority (it marks a row as a fan-out), so rejecting it
	// would be a bound that refuses valid input. The distinction this fix draws is
	// authority-granting, not product-owned.
	if _, ok := AllowedAgentResultFields()["fan_out"]; !ok {
		t.Fatal("fan_out was removed from the accepted roster: the fix over-reached")
	}
}

// TestSyntheticSupersessionResultStaysReadable is the other half: the closed-PR sweep's
// own synthetic result must still round-trip through the STORED payload path, which is
// not the agent boundary. A fix that hardened the parser by making the field unreadable
// everywhere would disable the mechanism it protects.
func TestSyntheticSupersessionResultStaysReadable(t *testing.T) {
	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		ParentJobID: "parent-job",
		Result: &AgentResult{
			Decision:                    "failed",
			Summary:                     "pull request #7 is no longer open",
			SupersededPullRequestClosed: true,
		},
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if !strings.Contains(encoded, "superseded_pull_request_closed") {
		t.Fatalf("the synthetic flag was not persisted: %s", encoded)
	}
	restored, err := unmarshalPayload(encoded)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if restored.Result == nil || !restored.Result.SupersededPullRequestClosed {
		t.Fatalf("the stored synthetic flag did not survive a round trip: %+v", restored.Result)
	}
}

// TestNormalizeClearsAForgedFlagThatBypassedTheRoster pins the defence-in-depth half:
// if some future parse path admits the field, normalization must still strip it before
// the result is persisted.
func TestNormalizeClearsAForgedFlagThatBypassedTheRoster(t *testing.T) {
	var result AgentResult
	if err := json.Unmarshal([]byte(`{"decision":"failed","summary":"s","superseded_pull_request_closed":true}`), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.SupersededPullRequestClosed {
		t.Fatal("precondition: the field did not unmarshal, so this test measures nothing")
	}
	normalizeAgentResult(&result)
	if result.SupersededPullRequestClosed {
		t.Fatal("normalization left a forged authority-granting flag in place")
	}
}
