package pipeline

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var triggerConnectionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var triggerPayloadKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var triggerMapSelectors = []string{"subject", "from_address", "text", "message_id", "date"}

// ValidTriggerPayloadKey reports whether key is legal in both trigger.map and
// the bridge payload transport. Length is a byte bound because the bridge's
// decoded-size limits are byte budgets.
func ValidTriggerPayloadKey(key string) bool {
	return len(key) >= 1 && len(key) <= 64 && triggerPayloadKeyPattern.MatchString(key)
}

// normalize trims surrounding whitespace off every free-text field so validation
// and storage see canonical values. It does NOT lowercase or otherwise rewrite
// ids/commands — the raw YAML is what gets stored and hashed; normalize only
// affects the parsed Spec used for validation and downstream advancement.
func (s *Spec) normalize() {
	s.Name = strings.TrimSpace(s.Name)
	s.Repo = strings.TrimSpace(s.Repo)
	if s.Schedule != nil {
		s.Schedule.Interval = strings.TrimSpace(s.Schedule.Interval)
		s.Schedule.Jitter = strings.TrimSpace(s.Schedule.Jitter)
	}
	if s.Trigger != nil {
		s.Trigger.Kind = strings.TrimSpace(s.Trigger.Kind)
		s.Trigger.Pipeline = strings.TrimSpace(s.Trigger.Pipeline)
		s.Trigger.Connection = strings.TrimSpace(s.Trigger.Connection)
		s.Trigger.Mailbox = strings.TrimSpace(s.Trigger.Mailbox)
		if s.Trigger.Kind == "email" {
			if s.Trigger.Connection == "" {
				s.Trigger.Connection = "gmail-imap"
			}
			if s.Trigger.Mailbox == "" {
				s.Trigger.Mailbox = "INBOX"
			}
		}
	}
	s.SuccessDecisions = trimAll(s.SuccessDecisions)
	for i := range s.Stages {
		s.Stages[i].ID = strings.TrimSpace(s.Stages[i].ID)
		s.Stages[i].Cmd = strings.TrimSpace(s.Stages[i].Cmd)
		s.Stages[i].Agent = strings.TrimSpace(s.Stages[i].Agent)
		s.Stages[i].Prompt = strings.TrimSpace(s.Stages[i].Prompt)
		s.Stages[i].Action = strings.TrimSpace(s.Stages[i].Action)
		s.Stages[i].Gate = strings.TrimSpace(s.Stages[i].Gate)
		s.Stages[i].Merge = strings.TrimSpace(s.Stages[i].Merge)
		s.Stages[i].Source = strings.TrimSpace(s.Stages[i].Source)
		s.Stages[i].Timeout = strings.TrimSpace(s.Stages[i].Timeout)
		s.Stages[i].Check = strings.TrimSpace(s.Stages[i].Check)
		s.Stages[i].Writes = trimAll(s.Stages[i].Writes)
		s.Stages[i].Needs = trimAll(s.Stages[i].Needs)
		s.Stages[i].SuccessDecisions = trimAll(s.Stages[i].SuccessDecisions)
		// An agent stage defaults to the read-only "ask" verb when action is unset.
		// A shell stage never carries an action, so this only touches agent stages.
		if s.Stages[i].Agent != "" && s.Stages[i].Action == "" {
			s.Stages[i].Action = DefaultAgentStageAction
		}
	}
}

// Validate enforces the structural invariants of a pipeline spec: a name-safe
// pipeline name, at least one stage, unique name-safe stage ids, exactly one of
// cmd or agent per stage (an agent stage requiring a prompt and a read-only
// ask/review action), needs that reference known sibling ids (never the stage
// itself),
// a needs graph with no cycle, parseable positive timeouts, non-negative retry,
// a valid schedule, and success_decisions drawn from SuccessDecisionCandidates.
// It mirrors the shape of the delegation graph validator (workflow/result.go):
// catching these at parse time turns what would otherwise be a stuck run (unknown
// deps and cycles are permanently unsatisfiable) into a clean error at add time.
func (s Spec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("pipeline name is required")
	}
	if !validToken(s.Name) {
		return fmt.Errorf("pipeline name %q must be a name-safe token (letters, digits, '-', '_')", s.Name)
	}
	if len(s.Stages) == 0 {
		return fmt.Errorf("pipeline %q has no stages", s.Name)
	}
	if err := validateDecisions("pipeline "+s.Name, s.SuccessDecisions); err != nil {
		return err
	}
	if err := s.validateSchedule(); err != nil {
		return err
	}
	if err := s.validateTrigger(); err != nil {
		return err
	}

	known := make(map[string]struct{}, len(s.Stages))
	stageByID := make(map[string]Stage, len(s.Stages))
	for _, stage := range s.Stages {
		if stage.ID == "" {
			return fmt.Errorf("pipeline %q has a stage with no id", s.Name)
		}
		if !validToken(stage.ID) {
			return fmt.Errorf("pipeline %q stage id %q must be a name-safe token (letters, digits, '-', '_')", s.Name, stage.ID)
		}
		if _, ok := known[stage.ID]; ok {
			return fmt.Errorf("pipeline %q stage id %q is not unique", s.Name, stage.ID)
		}
		known[stage.ID] = struct{}{}
		stageByID[stage.ID] = stage
	}
	for _, stage := range s.Stages {
		if err := validateStageExecutor(s.Name, stage); err != nil {
			return err
		}
		// #768 mutating-implement safety model (spec double-key + scheduled-write gate).
		// Kept out of validateStageExecutor because it spans stage AND spec context (the
		// schedule block); an APPEND that leaves every read-only kind's validator intact.
		if err := s.validateMutatingStage(stage); err != nil {
			return err
		}
		// source is meaningful only for a gate or a review agent. Both bind to the
		// structured PR produced by an upstream implement stage, so validate the same
		// dependency and source-kind invariants at add time instead of silently ignoring
		// a stray source on another kind.
		if err := validateStageSource(s.Name, stage, stageByID); err != nil {
			return err
		}
		if err := s.validateGateMerge(stage); err != nil {
			return err
		}
		if stage.Retry < 0 {
			return fmt.Errorf("pipeline %q stage %q retry must be >= 0", s.Name, stage.ID)
		}
		if stage.Timeout != "" {
			parsed, err := time.ParseDuration(stage.Timeout)
			if err != nil {
				return fmt.Errorf("pipeline %q stage %q timeout %q is invalid: %w", s.Name, stage.ID, stage.Timeout, err)
			}
			if parsed <= 0 {
				return fmt.Errorf("pipeline %q stage %q timeout %q must be positive", s.Name, stage.ID, stage.Timeout)
			}
		}
		if err := validateDecisions(fmt.Sprintf("pipeline %q stage %q", s.Name, stage.ID), stage.SuccessDecisions); err != nil {
			return err
		}
		for _, dep := range stage.Needs {
			if dep == "" {
				continue
			}
			if dep == stage.ID {
				return fmt.Errorf("pipeline %q stage %q depends on itself", s.Name, stage.ID)
			}
			if _, ok := known[dep]; !ok {
				return fmt.Errorf("pipeline %q stage %q references unknown need %q", s.Name, stage.ID, dep)
			}
		}
	}
	return s.detectCycle()
}

// validateGateMerge enforces the auto-merge double key and robot-review rung.
// The stage-level merge: auto opt-in is valid only on gates; the pipeline-level
// allow_auto_merge key must independently authorize it. At least one review
// stage must bind to the same implement source so auto-ship is never unreviewed.
func (s Spec) validateGateMerge(stage Stage) error {
	merge := strings.TrimSpace(stage.Merge)
	if merge == "" {
		return nil
	}
	if stage.Kind() != StageKindGate {
		return fmt.Errorf("pipeline %q stage %q sets merge %q but is not a gate stage; merge is only valid on gate stages", s.Name, stage.ID, merge)
	}
	if merge != GateMergeAuto {
		return fmt.Errorf("pipeline %q stage %q merge mode %q is invalid; use %q or omit merge for the human-merge default", s.Name, stage.ID, merge, GateMergeAuto)
	}
	if !s.AllowAutoMerge {
		return fmt.Errorf("pipeline %q stage %q sets merge: auto without allow_auto_merge: true", s.Name, stage.ID)
	}
	for _, candidate := range s.Stages {
		if candidate.Kind() == StageKindAgentReview && strings.TrimSpace(candidate.Source) == strings.TrimSpace(stage.Source) {
			return nil
		}
	}
	return fmt.Errorf("pipeline %q stage %q sets merge: auto but source %q has no source-bound review stage; auto-merge requires at least one action: review stage with the same source", s.Name, stage.ID, stage.Source)
}

// validateStageSource applies the spec-level half of source binding. Gate-local
// requirements (source is mandatory, non-self, and in Needs) remain in
// validateGateStage; this helper adds the source-stage kind check shared by gates
// and reviews and rejects source everywhere else.
func validateStageSource(pipelineName string, stage Stage, stageByID map[string]Stage) error {
	source := strings.TrimSpace(stage.Source)
	switch stage.Kind() {
	case StageKindGate:
		if src, ok := stageByID[source]; ok && src.Kind() != StageKindAgentImplement {
			return fmt.Errorf("pipeline %q stage %q gate source %q must be a mutating implement stage (action: implement) so the gate has a PR to watch", pipelineName, stage.ID, source)
		}
		return nil
	case StageKindAgentReview:
		if source == "" {
			return nil
		}
		if source == stage.ID {
			return fmt.Errorf("pipeline %q stage %q review source cannot be the stage itself", pipelineName, stage.ID)
		}
		if !containsToken(stage.Needs, source) {
			return fmt.Errorf("pipeline %q stage %q review source %q must be one of the stage's needs (a review binds to an upstream stage it depends on)", pipelineName, stage.ID, source)
		}
		if src, ok := stageByID[source]; ok && src.Kind() != StageKindAgentImplement {
			return fmt.Errorf("pipeline %q stage %q review source %q must be a mutating implement stage (action: implement) so the review has a PR to inspect", pipelineName, stage.ID, source)
		}
		return nil
	default:
		if source != "" {
			return fmt.Errorf("pipeline %q stage %q sets source %q but is neither a gate nor an action: review agent stage", pipelineName, stage.ID, source)
		}
		return nil
	}
}

// validateSchedule checks the optional schedule block: a present block requires a
// positive interval and, when set, a non-negative jitter (both Go durations).
func (s Spec) validateSchedule() error {
	if s.Schedule == nil {
		return nil
	}
	if s.Schedule.Interval == "" {
		return fmt.Errorf("pipeline %q schedule requires an interval", s.Name)
	}
	interval, err := time.ParseDuration(s.Schedule.Interval)
	if err != nil {
		return fmt.Errorf("pipeline %q schedule interval %q is invalid: %w", s.Name, s.Schedule.Interval, err)
	}
	if interval <= 0 {
		return fmt.Errorf("pipeline %q schedule interval %q must be positive", s.Name, s.Schedule.Interval)
	}
	if s.Schedule.Jitter != "" {
		jitter, err := time.ParseDuration(s.Schedule.Jitter)
		if err != nil {
			return fmt.Errorf("pipeline %q schedule jitter %q is invalid: %w", s.Name, s.Schedule.Jitter, err)
		}
		if jitter < 0 {
			return fmt.Errorf("pipeline %q schedule jitter %q must be >= 0", s.Name, s.Schedule.Jitter)
		}
	}
	return nil
}

// validateTrigger checks the deliberately narrow external-trigger MVP. The
// connection token is embedded in a quoted Activepieces expression, so it uses
// a stricter external-id pattern (including an alphanumeric first byte) as an
// injection guard.
func (s Spec) validateTrigger() error {
	if s.Trigger == nil {
		return nil
	}
	if s.Trigger.Kind == "" {
		return fmt.Errorf("pipeline %q trigger kind is required; supported kinds: email, pipeline", s.Name)
	}
	if s.Trigger.Kind != "email" && s.Trigger.Kind != "pipeline" {
		return fmt.Errorf("pipeline %q trigger kind %q is not supported; supported kinds: email, pipeline", s.Name, s.Trigger.Kind)
	}
	// The bridge rejects runs of repo-less pipelines, so a triggered flow would
	// fire 400s forever; fail at add time instead of silently at every email.
	if strings.TrimSpace(s.Repo) == "" {
		return fmt.Errorf("pipeline %q declares a trigger but no repo; triggered runs need a managed repo (add `repo: owner/name`)", s.Name)
	}
	if s.Trigger.Kind == "pipeline" {
		if s.Trigger.Pipeline == "" {
			return fmt.Errorf("pipeline %q pipeline trigger requires an upstream pipeline name", s.Name)
		}
		if !validToken(s.Trigger.Pipeline) {
			return fmt.Errorf("pipeline %q upstream pipeline %q must be a name-safe token (letters, digits, '-', '_')", s.Name, s.Trigger.Pipeline)
		}
		if s.Trigger.Pipeline == s.Name {
			return fmt.Errorf("pipeline %q pipeline trigger cannot reference itself", s.Name)
		}
		if s.Schedule != nil {
			return fmt.Errorf("pipeline %q combines schedule and pipeline trigger; schedule+pipeline hybrid semantics are not supported", s.Name)
		}
		if s.Trigger.Connection != "" || s.Trigger.Mailbox != "" || s.Trigger.Map != nil {
			return fmt.Errorf("pipeline %q pipeline trigger sets email-only fields; connection, mailbox, and map are only valid for kind: email", s.Name)
		}
		return nil
	}
	if s.Trigger.Pipeline != "" {
		return fmt.Errorf("pipeline %q email trigger sets pipeline %q; pipeline is only valid for kind: pipeline", s.Name, s.Trigger.Pipeline)
	}
	if !triggerConnectionPattern.MatchString(s.Trigger.Connection) {
		return fmt.Errorf("pipeline %q trigger connection %q must match [A-Za-z0-9][A-Za-z0-9_-]*", s.Name, s.Trigger.Connection)
	}
	if strings.Contains(s.Trigger.Mailbox, "{{") {
		return fmt.Errorf("pipeline %q trigger mailbox must not contain an Activepieces expression", s.Name)
	}
	if s.Trigger.Map != nil && len(s.Trigger.Map) == 0 {
		return fmt.Errorf("pipeline %q trigger map is explicitly empty; omit map or declare at least one output", s.Name)
	}
	keys := make([]string, 0, len(s.Trigger.Map))
	for key := range s.Trigger.Map {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !ValidTriggerPayloadKey(key) {
			return fmt.Errorf("pipeline %q trigger map output %q must be 1-64 bytes and match ^[a-z][a-z0-9_]*$", s.Name, key)
		}
		selector := s.Trigger.Map[key]
		if !containsToken(triggerMapSelectors, selector) {
			return fmt.Errorf("pipeline %q trigger map output %q selector %q is invalid; use one of: %s", s.Name, key, selector, strings.Join(triggerMapSelectors, ", "))
		}
	}
	return nil
}

// detectCycle runs a depth-first search over the stage needs graph and reports
// the first cycle it finds, naming the stages involved. It mirrors
// workflow.detectDelegationCycle: unknown needs are already rejected by Validate,
// so the traversal only walks real edges.
func (s Spec) detectCycle() error {
	edges := make(map[string][]string, len(s.Stages))
	for _, stage := range s.Stages {
		for _, dep := range stage.Needs {
			if dep != "" {
				edges[stage.ID] = append(edges[stage.ID], dep)
			}
		}
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(s.Stages))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		stack = append(stack, id)
		for _, dep := range edges[id] {
			switch state[dep] {
			case visiting:
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

	for _, stage := range s.Stages {
		if state[stage.ID] == unvisited {
			if cycle := visit(stage.ID); cycle != nil {
				return fmt.Errorf("pipeline %q stages form a dependency cycle: %s", s.Name, strings.Join(cycle, " -> "))
			}
		}
	}
	return nil
}

// validateStageExecutor enforces the EXACTLY-ONE-OF cmd|agent contract (#757). A
// shell stage (cmd set, agent empty) is unchanged. An agent stage (agent set, cmd
// empty) must carry a non-empty prompt and a read-only action (ask/review, never
// implement); normalize() already defaulted an empty action to "ask", so a blank
// action here means the stage set neither cmd nor agent. Setting both, or neither,
// is an error.
func validateStageExecutor(pipelineName string, stage Stage) error {
	hasCmd := stage.Cmd != ""
	hasAgent := stage.Agent != ""
	hasGate := stage.Gate != ""
	// The exactly-one-of executor guard is shared across kinds: it must run before
	// Stage.Kind() (which is only meaningful once exactly one executor is set). A
	// future kind that lives on the EXISTING axes (e.g. implement = agent + an
	// implement action) is a pure append below; the jobless gate (#768 Phase 2)
	// introduces a NEW executor field (gate: predicate), which is why this count widens
	// to exactly-one-of {cmd, agent, gate}. That count widening is inherent to adding an
	// executor axis, not a per-kind settle-logic edit — the seams the foundation
	// guarantees (validateStageExecutor's dispatch, stageSettleOutcome) stay append-only.
	// The cmd+agent and neither messages stay BYTE-IDENTICAL (the neither case only
	// gains the && !hasGate term so a pure gate stage is not mis-rejected as "neither").
	switch {
	case hasCmd && hasAgent:
		return fmt.Errorf("pipeline %q stage %q sets both cmd and agent; a stage is exactly one of a shell cmd or an agent", pipelineName, stage.ID)
	case hasGate && hasCmd:
		return fmt.Errorf("pipeline %q stage %q sets both gate and cmd; a stage is exactly one of a shell cmd, an agent, or a gate", pipelineName, stage.ID)
	case hasGate && hasAgent:
		return fmt.Errorf("pipeline %q stage %q sets both gate and agent; a stage is exactly one of a shell cmd, an agent, or a gate", pipelineName, stage.ID)
	case !hasCmd && !hasAgent && !hasGate:
		return fmt.Errorf("pipeline %q stage %q has neither cmd nor agent; a stage needs exactly one", pipelineName, stage.ID)
	}
	// Exactly one executor is set; dispatch the per-kind rules by Stage.Kind(). A
	// NEW stage kind (implement/gate/orchestrate) adds a case here — it never edits
	// another kind's validator.
	switch stage.Kind() {
	case StageKindShell:
		return validateShellStage(pipelineName, stage)
	case StageKindAgentAsk, StageKindAgentReview:
		return validateAgentStage(pipelineName, stage)
	case StageKindOrchestrate:
		return validateOrchestrateStage(pipelineName, stage)
	case StageKindAgentImplement:
		return validateImplementStage(pipelineName, stage)
	case StageKindAgentProduce:
		return validateProduceStage(pipelineName, stage)
	case StageKindGate:
		return validateGateStage(pipelineName, stage)
	default:
		// StageKindUnknown past the exactly-one-of guard = an agent stage (Agent
		// set, Cmd empty) with an unrecognized action; validateAgentStage produces
		// the #757 read-only-leaf rejection (implement) or the invalid-action error.
		return validateAgentStage(pipelineName, stage)
	}
}

// validateShellStage validates a shell (cmd) stage: it carries no agent-only
// fields, so a stray prompt/action is a mis-declared stage (very likely a
// forgotten agent: key). Byte-identical to the pre-refactor shell branch.
func validateShellStage(pipelineName string, stage Stage) error {
	if stage.Prompt != "" {
		return fmt.Errorf("pipeline %q stage %q sets prompt but is a shell (cmd) stage; prompt is only for agent stages", pipelineName, stage.ID)
	}
	if stage.Action != "" {
		return fmt.Errorf("pipeline %q stage %q sets action but is a shell (cmd) stage; action is only for agent stages", pipelineName, stage.ID)
	}
	return nil
}

// validateAgentStage validates a read-only agent stage (#757): a name-safe agent
// token, a non-empty prompt, and an ask/review action ("implement" is rejected as
// a non-leaf verb). Byte-identical to the pre-refactor agent branch.
func validateAgentStage(pipelineName string, stage Stage) error {
	if !validToken(stage.Agent) {
		return fmt.Errorf("pipeline %q stage %q agent %q must be a name-safe token (letters, digits, '-', '_')", pipelineName, stage.ID, stage.Agent)
	}
	if stage.Prompt == "" {
		return fmt.Errorf("pipeline %q stage %q agent stage requires a non-empty prompt", pipelineName, stage.ID)
	}
	if stage.Action == "implement" {
		return fmt.Errorf("pipeline %q stage %q agent stage action %q is not allowed; an agent stage is a read-only leaf (use one of: %s)", pipelineName, stage.ID, stage.Action, strings.Join(AgentStageActionCandidates, ", "))
	}
	if !containsToken(AgentStageActionCandidates, stage.Action) {
		return fmt.Errorf("pipeline %q stage %q agent stage action %q is invalid; use one of: %s", pipelineName, stage.ID, stage.Action, strings.Join(AgentStageActionCandidates, ", "))
	}
	return nil
}

// validateOrchestrateStage validates an orchestrate agent stage (#758): a
// name-safe agent token, a non-empty coordinator prompt, and the read-only "ask"
// verb. An orchestrate coordinator ASKS and fans out a bounded sub-tree; review
// and implement are rejected (review does not fan out; implement is not a
// read-only entry verb). normalize() already defaults an empty action to "ask",
// so the only way action != "ask" here is an explicit review/implement (or another
// token) the author set. The Orchestrate flag itself is guaranteed true by
// Stage.Kind() before this is reached, so it is not re-checked. This is a pure
// APPEND: the shell and #757 agent validators are byte-identical.
func validateOrchestrateStage(pipelineName string, stage Stage) error {
	if !validToken(stage.Agent) {
		return fmt.Errorf("pipeline %q stage %q agent %q must be a name-safe token (letters, digits, '-', '_')", pipelineName, stage.ID, stage.Agent)
	}
	if stage.Prompt == "" {
		return fmt.Errorf("pipeline %q stage %q orchestrate stage requires a non-empty coordinator prompt", pipelineName, stage.ID)
	}
	if stage.Action != DefaultAgentStageAction {
		return fmt.Errorf("pipeline %q stage %q orchestrate stage action %q is not allowed; an orchestrate coordinator runs the read-only %q verb and fans out a bounded sub-tree", pipelineName, stage.ID, stage.Action, DefaultAgentStageAction)
	}
	return nil
}

// validateImplementStage validates a MUTATING implement agent stage (#768): a
// name-safe agent token and a non-empty prompt — exactly like a read-only agent
// stage, but WITHOUT validateAgentStage's read-only-leaf rejection, since a validated
// implement stage is allowed. The write: true acknowledgement and the scheduled-write
// gate live in Spec.validateMutatingStage (they need spec-level context). The token /
// prompt messages are byte-identical to validateAgentStage so a mis-typed implement
// stage fails with the same wording an ask stage would.
func validateImplementStage(pipelineName string, stage Stage) error {
	if !validToken(stage.Agent) {
		return fmt.Errorf("pipeline %q stage %q agent %q must be a name-safe token (letters, digits, '-', '_')", pipelineName, stage.ID, stage.Agent)
	}
	if stage.Prompt == "" {
		return fmt.Errorf("pipeline %q stage %q agent stage requires a non-empty prompt", pipelineName, stage.ID)
	}
	return nil
}

// validateProduceStage validates the pure spec half of a Codex data-producing
// stage. Filesystem overlap and symlink checks require store/home context and run
// separately at `pipeline add` preflight.
func validateProduceStage(pipelineName string, stage Stage) error {
	if !validToken(stage.Agent) {
		return fmt.Errorf("pipeline %q stage %q agent %q must be a name-safe token (letters, digits, '-', '_')", pipelineName, stage.ID, stage.Agent)
	}
	if stage.Prompt == "" {
		return fmt.Errorf("pipeline %q stage %q produce stage requires a non-empty prompt", pipelineName, stage.ID)
	}
	if len(stage.Writes) == 0 {
		return fmt.Errorf("pipeline %q stage %q produce stage requires at least one writes path", pipelineName, stage.ID)
	}
	for _, path := range stage.Writes {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("pipeline %q stage %q writes path %q must be absolute", pipelineName, stage.ID, path)
		}
		if filepath.Clean(path) != path {
			return fmt.Errorf("pipeline %q stage %q writes path %q must be cleaned (use %q)", pipelineName, stage.ID, path, filepath.Clean(path))
		}
	}
	if stage.CheckRetries != nil {
		if *stage.CheckRetries < 0 {
			return fmt.Errorf("pipeline %q stage %q check_retries must be >= 0", pipelineName, stage.ID)
		}
		if stage.Check == "" {
			return fmt.Errorf("pipeline %q stage %q sets check_retries without a check command", pipelineName, stage.ID)
		}
	}
	return nil
}

// validateGateStage validates a JOBLESS GATE stage (#768 Phase 2): a recognized
// predicate token (only pr_merged today) plus a Source that names an upstream stage
// this gate depends on. Requiring Source to be one of the gate's own Needs is what
// makes "an existing upstream needs stage" fall out for free — Validate separately
// rejects any need that does not reference a known stage, so a source-in-needs also
// references a known stage. A gate carries no prompt/action (those are agent-only), so
// a stray one is a mis-declared stage (very likely a forgotten agent: key).
func validateGateStage(pipelineName string, stage Stage) error {
	if !containsToken(GatePredicateCandidates, stage.Gate) {
		return fmt.Errorf("pipeline %q stage %q gate predicate %q is invalid; use one of: %s", pipelineName, stage.ID, stage.Gate, strings.Join(GatePredicateCandidates, ", "))
	}
	if stage.Prompt != "" {
		return fmt.Errorf("pipeline %q stage %q sets prompt but is a gate stage; prompt is only for agent stages", pipelineName, stage.ID)
	}
	if stage.Action != "" {
		return fmt.Errorf("pipeline %q stage %q sets action but is a gate stage; action is only for agent stages", pipelineName, stage.ID)
	}
	if stage.Source == "" {
		return fmt.Errorf("pipeline %q stage %q gate stage requires a source (the upstream stage whose PR to watch)", pipelineName, stage.ID)
	}
	if stage.Source == stage.ID {
		return fmt.Errorf("pipeline %q stage %q gate source cannot be the stage itself", pipelineName, stage.ID)
	}
	if !containsToken(stage.Needs, stage.Source) {
		return fmt.Errorf("pipeline %q stage %q gate source %q must be one of the stage's needs (a gate watches an upstream stage it depends on)", pipelineName, stage.ID, stage.Source)
	}
	return nil
}

// validateMutatingStage enforces the #768 mutating-implement SAFETY MODEL, which
// spans stage-level AND spec-level context (so it is a Spec method, not part of the
// per-kind executor dispatch):
//   - Spec double-key: `action: implement` REQUIRES `write: true` and, conversely,
//     `write: true` is valid ONLY on an implement stage — so neither a prompt injection
//     nor a template typo can flip a read-only pipeline into a writing one.
//   - Schedule gate: a MUTATING stage on a SCHEDULED pipeline (a schedule: block) is
//     rejected unless the pipeline sets `allow_scheduled_writes: true`, so an unattended
//     nightly writing code is always a deliberate, spelled-twice choice. A manual-only
//     pipeline (no schedule) needs only the per-stage write: true.
//
// It reads stage.Action / stage.Write directly (not Stage.Kind()) so it also catches
// write: true on a NON-implement stage, which Kind() classifies as its base kind.
func (s Spec) validateMutatingStage(stage Stage) error {
	isImplement := stage.Agent != "" && stage.Action == "implement"
	isProduce := stage.Agent != "" && stage.Action == "produce"
	isMutating := isImplement || isProduce
	if stage.Write && !isMutating {
		return fmt.Errorf("pipeline %q stage %q sets write: true but is not a mutating stage; write: true is only valid with action: implement or action: produce", s.Name, stage.ID)
	}
	if isMutating && !stage.Write {
		return fmt.Errorf("pipeline %q stage %q sets action %q without write: true; a mutating stage must acknowledge writes with write: true", s.Name, stage.ID, stage.Action)
	}
	if isMutating && s.Schedule != nil && !s.AllowScheduledWrites {
		return fmt.Errorf("pipeline %q stage %q is a mutating %s stage on a scheduled pipeline; set allow_scheduled_writes: true to permit unattended writes", s.Name, stage.ID, stage.Action)
	}
	if isMutating && s.Trigger != nil && !s.AllowTriggeredWrites {
		return fmt.Errorf("pipeline %q stage %q is a mutating %s stage on a triggered pipeline; set allow_triggered_writes: true to permit externally triggered writes", s.Name, stage.ID, stage.Action)
	}
	if !isProduce {
		if len(stage.Writes) > 0 {
			return fmt.Errorf("pipeline %q stage %q sets writes but is not an action: produce stage", s.Name, stage.ID)
		}
		if stage.Network {
			return fmt.Errorf("pipeline %q stage %q sets network: true but is not an action: produce stage", s.Name, stage.ID)
		}
		if stage.Check != "" {
			return fmt.Errorf("pipeline %q stage %q sets check but is not an action: produce stage", s.Name, stage.ID)
		}
		if stage.CheckRetries != nil {
			return fmt.Errorf("pipeline %q stage %q sets check_retries but is not an action: produce stage", s.Name, stage.ID)
		}
	}
	return nil
}

func containsToken(candidates []string, value string) bool {
	for _, c := range candidates {
		if c == value {
			return true
		}
	}
	return false
}

// validateDecisions rejects any success_decisions value outside
// SuccessDecisionCandidates, so blocked/failed/typos are caught at add time.
func validateDecisions(scope string, decisions []string) error {
	if len(decisions) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(SuccessDecisionCandidates))
	for _, d := range SuccessDecisionCandidates {
		allowed[d] = struct{}{}
	}
	for _, d := range decisions {
		if _, ok := allowed[d]; !ok {
			return fmt.Errorf("%s success_decisions %q is invalid; use one of: %s", scope, d, strings.Join(SuccessDecisionCandidates, ", "))
		}
	}
	return nil
}

// validToken reports whether value is a non-empty name-safe token (letters,
// digits, '-', '_'). Pipeline names and stage ids must satisfy it: the name is a
// DB primary key and the stem of the runner agent name, and stage ids appear
// verbatim in job fingerprints and deterministic job ids.
func validToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func trimAll(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}

func indexOf(values []string, target string) int {
	for i, v := range values {
		if v == target {
			return i
		}
	}
	return 0
}
