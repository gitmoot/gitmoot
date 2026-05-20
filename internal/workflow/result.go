package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const resultKey = `"gitmoot_result"`

type AgentResult struct {
	Decision    string            `json:"decision"`
	Summary     string            `json:"summary"`
	Findings    []json.RawMessage `json:"findings"`
	ChangesMade []string          `json:"changes_made"`
	TestsRun    []string          `json:"tests_run"`
	Needs       []string          `json:"needs"`
	NextAgents  []string          `json:"next_agents"`
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

func validateAgentResult(result AgentResult) error {
	switch result.Decision {
	case "approved", "changes_requested", "blocked", "implemented", "failed":
	default:
		return fmt.Errorf("unsupported gitmoot_result decision %q", result.Decision)
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("gitmoot_result summary is required")
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
	if result.NextAgents == nil {
		result.NextAgents = []string{}
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
