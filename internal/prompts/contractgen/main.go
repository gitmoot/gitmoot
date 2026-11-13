// Command contractgen is the build-time single-source generator for the
// gitmoot_result reply contract that agents must follow.
//
// gitmoot's runtime adapters return raw text, so there is no in-process LLM
// layer to compile a typed signature into a prompt. Instead this standalone
// generator runs at build time (`go generate ./...`), imports the workflow
// package, and reflects over the result structs to recover the contract's field
// set and required/optional status. It then merges a small hand-curated
// annotation table (the enum/xor/conditional rules that JSON tags cannot
// express) and writes a checked-in internal/prompts/contract_generated.go.
//
// Why build-time and not runtime: the workflow package already imports prompts
// (mailbox.go calls prompts.RenderJob), so prompts cannot reflect over workflow
// at runtime without an import cycle. A separate `package main` tool can import
// workflow freely; its output is plain Go (package prompts) with no import of
// workflow at all, so the cycle never forms.
//
// Drift guard: every struct field MUST have an annotation. An un-annotated NEW
// field is a hard error here — that is the mechanism by which adding a field to
// AgentResult/HumanQuestion forces the prompt
// to be regenerated. CI
// re-runs `go generate ./... && git diff --exit-code` so a stale checked-in
// artifact fails the build.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"reflect"
	"strings"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// fieldAnnotation carries the hand-curated prose for a single JSON field that
// the struct tags cannot express on their own.
type fieldAnnotation struct {
	// example is the placeholder value rendered into the byte-identical
	// {"gitmoot_result":{…}} example literal. It is only consulted for the
	// non-omitempty AgentResult fields that appear in that literal.
	example string
	// help is the human-readable, one-line description rendered into the
	// result field help. Empty help means the field is documented
	// structurally (e.g. via the example literal) or not offered to agents.
	help string
}

// resultFieldAnnotations covers every JSON field of workflow.AgentResult. The
// example values reproduce the battle-tested literal shape verbatim; changing
// one here changes the shape agents copy, so treat them as load-bearing. A
// field with neither example nor help is not offered to agents at all.
var resultFieldAnnotations = map[string]fieldAnnotation{
	"decision":     {example: enumExample(workflow.ResultDecisions)},
	"severity":     {help: `top-level severity (string, one of ` + enumList(workflow.ReviewSeverities) + `): required when a review returns changes_requested; report the highest-severity finding. Omit it for non-review results and approved reviews with no findings.`},
	"summary":      {example: `"..."`},
	"findings":     {example: "[]"},
	"changes_made": {example: "[]"},
	"tests_run":    {example: "[]"},
	"needs":        {example: "[]"},
	// delegations is NOT offered to agents (#2588): reviewers no longer fan
	// out, and a result that still carries delegations fails the result
	// check. The field stays on AgentResult only to read historical results.
	"delegations": {},
	"evidence":    {help: `top-level evidence (string, one of ` + enumList(workflow.EvidenceKinds) + `): REQUIRED on reviews. Say "executed" only if you actually ran the commands you cite; say "static_only" when you read code and could not run it (no toolchain, no network, a sandbox refusal). Never infer test success from inability to execute, and never claim "executed" for a command you could not run - a static-only review is a legitimate verdict, a mislabeled one is not. Omitting it is recorded as static_only.`},
	// artifact_body was only required by delegations that requested
	// artifacts (#2588), so it is no longer offered to agents.
	"artifact_body":   {},
	"human_questions": {help: `top-level human_questions (object[], optional): use SPARINGLY to pause for a specific human decision instead of guessing; each entry is {` + humanQuestionFieldsHelp() + `}. Returning it pauses the tree awaiting a human answer (no leg fails, no continuation runs); a human replies with /gitmoot resume <job> answer "<id>: ...". Leave it absent when you can proceed.`},
	// fan_out is PRODUCT-OWNED and deliberately carries neither an example nor
	// help: agents are never asked for it, so advertising it in the prompt would
	// invite a value the product owns. It records that a result WAS a
	// coordinator fan-out, even on a stored row whose executable delegations[]
	// are absent (#1685). The empty entry still satisfies the
	// generator's every-field-accounted-for check, so a future rename cannot
	// silently drop it.
	"fan_out": {},
	// superseded_pull_request_closed is PRODUCT-OWNED for the same reason as fan_out,
	// so it carries neither an example nor help: no agent is ever asked for it. The
	// closed-PR sweep sets it on the SYNTHETIC result it mints for a delegation child
	// whose pull request is no longer open, and the retry actuator reads it to know a
	// delivery-checkout preflight can never validate for that child (#1673). It lives
	// in the result rather than the event log precisely so RetryJob clears it with the
	// result. Advertising it in the prompt would invite an agent to claim a lifecycle
	// fact the product owns. The empty entry still satisfies the every-field-accounted
	// -for check, so a future rename cannot silently drop it.
	"superseded_pull_request_closed": {},
	// evidence_declared is ENGINE-OWNED for the same reason, so it carries neither
	// an example nor help. normalizeAgentResult assigns it from whether the
	// producer sent `evidence` at all, BEFORE the safe default overwrites the
	// value, so an honest static_only stays distinguishable from a producer that
	// never emitted the field (#1817 - measured at 93% of succeeded reviews
	// omitting it, which is why a policy refusing static_only today would refuse
	// almost every review). Advertising it would invite an agent to assert its own
	// provenance, and provenance about a value must not come from that value's
	// author; normalization overwrites any supplied value for exactly that reason.
	// The empty entry still satisfies the every-field-accounted-for check, so a
	// future rename cannot silently drop it.
	"evidence_declared": {},
	"checks_blocked":    {help: `top-level checks_blocked (string, optional, one of ` + enumList(workflow.ChecksBlockedKinds) + `): when you could not run some or all of the project's checks, name the main reason: toolchain (the language toolchain or test runner is missing or not runnable), dependencies (it ran but the project's dependencies were missing), sandbox (the sandbox refused a path or device), network, service (a database, browser or credentialed API was needed), or other. Say what you could not run in summary or tests_run as well. Omit it when every check you wanted to run ran.`},
}

var humanQuestionFieldAnnotations = map[string]fieldAnnotation{
	"id":      {help: `id (string, required, unique)`},
	"prompt":  {help: `prompt (string, required)`},
	"choices": {help: `choices (string[], optional)`},
}

const resultDecisionHelp = "\nDecision semantics:\n- Use decision skipped only when the task itself had no work to do. Do not use skipped in a PR review to mean nothing to flag; use approved. - skipped is an abstention for quorum and verify. Vote still counts the skipped child's succeeded job state.\n"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "contractgen:", err)
		os.Exit(1)
	}
}

func run() error {
	// Validate every struct field is annotated. An un-annotated NEW field is a
	// hard failure — this is the drift mechanism.
	if err := requireAnnotations(reflect.TypeOf(workflow.AgentResult{}), resultFieldAnnotations, "AgentResult"); err != nil {
		return err
	}
	if err := requireAnnotations(reflect.TypeOf(workflow.HumanQuestion{}), humanQuestionFieldAnnotations, "HumanQuestion"); err != nil {
		return err
	}

	shape := renderResultShape()
	help := renderResultFieldHelp()

	src := renderFile(shape, help)
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}

	const out = "contract_generated.go"
	if err := os.WriteFile(out, formatted, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}

// jsonField returns (name, omitempty) for a struct field, or ("", false) when
// the field is unexported or json-ignored.
func jsonField(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	omitempty := false
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// requireAnnotations hard-fails if any exported JSON field of t lacks an entry
// in table. This is the "un-annotated NEW field hard-fails generation" rule.
func requireAnnotations(t reflect.Type, table map[string]fieldAnnotation, name string) error {
	seen := map[string]struct{}{}
	for i := 0; i < t.NumField(); i++ {
		field, _ := jsonField(t.Field(i))
		if field == "" {
			continue
		}
		seen[field] = struct{}{}
		if _, ok := table[field]; !ok {
			return fmt.Errorf("%s field %q has no contract annotation; add it to the generator's annotation table so the prompt stays in sync", name, field)
		}
	}
	// Also reject stale annotations for fields that no longer exist, so the
	// table cannot quietly drift the other way.
	for field := range table {
		if _, ok := seen[field]; !ok {
			return fmt.Errorf("%s annotation %q has no matching struct field; remove it from the generator's annotation table", name, field)
		}
	}
	return nil
}

// renderResultShape builds the byte-identical {"gitmoot_result":{…}} example
// literal from the AgentResult fields, in declaration order, skipping omitempty
// fields (which do not appear in the example shape agents copy) and fields
// with no example (which are not offered to agents).
func renderResultShape() string {
	t := reflect.TypeOf(workflow.AgentResult{})
	var parts []string
	for i := 0; i < t.NumField(); i++ {
		name, omitempty := jsonField(t.Field(i))
		if name == "" || omitempty || resultFieldAnnotations[name].example == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%q:%s", name, resultFieldAnnotations[name].example))
	}
	return `{"gitmoot_result":{` + strings.Join(parts, ",") + `}}`
}

// renderResultFieldHelp builds the prose help for the top-level result
// fields that the example shape cannot express.
func renderResultFieldHelp() string {
	var b strings.Builder
	b.WriteString("\nResult fields:\n")
	// severity is a top-level review field. It stays out of the generic example
	// shape because non-review results must not invent a review severity.
	if h := resultFieldAnnotations["severity"].help; h != "" {
		b.WriteString("- " + h + "\n")
	}
	if h := resultFieldAnnotations["evidence"].help; h != "" {
		b.WriteString("- " + h + "\n")
	}
	if h := resultFieldAnnotations["checks_blocked"].help; h != "" {
		b.WriteString("- " + h + "\n")
	}
	// #1839: the seat is HANDED prior verdicts and had no way to learn it.
	// Runtime agents receive only this prompt - not the skill docs - so an
	// artifact announced solely through an environment variable and an
	// operator-facing troubleshooting page is staged and undirected.
	b.WriteString("- if the environment variable GITMOOT_PRIOR_VERDICTS names a file, it holds the prior review verdicts for the repo under review, rendered as JSON and frozen at its own as_of time. READ IT before concluding there are none, and treat a verdict recorded after as_of as absent rather than missing. Each entry carries the reviewer, head_sha, decision, severity, findings count and that verdict's own evidence mode.\n")
	// human_questions is a top-level ask-gate field (#445), documented in the
	// prompt block the runtime agent actually receives.
	if h := resultFieldAnnotations["human_questions"].help; h != "" {
		b.WriteString("- " + h + "\n")
	}
	return b.String()
}

// enumExample renders an enum slice as the pipe-joined example value used in the
// result shape literal (e.g. "approved|changes_requested|...").
func enumExample(values []string) string {
	return `"` + strings.Join(values, "|") + `"`
}

// enumList renders an enum slice as a backtick-free, comma-joined prose list.
func enumList(values []string) string {
	return strings.Join(values, "|")
}

func humanQuestionFieldsHelp() string {
	return structFieldsHelp(reflect.TypeOf(workflow.HumanQuestion{}), humanQuestionFieldAnnotations)
}

func structFieldsHelp(t reflect.Type, annotations map[string]fieldAnnotation) string {
	var parts []string
	for i := 0; i < t.NumField(); i++ {
		name, _ := jsonField(t.Field(i))
		if name == "" {
			continue
		}
		parts = append(parts, annotations[name].help)
	}
	return strings.Join(parts, ", ")
}

func renderFile(shape, help string) string {
	var b bytes.Buffer
	b.WriteString("// Code generated by internal/prompts/contractgen; DO NOT EDIT.\n")
	b.WriteString("// Regenerate with `go generate ./...` after changing the workflow result\n")
	b.WriteString("// structs (AgentResult/HumanQuestion) or the generator's\n")
	b.WriteString("// annotation table. CI runs `go generate ./... && git diff --exit-code`.\n\n")
	b.WriteString("package prompts\n\n")
	b.WriteString("// resultContractShape is the byte-identical {\"gitmoot_result\":{…}} example\n")
	b.WriteString("// literal that RenderJob and RenderRepairPrompt both emit. Agents copy it.\n")
	b.WriteString("const resultContractShape = " + goQuote(shape) + "\n\n")
	b.WriteString("// resultDecisionHelp documents semantic constraints that the decision enum alone\n")
	b.WriteString("// cannot express. It is emitted beside the shape in job and repair prompts.\n")
	b.WriteString("const resultDecisionHelp = " + goQuote(resultDecisionHelp) + "\n\n")
	b.WriteString("// resultFieldHelp documents the top-level result fields the shape cannot\n")
	b.WriteString("// express. Runtime agents only receive this prompt, not the gitmoot skill docs.\n")
	b.WriteString("const resultFieldHelp = " + goQuote(help) + "\n")
	return b.String()
}

// goQuote renders s as a Go double-quoted string literal. We avoid raw string
// literals because help embeds backslash-escaped quotes and newlines.
func goQuote(s string) string {
	return fmt.Sprintf("%q", s)
}
