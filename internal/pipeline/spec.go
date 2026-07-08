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
// explicit override, mark a pipeline stage succeeded. They mirror the
// gitmoot_result contract's "the work landed" terminal decisions. blocked and
// failed are deliberately NOT here: the advancer treats them as park states, so
// they can never mean success (see SuccessDecisionCandidates).
var DefaultSuccessDecisions = []string{"approved", "implemented"}

// SuccessDecisionCandidates are the gitmoot_result decisions a spec MAY list in
// success_decisions (top-level or per-stage). It is the subset of the contract's
// ResultDecisions that can plausibly mean "this stage succeeded": blocked and
// failed are excluded because the advancer treats them as park states, so listing
// either as a success would contradict the advance semantics.
var SuccessDecisionCandidates = []string{"approved", "implemented", "changes_requested"}

// DefaultAgentStageAction is the read-only agent verb an agent stage runs when
// action is unset (#757). Agent stages are leaves, so the default is "ask".
const DefaultAgentStageAction = "ask"

// AgentStageActionCandidates are the read-only verbs an agent stage MAY set in
// action (#757). Both are read-only; "implement" is deliberately excluded — an
// agent stage must never write, fan out, or leave the read-only leaf contract.
var AgentStageActionCandidates = []string{"ask", "review"}

// Spec is the parsed, validated declaration of a pipeline.
type Spec struct {
	// Name is the pipeline's stable identifier (the DB primary key and the stem of
	// its hidden shell runner agent name). Must be a name-safe token.
	Name string `yaml:"name"`
	// Repo is the optional managed repo (owner/repo) the stages run against. It is
	// carried onto the run's stage jobs and the runner agent's repo scope.
	Repo string `yaml:"repo,omitempty"`
	// Schedule, when present, drives interval-based auto-runs (heartbeat idiom: an
	// interval plus optional jitter; no cron in v1).
	Schedule *Schedule `yaml:"schedule,omitempty"`
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
	// Action is the read-only agent verb for an agent stage: "ask" (default) or
	// "review". "implement" is rejected — an agent stage must stay a read-only
	// leaf. Ignored for a shell stage.
	Action string `yaml:"action,omitempty"`
	// Needs lists the ids of stages that must succeed before this stage is enqueued.
	Needs []string `yaml:"needs,omitempty"`
	// Timeout optionally bounds the stage job (a positive Go duration when set).
	Timeout string `yaml:"timeout,omitempty"`
	// Retry is how many times a failed stage may be re-attempted (>= 0).
	Retry int `yaml:"retry,omitempty"`
	// SuccessDecisions optionally overrides the pipeline default for this stage.
	SuccessDecisions []string `yaml:"success_decisions,omitempty"`
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
