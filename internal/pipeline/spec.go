// Package pipeline defines the declarative spec for the #681 pipeline primitive:
// a small DAG of shell stages plus an optional interval schedule. A spec is
// loaded from a YAML file by `gitmoot pipeline add`, validated structurally, and
// stored VERBATIM in the DB (the raw bytes + a content hash). Each run snapshots
// the content hash it was created from, so a run always executes the spec it was
// created against — not the file's (or the row's) later state. The package is a
// leaf: it depends only on the standard library and gopkg.in/yaml.v3, so the CLI
// and the (later) advancer can both parse/validate without importing the engine.
package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// DefaultSuccessDecisions are the gitmoot_result decisions that, absent an
// explicit override, mark a pipeline stage succeeded. skipped is default-on
// because an honest no-work result is a successful pipeline stage. blocked and
// failed are deliberately NOT here: the advancer treats them as park states, so
// they can never mean success (see SuccessDecisionCandidates).
var DefaultSuccessDecisions = []string{"approved", "implemented", "skipped"}

// SuccessDecisionCandidates are the gitmoot_result decisions a spec MAY list in
// success_decisions (top-level or per-stage). It is the subset of the contract's
// ResultDecisions that can plausibly mean "this stage succeeded": blocked and
// failed are excluded because the advancer treats them as park states, so listing
// either as a success would contradict the advance semantics. An explicit list is
// strict: if it omits skipped, a skipped result folds failed because the author
// required real work.
var SuccessDecisionCandidates = []string{"approved", "implemented", "changes_requested", "skipped"}

// DefaultAgentStageAction is the read-only agent verb an agent stage runs when
// action is unset (#757). Agent stages are leaves, so the default is "ask".
const DefaultAgentStageAction = "ask"

// AgentStageActionCandidates are the read-only verbs an agent stage MAY set in
// action (#757). Both are read-only; "implement" is deliberately excluded — an
// agent stage must never write, fan out, or leave the read-only leaf contract.
var AgentStageActionCandidates = []string{"ask", "review"}

// GatePredicatePRMerged is the only gate predicate today (#768 Phase 2): a jobless
// gate stage carrying `gate: pr_merged` folds success once the PR opened by its
// upstream source (implement) stage has MERGED. Human merge is the default; the
// separately double-keyed merge:auto mode lets the advancer satisfy the same
// composable wait after bound review approval and green checks.
const GatePredicatePRMerged = "pr_merged"

// GateMergeAuto is the sole opt-in gate merge mode. When selected on a
// pr_merged gate, the pipeline advancer may squash-merge the source PR after all
// source-bound review rungs approve the stamped head and GitHub reports it ready.
// An absent merge field keeps the historical human-merge wait unchanged.
const GateMergeAuto = "auto"

// GatePredicateCandidates are the predicate tokens a gate stage's `gate:` MAY set
// (#768 Phase 2). It starts deliberately small — just pr_merged — so the vocab is a
// pure append when a future external-gate predicate (e.g. #758 subtree_settled) lands.
var GatePredicateCandidates = []string{GatePredicatePRMerged}

// Spec is the parsed, validated declaration of a pipeline.
type Spec struct {
	// Name is the pipeline's stable identifier (the DB primary key and the stem of
	// its hidden shell runner agent name). Must be a name-safe token.
	Name string `yaml:"name"`
	// Repo is the optional managed repo (owner/repo) the stages run against. It is
	// carried onto the run's stage jobs and the runner agent's repo scope.
	Repo string `yaml:"repo,omitempty"`
	// Group is optional display metadata used to organize pipelines independently
	// of their repo. Empty means callers should fall back to Repo for display.
	Group string `yaml:"group,omitempty"`
	// Description is optional display metadata explaining a pipeline's purpose.
	// It is shown verbatim on detail surfaces and has no execution semantics.
	Description string `yaml:"description,omitempty"`
	// EnvFile is an optional operator-owned 0600 file containing secret
	// KEY=VALUE entries. Shell stages opt into individual names through EnvKeys.
	EnvFile string `yaml:"env_file,omitempty"`
	// Env contains non-secret defaults. Entries are delivered only when a shell
	// stage explicitly names them in EnvKeys.
	Env map[string]string `yaml:"env,omitempty"`
	// Schedule, when present, drives interval-based auto-runs (heartbeat idiom: an
	// interval plus optional jitter; no cron in v1).
	Schedule *Schedule `yaml:"schedule,omitempty"`
	// Trigger, when present, declares an event source materialized by Gitmoot.
	// Email uses an owned Activepieces flow; pipeline waits for a successful run
	// of another named pipeline.
	Trigger *Trigger `yaml:"trigger,omitempty"`
	// AllowScheduledWrites, when true, permits MUTATING (implement) stages on a
	// SCHEDULED pipeline (one carrying a schedule: block) (#768 safety layer 2).
	// Absent (the default), a scheduled pipeline REJECTS any mutating stage at
	// validation, so an unattended nightly can never write code + open a PR without a
	// deliberate, spelled-twice opt-in. Irrelevant to a manual-only pipeline (no
	// schedule), which a mutating stage enters with only its per-stage write: true.
	AllowScheduledWrites bool `yaml:"allow_scheduled_writes,omitempty"`
	// AllowTriggeredWrites, when true, permits MUTATING (implement or produce)
	// stages on a pipeline carrying a trigger block. External event content is
	// untrusted, so triggered writes require this explicit pipeline-level opt-in
	// in addition to the stage's own write: true acknowledgement.
	AllowTriggeredWrites bool `yaml:"allow_triggered_writes,omitempty"`
	// AllowAutoMerge is the pipeline-level half of the auto-merge double key.
	// A gate that declares merge: auto is rejected unless this is explicitly true.
	// It does not authorize scheduled writes; a scheduled pipeline still needs
	// allow_scheduled_writes for its source implement stage.
	AllowAutoMerge bool `yaml:"allow_auto_merge,omitempty"`
	// SuccessDecisions overrides DefaultSuccessDecisions for every stage that does
	// not set its own. Empty means DefaultSuccessDecisions.
	SuccessDecisions []string `yaml:"success_decisions,omitempty"`
	// Stages is the DAG of shell stages, keyed by unique id and wired by needs.
	Stages []Stage `yaml:"stages"`
}

// Schedule is the interval-based auto-run cadence for a pipeline.
type Schedule struct {
	// Interval is the base cadence (e.g. "24h"); required when a schedule block is
	// present and must parse as a positive Go duration.
	Interval string `yaml:"interval,omitempty"`
	// Jitter is an optional random delay added to each interval (e.g. "15m"); must
	// parse as a non-negative duration when set.
	Jitter string `yaml:"jitter,omitempty"`
}

// Trigger is an event source for a pipeline. Email triggers use the
// Activepieces-specific connection/mailbox/map fields; pipeline triggers name an
// upstream pipeline whose successful runs start this pipeline.
type Trigger struct {
	Kind       string            `yaml:"kind"`
	Pipeline   string            `yaml:"pipeline,omitempty"`
	Connection string            `yaml:"connection,omitempty"`
	Mailbox    string            `yaml:"mailbox,omitempty"`
	Map        map[string]string `yaml:"map,omitempty"`
}

// Stage is one step in the pipeline DAG. A stage is EITHER a shell step (cmd) or
// an agent step (agent) — exactly one of the two (see Validate). An agent stage
// runs a named managed gitmoot agent as a LEAF (#757): it may only ask/review
// (read-only), never fan out, and its result folds by decision just like a shell
// stage.
type Stage struct {
	// ID is the stage's unique, name-safe identifier. It appears verbatim in the
	// stage job fingerprint and deterministic job id, so it must be a safe token.
	ID string `yaml:"id"`
	// Cmd is the shell command run verbatim via `sh -c`. Exactly one of Cmd or
	// Agent must be set per stage (#757); a cmd stage's behavior is unchanged.
	Cmd string `yaml:"cmd,omitempty"`
	// Agent, when set, runs the named managed gitmoot agent for this stage instead
	// of a shell command (#757). The agent runs as a LEAF: it may only ask/review
	// (read-only) and its delegations are stripped. Mutually exclusive with Cmd.
	Agent string `yaml:"agent,omitempty"`
	// Prompt is the instruction handed to an agent stage's agent (required when
	// Agent is set; ignored for a shell stage). Builder 2 prepends upstream
	// needs-context; for now the runtime prompt is Prompt verbatim.
	Prompt string `yaml:"prompt,omitempty"`
	// Action is the agent verb for an agent stage: "ask" (default) or "review"
	// (read-only leaves), or "implement" (a MUTATING stage, #768) which additionally
	// requires Write. Ignored for a shell stage.
	Action string `yaml:"action,omitempty"`
	// Write, when true, ACKNOWLEDGES that this stage MUTATES the repo (#768). It is
	// REQUIRED for — and valid ONLY on — an `action: implement` stage: the spec
	// double-key that stops a prompt injection or a template typo from silently
	// flipping a read-only pipeline into a writing one. Rejected on any other stage.
	Write bool `yaml:"write,omitempty"`
	// Gate, when set, declares a JOBLESS GATE stage (#768 Phase 2): it runs NEITHER a
	// shell cmd NOR an agent — it WAITS on an external predicate and folds only once
	// the predicate holds. The value is the predicate token (today only "pr_merged";
	// see GatePredicateCandidates). A gate mints no worker job, no runtime session; the
	// advancer's settle pass evaluates its predicate per scan. Mutually exclusive with
	// Cmd and Agent (exactly one of the three executors). Pairs with Source.
	Gate string `yaml:"gate,omitempty"`
	// Merge optionally selects who satisfies a pr_merged gate. Empty preserves the
	// default human-merge wait. The only accepted value is "auto", valid only on a
	// gate and protected by the top-level allow_auto_merge double key plus at least
	// one source-bound review rung.
	Merge string `yaml:"merge,omitempty"`
	// Source is the id of the upstream implement stage whose opened PR this stage
	// binds to. It is required on a gate stage and optional on an action: review
	// agent stage; it is rejected on every other stage kind. Source must be one of
	// the stage's own Needs, so binding begins only after the implement stage has
	// succeeded and its job payload carries the final PR stamp.
	Source string `yaml:"source,omitempty"`
	// Needs lists the ids of stages that must succeed before this stage is enqueued.
	Needs []string `yaml:"needs,omitempty"`
	// Timeout optionally bounds the stage job (a positive Go duration when set).
	Timeout string `yaml:"timeout,omitempty"`
	// Retry is how many times a failed stage may be re-attempted (>= 0).
	Retry int `yaml:"retry,omitempty"`
	// EnvKeys is the deny-by-default list of environment names (or glob selectors)
	// made available to this shell stage. Agent and gate stages may not request it.
	EnvKeys []string `yaml:"env_keys,omitempty"`
	// SuccessDecisions optionally overrides the pipeline default for this stage.
	SuccessDecisions []string `yaml:"success_decisions,omitempty"`
	// Orchestrate, when true on an agent stage, promotes the stage from a read-only
	// LEAF (#757) to a bounded agent SUB-TREE root (#758): the stage's agent runs as
	// a coordinator whose delegations[] are NOT stripped but fan out as children
	// OWNED by the stage job (ParentJobID = the stage job, RootJobID = the stage
	// job's own id). It requires Agent + Prompt and the read-only "ask" verb; it is
	// mutually exclusive with Cmd. Opt-in per stage so the cheap, reproducible shell
	// line stays the default (the #758 "when in doubt, use a #757 leaf" rule).
	Orchestrate bool `yaml:"orchestrate,omitempty"`
	// Writes declares the absolute operator-owned data paths an action: produce
	// stage may write. These are additive Codex workspace-write grants; they do not
	// replace Codex's default writable roots. Required on produce and rejected on
	// every other stage kind.
	Writes []string `yaml:"writes,omitempty"`
	// Network opts a produce stage into Codex workspace-write network access.
	Network bool `yaml:"network,omitempty"`
	// Check is a trusted operator command run after each valid produce result. A
	// non-zero exit re-asks the same runtime session up to CheckRetries times.
	Check string `yaml:"check,omitempty"`
	// CheckRetries is a pointer so validation can distinguish an omitted default
	// from an explicitly declared check_retries: 0 on a non-produce stage.
	CheckRetries *int `yaml:"check_retries,omitempty"`
}

// StageKind classifies a stage by its declared fields. It is the SINGLE place a
// stage's kind is decided: validation dispatches its per-kind rules on it
// (validateStageExecutor) and the advancer's fold/settle pass dispatches the
// per-kind settle predicate on it (stageSettleOutcome). Adding a future kind —
// mutating implement (#768), an external gate (#768 Phase 2), or an orchestrate
// sub-tree (#758) — is an APPEND here plus a new case at each dispatch point,
// never an edit to an existing case. Kinds on the existing cmd|agent axes
// (implement, orchestrate) are a pure classifier append; a jobless gate additionally
// adds its own executor field and a `case s.Gate != ""` branch below (and widens the
// exactly-one-of count in validateStageExecutor) — still no existing case is edited.
type StageKind int

const (
	// StageKindUnknown is the zero value: a structurally malformed stage (neither
	// or both executors set, or an agent stage with an unrecognized action). A
	// validated spec never yields it — Validate rejects such stages first.
	StageKindUnknown StageKind = iota
	// StageKindShell is a shell stage: Cmd set, Agent empty. Runs `sh -c` and folds
	// by its gitmoot_result decision.
	StageKindShell
	// StageKindAgentAsk is a read-only agent stage (#757) running the "ask" verb
	// (the default when action is unset). A leaf: reads + decides, never mutates or
	// fans out.
	StageKindAgentAsk
	// StageKindAgentReview is a read-only agent stage (#757) running the "review"
	// verb. Same leaf contract as ask; kept distinct so a future kind is appended as
	// a sibling case rather than by editing a shared agent branch.
	StageKindAgentReview
	// StageKindAgentImplement is a MUTATING agent stage (#768 Model A): Agent set,
	// action "implement", plus the required Write acknowledgement. Unlike the
	// read-only ask/review kinds it takes a WRITABLE task-worktree, commits + pushes +
	// opens a PR, and folds success only once a PR exists (fold-on-PR-opened). It is
	// still a LEAF — it may mutate but never fan out (delegations/human_questions are
	// stripped). The implement job never merges its own PR; only an explicitly
	// authorized downstream auto-merge gate may do so.
	StageKindAgentImplement
	// StageKindGate is a JOBLESS GATE stage (#768 Phase 2): Gate set, neither Cmd nor
	// Agent. It mints NO worker job — the ENQUEUE pass marks it in-flight without a job
	// and the advancer's SETTLE pass folds it when its external predicate holds
	// (pr_merged: the PR opened by the upstream Source implement stage has merged),
	// parking the run on the stage timeout if it never does. Its executor is a NEW axis
	// (Gate) alongside cmd|agent, which is why validateStageExecutor's exactly-one-of
	// guard widens to {cmd, agent, gate}.
	StageKindGate
	// StageKindOrchestrate is an agent stage carrying orchestrate:true (#758): the
	// stage's agent runs as a bounded sub-tree COORDINATOR. Unlike the agent-leaf
	// kinds its delegations[] are NOT stripped — they fan out as children owned by
	// the stage job. It is classified by the Orchestrate flag BEFORE the plain agent
	// action switch below, so an orchestrate stage is never mistaken for a leaf
	// ask/review. Appended as a sibling kind; the existing cases are untouched.
	StageKindOrchestrate
	// StageKindAgentProduce is a data-writing, sandboxed agent leaf. It receives
	// additive writable-path grants, never a branch/task/PR, and folds by decision.
	StageKindAgentProduce
)

// Kind classifies the stage. A shell stage (Cmd set) is StageKindShell; an agent
// stage (Agent set) is StageKindAgentAsk / StageKindAgentReview by its action (an
// empty action defaults to ask, matching normalize()). Anything else — both
// executors set, neither set, or an unrecognized agent action such as
// "implement" — is StageKindUnknown, which Validate rejects. This method performs
// NO validation; it only names the shape. Callers must have validated first, or be
// validateStageExecutor itself, which classifies AFTER its shared exactly-one-of
// guard so that here exactly one executor is set.
func (s Stage) Kind() StageKind {
	switch {
	case s.Orchestrate && s.Cmd == "" && s.Agent != "":
		// #758: an agent stage with orchestrate:true is a sub-tree coordinator,
		// classified here BEFORE the plain agent action switch so it is never folded
		// as a read-only leaf. Its action rules (ask only) live in
		// validateOrchestrateStage; a Cmd set alongside falls through to the
		// both-executors StageKindUnknown rejection like any other malformed stage.
		return StageKindOrchestrate
	case s.Cmd != "" && s.Agent != "":
		return StageKindUnknown
	case s.Cmd != "":
		return StageKindShell
	case s.Agent != "":
		switch s.Action {
		case "", DefaultAgentStageAction: // "" defaults to ask (see normalize)
			return StageKindAgentAsk
		case "review":
			return StageKindAgentReview
		case "implement":
			return StageKindAgentImplement
		case "produce":
			return StageKindAgentProduce
		}
	case s.Gate != "":
		// A jobless gate (#768 Phase 2): no cmd, no agent, a gate predicate instead.
		// Reached only after validateStageExecutor's widened exactly-one-of guard has
		// confirmed gate is the SOLE executor set.
		return StageKindGate
	}
	return StageKindUnknown
}

// Load parses raw YAML into a Spec, normalizes it, and validates it. Unknown
// fields are rejected (KnownFields) so a typo'd key surfaces as an error at
// `pipeline add` time rather than being silently ignored. The returned Spec is
// safe to persist; the raw bytes are what get stored verbatim (see Hash).
func Load(raw []byte) (Spec, error) {
	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("parse pipeline spec: %w", err)
	}
	spec.normalize()
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Hash returns the hex-encoded SHA-256 of the verbatim spec bytes. It is stored
// alongside the YAML and snapshotted onto each run so a run can be tied to the
// exact spec content it was created from (editing the file — even whitespace —
// changes the hash, which is intentional: a run executes its snapshot).
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// EffectiveSuccessDecisions returns the decisions that mark the given stage
// succeeded: the stage's own success_decisions if set, else the pipeline
// top-level override if set, else DefaultSuccessDecisions. The result is always a
// fresh non-empty slice (a defensive copy), so callers may retain it freely.
func (s Spec) EffectiveSuccessDecisions(stage Stage) []string {
	switch {
	case len(stage.SuccessDecisions) > 0:
		return append([]string(nil), stage.SuccessDecisions...)
	case len(s.SuccessDecisions) > 0:
		return append([]string(nil), s.SuccessDecisions...)
	default:
		return append([]string(nil), DefaultSuccessDecisions...)
	}
}
