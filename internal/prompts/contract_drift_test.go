package prompts_test

// These drift-guard tests live in the EXTERNAL test package (prompts_test) on
// purpose: the production prompts package must not import workflow (workflow
// already imports prompts via mailbox.go, so an in-package import would form a
// cycle). The external test package may import both, which lets it reflect over
// the real result structs and assert the rendered prompt covers every field.
//
// Layer (a): field-coverage — every JSON field name of the contract structs
//            must appear in the rendered job/repair prompts. A NEW struct field
//            that the generator/prompt does not document fails this test.
// Layer (c): golden — the battle-tested phrasings agents rely on stay verbatim,
//            and the {"gitmoot_result":{…}} example literal stays byte-identical.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/prompts"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// jsonFieldNames returns the JSON field names declared on v's struct type, in
// declaration order, skipping json:"-" and unexported fields.
func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

func renderedJob() string {
	return prompts.RenderJob(prompts.JobPrompt{
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-422",
		Action:       "implement",
		Instructions: "single-source the contract",
	})
}

// normalizationOwnedResultFields are AgentResult JSON fields the PRODUCT sets and
// never asks an agent for. They are deliberately absent from the prompt: naming
// a field there is an invitation to return it, and these carry no meaning coming
// from an agent.
//
// The exclusion is by exact name, so a rename still fails the drift guard — the
// field simply reappears as uncovered under its new name, which is the outcome
// this test exists to force.
var normalizationOwnedResultFields = map[string]string{
	// Set by the closed-PR sweep on the SYNTHETIC result it mints for a delegation
	// child whose pull request is no longer open (#1673). The retry actuator reads it
	// to know a delivery-checkout preflight can never validate for that child, and it
	// lives in the result so RetryJob clears it along with the result. An agent
	// claiming it would be asserting a lifecycle fact the product owns.
	"superseded_pull_request_closed": "set by the closed-pull-request sweep, never requested from an agent",
	// Assigned by normalizeAgentResult from whether the producer sent `evidence`
	// at all, so an honest static_only stays distinguishable from a producer that
	// never emitted the field (#1817). Naming it in the prompt would invite an
	// agent to assert its own provenance, which is the one thing it must not do:
	// normalization overwrites any supplied value for exactly that reason.
	"evidence_declared": "assigned by result normalization from the producer's own silence, never requested from an agent",
	// Kept on AgentResult only to read historical results (#2588): agents are
	// no longer offered delegation, and a result carrying delegations fails
	// the result check.
	"delegations": "no longer offered to agents; kept only to read historical results",
	// Only ever required by delegations that requested artifacts (#2588).
	"artifact_body": "only required by delegations, which are no longer offered to agents",
}

func contractFieldNames() []string {
	var fields []string
	for _, field := range jsonFieldNames(workflow.AgentResult{}) {
		if _, owned := normalizationOwnedResultFields[field]; owned {
			continue
		}
		fields = append(fields, field)
	}
	fields = append(fields, jsonFieldNames(workflow.HumanQuestion{})...)
	return fields
}

// TestNormalizationOwnedFieldsStayOutOfThePrompt is the other half of the
// exclusion above: a field the product owns must not appear in the prompt, or
// agents will start returning it. Without this, the exclusion list would be a
// silent hole rather than a stated contract.
func TestNormalizationOwnedFieldsStayOutOfThePrompt(t *testing.T) {
	prompt := renderedJob()
	for field, why := range normalizationOwnedResultFields {
		if strings.Contains(prompt, field) {
			t.Fatalf("prompt names %q, which is %s; agents must not be asked for it", field, why)
		}
	}
}

// TestContractFieldsCoveredInJobPrompt is the load-bearing drift guard: every
// offered JSON field of AgentResult and HumanQuestion must be
// named in the rendered job prompt. Adding a field to any of those structs
// without teaching the generator (and thus the prompt) about it fails here.
func TestContractFieldsCoveredInJobPrompt(t *testing.T) {
	prompt := renderedJob()

	for _, field := range contractFieldNames() {
		if !strings.Contains(prompt, field) {
			t.Fatalf("rendered job prompt does not mention contract field %q; "+
				"regenerate internal/prompts/contract_generated.go (go generate ./...) "+
				"and add an annotation for it in internal/prompts/contractgen\n--- prompt ---\n%s", field, prompt)
		}
	}
}

// TestContractFieldsCoveredInRepairPrompt extends the same coverage guarantee to
// the repair prompt, which must teach the same field set so a coordinator can
// fix a malformed batch in one round.
func TestContractFieldsCoveredInRepairPrompt(t *testing.T) {
	prompt := prompts.RenderRepairPrompt("garbage", nil)

	for _, field := range contractFieldNames() {
		if !strings.Contains(prompt, field) {
			t.Fatalf("rendered repair prompt does not mention contract field %q\n--- prompt ---\n%s", field, prompt)
		}
	}
}

// TestEnumValuesCoveredInJobPrompt asserts that the exported validator allowed
// sets (the single source of truth) all surface in the prompt prose, so the
// enums an agent is told about match exactly what the validator accepts.
func TestEnumValuesCoveredInJobPrompt(t *testing.T) {
	prompt := renderedJob()

	var values []string
	values = append(values, workflow.ResultDecisions...)
	values = append(values, workflow.ChecksBlockedKinds...)

	for _, v := range values {
		if !strings.Contains(prompt, v) {
			t.Fatalf("rendered job prompt omits allowed enum value %q\n--- prompt ---\n%s", v, prompt)
		}
	}
}

// TestExampleShapeIsByteIdentical pins the {"gitmoot_result":{…}} example
// literal. Agents copy this verbatim, so it must never change shape; the
// generator reproduces it from the struct field set.
func TestExampleShapeIsByteIdentical(t *testing.T) {
	const want = `{"gitmoot_result":{"decision":"approved|changes_requested|blocked|implemented|failed|skipped","summary":"...","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`

	for name, prompt := range map[string]string{
		"job":    renderedJob(),
		"repair": prompts.RenderRepairPrompt("garbage", nil),
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("%s prompt missing byte-identical example shape\nwant: %s\n--- prompt ---\n%s", name, want, prompt)
		}
	}
}

func TestSkippedDecisionSemanticsAreRendered(t *testing.T) {
	for name, prompt := range map[string]string{
		"job":    renderedJob(),
		"repair": prompts.RenderRepairPrompt("garbage", nil),
	} {
		for _, want := range []string{
			"task itself had no work to do",
			"Do not use skipped in a PR review",
			"skipped is an abstention for quorum and verify",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("%s prompt missing %q\n--- prompt ---\n%s", name, want, prompt)
			}
		}
	}
}
