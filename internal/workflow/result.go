package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

const resultKey = `"gitmoot_result"`

// The following slices are the single source of truth for the enum-valued
// fields of the gitmoot_result contract. The validator below checks against
// them, and the build-time contract generator (internal/prompts/contractgen)
// reflects over them so the prompt prose and the validator can never drift
// apart. Keep them in declaration order — the generated prose lists them in
// the same order.

// ResultDecisions are the allowed values of AgentResult.Decision.
var ResultDecisions = []string{"approved", "changes_requested", "blocked", "implemented", "failed"}

// DelegationFailurePolicies are the allowed values of Delegation.FailurePolicy
// (the empty string falls back to the default and is accepted separately).
var DelegationFailurePolicies = []string{"block_parent", "continue", "escalate", "escalate_human"}

// DelegationSynthesisRules are the allowed values of Delegation.SynthesisRule
// (the empty string falls back to the default and is accepted separately).
var DelegationSynthesisRules = []string{"summary", "vote", "quorum"}

// EphemeralRuntimes are the allowed values of EphemeralSpec.Runtime. They are a
// subset of the registered runtimes: an ephemeral worker is never a raw shell.
var EphemeralRuntimes = []string{runtime.CodexRuntime, runtime.ClaudeRuntime, runtime.KimiRuntime}

// allowedSet returns a lookup set for the given allowed-value slice.
func allowedSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

// EphemeralSpec describes an on-demand worker that Gitmoot should materialize
// for a delegation instead of routing it to a pre-registered agent. The daemon
// builds the worker from this spec (default sandbox read-only); the workflow
// engine treats a delegation carrying a spec as inheriting the coordinator's
// allowed repo scope, so it bypasses the registered-agent existence, repo-access,
// and capability checks. Runtime is REQUIRED and must be one of codex/claude/kimi
// — never shell.
type EphemeralSpec struct {
	Runtime        string   `json:"runtime"`
	Model          string   `json:"model,omitempty"`
	Template       string   `json:"template,omitempty"`
	Role           string   `json:"role,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	AutonomyPolicy string   `json:"autonomy_policy,omitempty"`
}

// Delegation describes a child job that an agent wants Gitmoot to enqueue on
// its behalf.
type Delegation struct {
	ID            string         `json:"id"`
	Agent         string         `json:"agent"`
	Ephemeral     *EphemeralSpec `json:"ephemeral,omitempty"`
	Action        string         `json:"action"`
	Worktree      string         `json:"worktree,omitempty"`
	Prompt        string         `json:"prompt"`
	Artifacts     []string       `json:"artifacts,omitempty"`
	Deps          []string       `json:"deps,omitempty"`
	Timeout       string         `json:"timeout,omitempty"`
	Retry         int            `json:"retry,omitempty"`
	FailurePolicy string         `json:"failure_policy,omitempty"`
	Fingerprint   string         `json:"fingerprint,omitempty"`
	SynthesisRule string         `json:"synthesis_rule,omitempty"`
	Quorum        int            `json:"quorum,omitempty"`
	Model         string         `json:"model,omitempty"`
	Phase         string         `json:"phase,omitempty"`
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

// DelegationValidationError is a per-field validation failure on a single
// delegation. It carries the delegation's 0-based position and id so that, when
// multiple field errors are aggregated with errors.Join, the coordinator can see
// exactly which entry and which field each failure refers to and repair the whole
// batch in one round.
type DelegationValidationError struct {
	Index int    // 0-based position in delegations[]
	ID    string // delegation id; "" when missing/blank
	Field string // "id" | "action" | "prompt"
	Msg   string // e.g. "is required"
}

func (e DelegationValidationError) Error() string {
	id := e.ID
	if strings.TrimSpace(id) == "" {
		id = "<missing>"
	}
	return fmt.Sprintf("delegations[%d] (id %q): %s %s", e.Index, id, e.Field, e.Msg)
}

// delegationFieldError builds a DelegationValidationError for the delegation at
// index i, annotated with its id and the offending field/message.
func delegationFieldError(i int, d Delegation, field, msg string) error {
	return DelegationValidationError{Index: i, ID: d.ID, Field: field, Msg: msg}
}

func validateAgentResult(result AgentResult) error {
	if _, ok := allowedSet(ResultDecisions)[result.Decision]; !ok {
		return fmt.Errorf("unsupported gitmoot_result decision %q", result.Decision)
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("gitmoot_result summary is required")
	}
	var errs []error
	for i, d := range result.Delegations {
		if strings.TrimSpace(d.ID) == "" {
			errs = append(errs, delegationFieldError(i, d, "id", "is required"))
		} else if d.ID != strings.TrimSpace(d.ID) {
			// The engine keys child jobs, the dedup map, and the DAG on the raw id
			// while deps are matched trimmed; a surrounding-whitespace id would make
			// those disagree and silently drop the delegation. Require them equal.
			errs = append(errs, delegationFieldError(i, d, "id", "must not have leading or trailing whitespace"))
		}
		if err := validateDelegationTarget(d); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(d.Action) == "" {
			errs = append(errs, delegationFieldError(i, d, "action", "is required"))
		}
		if strings.TrimSpace(d.Prompt) == "" {
			errs = append(errs, delegationFieldError(i, d, "prompt", "is required"))
		}
		if err := validateDelegationLifecycle(d); err != nil {
			errs = append(errs, err)
		}
	}
	// Aggregate every per-delegation field failure before the graph check, which
	// assumes well-formed ids.
	if joined := errors.Join(errs...); joined != nil {
		return joined
	}
	if err := validateDelegationGraph(result.Delegations); err != nil {
		return err
	}
	// A quorum threshold larger than the number of delegations can never be
	// satisfied (at most len(delegations) children can approve), which would block
	// the coordinator forever. Reject it at extraction with a clear message rather
	// than letting the gate block silently. (Per-delegation validation already
	// guarantees each quorum delegation has Quorum > 0, so the threshold is >= 1.)
	if delegationSynthesisRequiresQuorum(result.Delegations) {
		if k := delegationQuorumThreshold(result.Delegations); k > len(result.Delegations) {
			return fmt.Errorf("synthesis_rule quorum requires quorum (%d) to be <= the number of delegations (%d)", k, len(result.Delegations))
		}
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
	failurePolicies := allowedSet(DelegationFailurePolicies)
	if fp := strings.ToLower(strings.TrimSpace(d.FailurePolicy)); fp != "" {
		if _, ok := failurePolicies[fp]; !ok {
			return fmt.Errorf("delegation %q failure_policy %q is invalid", d.ID, d.FailurePolicy)
		}
	}
	synthesisRules := allowedSet(DelegationSynthesisRules)
	if sr := strings.ToLower(strings.TrimSpace(d.SynthesisRule)); sr != "" {
		if _, ok := synthesisRules[sr]; !ok {
			return fmt.Errorf("delegation %q synthesis_rule %q is invalid", d.ID, d.SynthesisRule)
		}
		if sr == "quorum" && d.Quorum <= 0 {
			return fmt.Errorf("delegation %q synthesis_rule quorum requires quorum > 0", d.ID)
		}
	}
	return nil
}

// validateDelegationTarget enforces that a delegation routes to EXACTLY ONE of a
// pre-registered agent or an inline ephemeral worker spec. A delegation with both
// is ambiguous (which identity owns the branch lock and repo scope?), and one with
// neither has no executor at all; both are rejected at result-parse time before any
// side effects. When an ephemeral spec is present, its runtime must be one of
// codex/claude/kimi (never shell or empty) because the daemon materializes a real
// runtime worker from it.
func validateDelegationTarget(d Delegation) error {
	hasAgent := strings.TrimSpace(d.Agent) != ""
	hasEphemeral := d.Ephemeral != nil
	switch {
	case hasAgent && hasEphemeral:
		return fmt.Errorf("delegation %q sets both agent and ephemeral; exactly one is allowed", d.ID)
	case !hasAgent && !hasEphemeral:
		return fmt.Errorf("delegation %q must set exactly one of agent or ephemeral", d.ID)
	}
	if hasEphemeral {
		if err := validateEphemeralSpec(d.ID, d.Ephemeral); err != nil {
			return err
		}
	}
	return nil
}

// validateEphemeralSpec validates the inline worker spec on a delegation: the
// runtime is required and must be a real agent runtime (codex/claude/kimi); shell
// and unknown runtimes are rejected so an ephemeral worker is never a raw shell.
func validateEphemeralSpec(delegationID string, spec *EphemeralSpec) error {
	if spec == nil {
		return nil
	}
	switch rt := strings.TrimSpace(spec.Runtime); {
	case rt == "":
		return fmt.Errorf("delegation %q ephemeral runtime is required", delegationID)
	default:
		if _, ok := allowedSet(EphemeralRuntimes)[rt]; !ok {
			return fmt.Errorf("delegation %q ephemeral runtime %q is invalid; expected one of codex/claude/kimi", delegationID, spec.Runtime)
		}
	}
	// Reject an unknown autonomy_policy at parse time: an unrecognized value
	// would otherwise normalize to "auto" (writable) downstream and silently
	// defeat the least-privilege read-only default.
	if policy := strings.TrimSpace(spec.AutonomyPolicy); policy != "" {
		if _, err := runtime.NormalizeAutonomyPolicy(policy); err != nil {
			return fmt.Errorf("delegation %q ephemeral autonomy_policy %q is invalid", delegationID, spec.AutonomyPolicy)
		}
	}
	return nil
}

// ephemeralAgentName derives the synthetic agent name for an ephemeral delegation
// child from the delegation id and parent job id. The name always contains the
// literal infix "-ephemeral-" so the TUI can filter ephemeral workers off that
// marker, and the trailing short hash of (parentJobID+delegationID) keeps the name
// stable and unique per (parent, delegation) pair.
func ephemeralAgentName(delegationID, parentJobID string) string {
	return slugForName(delegationID) + "-ephemeral-" + shortHash(parentJobID+delegationID)
}

// slugForName lowercases an id and reduces it to a short, name-safe slug
// (alphanumerics and dashes), so the ephemeral agent name stays readable while
// the trailing hash guarantees uniqueness.
func slugForName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && builder.Len() > 0 {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "ephem"
	}
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	return slug
}

// shortHash returns a short, stable hex fingerprint of value used as the unique
// suffix on an ephemeral agent name.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:10]
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
