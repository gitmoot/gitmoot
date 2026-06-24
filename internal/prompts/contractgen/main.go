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
// AgentResult/Delegation/EphemeralSpec forces the prompt to be regenerated. CI
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

	"github.com/jerryfane/gitmoot/internal/workflow"
)

// fieldAnnotation carries the hand-curated prose for a single JSON field that
// the struct tags cannot express on their own.
type fieldAnnotation struct {
	// example is the placeholder value rendered into the byte-identical
	// {"gitmoot_result":{…}} example literal. It is only consulted for the
	// non-omitempty AgentResult fields that appear in that literal.
	example string
	// help is the human-readable, one-line description rendered into the
	// delegations[] schema help. Empty help means the field is documented
	// structurally (e.g. via the example literal) and needs no prose line.
	help string
}

// resultFieldAnnotations covers every JSON field of workflow.AgentResult. The
// example values reproduce the battle-tested literal shape verbatim; changing
// one here changes the shape agents copy, so treat them as load-bearing.
var resultFieldAnnotations = map[string]fieldAnnotation{
	"decision":      {example: enumExample(workflow.ResultDecisions)},
	"summary":       {example: `"..."`},
	"findings":      {example: "[]"},
	"changes_made":  {example: "[]"},
	"tests_run":     {example: "[]"},
	"needs":         {example: "[]"},
	"delegations":   {example: "[]"},
	"artifact_body": {help: `top-level artifact_body (string) is required when any delegation requests artifacts.`},
}

// delegationFieldAnnotations covers every JSON field of workflow.Delegation.
// The order of rendering follows the struct's declaration order (reflection),
// not this map; this map only supplies the prose.
var delegationFieldAnnotations = map[string]fieldAnnotation{
	"id":             {help: `id (string, required): unique within this batch; deps reference it and validation names entries as delegations[<index>] (id "<id>").`},
	"agent":          {help: `agent (string): the target Gitmoot agent's NAME — use "agent", not "to". Set exactly one of agent or ephemeral.`},
	"ephemeral":      {help: `ephemeral (object): an inline one-off worker instead of a registered agent; exactly one of agent or ephemeral. Fields: ` + ephemeralFieldsHelp() + ".\n  Ephemeral workers are leaf-only and cannot themselves delegate."},
	"action":         {help: `action (string, required): one of ask|review|implement.`},
	"worktree":       {help: `worktree (string, optional): worktree path for the child job.`},
	"prompt":         {help: `prompt (string, required): what the agent should do.`},
	"artifacts":      {help: `artifacts (string[], optional): named artifact handles passed to the child; when any delegation sets this, the parent result must also set the top-level artifact_body.`},
	"deps":           {help: `deps (string[], optional): sibling delegation ids this entry waits for; it runs only after every dep succeeds.`},
	"timeout":        {help: `timeout (string, optional): a positive Go duration (e.g. "10m").`},
	"retry":          {help: `retry (integer, optional): >= 0; how many times to re-run the leg on failure.`},
	"failure_policy": {help: `failure_policy (string, optional): one of ` + enumList(workflow.DelegationFailurePolicies) + `; controls what happens to the parent when this leg fails.`},
	"fingerprint":    {help: `fingerprint (string, optional): dedup key; identical fingerprints are deduplicated.`},
	"synthesis_rule": {help: `synthesis_rule (string, optional): one of ` + enumList(workflow.DelegationSynthesisRules) + `; how the coordinator combines child results.`},
	"quorum":         {help: `quorum (integer, optional): K > 0, required when synthesis_rule is quorum; the continuation proceeds only if at least K children approve (K <= number of delegations).`},
	"model":          {help: `model (string, optional): a free-form, runtime-scoped model string for the child; omit to use the agent's configured default.`},
	"phase":          {help: `phase (string, optional): a free-form pass-through label (metadata only; does not change routing).`},
}

// ephemeralFieldAnnotations covers every JSON field of workflow.EphemeralSpec.
var ephemeralFieldAnnotations = map[string]fieldAnnotation{
	"runtime":         {help: `runtime (required, one of ` + enumList(workflow.EphemeralRuntimes) + `)`},
	"model":           {help: `model (optional)`},
	"template":        {help: `template (optional)`},
	"role":            {help: `role (optional)`},
	"capabilities":    {help: `capabilities (string[], optional)`},
	"autonomy_policy": {help: `autonomy_policy (optional)`},
}

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
	if err := requireAnnotations(reflect.TypeOf(workflow.Delegation{}), delegationFieldAnnotations, "Delegation"); err != nil {
		return err
	}
	if err := requireAnnotations(reflect.TypeOf(workflow.EphemeralSpec{}), ephemeralFieldAnnotations, "EphemeralSpec"); err != nil {
		return err
	}

	shape := renderResultShape()
	help := renderDelegationHelp()

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
// fields (which do not appear in the example shape agents copy).
func renderResultShape() string {
	t := reflect.TypeOf(workflow.AgentResult{})
	var parts []string
	for i := 0; i < t.NumField(); i++ {
		name, omitempty := jsonField(t.Field(i))
		if name == "" || omitempty {
			continue
		}
		parts = append(parts, fmt.Sprintf("%q:%s", name, resultFieldAnnotations[name].example))
	}
	return `{"gitmoot_result":{` + strings.Join(parts, ",") + `}}`
}

// renderDelegationHelp builds the complete delegations[] schema help, covering
// every Delegation field in declaration order. The fixed lead and trailing
// lines preserve the battle-tested phrasings verbatim.
func renderDelegationHelp() string {
	t := reflect.TypeOf(workflow.Delegation{})
	var b strings.Builder
	b.WriteString("\nEach delegations[] entry is an object. Fields:\n")
	for i := 0; i < t.NumField(); i++ {
		name, _ := jsonField(t.Field(i))
		if name == "" {
			continue
		}
		help := delegationFieldAnnotations[name].help
		if help == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(help)
		b.WriteByte('\n')
	}
	// Battle-tested phrasings preserved verbatim (golden-tested): the
	// "empty delegations = done" signal and the delegations[<index>] error shape.
	b.WriteString("- Leave delegations empty ([]) when no follow-up agents are needed — that is how you signal the work is complete.\n")
	b.WriteString("- Validation errors name the offending entry as delegations[<index>] (id \"<id>\"); fix every listed field.\n")
	// artifact_body lives on the top-level result, not on a delegation, but it
	// is conditionally required by delegations, so document it here.
	if h := resultFieldAnnotations["artifact_body"].help; h != "" {
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

// ephemeralFieldsHelp renders the EphemeralSpec sub-field roster inline, in
// declaration order, for the ephemeral delegation field's help line.
func ephemeralFieldsHelp() string {
	t := reflect.TypeOf(workflow.EphemeralSpec{})
	var parts []string
	for i := 0; i < t.NumField(); i++ {
		name, _ := jsonField(t.Field(i))
		if name == "" {
			continue
		}
		parts = append(parts, ephemeralFieldAnnotations[name].help)
	}
	return strings.Join(parts, ", ")
}

func renderFile(shape, help string) string {
	var b bytes.Buffer
	b.WriteString("// Code generated by internal/prompts/contractgen; DO NOT EDIT.\n")
	b.WriteString("// Regenerate with `go generate ./...` after changing the workflow result\n")
	b.WriteString("// structs (AgentResult/Delegation/EphemeralSpec) or the generator's\n")
	b.WriteString("// annotation table. CI runs `go generate ./... && git diff --exit-code`.\n\n")
	b.WriteString("package prompts\n\n")
	b.WriteString("// resultContractShape is the byte-identical {\"gitmoot_result\":{…}} example\n")
	b.WriteString("// literal that RenderJob and RenderRepairPrompt both emit. Agents copy it.\n")
	b.WriteString("const resultContractShape = " + goQuote(shape) + "\n\n")
	b.WriteString("// delegationSchemaHelp documents the full delegations[] object shape inside\n")
	b.WriteString("// the rendered prompt. Runtime agents only receive this prompt, not the\n")
	b.WriteString("// gitmoot skill docs, so without it they guess field names (e.g. \"to\"\n")
	b.WriteString("// instead of \"agent\") and the result fails validation.\n")
	b.WriteString("const delegationSchemaHelp = " + goQuote(help) + "\n")
	return b.String()
}

// goQuote renders s as a Go double-quoted string literal. We avoid raw string
// literals because help embeds backslash-escaped quotes and newlines.
func goQuote(s string) string {
	return fmt.Sprintf("%q", s)
}
