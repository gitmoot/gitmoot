package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

const resultKey = `"gitmoot_result"`

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

// ChecksBlockedKinds are the allowed values of AgentResult.ChecksBlocked: why a
// reviewer could not run some or all of the project's checks (#2314). One
// source so the prompt and the validator agree, and so the blocker table that
// decides what moves to disposable VMs is counted rather than searched.
var ChecksBlockedKinds = []string{
	ChecksBlockedToolchain,
	ChecksBlockedDependencies,
	ChecksBlockedSandbox,
	ChecksBlockedNetwork,
	ChecksBlockedService,
	ChecksBlockedOther,
}

// SessionJobActions is the set of job types a session job may be opened,
// recorded or closed as (`gitmoot job open|close|record`, `agent prompt
// --record`). It keeps "implement" because a seat that implements in its own
// session records that work as an implement job — the live traffic this surface
// carries — even though nothing in Gitmoot dispatches one.
var SessionJobActions = []string{"ask", "review", "implement"}

// allowedSet returns a lookup set for the given allowed-value slice.
func allowedSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		set[v] = struct{}{}
	}
	return set
}

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
	ArtifactBody string            `json:"artifact_body,omitempty"`
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
	// EvidenceDeclared records whether the PRODUCER sent an evidence value, as
	// distinct from the engine defaulting one. It is ENGINE-OWNED: normalizeAgentResult
	// assigns it unconditionally, so a producer that sends it is overwritten and
	// cannot assert its own provenance. Same idiom as ReviewStatusGrade - a fact
	// about where a value came from does not come from the value's author.
	//
	// It exists because the default above is safe but LOSSY, and the loss is
	// large: measured over 3,431 succeeded review jobs, 3,192 of them (93%,
	// 14.328 G tokens) omit evidence entirely, 198 declare executed and 41
	// declare static_only. So "I could not execute anything" and "I have never
	// heard of this field" stored the same value, which makes the field
	// uninformative in aggregate and makes any policy built on it unsafe: a
	// refusal on static_only today would refuse 93% of all reviews, the
	// overwhelming majority of which never made the claim at all.
	//
	// It changes no policy. Evidence still normalises to static_only, and
	// EvidenceWasExecuted still answers the same question. This only makes the
	// silence countable, which is the prerequisite for anyone acting on it.
	EvidenceDeclared bool `json:"evidence_declared,omitempty"`
	// ChecksBlocked names WHY a reviewer could not run some or all of the
	// project's checks, one of ChecksBlockedKinds (#2314). It is optional and
	// carries no policy: Evidence still says whether anything was executed. It
	// exists so the reasons reviews end static-only can be counted from stored
	// results instead of searched for in summaries. A non-empty value outside
	// the enum is rejected, like Evidence, so a misspelling is a visible error
	// rather than an uncountable row.
	ChecksBlocked string `json:"checks_blocked,omitempty"`
}

// The two values Evidence accepts. A closed pair of strings rather than a
// bool: both read correctly in a stored result and in an operator surface,
// while `executed: false` invites being read as a missing field.
const (
	EvidenceExecuted   = "executed"
	EvidenceStaticOnly = "static_only"
)

// The values ChecksBlocked accepts.
const (
	// ChecksBlockedToolchain: the language toolchain or test runner is not
	// available or not runnable (no python3/pytest, cargo, node, ...).
	ChecksBlockedToolchain = "toolchain"
	// ChecksBlockedDependencies: the toolchain ran but the project's
	// dependencies were missing (empty module cache, packages not installed).
	ChecksBlockedDependencies = "dependencies"
	// ChecksBlockedSandbox: the sandbox refused a path or device the check
	// needed (a write outside the seat, /dev/ptmx).
	ChecksBlockedSandbox = "sandbox"
	// ChecksBlockedNetwork: the check needed network access it did not have.
	ChecksBlockedNetwork = "network"
	// ChecksBlockedService: the check needed an external service (a database,
	// a browser, a credentialed API).
	ChecksBlockedService = "service"
	ChecksBlockedOther   = "other"
)

// ValidChecksBlocked reports whether value is one of ChecksBlockedKinds.
func ValidChecksBlocked(value string) bool {
	value = strings.TrimSpace(value)
	for _, kind := range ChecksBlockedKinds {
		if value == kind {
			return true
		}
	}
	return false
}

func validateChecksBlocked(result AgentResult) error {
	if strings.TrimSpace(result.ChecksBlocked) != "" && !ValidChecksBlocked(result.ChecksBlocked) {
		return fmt.Errorf("gitmoot_result checks_blocked %q is not recognised; use one of %s", result.ChecksBlocked, strings.Join(ChecksBlockedKinds, ", "))
	}
	return nil
}

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

// EvidenceWasDeclared reports whether the producer actually stated its evidence
// mode, rather than the engine defaulting it to static_only.
//
// A consumer that wants to ACT on static_only - refuse it, escalate it, count
// it - must ask this too. Without it, an honest reviewer reporting that it could
// run nothing is indistinguishable from a producer that has never emitted the
// field, and on this store the second group is 93% of succeeded reviews.
func EvidenceWasDeclared(r AgentResult) bool {
	return r.EvidenceDeclared
}

// authorityGrantingResultFields are AgentResult fields an agent may NEVER supply,
// because setting them GRANTS the job authority it would not otherwise have (#1673).
//
// The distinction is not "product-owned". EvidenceDeclared is product-owned too and is
// safe to accept, because normalization overwrites whatever an agent supplies.
// SupersededPullRequestClosed is the opposite: it tells the retry actuator that this
// child's pull request is gone, which authorizes a route that SKIPS the
// delivery-checkout preflight. An agent that could set it could bypass a safety
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
	// #1507 fix 3: name the cause when the output is TRUNCATED rather than
	// merely envelope-less. "missing valid gitmoot_result JSON object" told an
	// operator nothing about a 3.8 KB verdict that was one closing brace short.
	return AgentResult{}, unterminatedEnvelopeError(output)
}

// extractAgentResultForAction applies action-specific contract rules after the
// generic wire object is parsed. Only review jobs require a severity.
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
	if err := validateChecksBlocked(result); err != nil {
		return err
	}
	return nil
}

// retiredAgentResultFields are fields an older prompt contract offered and the
// engine no longer acts on (#2588): delegations (Orchestra fan-out) and
// human_questions (the ask-gate pause answered by `job answer`). Refusing them
// BY NAME keeps a result that still carries one a clear contract failure rather
// than a silently dropped request.
var retiredAgentResultFields = map[string]string{
	"delegations":     "gitmoot no longer dispatches delegations; do the work in this job or report it in summary and needs, and omit delegations",
	"human_questions": "gitmoot no longer pauses a job for human answers; report open questions in needs and omit human_questions",
}

func validateAgentResultFields(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for field := range fields {
		if why, retired := retiredAgentResultFields[field]; retired {
			return fmt.Errorf("unsupported gitmoot_result field %q: %s", field, why)
		}
		if _, ok := agentResultAllowedFields[field]; !ok {
			return fmt.Errorf("unsupported gitmoot_result field %q", field)
		}
	}
	return nil
}

func validateAgentResult(result AgentResult) error {
	if _, ok := allowedSet(ResultDecisions)[result.Decision]; !ok {
		return fmt.Errorf("unsupported gitmoot_result decision %q", result.Decision)
	}
	if err := validateAgentResultSeverity(result); err != nil {
		return err
	}
	if err := validateChecksBlocked(result); err != nil {
		return err
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("gitmoot_result summary is required")
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
	//
	// #1817's adoption measurement showed the default is also LOSSY: 3,192 of
	// 3,431 succeeded reviews omit the field, so an honest static_only and a
	// producer that never emitted one stored the same value. Capture DECLARED
	// before the default overwrites it. Assigned unconditionally, so a producer
	// that supplies evidence_declared itself is overwritten and cannot assert
	// its own provenance.
	result.EvidenceDeclared = ValidEvidence(result.Evidence)
	if !result.EvidenceDeclared {
		result.Evidence = EvidenceStaticOnly
	}
	// Validation accepts surrounding whitespace; store the bare kind so the
	// field counts cleanly (#2314).
	result.ChecksBlocked = strings.TrimSpace(result.ChecksBlocked)
	if result.Needs == nil {
		result.Needs = []string{}
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

// AgentResultRepairDelimiterBalance marks an envelope the ENGINE closed: the
// agent's bytes were one or more JSON closers short of parsing, and appending
// exactly the missing closers produced a schema-valid gitmoot_result (#1507).
//
// It is a marker on the engine's own envelope record, never a field inside the
// agent-authored gitmoot_result object. The agent did not write those closers;
// writing engine-derived data into the agent's object would blur the provenance
// boundary this marker exists to preserve, and would mean the stored object no
// longer matches raw_outputs - which is the audit trail the recovery relies on.
// A clean parse carries NO marker, and contract_version is untouched because a
// payload-side annotation is not a contract field.
const AgentResultRepairDelimiterBalance = "delimiter-balance"

// RetryCannotDiffer reports whether re-running a deterministic operation
// produced output byte-identical to the previous attempt.
//
// "A RETRY THAT CANNOT DIFFER IS NOT A RETRY" (#1507). It is deliberately
// general rather than welded to the result-parse path: any loop that re-runs an
// operation hoping for a different answer can consult it, and the caller decides
// what to do with the answer. Measured on this host's store, 51 of 63 retained
// malformed-envelope job deaths (81%) contained a byte-identical consecutive
// re-ask - each one a paid LLM round-trip that could not have succeeded.
//
// Empty previous output is NOT a match: there is nothing to have repeated.
func RetryCannotDiffer(previous, next string) bool {
	return previous != "" && previous == next
}

// jsonCloserDeficit returns the closers a truncated JSON document is missing,
// in the order that would terminate it, plus whether the scan ended INSIDE a
// string literal.
//
// A scan that ends mid-string is NOT repairable by appending closers: the value
// itself is cut, so any completion the engine invents is content rather than
// punctuation. The caller must refuse those.
func jsonCloserDeficit(output string) (string, bool) {
	inString := false
	escaped := false
	var stack []byte
	for index := 0; index < len(output); index++ {
		char := output[index]
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
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 {
				// More closers than openers: not a truncation, and appending
				// anything would be guesswork on a malformed document.
				return "", inString
			}
			stack = stack[:len(stack)-1]
		}
	}
	if inString {
		return "", true
	}
	closers := make([]byte, 0, len(stack))
	for index := len(stack) - 1; index >= 0; index-- {
		closers = append(closers, stack[index])
	}
	return string(closers), false
}

// unterminatedEnvelopeError names WHY the envelope did not parse when the cause
// is a truncation, instead of the generic "missing valid gitmoot_result JSON
// object" that told an operator nothing about a document that was one character
// short (#1507).
func unterminatedEnvelopeError(output string) error {
	closers, midString := jsonCloserDeficit(strings.TrimSpace(output))
	switch {
	case midString:
		return errors.New("unterminated string at EOF: the output ends inside a JSON string literal, so no closer can complete it")
	case closers != "":
		braces := strings.Count(closers, "}")
		brackets := strings.Count(closers, "]")
		switch {
		case brackets == 0:
			return fmt.Errorf("unterminated object: %d unclosed '{' at EOF", braces)
		case braces == 0:
			return fmt.Errorf("unterminated array: %d unclosed '[' at EOF", brackets)
		default:
			return fmt.Errorf("unterminated object: %d unclosed '{' and %d unclosed '[' at EOF", braces, brackets)
		}
	default:
		return errors.New("missing valid gitmoot_result JSON object")
	}
}

// extractAgentResultForActionAllowingRepair is the engine's parse seam for a
// delivered output. It returns the repair marker that applies to the accepted
// parse, or "" for a clean one.
//
// RANKING, which is the point rather than a detail: a CLEAN parse always wins.
// The delimiter close is attempted ONLY after the clean path has failed on the
// whole output, so an output carrying both a truncated envelope and a complete
// one parses clean and carries no marker (one such job exists in the measured
// record). A clean parse at any later attempt therefore also clears a marker set
// by an earlier one - the marker describes the accepted parse, not the job.
//
// The close is refused unless it yields a SCHEMA-VALID result for this action:
// balancing delimiters must not turn a contract violation into an accepted
// verdict.
func extractAgentResultForActionAllowingRepair(output, action string) (AgentResult, string, error) {
	result, err := extractAgentResultForAction(output, action)
	if err == nil {
		return result, "", nil
	}
	closers, midString := jsonCloserDeficit(strings.TrimSpace(output))
	if midString || closers == "" {
		return AgentResult{}, "", err
	}
	repaired, repairErr := extractAgentResultForAction(strings.TrimSpace(output)+closers, action)
	if repairErr != nil {
		// The deficit was real but closing it did not produce a valid contract
		// object. Report the ORIGINAL failure: the engine's speculative close is
		// not a fact about the agent's output.
		return AgentResult{}, "", err
	}
	return repaired, AgentResultRepairDelimiterBalance, nil
}
