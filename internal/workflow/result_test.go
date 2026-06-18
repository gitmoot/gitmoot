package workflow

import (
	"strings"
	"testing"
)

func TestExtractAgentResultFromFencedOutput(t *testing.T) {
	output := "done\n```json\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}` +
		"\n```"

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Decision != "approved" || result.Summary != "looks good" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.TestsRun) != 1 || result.TestsRun[0] != "go test ./..." {
		t.Fatalf("tests_run = %+v", result.TestsRun)
	}
}

func TestExtractAgentResultRejectsMalformedOutput(t *testing.T) {
	if _, err := ExtractAgentResult("plain text without contract"); err == nil {
		t.Fatal("ExtractAgentResult accepted output without gitmoot_result")
	}
}

func TestExtractAgentResultNormalizesMissingArrays(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"minimal"}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Findings == nil || result.ChangesMade == nil || result.TestsRun == nil || result.Needs == nil || result.Delegations == nil {
		t.Fatalf("result arrays were not normalized: %+v", result)
	}
}

func TestExtractAgentResultSkipsDecoyJSONCandidate(t *testing.T) {
	output := `{"note":"mentions gitmoot_result but is not the envelope"}` + "\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"real result","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Summary != "real result" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExtractAgentResultRejectsUnsupportedDecision(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"maybe","summary":"unclear","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	if _, err := ExtractAgentResult(output); err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported decision")
	}
}

func TestExtractAgentResultRejectsUnsupportedField(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"unexpected_field":[]}}`

	_, err := ExtractAgentResult(output)

	if err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported field")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result field "unexpected_field"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractAgentResultAcceptsDelegationDeps(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"implemented","summary":"fan out",` +
		`"delegations":[` +
		`{"id":"api","agent":"impl","action":"implement","prompt":"build api"},` +
		`{"id":"ui","agent":"impl","action":"implement","prompt":"build ui"},` +
		`{"id":"integrate","agent":"impl","action":"implement","prompt":"wire up","deps":["api","ui"]}` +
		`]}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if len(result.Delegations) != 3 {
		t.Fatalf("delegations = %+v", result.Delegations)
	}
	if got := result.Delegations[2].Deps; len(got) != 2 || got[0] != "api" || got[1] != "ui" {
		t.Fatalf("deps = %+v", got)
	}
}

func TestExtractAgentResultRejectsNextAgentsField(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"next_agents":["impl"]}}`

	_, err := ExtractAgentResult(output)

	if err == nil {
		t.Fatal("ExtractAgentResult accepted next_agents field")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result field "next_agents"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsDelegationMissingFields(t *testing.T) {
	cases := []struct {
		name string
		del  Delegation
		want string
	}{
		{"missing id", Delegation{Agent: "impl", Action: "implement", Prompt: "do"}, "delegation id is required"},
		{"missing agent", Delegation{ID: "a", Action: "implement", Prompt: "do"}, `delegation "a" must set exactly one of agent or ephemeral`},
		{"missing action", Delegation{ID: "a", Agent: "impl", Prompt: "do"}, "delegation action is required"},
		{"missing prompt", Delegation{ID: "a", Agent: "impl", Action: "implement"}, "delegation prompt is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := AgentResult{Decision: "implemented", Summary: "x", Delegations: []Delegation{tc.del}}
			err := validateAgentResult(result)
			if err == nil {
				t.Fatalf("validateAgentResult accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateAgentResultRejectsDuplicateDelegationID(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "dup", Agent: "impl", Action: "implement", Prompt: "first"},
			{ID: "dup", Agent: "other", Action: "implement", Prompt: "second"},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted duplicate delegation id")
	}
	if !strings.Contains(err.Error(), `delegation id "dup" is not unique`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsUnknownDep(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "a", Agent: "impl", Action: "implement", Prompt: "do", Deps: []string{"nonexistent"}},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted unknown dep")
	}
	if !strings.Contains(err.Error(), `delegation "a" references unknown dep "nonexistent"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsSelfDep(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "a", Agent: "impl", Action: "implement", Prompt: "do", Deps: []string{"a"}},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted self-dep")
	}
	if !strings.Contains(err.Error(), `delegation "a" depends on itself`) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsDependencyCycle(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "a", Agent: "impl", Action: "implement", Prompt: "do", Deps: []string{"b"}},
			{ID: "b", Agent: "impl", Action: "implement", Prompt: "do", Deps: []string{"a"}},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted dependency cycle")
	}
	if !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("error = %v", err)
	}
	// The error must name both delegations in the cycle.
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("cycle error does not name the delegations: %v", err)
	}
}

func TestValidateAgentResultRejectsArtifactsWithoutBody(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "a", Agent: "impl", Action: "implement", Prompt: "do", Artifacts: []string{"brief.md"}},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted delegation requesting artifacts with empty body")
	}
	if !strings.Contains(err.Error(), "artifact_body is required when delegations request artifacts") {
		t.Fatalf("error = %v", err)
	}

	// With a brief written, the same delegations are valid.
	result.ArtifactBody = "shared brief"
	if err := validateAgentResult(result); err != nil {
		t.Fatalf("validateAgentResult rejected delegations with a written brief: %v", err)
	}
}

func TestValidateAgentResultAcceptsEphemeralDelegation(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"implemented","summary":"fan out",` +
		`"delegations":[` +
		`{"id":"worker","ephemeral":{"runtime":"codex","model":"gpt-5.4"},"action":"implement","prompt":"hi"}` +
		`]}}`

	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult rejected a valid ephemeral delegation: %v", err)
	}
	if len(result.Delegations) != 1 {
		t.Fatalf("delegations = %+v", result.Delegations)
	}
	spec := result.Delegations[0].Ephemeral
	if spec == nil {
		t.Fatalf("ephemeral spec was not parsed: %+v", result.Delegations[0])
	}
	if spec.Runtime != "codex" || spec.Model != "gpt-5.4" {
		t.Fatalf("ephemeral spec = %+v", spec)
	}
	if strings.TrimSpace(result.Delegations[0].Agent) != "" {
		t.Fatalf("ephemeral delegation should not carry an agent: %q", result.Delegations[0].Agent)
	}
}

func TestValidateAgentResultRejectsBothAgentAndEphemeral(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "d", Agent: "impl", Ephemeral: &EphemeralSpec{Runtime: "codex"}, Action: "implement", Prompt: "go"},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted a delegation with both agent and ephemeral")
	}
	if !strings.Contains(err.Error(), "sets both agent and ephemeral") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsNeitherAgentNorEphemeral(t *testing.T) {
	result := AgentResult{
		Decision: "implemented",
		Summary:  "x",
		Delegations: []Delegation{
			{ID: "d", Action: "implement", Prompt: "go"},
		},
	}
	err := validateAgentResult(result)
	if err == nil {
		t.Fatal("validateAgentResult accepted a delegation with neither agent nor ephemeral")
	}
	if !strings.Contains(err.Error(), "must set exactly one of agent or ephemeral") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateAgentResultRejectsShellEphemeralRuntime(t *testing.T) {
	cases := map[string]string{
		"shell":   "shell",
		"empty":   "",
		"unknown": "bash",
	}
	for name, rt := range cases {
		t.Run(name, func(t *testing.T) {
			result := AgentResult{
				Decision: "implemented",
				Summary:  "x",
				Delegations: []Delegation{
					{ID: "d", Ephemeral: &EphemeralSpec{Runtime: rt}, Action: "implement", Prompt: "go"},
				},
			}
			err := validateAgentResult(result)
			if err == nil {
				t.Fatalf("validateAgentResult accepted ephemeral runtime %q", rt)
			}
			if !strings.Contains(err.Error(), "ephemeral runtime") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEphemeralAgentNameContainsInfix(t *testing.T) {
	name := ephemeralAgentName("worker-1", "parent-job")
	if !strings.Contains(name, "-ephemeral-") {
		t.Fatalf("ephemeral agent name %q does not contain the %q infix", name, "-ephemeral-")
	}
	// The name is stable for the same (delegation, parent) pair and distinct for
	// different parents, so the TUI filter and idempotent enqueue agree.
	if again := ephemeralAgentName("worker-1", "parent-job"); again != name {
		t.Fatalf("ephemeralAgentName not stable: %q vs %q", name, again)
	}
	if other := ephemeralAgentName("worker-1", "other-parent"); other == name {
		t.Fatalf("ephemeralAgentName collided across parents: %q", name)
	}
}
