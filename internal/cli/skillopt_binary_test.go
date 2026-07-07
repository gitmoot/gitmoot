package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

const binaryTestSetYAML = `
version: 1
template_or_task_kind: bugfix
dimensions:
  - name: correctness
    weight: 2
    questions:
      - id: has_test
        text: Does the output add a Go test?
        contains: "func Test"
      - id: no_todo
        text: Is the output free of TODO markers?
        not_contains: "TODO"
  - name: style
    questions:
      - id: has_pkg
        text: Does the output declare a package?
        regex: "^package "
`

func writeBinaryFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestSkillOptBinaryDeterministicE2E drives the REAL `gitmoot skillopt binary
// run` + `show` through the top-level Run dispatcher against an isolated temp
// home with the deterministic (no-LLM) runner, then asserts the aggregated
// scores and the persisted/re-read verdicts.
func TestSkillOptBinaryDeterministicE2E(t *testing.T) {
	home := t.TempDir()
	setPath := writeBinaryFixture(t, home, "set.yaml", binaryTestSetYAML)
	// Source hits has_test (func Test) and has_pkg (package ...) but contains a TODO,
	// so no_todo fails: correctness = 1/2 (weighted equal q-weights), style = 1/1.
	source := "package main\n\nfunc TestFoo() {} // TODO: finish\n"
	sourcePath := writeBinaryFixture(t, home, "out.go", source)

	var out, errBuf bytes.Buffer
	code := Run([]string{"skillopt", "binary", "run", "--home", home, "--set", setPath, "--run", "run-1", "--source", sourcePath, "--deterministic", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("binary run exit=%d stderr=%s", code, errBuf.String())
	}

	var runResult struct {
		RunID           string                   `json:"run_id"`
		Overall         float64                  `json:"overall"`
		DimensionScores map[string]float64       `json:"dimension_scores"`
		Verdicts        []skillopt.BinaryVerdict `json:"verdicts"`
	}
	if err := json.Unmarshal(out.Bytes(), &runResult); err != nil {
		t.Fatalf("decode run json: %v (%s)", err, out.String())
	}
	if len(runResult.Verdicts) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(runResult.Verdicts))
	}
	if got := runResult.DimensionScores["correctness"]; got != 0.5 {
		t.Fatalf("correctness = %v, want 0.5", got)
	}
	if got := runResult.DimensionScores["style"]; got != 1.0 {
		t.Fatalf("style = %v, want 1.0", got)
	}
	// overall = (2*0.5 + 1*1.0)/3 = 0.6667
	if runResult.Overall < 0.66 || runResult.Overall > 0.67 {
		t.Fatalf("overall = %v, want ~0.667", runResult.Overall)
	}

	// show re-reads the persisted verdicts.
	out.Reset()
	errBuf.Reset()
	code = Run([]string{"skillopt", "binary", "show", "--home", home, "--run", "run-1", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("binary show exit=%d stderr=%s", code, errBuf.String())
	}
	var showResult struct {
		Verdicts        []skillopt.BinaryVerdict `json:"verdicts"`
		DimensionScores map[string]float64       `json:"dimension_scores"`
		Overall         float64                  `json:"overall"`
	}
	if err := json.Unmarshal(out.Bytes(), &showResult); err != nil {
		t.Fatalf("decode show json: %v (%s)", err, out.String())
	}
	if len(showResult.Verdicts) != 3 {
		t.Fatalf("persisted verdicts = %d, want 3", len(showResult.Verdicts))
	}
	// Regression guard (#525 review): `show` must reproduce the SAME weighted
	// scores `run` emitted for this run — not an unweighted mean. The fixture's
	// correctness dimension carries weight 2, so an unweighted `show` would have
	// reported overall 0.75 (=(0.5+1.0)/2) instead of 0.667 (=(2*0.5+1*1.0)/3).
	if showResult.Overall != runResult.Overall {
		t.Fatalf("show overall = %v, want run overall %v (weighted scores must match)", showResult.Overall, runResult.Overall)
	}
	if showResult.DimensionScores["correctness"] != runResult.DimensionScores["correctness"] {
		t.Fatalf("show correctness = %v, want %v", showResult.DimensionScores["correctness"], runResult.DimensionScores["correctness"])
	}
	if showResult.DimensionScores["style"] != runResult.DimensionScores["style"] {
		t.Fatalf("show style = %v, want %v", showResult.DimensionScores["style"], runResult.DimensionScores["style"])
	}
	// Weights round-trip through persistence so the re-read is self-describing.
	for _, v := range showResult.Verdicts {
		if v.Dimension == "correctness" && v.DimensionWeight != 2 {
			t.Fatalf("correctness verdict dimension_weight = %v, want 2 (persisted)", v.DimensionWeight)
		}
		if v.QuestionWeight != 1 {
			t.Fatalf("question %s question_weight = %v, want 1 (persisted default)", v.QuestionID, v.QuestionWeight)
		}
	}
	// Verify the specific failing verdict persisted with an explanation.
	var noTodo *skillopt.BinaryVerdict
	for i := range showResult.Verdicts {
		if showResult.Verdicts[i].QuestionID == "no_todo" {
			noTodo = &showResult.Verdicts[i]
		}
	}
	if noTodo == nil || noTodo.Verdict != "no" {
		t.Fatalf("no_todo verdict = %+v, want no", noTodo)
	}
	if !strings.Contains(noTodo.Explanation, "not_contains") {
		t.Fatalf("no_todo explanation = %q, want deterministic assertion note", noTodo.Explanation)
	}
}

// TestSkillOptBinaryRunRequiresSource proves the arg validation.
func TestSkillOptBinaryRunRequiresFlags(t *testing.T) {
	home := t.TempDir()
	setPath := writeBinaryFixture(t, home, "set.yaml", binaryTestSetYAML)
	var out, errBuf bytes.Buffer
	code := Run([]string{"skillopt", "binary", "run", "--home", home, "--set", setPath, "--run", "r", "--deterministic"}, &out, &errBuf)
	if code == 0 {
		t.Fatal("expected non-zero exit without --source")
	}
	if !strings.Contains(errBuf.String(), "--source") {
		t.Fatalf("stderr = %q, want --source hint", errBuf.String())
	}
}

// TestSkillOptBinaryLLMRunnerViaFakeAdapter exercises the OPT-IN LLM-backed
// runner end to end WITHOUT any live runtime by overriding the shared deliver
// seam with a fake adapter that returns the yes/no JSON the parser expects.
func TestSkillOptBinaryLLMRunnerViaFakeAdapter(t *testing.T) {
	home := t.TempDir()
	setPath := writeBinaryFixture(t, home, "set.yaml", binaryTestSetYAML)
	sourcePath := writeBinaryFixture(t, home, "out.txt", "irrelevant to a fake judge")

	orig := skillOptBinaryDeliver
	t.Cleanup(func() { skillOptBinaryDeliver = orig })
	var delivered int
	skillOptBinaryDeliver = func(_ context.Context, agent runtime.Agent, prompt string) (string, error) {
		delivered++
		if agent.Runtime != "codex" {
			t.Errorf("judge runtime = %q, want codex (cross-family)", agent.Runtime)
		}
		// Answer "yes" for the has_test question, "no" otherwise.
		if strings.Contains(prompt, "add a Go test") {
			return `sure -> {"verdict":"yes","explanation":"fake yes"}`, nil
		}
		return `{"verdict":"no","explanation":"fake no"}`, nil
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{"skillopt", "binary", "run", "--home", home, "--set", setPath, "--run", "llm-run", "--source", sourcePath, "--reviewer", "codex", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("binary run (llm) exit=%d stderr=%s", code, errBuf.String())
	}
	if delivered != 3 {
		t.Fatalf("deliver calls = %d, want 3 (one per question)", delivered)
	}
	var res struct {
		DimensionScores map[string]float64 `json:"dimension_scores"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only has_test answered yes: correctness = 1/2, style has_pkg = no -> 0.
	if res.DimensionScores["correctness"] != 0.5 {
		t.Fatalf("correctness = %v, want 0.5", res.DimensionScores["correctness"])
	}
	if res.DimensionScores["style"] != 0.0 {
		t.Fatalf("style = %v, want 0.0", res.DimensionScores["style"])
	}
}

func TestParseBinaryVerdict(t *testing.T) {
	cases := []struct {
		raw     string
		verdict string
		ok      bool
	}{
		{`{"verdict":"yes","explanation":"x"}`, "yes", true},
		{`prose {"verdict":"no"} trailing`, "no", true},
		{`{"gitmoot_result":{"verdict":"yes"}}`, "yes", true},
		{`no json here`, "", false},
		{`{"verdict":"maybe"}`, "", false},
	}
	for _, c := range cases {
		v, _, ok := parseBinaryVerdict(c.raw)
		if ok != c.ok || v != c.verdict {
			t.Fatalf("parseBinaryVerdict(%q) = (%q,%v), want (%q,%v)", c.raw, v, ok, c.verdict, c.ok)
		}
	}
}
