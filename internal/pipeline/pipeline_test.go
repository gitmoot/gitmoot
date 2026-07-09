package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

const validSpec = `
name: deploy-flow
repo: jerryfane/gitmoot
schedule:
  interval: 24h
  jitter: 15m
success_decisions: [approved, implemented]
stages:
  - id: source
    cmd: git fetch --all
  - id: score
    cmd: ./score.sh
    needs: [source]
    timeout: 10m
    retry: 2
  - id: deploy
    cmd: ./deploy.sh
    needs: [score]
    success_decisions: [approved]
`

func TestLoadValidSpec(t *testing.T) {
	spec, err := Load([]byte(validSpec))
	if err != nil {
		t.Fatalf("Load valid spec: %v", err)
	}
	if spec.Name != "deploy-flow" || spec.Repo != "jerryfane/gitmoot" {
		t.Fatalf("unexpected header: %+v", spec)
	}
	if spec.Schedule == nil || spec.Schedule.Interval != "24h" || spec.Schedule.Jitter != "15m" {
		t.Fatalf("unexpected schedule: %+v", spec.Schedule)
	}
	if len(spec.Stages) != 3 {
		t.Fatalf("stages = %d, want 3", len(spec.Stages))
	}
	if spec.Stages[1].ID != "score" || spec.Stages[1].Retry != 2 || spec.Stages[1].Timeout != "10m" {
		t.Fatalf("unexpected score stage: %+v", spec.Stages[1])
	}
	if !reflect.DeepEqual(spec.Stages[1].Needs, []string{"source"}) {
		t.Fatalf("score needs = %v", spec.Stages[1].Needs)
	}
}

// TestLoadValidAgentSpec exercises the #757 agent-stage schema: an agent stage
// with no explicit action defaults to "ask", an explicit review action is kept,
// and shell + agent stages coexist in one DAG.
func TestLoadValidAgentSpec(t *testing.T) {
	const agentSpec = `
name: mixed-flow
repo: jerryfane/gitmoot
stages:
  - id: build
    cmd: make build
  - id: review
    agent: reviewer
    prompt: Review the build output.
    needs: [build]
  - id: audit
    agent: auditor
    prompt: Audit dependencies.
    action: review
    needs: [build]
`
	spec, err := Load([]byte(agentSpec))
	if err != nil {
		t.Fatalf("Load valid agent spec: %v", err)
	}
	if spec.Stages[0].Cmd != "make build" || spec.Stages[0].Agent != "" {
		t.Fatalf("unexpected shell stage: %+v", spec.Stages[0])
	}
	if spec.Stages[1].Agent != "reviewer" || spec.Stages[1].Cmd != "" {
		t.Fatalf("unexpected agent stage: %+v", spec.Stages[1])
	}
	if spec.Stages[1].Action != DefaultAgentStageAction {
		t.Fatalf("stage review action = %q, want default %q", spec.Stages[1].Action, DefaultAgentStageAction)
	}
	if spec.Stages[1].Prompt != "Review the build output." {
		t.Fatalf("stage review prompt = %q", spec.Stages[1].Prompt)
	}
	if spec.Stages[2].Action != "review" {
		t.Fatalf("stage audit action = %q, want review", spec.Stages[2].Action)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantSub string
	}{
		{
			name:    "missing name",
			spec:    "stages:\n  - {id: a, cmd: echo}\n",
			wantSub: "pipeline name is required",
		},
		{
			name:    "bad name",
			spec:    "name: bad name\nstages:\n  - {id: a, cmd: echo}\n",
			wantSub: "name-safe token",
		},
		{
			name:    "no stages",
			spec:    "name: p\n",
			wantSub: "has no stages",
		},
		{
			name:    "duplicate id",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: a, cmd: echo}\n",
			wantSub: `stage id "a" is not unique`,
		},
		{
			name:    "neither cmd nor agent",
			spec:    "name: p\nstages:\n  - {id: a, cmd: \"\"}\n",
			wantSub: `stage "a" has neither cmd nor agent`,
		},
		{
			name:    "both cmd and agent",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, agent: rev, prompt: hi}\n",
			wantSub: `stage "a" sets both cmd and agent`,
		},
		{
			name:    "agent without prompt",
			spec:    "name: p\nstages:\n  - {id: a, agent: rev}\n",
			wantSub: `stage "a" agent stage requires a non-empty prompt`,
		},
		{
			name:    "agent implement rejected",
			spec:    "name: p\nstages:\n  - {id: a, agent: rev, prompt: hi, action: implement}\n",
			wantSub: `action "implement" is not allowed`,
		},
		{
			name:    "agent bad action",
			spec:    "name: p\nstages:\n  - {id: a, agent: rev, prompt: hi, action: deploy}\n",
			wantSub: `action "deploy" is invalid`,
		},
		{
			name:    "shell stage with prompt",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, prompt: hi}\n",
			wantSub: `stage "a" sets prompt but is a shell (cmd) stage`,
		},
		{
			name:    "unknown need",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, needs: [zzz]}\n",
			wantSub: `references unknown need "zzz"`,
		},
		{
			name:    "self dependency",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, needs: [a]}\n",
			wantSub: `stage "a" depends on itself`,
		},
		{
			name:    "cycle",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, needs: [b]}\n  - {id: b, cmd: echo, needs: [a]}\n",
			wantSub: "dependency cycle",
		},
		{
			name:    "bad timeout",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, timeout: nope}\n",
			wantSub: `timeout "nope" is invalid`,
		},
		{
			name:    "non-positive timeout",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, timeout: 0s}\n",
			wantSub: "must be positive",
		},
		{
			name:    "negative retry",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, retry: -1}\n",
			wantSub: "retry must be >= 0",
		},
		{
			name:    "bad success decision",
			spec:    "name: p\nsuccess_decisions: [blocked]\nstages:\n  - {id: a, cmd: echo}\n",
			wantSub: `success_decisions "blocked" is invalid`,
		},
		{
			name:    "schedule without interval",
			spec:    "name: p\nschedule:\n  jitter: 5m\nstages:\n  - {id: a, cmd: echo}\n",
			wantSub: "schedule requires an interval",
		},
		{
			name:    "bad schedule interval",
			spec:    "name: p\nschedule:\n  interval: soon\nstages:\n  - {id: a, cmd: echo}\n",
			wantSub: `interval "soon" is invalid`,
		},
		{
			name:    "unknown field",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, need: [b]}\n",
			wantSub: "field need not found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.spec))
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestStageKind pins Stage.Kind() over every current stage shape — the SINGLE
// place a stage kind is decided. It covers the well-formed kinds (shell, agent
// ask via explicit + defaulted action, agent review) and the malformed shapes that
// classify as StageKindUnknown (both executors, neither, an unrecognized/implement
// action). A future kind adds rows here; existing rows must not change.
func TestPipelineStageKind(t *testing.T) {
	cases := []struct {
		name  string
		stage Stage
		want  StageKind
	}{
		{"shell", Stage{ID: "a", Cmd: "echo hi"}, StageKindShell},
		{"agent ask explicit", Stage{ID: "a", Agent: "asker", Prompt: "q", Action: "ask"}, StageKindAgentAsk},
		{"agent ask defaulted", Stage{ID: "a", Agent: "asker", Prompt: "q", Action: DefaultAgentStageAction}, StageKindAgentAsk},
		{"agent ask empty action", Stage{ID: "a", Agent: "asker", Prompt: "q"}, StageKindAgentAsk},
		{"agent review", Stage{ID: "a", Agent: "rev", Prompt: "q", Action: "review"}, StageKindAgentReview},
		{"both executors", Stage{ID: "a", Cmd: "echo", Agent: "rev", Prompt: "q"}, StageKindUnknown},
		{"neither executor", Stage{ID: "a"}, StageKindUnknown},
		{"agent implement", Stage{ID: "a", Agent: "impl", Prompt: "q", Action: "implement"}, StageKindUnknown},
		{"agent bad action", Stage{ID: "a", Agent: "x", Prompt: "q", Action: "deploy"}, StageKindUnknown},
		// #758: orchestrate:true on an agent stage classifies as StageKindOrchestrate,
		// checked BEFORE the plain agent action switch (so an orchestrate stage is
		// never mistaken for a leaf), even when the (to-be-rejected) action is review.
		{"orchestrate default action", Stage{ID: "a", Agent: "coord", Prompt: "q", Action: DefaultAgentStageAction, Orchestrate: true}, StageKindOrchestrate},
		{"orchestrate empty action", Stage{ID: "a", Agent: "coord", Prompt: "q", Orchestrate: true}, StageKindOrchestrate},
		{"orchestrate review action", Stage{ID: "a", Agent: "coord", Prompt: "q", Action: "review", Orchestrate: true}, StageKindOrchestrate},
		// orchestrate:true with a cmd set is still the both-executors malformed shape.
		{"orchestrate with cmd", Stage{ID: "a", Cmd: "echo", Agent: "coord", Prompt: "q", Orchestrate: true}, StageKindUnknown},
		// orchestrate:true with no agent is not an orchestrate coordinator (nothing to run).
		{"orchestrate no agent", Stage{ID: "a", Prompt: "q", Orchestrate: true}, StageKindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stage.Kind(); got != tc.want {
				t.Fatalf("Kind() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestStageKindMatchesLoadedSpec cross-checks Kind() against real normalized specs
// loaded through Load (so the defaulted-action path — normalize() setting a blank
// agent action to "ask" — is exercised end to end, matching what the advancer sees).
func TestPipelineStageKindMatchesLoadedSpec(t *testing.T) {
	const spec = `name: kinds
stages:
  - id: build
    cmd: make build
  - id: ask
    agent: asker
    prompt: What changed?
    needs: [build]
  - id: review
    agent: reviewer
    prompt: Review it.
    action: review
    needs: [build]
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []StageKind{StageKindShell, StageKindAgentAsk, StageKindAgentReview}
	for i, stage := range loaded.Stages {
		if got := stage.Kind(); got != want[i] {
			t.Fatalf("stage %q Kind() = %d, want %d", stage.ID, got, want[i])
		}
	}
}

// TestPipelineOrchestrateStageValidation pins the #758 orchestrate stage spec
// contract end-to-end through Load: a valid orchestrate stage (agent + prompt +
// orchestrate:true, default/explicit ask verb) loads and classifies as
// StageKindOrchestrate, while the malformed shapes are rejected at add time.
func TestPipelineOrchestrateStageValidation(t *testing.T) {
	const validSpec = `name: orch
repo: jerryfane/gitmoot
stages:
  - id: extract
    cmd: echo facts
  - id: decompose
    agent: coordinator
    prompt: Decompose the work and fan out reviewers.
    orchestrate: true
    needs: [extract]
`
	spec, err := Load([]byte(validSpec))
	if err != nil {
		t.Fatalf("Load valid orchestrate spec: %v", err)
	}
	orch := spec.Stages[1]
	if !orch.Orchestrate {
		t.Fatalf("orchestrate flag not parsed: %+v", orch)
	}
	if orch.Action != DefaultAgentStageAction {
		t.Fatalf("orchestrate action = %q, want defaulted to %q", orch.Action, DefaultAgentStageAction)
	}
	if orch.Kind() != StageKindOrchestrate {
		t.Fatalf("orchestrate stage Kind() = %d, want StageKindOrchestrate", orch.Kind())
	}

	errCases := []struct {
		name    string
		spec    string
		wantSub string
	}{
		{
			name:    "orchestrate without prompt",
			spec:    "name: p\nstages:\n  - {id: a, agent: coord, orchestrate: true}\n",
			wantSub: "orchestrate stage requires a non-empty coordinator prompt",
		},
		{
			name:    "orchestrate review action rejected",
			spec:    "name: p\nstages:\n  - {id: a, agent: coord, prompt: hi, orchestrate: true, action: review}\n",
			wantSub: `orchestrate stage action "review" is not allowed`,
		},
		{
			name:    "orchestrate implement action rejected",
			spec:    "name: p\nstages:\n  - {id: a, agent: coord, prompt: hi, orchestrate: true, action: implement}\n",
			wantSub: `orchestrate stage action "implement" is not allowed`,
		},
		{
			name:    "orchestrate with cmd is both executors",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, agent: coord, prompt: hi, orchestrate: true}\n",
			wantSub: `stage "a" sets both cmd and agent`,
		},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.spec))
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestHashDeterministicAndSensitive(t *testing.T) {
	a := []byte("name: p\nstages:\n  - {id: a, cmd: echo}\n")
	b := []byte("name: p\nstages:\n  - {id: a, cmd: echo}\n")
	c := []byte("name: p\nstages:\n  - {id: a, cmd: echo2}\n")
	if Hash(a) != Hash(b) {
		t.Fatalf("identical bytes hashed differently")
	}
	if Hash(a) == Hash(c) {
		t.Fatalf("different bytes hashed identically")
	}
	if len(Hash(a)) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(Hash(a)))
	}
}

func TestEffectiveSuccessDecisions(t *testing.T) {
	spec := Spec{SuccessDecisions: []string{"approved"}}
	// Stage override wins.
	got := spec.EffectiveSuccessDecisions(Stage{SuccessDecisions: []string{"changes_requested"}})
	if !reflect.DeepEqual(got, []string{"changes_requested"}) {
		t.Fatalf("stage override = %v", got)
	}
	// Top-level override when the stage has none.
	got = spec.EffectiveSuccessDecisions(Stage{})
	if !reflect.DeepEqual(got, []string{"approved"}) {
		t.Fatalf("top-level override = %v", got)
	}
	// Package default when neither is set.
	got = Spec{}.EffectiveSuccessDecisions(Stage{})
	if !reflect.DeepEqual(got, DefaultSuccessDecisions) {
		t.Fatalf("default = %v, want %v", got, DefaultSuccessDecisions)
	}
	// The returned slice is a copy — mutating it must not corrupt the defaults.
	got[0] = "mutated"
	if DefaultSuccessDecisions[0] == "mutated" {
		t.Fatalf("EffectiveSuccessDecisions leaked the shared default slice")
	}
}
