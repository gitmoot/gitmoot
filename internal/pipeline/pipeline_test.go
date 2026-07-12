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

func TestLoadEmailTriggerDefaultsAndValidation(t *testing.T) {
	loaded, err := Load([]byte("name: mail\nrepo: jerryfane/gitmoot\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo}\n"))
	if err != nil {
		t.Fatalf("Load email trigger: %v", err)
	}
	if loaded.Trigger == nil || loaded.Trigger.Connection != "gmail-imap" || loaded.Trigger.Mailbox != "INBOX" {
		t.Fatalf("trigger defaults = %+v", loaded.Trigger)
	}
	mapped, err := Load([]byte("name: mail\nrepo: jerryfane/gitmoot\ntrigger:\n  kind: email\n  map:\n    sender: from_address\n    subject: subject\n    received_at: date\nstages:\n  - {id: run, cmd: echo}\n"))
	if err != nil {
		t.Fatalf("Load mapped email trigger: %v", err)
	}
	if !reflect.DeepEqual(mapped.Trigger.Map, map[string]string{"received_at": "date", "sender": "from_address", "subject": "subject"}) {
		t.Fatalf("trigger map = %+v", mapped.Trigger.Map)
	}

	cases := []struct{ name, trigger, want string }{
		{"missing kind", "{}", "supported kinds: email"},
		{"unknown kind", "{kind: webhook}", "supported kinds: email"},
		{"unsafe connection", "{kind: email, connection: \"x']}}\"}", "must match"},
		{"leading dash connection", "{kind: email, connection: -gmail}", "must match"},
		{"leading underscore connection", "{kind: email, connection: _gmail}", "must match"},
		{"mailbox expression", "{kind: email, mailbox: \"{{danger}}\"}", "must not contain"},
		{"empty mapping", "{kind: email, map: {}}", "explicitly empty"},
		{"invalid output", "{kind: email, map: {Bad-Key: subject}}", "must be 1-64 bytes"},
		{"invalid selector", "{kind: email, map: {subject: '{{trigger.subject}}'}}", "use one of: subject, from_address, text, message_id, date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte("name: mail\nrepo: jerryfane/gitmoot\ntrigger: " + tc.trigger + "\nstages:\n  - {id: run, cmd: echo}\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
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

// TestLoadValidImplementSpec exercises the #768 mutating implement schema: an
// `action: implement` stage with `write: true` on a SCHEDULED pipeline that opts in
// via `allow_scheduled_writes: true` loads, parses both new fields, and classifies as
// StageKindAgentImplement.
func TestLoadValidImplementSpec(t *testing.T) {
	const spec = `name: build-flow
repo: owner/repo
schedule:
  interval: 24h
allow_scheduled_writes: true
stages:
  - id: fix
    agent: coder
    prompt: Fix the reported bug.
    action: implement
    write: true
    retry: 1
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load valid implement spec: %v", err)
	}
	if !loaded.AllowScheduledWrites {
		t.Fatalf("allow_scheduled_writes did not parse: %+v", loaded)
	}
	st := loaded.Stages[0]
	if st.Action != "implement" || !st.Write {
		t.Fatalf("implement stage = %+v, want action=implement write=true", st)
	}
	if st.Kind() != StageKindAgentImplement {
		t.Fatalf("Kind() = %d, want StageKindAgentImplement", st.Kind())
	}
}

func TestLoadValidTriggeredImplementSpec(t *testing.T) {
	const spec = `name: mail-fix
repo: owner/repo
trigger:
  kind: email
  map: {subject: subject}
allow_triggered_writes: true
stages:
  - id: fix
    agent: coder
    prompt: Fix the reported bug.
    action: implement
    write: true
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load valid triggered implement spec: %v", err)
	}
	if !loaded.AllowTriggeredWrites || loaded.Stages[0].Kind() != StageKindAgentImplement {
		t.Fatalf("triggered implement did not round-trip: %+v", loaded)
	}
}

// TestLoadValidManualImplementSpec proves a MANUAL (no schedule) implement pipeline
// needs only the per-stage write: true — the scheduled-write gate does not apply.
func TestLoadValidManualImplementSpec(t *testing.T) {
	const spec = `name: manual-flow
repo: owner/repo
stages:
  - id: fix
    agent: coder
    prompt: Fix it.
    action: implement
    write: true
`
	if _, err := Load([]byte(spec)); err != nil {
		t.Fatalf("manual implement spec should validate: %v", err)
	}
}

// TestLoadValidGateSpec proves a JOBLESS gate stage (#768 Phase 2) loads: a
// pr_merged predicate watching an upstream implement stage it needs, classifying as
// StageKindGate and expressing [implement] -> [gate] -> [deploy].
func TestLoadValidGateSpec(t *testing.T) {
	const spec = `name: gate-flow
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
    timeout: 24h
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load valid gate spec: %v", err)
	}
	gate := loaded.Stages[1]
	if gate.Gate != "pr_merged" || gate.Source != "impl" {
		t.Fatalf("gate stage = %+v, want gate=pr_merged source=impl", gate)
	}
	if gate.Kind() != StageKindGate {
		t.Fatalf("gate Kind() = %d, want StageKindGate", gate.Kind())
	}
}

func TestHumanMergeGateDefaultsRemainByteIdentical(t *testing.T) {
	const raw = "name: human-default\nrepo: owner/repo\nstages:\n  - id: impl\n    agent: coder\n    prompt: Fix it.\n    action: implement\n    write: true\n  - id: wait\n    gate: pr_merged\n    source: impl\n    needs: [impl]\n"
	loaded, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("Load human-default gate: %v", err)
	}
	if loaded.AllowAutoMerge || loaded.Stages[1].Merge != "" {
		t.Fatalf("human-default auto-merge fields = allow:%v merge:%q", loaded.AllowAutoMerge, loaded.Stages[1].Merge)
	}
	if got, want := Hash([]byte(raw)), "e7c662207566643a01b2e2dbcf2b7cee11f380409a7decf4de70e670fecc4485"; got != want {
		t.Fatalf("human-default raw spec hash = %s, want %s", got, want)
	}
}

func TestLoadValidSourceBoundReviewSpec(t *testing.T) {
	const spec = `name: review-flow
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: Review the implementation PR.
    action: review
    source: impl
    needs: [impl]
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load valid source-bound review spec: %v", err)
	}
	review := loaded.Stages[1]
	if review.Kind() != StageKindAgentReview || review.Source != "impl" {
		t.Fatalf("review stage = %+v, want action: review source: impl", review)
	}
}

func TestLoadValidAutoMergeGateSpec(t *testing.T) {
	const spec = `name: auto-merge-flow
repo: owner/repo
schedule:
  interval: 24h
allow_scheduled_writes: true
allow_auto_merge: true
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: Review the implementation PR.
    action: review
    source: impl
    needs: [impl]
    success_decisions: [approved]
  - id: merge
    gate: pr_merged
    merge: auto
    source: impl
    needs: [impl, review]
`
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("Load valid auto-merge spec: %v", err)
	}
	if !loaded.AllowScheduledWrites || !loaded.AllowAutoMerge || loaded.Stages[2].Merge != GateMergeAuto {
		t.Fatalf("auto-merge keys did not parse: %+v", loaded)
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
			name:    "implement without write rejected",
			spec:    "name: p\nstages:\n  - {id: a, agent: rev, prompt: hi, action: implement}\n",
			wantSub: `sets action "implement" without write: true`,
		},
		{
			name:    "write without implement rejected",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo, write: true}\n",
			wantSub: "write: true is only valid with action: implement",
		},
		{
			name:    "scheduled implement without allow rejected",
			spec:    "name: p\nschedule:\n  interval: 24h\nstages:\n  - {id: a, agent: rev, prompt: hi, action: implement, write: true}\n",
			wantSub: "set allow_scheduled_writes: true",
		},
		{
			name:    "triggered implement without allow rejected",
			spec:    "name: p\nrepo: owner/repo\ntrigger: {kind: email}\nstages:\n  - {id: a, agent: rev, prompt: hi, action: implement, write: true}\n",
			wantSub: "set allow_triggered_writes: true",
		},
		{
			name:    "agent bad action",
			spec:    "name: p\nstages:\n  - {id: a, agent: rev, prompt: hi, action: deploy}\n",
			wantSub: `action "deploy" is invalid`,
		},
		{
			name:    "gate bad predicate",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: g, gate: pr_reviewed, source: a, needs: [a]}\n",
			wantSub: `gate predicate "pr_reviewed" is invalid`,
		},
		{
			name:    "gate without source",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: g, gate: pr_merged, needs: [a]}\n",
			wantSub: "gate stage requires a source",
		},
		{
			name:    "gate source not in needs",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: b, cmd: echo}\n  - {id: g, gate: pr_merged, source: a, needs: [b]}\n",
			wantSub: `gate source "a" must be one of the stage's needs`,
		},
		{
			name:    "gate source is self",
			spec:    "name: p\nstages:\n  - {id: g, gate: pr_merged, source: g, needs: [g]}\n",
			wantSub: "gate source cannot be the stage itself",
		},
		{
			name:    "gate source is not an implement stage",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: g, gate: pr_merged, source: a, needs: [a]}\n",
			wantSub: `gate source "a" must be a mutating implement stage`,
		},
		{
			name:    "gate with cmd rejected",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: g, gate: pr_merged, source: a, needs: [a], cmd: echo}\n",
			wantSub: `stage "g" sets both gate and cmd`,
		},
		{
			name:    "gate with agent rejected",
			spec:    "name: p\nstages:\n  - {id: a, cmd: echo}\n  - {id: g, gate: pr_merged, source: a, needs: [a], agent: rev, prompt: hi}\n",
			wantSub: `stage "g" sets both gate and agent`,
		},
		{
			name:    "auto merge without pipeline allow rejected",
			spec:    "name: p\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: impl, needs: [impl]}\n  - {id: g, gate: pr_merged, merge: auto, source: impl, needs: [impl, review]}\n",
			wantSub: "merge: auto without allow_auto_merge: true",
		},
		{
			name:    "merge mode on non-gate rejected",
			spec:    "name: p\nallow_auto_merge: true\nstages:\n  - {id: shell, cmd: echo, merge: auto}\n",
			wantSub: "merge is only valid on gate stages",
		},
		{
			name:    "invalid gate merge mode rejected",
			spec:    "name: p\nallow_auto_merge: true\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: impl, needs: [impl]}\n  - {id: g, gate: pr_merged, merge: human, source: impl, needs: [impl, review]}\n",
			wantSub: `merge mode "human" is invalid`,
		},
		{
			name:    "auto merge without source-bound review rejected",
			spec:    "name: p\nallow_auto_merge: true\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: g, gate: pr_merged, merge: auto, source: impl, needs: [impl]}\n",
			wantSub: "has no source-bound review stage",
		},
		{
			name:    "scheduled auto merge still needs scheduled writes allow",
			spec:    "name: p\nschedule: {interval: 24h}\nallow_auto_merge: true\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: impl, needs: [impl]}\n  - {id: g, gate: pr_merged, merge: auto, source: impl, needs: [impl, review]}\n",
			wantSub: "set allow_scheduled_writes: true",
		},
		{
			name:    "scheduled auto merge still needs auto merge allow",
			spec:    "name: p\nschedule: {interval: 24h}\nallow_scheduled_writes: true\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: impl, needs: [impl]}\n  - {id: g, gate: pr_merged, merge: auto, source: impl, needs: [impl, review]}\n",
			wantSub: "merge: auto without allow_auto_merge: true",
		},
		{
			name:    "review source not in needs",
			spec:    "name: p\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: other, cmd: echo}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: impl, needs: [other]}\n",
			wantSub: `review source "impl" must be one of the stage's needs`,
		},
		{
			name:    "review source wrong kind",
			spec:    "name: p\nstages:\n  - {id: prep, cmd: echo}\n  - {id: review, agent: rev, prompt: inspect, action: review, source: prep, needs: [prep]}\n",
			wantSub: `review source "prep" must be a mutating implement stage`,
		},
		{
			name:    "shell source rejected",
			spec:    "name: p\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: shell, cmd: echo, source: impl, needs: [impl]}\n",
			wantSub: `stage "shell" sets source "impl" but is neither a gate nor an action: review agent stage`,
		},
		{
			name:    "ask source rejected",
			spec:    "name: p\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: ask, agent: helper, prompt: inspect, source: impl, needs: [impl]}\n",
			wantSub: `stage "ask" sets source "impl" but is neither a gate nor an action: review agent stage`,
		},
		{
			name:    "implement source rejected",
			spec:    "name: p\nstages:\n  - {id: first, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: second, agent: coder, prompt: more, action: implement, write: true, source: first, needs: [first]}\n",
			wantSub: `stage "second" sets source "first" but is neither a gate nor an action: review agent stage`,
		},
		{
			name:    "orchestrate source rejected",
			spec:    "name: p\nstages:\n  - {id: impl, agent: coder, prompt: fix, action: implement, write: true}\n  - {id: orch, agent: lead, prompt: coordinate, orchestrate: true, source: impl, needs: [impl]}\n",
			wantSub: `stage "orch" sets source "impl" but is neither a gate nor an action: review agent stage`,
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
		{"agent implement", Stage{ID: "a", Agent: "impl", Prompt: "q", Action: "implement", Write: true}, StageKindAgentImplement},
		{"agent implement no write still a kind", Stage{ID: "a", Agent: "impl", Prompt: "q", Action: "implement"}, StageKindAgentImplement},
		{"agent produce", Stage{ID: "a", Agent: "producer", Prompt: "q", Action: "produce", Write: true, Writes: []string{"/data"}}, StageKindAgentProduce},
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
		{"gate pr_merged", Stage{ID: "g", Gate: "pr_merged", Source: "impl", Needs: []string{"impl"}}, StageKindGate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.stage.Kind(); got != tc.want {
				t.Fatalf("Kind() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestProduceStageValidation(t *testing.T) {
	valid := `name: produce
repo: owner/repo
stages:
  - id: export
    agent: worker
    action: produce
    prompt: Write the export.
    write: true
    writes: [/var/lib/example-data]
    network: true
    check: test -s /var/lib/example-data/out.json
    check_retries: 2
`
	loaded, err := Load([]byte(valid))
	if err != nil {
		t.Fatalf("Load valid produce stage: %v", err)
	}
	if loaded.Stages[0].Kind() != StageKindAgentProduce || loaded.Stages[0].CheckRetries == nil || *loaded.Stages[0].CheckRetries != 2 {
		t.Fatalf("produce stage did not round-trip: %+v", loaded.Stages[0])
	}

	cases := []struct {
		name string
		spec string
		want string
	}{
		{"requires write", "name: p\nstages:\n- {id: a, agent: w, action: produce, prompt: x, writes: [/data]}\n", "without write: true"},
		{"requires writes", "name: p\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true}\n", "requires at least one writes path"},
		{"absolute writes", "name: p\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true, writes: [data]}\n", "must be absolute"},
		{"clean writes", "name: p\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true, writes: [/data/../out]}\n", "must be cleaned"},
		{"writes elsewhere", "name: p\nstages:\n- {id: a, cmd: echo, writes: [/data]}\n", "sets writes"},
		{"network elsewhere", "name: p\nstages:\n- {id: a, agent: w, prompt: x, network: true}\n", "sets network: true"},
		{"check elsewhere", "name: p\nstages:\n- {id: a, agent: w, prompt: x, check: true}\n", "sets check"},
		{"check retries elsewhere", "name: p\nstages:\n- {id: a, agent: w, prompt: x, check_retries: 0}\n", "sets check_retries"},
		{"negative check retries", "name: p\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true, writes: [/data], check: true, check_retries: -1}\n", "check_retries must be >= 0"},
		{"scheduled gate", "name: p\nschedule: {interval: 1h}\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true, writes: [/data]}\n", "allow_scheduled_writes: true"},
		{"triggered gate", "name: p\nrepo: owner/repo\ntrigger: {kind: email}\nstages:\n- {id: a, agent: w, action: produce, prompt: x, write: true, writes: [/data]}\n", "allow_triggered_writes: true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.spec))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want substring %q", err, tc.want)
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

// A trigger-bound pipeline without a repo would make the bridge 400 on every
// email; validation refuses it at add time instead.
func TestLoadEmailTriggerRequiresRepo(t *testing.T) {
	_, err := Load([]byte("name: mail\ntrigger:\n  kind: email\nstages:\n  - {id: run, cmd: echo}\n"))
	if err == nil || !strings.Contains(err.Error(), "declares a trigger but no repo") {
		t.Fatalf("Load error = %v, want repo-required error", err)
	}
}
