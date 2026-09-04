package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const resultKey = `"gitmoot_result"`

// PipelineJobSender is the Sender stamped on every #681 pipeline stage job. It is
// the single source of truth shared by the CLI (which builds the stage
// JobRequest) and the mailbox (which strips a stage result's delegations[] so a
// pipeline stage is provably a LEAF — see Mailbox.Run). A pipeline stage carries
// an EMPTY ParentJobID, so its own result-borne delegations would otherwise
// dispatch as top-level jobs; stripping them here keeps stages leaves and matches
// the advancer, which ignores stage delegations entirely.
const PipelineJobSender = "pipeline"

// The following slices are the single source of truth for the enum-valued
// fields of the gitmoot_result contract. The validator below checks against
// them, and the build-time contract generator (internal/prompts/contractgen)
// reflects over them so the prompt prose and the validator can never drift
// apart. Keep them in declaration order — the generated prose lists them in
// the same order.

// ResultDecisions are the allowed values of AgentResult.Decision.
var ResultDecisions = []string{"approved", "changes_requested", "blocked", "implemented", "failed", "skipped"}

// ReviewSeverities are ordered from most to least severe. A review returning
// changes_requested must report the highest-severity finding through this
// engine-readable field rather than only in prose.
var ReviewSeverities = reviewseverity.Values

// EvidenceKinds are the allowed values of AgentResult.Evidence, ordered from
// strongest to weakest. One source so the prompt, the validator and the gate
// cannot disagree about what a reviewer may say.
var EvidenceKinds = []string{EvidenceExecuted, EvidenceStaticOnly}

// DelegationActions is the canonical set of delegation/session job actions.
var DelegationActions = []string{"ask", "review", "implement"}

// DelegationFailurePolicies are the allowed values of Delegation.FailurePolicy
// (the empty string falls back to the default and is accepted separately).
var DelegationFailurePolicies = []string{"block_parent", "continue", "escalate", "escalate_human"}

// DelegationSynthesisRules are the allowed values of Delegation.SynthesisRule
// (the empty string falls back to the default and is accepted separately).
var DelegationSynthesisRules = []string{"summary", "vote", "quorum", "verify"}

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
	Effort         string   `json:"effort,omitempty"`
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
	ReviewScope   *ReviewScope   `json:"review_scope,omitempty"`
	Artifacts     []string       `json:"artifacts,omitempty"`
	Deps          []string       `json:"deps,omitempty"`
	Timeout       string         `json:"timeout,omitempty"`
	Retry         int            `json:"retry,omitempty"`
	FailurePolicy string         `json:"failure_policy,omitempty"`
	Fingerprint   string         `json:"fingerprint,omitempty"`
	SynthesisRule string         `json:"synthesis_rule,omitempty"`
	Quorum        int            `json:"quorum,omitempty"`
	Model         string         `json:"model,omitempty"`
	Effort        string         `json:"effort,omitempty"`
	Phase         string         `json:"phase,omitempty"`
}

// HumanQuestion is a single mid-run clarifying question a healthy
// worker/coordinator can return to pause its tree for a specific human decision
// (#445). It is orthogonal to AgentResult.Decision: an agent may return one or
// more on ANY healthy decision instead of guessing. ID is a stable handle the
// human answers against (`/gitmoot resume <job> answer "<id>: ..."`), Prompt is
// the question text, and Choices are optional suggested answers.
type HumanQuestion struct {
	ID      string   `json:"id"`
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
}

// Learning is one durable, keyed fact an agent may return in gitmoot_result at
// job end (#626). Each entry is a single self-contained fact ("this repo's
// arm64 CI is flaky"), NOT a directive. Key is a short stable handle used for
// keyed dedup/upsert in the memory store; Scope is "repo" (a fact about this
// repository, the default) or "general" (a fact that travels with the owner
// into every repository); Content is the fact itself. In Phase 1 (observation
// mode) returned learnings are SHADOW-logged to memory_observations only — never
// injected back into any prompt and never promoted.
type Learning struct {
	Key     string `json:"key"`
	Scope   string `json:"scope,omitempty"`
	Content string `json:"content"`
}

// LearningScopes are the allowed values of Learning.Scope (the empty string
// falls back to the default "repo" and is accepted separately).
var LearningScopes = []string{"repo", "general"}

// ResultStringList is a gitmoot_result list of strings that also accepts OBJECT
// elements, because agents send them and a strict element decode failed the
// whole envelope.
//
// Measured: review job local-review-gm-review-opus-18d1b91052453964 returned
//
//	"tests_run":[{"name":"PR diff scope confirmation","command":"git show --stat 663321c9...","result":"pass","detail":"..."}]
//
// and the decode failed with "json: cannot unmarshal object into Go struct
// field AgentResult.tests_run of type string", which burned repair_retry
// attempt 1 of 2. The job had done the work and reported it in a richer shape;
// the parser made that indistinguishable from returning nothing.
//
// An object element is re-encoded as compact JSON rather than reduced to one of
// its fields. The contract names no element fields, so picking "command" or
// "name" would be a guess, and dropping the rest would lose what the agent
// measured. Consumers treat entries as opaque strings (they are printed,
// written into PR comments, counted, and digested), so a JSON-shaped entry is
// legible and lossless.
//
// Nothing else widens. A number, a boolean, null in place of the array, or a
// bare string where an array belongs still fails: there is no evidence agents
// send those, and accepting them would change the contract on a guess. The
// prompt contract keeps asking for strings.
type ResultStringList []string

func (l *ResultStringList) UnmarshalJSON(data []byte) error {
	if l == nil {
		return errors.New("unmarshal into nil ResultStringList")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(trimmed, &elements); err != nil {
		return err
	}
	list := make(ResultStringList, 0, len(elements))
	for _, element := range elements {
		// No TrimSpace and no empty-element guard: encoding/json hands each
		// element as a complete JSON value with no surrounding whitespace and
		// never zero-length, so both were unreachable and no mutant could kill
		// them (#1809 review, P3-5). Measured rather than reasoned: decoding
		// `[ "a" , "b" ]` and a multi-line array yields elements "a" and "b"
		// with no padding. A guard nothing can reach is a claim about the input
		// that the input does not make.
		switch element[0] {
		case 'n':
			// A null element decoded to an EMPTY ENTRY before #1805 (verified
			// differentially at parent 0965c458: ["a",null,"b"] produced
			// ["a","","b"]). Rejecting it would narrow the contract in the same
			// breath as widening it, and burn a repair attempt on an envelope
			// that used to parse - the exact failure #1805 exists to remove.
			// Only the literal null reaches here; anything else beginning with
			// 'n' falls through to the default arm below.
			if !bytes.Equal(element, []byte("null")) {
				return fmt.Errorf("gitmoot_result list element must be a string or an object, got %s", string(element))
			}
			list = append(list, "")
		case '"':
			var value string
			if err := json.Unmarshal(element, &value); err != nil {
				return err
			}
			list = append(list, value)
		case '{':
			// Re-encode through a map so key order is canonical (encoding/json
			// sorts map keys) and two agents reporting the same object produce
			// the same entry.
			//
			// DUPLICATE KEYS ARE REJECTED, not collapsed. map[string]json.RawMessage
			// silently keeps the LAST duplicate, so [{"name":"first","name":"second"}]
			// used to decode without error to {"name":"second"} - losing a field while
			// the doc claimed every field is kept. Duplicate keys are exactly the
			// malformed-but-parseable JSON a runtime emits, and silently dropping
			// data is the one option that is wrong; rejecting is visible and the
			// repair prompt can act on it.
			object, err := decodeObjectElementRejectingDuplicates(element)
			if err != nil {
				return err
			}
			// SetEscapeHTML(false): json.Marshal escapes <, > and & to \u003c,
			// \u003e and \u0026, so a recorded command like `go test ./... 2>&1`
			// came back as `go test ./... 2\u003e\u00261` and could not be
			// copy-pasted back. A tests_run entry that is not the command that ran
			// is evidence damage.
			encoded, err := marshalCanonicalNoHTMLEscape(object)
			if err != nil {
				return err
			}
			list = append(list, encoded)
		default:
			return fmt.Errorf("gitmoot_result list element must be a string or an object, got %s", string(element))
		}
	}
	*l = list
	return nil
}

// decodeObjectElementRejectingDuplicates decodes one object element and refuses
// duplicate keys rather than letting map assignment keep the last one.
//
// It streams tokens instead of unmarshalling into a map because a map cannot
// represent the duplicate at all - by the time you hold the map, the evidence is
// already gone.
func decodeObjectElementRejectingDuplicates(element []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(element))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("gitmoot_result list element is not an object: %s", string(element))
	}
	object := map[string]json.RawMessage{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("gitmoot_result list element has a non-string key: %v", keyToken)
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("gitmoot_result list element has duplicate key %q: %s", key, string(element))
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return object, nil
}

// marshalCanonicalNoHTMLEscape marshals with map-key ordering (canonical, so two
// agents reporting the same object produce the same entry) but WITHOUT HTML
// escaping, so shell metacharacters in a recorded command survive verbatim.
func marshalCanonicalNoHTMLEscape(object map[string]json.RawMessage) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(object); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buffer.Bytes(), "\n")), nil
}

// nameFailingListField restores the FIELD NAME in a list-element decode error.
//
// Pre-#1805, encoding/json produced "cannot unmarshal object into Go struct
// field AgentResult.tests_run", and the repair prompt used that name to tell the
// agent which list to fix. ResultStringList.UnmarshalJSON returns its own error,
// which encoding/json passes through unwrapped, so the name was lost - and a
// repair attempt that cannot see WHICH field failed is a wasted attempt, the
// same cost #1805 exists to save (#1809 review, P3-4).
//
// It re-decodes each list field ALONE to find the offender rather than parsing
// the error string, so it cannot drift when a message is reworded. If no single
// field reproduces the failure the original error is returned unchanged: naming
// the wrong field would be worse than naming none.
func nameFailingListField(raw json.RawMessage, original error) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return original
	}
	for _, name := range []string{"changes_made", "tests_run", "needs"} {
		value, present := fields[name]
		if !present {
			continue
		}
		var list ResultStringList
		if err := json.Unmarshal(value, &list); err != nil {
			return fmt.Errorf("gitmoot_result field %s: %w", name, err)
		}
	}
	return original
}

type AgentResult struct {
	Decision string `json:"decision"`
	// SupersededPullRequestClosed marks a SYNTHETIC result minted by the closed-PR
	// sweep for a delegation child whose pull request is no longer open (#1673).
	//
	// It rides in the RESULT rather than in the event log on purpose. The retry actuator
	// needs to know that this child can never satisfy a delivery-checkout preflight, and
	// the event log cannot answer that question safely: job_events carries no lifecycle
	// generation, and RetryJob preserves prior events while starting a new run, so a
	// historical supersession would keep authorizing a bypass for a job that has since
	// been retried and failed normally. RetryJob clears Result, so this fact is cleared
	// with it - the staleness is IMPOSSIBLE rather than filtered.
	SupersededPullRequestClosed bool `json:"superseded_pull_request_closed,omitempty"`
	// Severity is the highest-severity review finding. It is optional for
	// non-review results and approved reviews, but required for a review that
	// requests changes.
	Severity     string            `json:"severity,omitempty"`
	Summary      string            `json:"summary"`
	Findings     []json.RawMessage `json:"findings"`
	ChangesMade  ResultStringList  `json:"changes_made"`
	TestsRun     ResultStringList  `json:"tests_run"`
	Needs        ResultStringList  `json:"needs"`
	Delegations  []Delegation      `json:"delegations"`
	ArtifactBody string            `json:"artifact_body,omitempty"`
	// Learnings, when non-empty, carries durable keyed facts the agent chose to
	// record for its persistent memory (#626). It is fully additive: a result that
	// omits it (the overwhelming common case) behaves byte-identically (omitempty),
	// and in Phase 1 the entries are shadow-logged to memory_observations only —
	// never injected, never promoted. Each entry is validated (non-blank key and
	// content; scope in {repo,general} or empty→repo).
	Learnings []Learning `json:"learnings,omitempty"`
	// HumanQuestions, when non-empty, asks the engine to pause the parent task at
	// awaiting_human for a specific human answer (#445) — a non-failure sibling of
	// the escalate_human pause. It is fully additive: a result that omits it
	// behaves byte-identically (omitempty), and the pause enqueues no continuation
	// and consumes zero compute until the human answers via the `answer` resume
	// verb. The same escalation TTL backstop auto-finalizes an unanswered ask.
	HumanQuestions []HumanQuestion `json:"human_questions,omitempty"`
	// FanOut records that this result WAS a coordinator fan-out. It exists because
	// the executable delegations[] do not always survive to the consumers: a
	// pipeline leaf stage has its delegations STRIPPED at the mailbox seam so a
	// stage can never spawn phantom children, and an approved announcement then
	// reached auto-merge indistinguishable from a real leaf approval (#1685).
	// Stripping the INSTRUCTIONS must not erase the CLASSIFICATION.
	//
	// Normalization owns this field; agents are never asked for it. An agent that
	// sets it anyway can only REMOVE its own verdict's authority, never add any:
	// ResultIsFanOut ORs it with the delegations it can see, so `fan_out: false`
	// beside a declared panel is still a fan-out.
	FanOut bool `json:"fan_out,omitempty"`
	// Evidence declares whether this verdict's claims were EXECUTED or are
	// STATIC-ONLY, so a merge decision can tell a review that ran the gate from
	// one that read the diff and could not run anything.
	//
	// ABSENCE IS NOT AN ERROR: normalizeAgentResult records an omitted field as
	// EvidenceStaticOnly, because silence must never be readable as an execution
	// claim, and because a producer built against an older prompt cannot know to
	// send it. A NON-EMPTY value that is not one of EvidenceKinds IS rejected -
	// a misspelling would otherwise be silently downgraded to static_only and
	// look like modesty rather than a bug.
	//
	// A static-only verdict is LEGITIMATE and no check fails it. What fails is
	// claiming execution while naming nothing that was run.
	Evidence string `json:"evidence,omitempty"`
}

// The two values Evidence accepts. A closed pair of strings rather than a
// bool: both read correctly in a stored result and in an operator surface,
// while `executed: false` invites being read as a missing field.
const (
	EvidenceExecuted   = "executed"
	EvidenceStaticOnly = "static_only"
)

// ValidEvidence reports whether value is one of the declared evidence modes.
func ValidEvidence(value string) bool {
	switch strings.TrimSpace(value) {
	case EvidenceExecuted, EvidenceStaticOnly:
		return true
	default:
		return false
	}
}

// EvidenceWasExecuted reports whether a result declares EXECUTED evidence. A
// consumer that wants to rely on a verdict's test claims must ask this rather
// than reading TestsRun, because a static-only verdict can carry a populated
// TestsRun describing what it could NOT run.
func EvidenceWasExecuted(r AgentResult) bool {
	return strings.TrimSpace(r.Evidence) == EvidenceExecuted
}

// authorityGrantingResultFields are AgentResult fields an agent may NEVER supply,
// because setting them GRANTS the job authority it would not otherwise have (#1673).
//
// The distinction is not "product-owned". FanOut is product-owned too and is safe to
// accept, because an agent that sets it can only REMOVE its own authority - it marks a
// row as a fan-out. SupersededPullRequestClosed is the opposite: it tells the retry
// actuator that this child's pull request is gone, which authorizes a route that SKIPS
// the delivery-checkout preflight. An agent that could set it could bypass a safety
// boundary by asserting a lifecycle fact about itself.
//
// Excluding a field from the PROMPT does not stop this. A prompt omission stops a
// cooperative agent; the parser is what stops an incorrect or adversarial one.
var authorityGrantingResultFields = map[string]struct{}{
	"superseded_pull_request_closed": {},
}

// agentResultAllowedFields is the accepted-input roster: every AgentResult JSON tag
// MINUS the fields that would grant authority.
var agentResultAllowedFields = agentAcceptedResultFields()

func agentAcceptedResultFields() map[string]struct{} {
	fields := jsonTagSet(reflect.TypeOf(AgentResult{}))
	for field := range authorityGrantingResultFields {
		delete(fields, field)
	}
	return fields
}

// AllowedAgentResultFields returns a copy of the JSON fields accepted in a
// gitmoot_result object. It is derived from AgentResult so contract consumers
// can drift-check hand-authored result objects without duplicating the roster.
func AllowedAgentResultFields() map[string]struct{} {
	fields := make(map[string]struct{}, len(agentResultAllowedFields))
	for field := range agentResultAllowedFields {
		fields[field] = struct{}{}
	}
	return fields
}

// jsonTagSet returns the JSON field names declared directly by t. Embedded,
// unexported, and json-ignored fields are deliberately omitted: AgentResult is
// a flat wire object and only its exported tagged fields are accepted.
func jsonTagSet(t reflect.Type) map[string]struct{} {
	fields := make(map[string]struct{})
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous || field.PkgPath != "" {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = struct{}{}
	}
	return fields
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
				validationErr = nameFailingListField(raw, err)
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

// extractAgentResultForAction applies action-specific contract rules after the
// generic wire object is parsed. changes_requested is also a valid pipeline
// produce outcome, so only review jobs require a severity.
func extractAgentResultForAction(output, action string) (AgentResult, error) {
	result, err := ExtractAgentResult(output)
	if err != nil {
		return AgentResult{}, err
	}
	if err := validateAgentResultForAction(result, action); err != nil {
		return AgentResult{}, err
	}
	return result, nil
}

// validateAgentResultForAction enforces constraints that depend on the job
// action. Callers that receive an AgentResult directly use this after resolving
// the job action from durable state.
func validateAgentResultForAction(result AgentResult, action string) error {
	if err := validateAgentResultSeverity(result); err != nil {
		return err
	}
	if strings.TrimSpace(action) == "review" && result.Decision == "changes_requested" && result.Severity == "" {
		return errors.New("gitmoot_result severity is required when a review requests changes")
	}
	if strings.TrimSpace(result.Evidence) != "" && !ValidEvidence(result.Evidence) {
		return fmt.Errorf("gitmoot_result evidence %q is not recognised; use %q or %q", result.Evidence, EvidenceExecuted, EvidenceStaticOnly)
	}
	return nil
}

func validateAgentResultFields(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for field := range fields {
		if _, ok := agentResultAllowedFields[field]; !ok {
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
	if err := validateAgentResultSeverity(result); err != nil {
		return err
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("gitmoot_result summary is required")
	}
	if result.Decision == "skipped" && len(result.Delegations) > 0 {
		return errors.New("gitmoot_result decision skipped cannot be used with delegations")
	}
	if err := validateHumanQuestions(result.HumanQuestions); err != nil {
		return err
	}
	if err := validateLearnings(result.Learnings); err != nil {
		return err
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
		action := strings.TrimSpace(d.Action)
		if action == "" {
			errs = append(errs, delegationFieldError(i, d, "action", "is required"))
		} else if d.Action != action {
			errs = append(errs, delegationFieldError(i, d, "action", "must not have leading or trailing whitespace"))
		} else if !slices.Contains(DelegationActions, action) {
			errs = append(errs, delegationFieldError(i, d, "action", "must be one of "+strings.Join(DelegationActions, ", ")))
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

func validateAgentResultSeverity(result AgentResult) error {
	severity := strings.TrimSpace(result.Severity)
	if severity != "" && (result.Severity != severity || !reviewseverity.Valid(severity)) {
		return fmt.Errorf("unsupported gitmoot_result severity %q (want one of %s)", result.Severity, strings.Join(ReviewSeverities, ", "))
	}
	return nil
}

// validateHumanQuestions validates the optional ask-gate field (#445): each
// entry must carry a non-blank id and a non-blank prompt, and ids must be unique
// within the batch so the human's `<id>: text` answer lines map unambiguously to
// a question. An empty/absent slice is valid (the field is fully optional and
// the byte-identical default when omitted). Choices are free-form and need no
// validation.
func validateHumanQuestions(questions []HumanQuestion) error {
	if len(questions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(questions))
	for i, q := range questions {
		id := strings.TrimSpace(q.ID)
		if id == "" {
			return fmt.Errorf("human_questions[%d] id is required", i)
		}
		if strings.TrimSpace(q.Prompt) == "" {
			return fmt.Errorf("human_questions[%d] (id %q) prompt is required", i, id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("human_questions[%d] id %q is not unique", i, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateLearnings validates the optional #626 learnings field: each entry
// must carry a non-blank key and non-blank content, and scope (when set) must be
// one of {repo,general}. An empty/absent slice is valid (the field is fully
// optional and the byte-identical default when omitted).
func validateLearnings(learnings []Learning) error {
	if len(learnings) == 0 {
		return nil
	}
	scopes := allowedSet(LearningScopes)
	for i, l := range learnings {
		if strings.TrimSpace(l.Key) == "" {
			return fmt.Errorf("learnings[%d] key is required", i)
		}
		if strings.TrimSpace(l.Content) == "" {
			return fmt.Errorf("learnings[%d] (key %q) content is required", i, strings.TrimSpace(l.Key))
		}
		if scope := strings.ToLower(strings.TrimSpace(l.Scope)); scope != "" {
			if _, ok := scopes[scope]; !ok {
				return fmt.Errorf("learnings[%d] (key %q) scope %q is invalid; expected repo or general", i, strings.TrimSpace(l.Key), l.Scope)
			}
		}
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
		if err := validateEphemeralSpec(d.ID, d.Action, d.Ephemeral); err != nil {
			return err
		}
	}
	return nil
}

// validateEphemeralSpec validates the inline worker spec on a delegation: the
// runtime is required and must be a real agent runtime (codex/claude/kimi); shell
// and unknown runtimes are rejected so an ephemeral worker is never a raw shell.
//
// action is the delegation's action: an ephemeral implement worker (action
// "implement" or an explicit "implement" capability on the spec) whose
// autonomy_policy is empty/auto/read-only is fail-closed with the same guidance
// the CLI emits, because an empty policy normalizes to auto → no write headlessly
// (ties #451: coordinators must set an explicit write policy when fanning out
// ephemeral implement workers).
func validateEphemeralSpec(delegationID string, action string, spec *EphemeralSpec) error {
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
	// Fail-closed implement guard: an ephemeral implement worker with a non-write
	// policy (empty/auto/read-only) is rejected with the shared guidance. The spec's
	// capabilities default to the delegation action when omitted, so fold the action
	// into the capability set the predicate inspects.
	capabilities := spec.Capabilities
	if strings.TrimSpace(action) != "" {
		capabilities = append(append([]string{}, spec.Capabilities...), action)
	}
	if err := runtime.ImplementWritePolicyError(capabilities, spec.AutonomyPolicy); err != nil {
		return fmt.Errorf("delegation %q ephemeral %v", delegationID, err)
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
	// DEFENSE IN DEPTH. The field roster above already refuses this field as unknown
	// input, so reaching here with it set would mean a second parse path exists. Clearing
	// it costs nothing and removes the possibility that a future caller bypasses the
	// roster and silently grants itself the checkout-bypass route (#1673).
	result.SupersededPullRequestClosed = false

	if result.Findings == nil {
		result.Findings = []json.RawMessage{}
	}
	if result.ChangesMade == nil {
		result.ChangesMade = []string{}
	}
	if result.TestsRun == nil {
		result.TestsRun = []string{}
	}
	// #1839: evidence is ALWAYS one of the two declared values in a stored
	// result, and an omission becomes STATIC_ONLY rather than executed.
	//
	// This is the safe direction and it is not a compatibility shim: the stored
	// result is explicit either way, so no consumer has to interpret an absent
	// field, and nothing can read "we could not run the gate" as "the gate
	// passed". A reviewer that DID execute says so; silence is treated as the
	// weaker claim, because the alternative is inferring an execution that may
	// never have happened.
	if !ValidEvidence(result.Evidence) {
		result.Evidence = EvidenceStaticOnly
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
