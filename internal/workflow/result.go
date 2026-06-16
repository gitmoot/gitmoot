package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const resultKey = `"gitmoot_result"`

// Delegation describes a child job that an agent wants Gitmoot to enqueue on
// its behalf.
type Delegation struct {
	ID            string   `json:"id"`
	Agent         string   `json:"agent"`
	Action        string   `json:"action"`
	Worktree      string   `json:"worktree,omitempty"`
	Prompt        string   `json:"prompt"`
	Artifacts     []string `json:"artifacts,omitempty"`
	Deps          []string `json:"deps,omitempty"`
	Timeout       string   `json:"timeout,omitempty"`
	Retry         int      `json:"retry,omitempty"`
	FailurePolicy string   `json:"failure_policy,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	SynthesisRule string   `json:"synthesis_rule,omitempty"`
}

type AgentResult struct {
	Decision     string            `json:"decision"`
	Summary      string            `json:"summary"`
	Findings     []json.RawMessage `json:"findings"`
	ChangesMade  []string          `json:"changes_made"`
	TestsRun     []string          `json:"tests_run"`
	Needs        []string          `json:"needs"`
	Delegations  []Delegation      `json:"delegations"`
	ArtifactBody string            `json:"artifact_body,omitempty"`
}

func ExtractAgentResult(output string) (AgentResult, error) {
	var validationErr error
	for _, candidate := range jsonObjectCandidates(output) {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
			continue
		}
		raw, ok := envelope["gitmoot_result"]
		if !ok {
			continue
		}
		if err := validateAgentResultFields(raw); err != nil {
			if validationErr == nil {
				validationErr = err
			}
			continue
		}
		var result AgentResult
		if err := json.Unmarshal(raw, &result); err != nil {
			if validationErr == nil {
				validationErr = err
			}
			continue
		}
		if err := validateAgentResult(result); err != nil {
			if validationErr == nil {
				validationErr = err
			}
			continue
		}
		normalizeAgentResult(&result)
		return result, nil
	}
	if validationErr != nil {
		return AgentResult{}, validationErr
	}
	return AgentResult{}, errors.New("missing valid gitmoot_result JSON object")
}

func validateAgentResultFields(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	allowed := map[string]struct{}{
		"decision":      {},
		"summary":       {},
		"findings":      {},
		"changes_made":  {},
		"tests_run":     {},
		"needs":         {},
		"delegations":   {},
		"artifact_body": {},
	}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("unsupported gitmoot_result field %q", field)
		}
	}
	return nil
}

func validateAgentResult(result AgentResult) error {
	switch result.Decision {
	case "approved", "changes_requested", "blocked", "implemented", "failed":
	default:
		return fmt.Errorf("unsupported gitmoot_result decision %q", result.Decision)
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("gitmoot_result summary is required")
	}
	for _, d := range result.Delegations {
		if strings.TrimSpace(d.ID) == "" {
			return errors.New("delegation id is required")
		}
		if strings.TrimSpace(d.Agent) == "" {
			return errors.New("delegation agent is required")
		}
		if strings.TrimSpace(d.Action) == "" {
			return errors.New("delegation action is required")
		}
		if strings.TrimSpace(d.Prompt) == "" {
			return errors.New("delegation prompt is required")
		}
		if len(d.Deps) > 0 {
			return fmt.Errorf("delegation %q uses dependencies, which are not yet supported", d.ID)
		}
	}
	return nil
}

func normalizeAgentResult(result *AgentResult) {
	if result.Findings == nil {
		result.Findings = []json.RawMessage{}
	}
	if result.ChangesMade == nil {
		result.ChangesMade = []string{}
	}
	if result.TestsRun == nil {
		result.TestsRun = []string{}
	}
	if result.Needs == nil {
		result.Needs = []string{}
	}
	if result.Delegations == nil {
		result.Delegations = []Delegation{}
	}
}

func stateForDecision(decision string) JobState {
	switch decision {
	case "blocked":
		return JobBlocked
	case "failed":
		return JobFailed
	default:
		return JobSucceeded
	}
}

func jsonObjectCandidates(output string) []string {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}

	candidates := make([]string, 0, 2)
	if strings.HasPrefix(output, "{") && strings.Contains(output, resultKey) {
		candidates = append(candidates, output)
	}

	for start := 0; start < len(output); start++ {
		if output[start] != '{' {
			continue
		}
		if candidate, ok := balancedJSONObject(output[start:]); ok && strings.Contains(candidate, resultKey) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func balancedJSONObject(input string) (string, bool) {
	depth := 0
	inString := false
	escaped := false

	for index := 0; index < len(input); index++ {
		char := input[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}

		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return input[:index+1], true
			}
			if depth < 0 {
				return "", false
			}
		}
	}
	return "", false
}
