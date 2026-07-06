package pipeline

import (
	"fmt"
	"strings"
	"time"
)

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
	s.SuccessDecisions = trimAll(s.SuccessDecisions)
	for i := range s.Stages {
		s.Stages[i].ID = strings.TrimSpace(s.Stages[i].ID)
		s.Stages[i].Cmd = strings.TrimSpace(s.Stages[i].Cmd)
		s.Stages[i].Timeout = strings.TrimSpace(s.Stages[i].Timeout)
		s.Stages[i].Needs = trimAll(s.Stages[i].Needs)
		s.Stages[i].SuccessDecisions = trimAll(s.Stages[i].SuccessDecisions)
	}
}

// Validate enforces the structural invariants of a pipeline spec: a name-safe
// pipeline name, at least one stage, unique name-safe stage ids, a required cmd
// per stage, needs that reference known sibling ids (never the stage itself),
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

	known := make(map[string]struct{}, len(s.Stages))
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
	}
	for _, stage := range s.Stages {
		if stage.Cmd == "" {
			return fmt.Errorf("pipeline %q stage %q has no cmd", s.Name, stage.ID)
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
