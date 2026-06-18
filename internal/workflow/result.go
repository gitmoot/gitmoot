package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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
	Model         string   `json:"model,omitempty"`
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
		// The engine keys child jobs, the dedup map, and the DAG on the raw id
		// while deps are matched trimmed; a surrounding-whitespace id would make
		// those disagree and silently drop the delegation. Require them equal.
		if d.ID != strings.TrimSpace(d.ID) {
			return fmt.Errorf("delegation id %q must not have leading or trailing whitespace", d.ID)
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
		if err := validateDelegationLifecycle(d); err != nil {
			return err
		}
	}
	if err := validateDelegationGraph(result.Delegations); err != nil {
		return err
	}
	if delegationsRequestArtifacts(result.Delegations) && strings.TrimSpace(result.ArtifactBody) == "" {
		return errors.New("artifact_body is required when delegations request artifacts")
	}
	return nil
}

// validateDelegationGraph enforces the structural invariants of the delegation
// dependency DAG: every id is unique, every declared dep references a known
// sibling id (and never the delegation itself), and the deps form no cycle. A
// coordinator that violates any of these would otherwise crash mid-dispatch
// (duplicate ids collide on the deterministic child job id) or silently drop a
// delegation while falsely signalling the batch is complete (unknown deps and
// cycles are treated as permanently blocked by the engine). Validating here
// turns those into clean errors at result-parse time, before any side effects.
func validateDelegationGraph(delegations []Delegation) error {
	known := make(map[string]struct{}, len(delegations))
	for _, d := range delegations {
		id := strings.TrimSpace(d.ID)
		if _, ok := known[id]; ok {
			return fmt.Errorf("delegation id %q is not unique", id)
		}
		known[id] = struct{}{}
	}
	for _, d := range delegations {
		id := strings.TrimSpace(d.ID)
		for _, dep := range d.Deps {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				continue
			}
			if dep == id {
				return fmt.Errorf("delegation %q depends on itself", id)
			}
			if _, ok := known[dep]; !ok {
				return fmt.Errorf("delegation %q references unknown dep %q", id, dep)
			}
		}
	}
	return detectDelegationCycle(delegations)
}

// detectDelegationCycle runs a depth-first search over the delegation deps and
// reports the first cycle it finds, naming the delegations involved. Deps that
// reference unknown ids are ignored here because validateDelegationGraph has
// already rejected those; this keeps the traversal focused on real edges.
func detectDelegationCycle(delegations []Delegation) error {
	edges := make(map[string][]string, len(delegations))
	for _, d := range delegations {
		id := strings.TrimSpace(d.ID)
		for _, dep := range d.Deps {
			dep = strings.TrimSpace(dep)
			if dep != "" {
				edges[id] = append(edges[id], dep)
			}
		}
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(delegations))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		stack = append(stack, id)
		for _, dep := range edges[id] {
			switch state[dep] {
			case visiting:
				// Found a back-edge; slice the stack from dep to close the cycle.
				cycle := append([]string{}, stack[indexOf(stack, dep):]...)
				return append(cycle, dep)
			case unvisited:
				if cycle := visit(dep); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return nil
	}

	for _, d := range delegations {
		id := strings.TrimSpace(d.ID)
		if state[id] == unvisited {
			if cycle := visit(id); cycle != nil {
				return fmt.Errorf("delegations form a dependency cycle: %s", strings.Join(cycle, " -> "))
			}
		}
	}
	return nil
}

func indexOf(values []string, target string) int {
	for i, v := range values {
		if v == target {
			return i
		}
	}
	return 0
}

// validateDelegationLifecycle validates the Phase 2 lifecycle controls on a
// delegation: timeout must parse as a positive duration, retry must be
// non-negative, and failure_policy/synthesis_rule must be drawn from their
// allowed sets. Empty values are accepted and fall back to defaults.
func validateDelegationLifecycle(d Delegation) error {
	if timeout := strings.TrimSpace(d.Timeout); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil {
			return fmt.Errorf("delegation %q timeout %q is invalid: %w", d.ID, timeout, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("delegation %q timeout %q must be positive", d.ID, timeout)
		}
	}
	if d.Retry < 0 {
		return fmt.Errorf("delegation %q retry must be >= 0", d.ID)
	}
	switch strings.ToLower(strings.TrimSpace(d.FailurePolicy)) {
	case "", "block_parent", "continue", "escalate":
	default:
		return fmt.Errorf("delegation %q failure_policy %q is invalid", d.ID, d.FailurePolicy)
	}
	switch strings.ToLower(strings.TrimSpace(d.SynthesisRule)) {
	case "", "summary", "vote":
	default:
		return fmt.Errorf("delegation %q synthesis_rule %q is invalid", d.ID, d.SynthesisRule)
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
