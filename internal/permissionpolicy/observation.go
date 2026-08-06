package permissionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const (
	WarningEventKind         = "permission_policy_not_applied"
	BaselineEventKind        = "permission_policy_baseline_recorded"
	BaselineGrowthEventKind  = "permission_policy_baseline_exceeded"
	BaselineLoweredEventKind = "permission_policy_baseline_lowered"
	ObservationJobID         = "permission-policy-observation"
	WarningText              = "gitmoot applied no permission-policy flag for this job; any sandboxing came from host runtime config gitmoot does not read."
	ShellRiskAcceptance      = "skills/gitmoot/references/SAFETY.md#shell-runtime-risk-acceptance-2026-08-05"
	WarningWindow            = 24 * time.Hour
)

type Config struct {
	Agent    string                              `json:"agent"`
	Runtime  string                              `json:"runtime"`
	Policy   string                              `json:"policy"`
	Property runtime.PermissionPolicyApplication `json:"property"`
}

func (c Config) Key() string {
	return strings.Join([]string{c.Agent, c.Runtime, c.Policy, string(c.Property)}, "\x1f")
}

func (c Config) String() string {
	return fmt.Sprintf("agent=%q runtime=%q policy=%q property=%q", c.Agent, c.Runtime, c.Policy, c.Property)
}

type Warning struct {
	Runtime        string                              `json:"runtime"`
	Policy         string                              `json:"policy"`
	Capability     string                              `json:"capability"`
	JobID          string                              `json:"job_id"`
	Property       runtime.PermissionPolicyApplication `json:"property"`
	Agent          string                              `json:"agent"`
	Warning        string                              `json:"warning"`
	RiskAcceptance string                              `json:"risk_acceptance,omitempty"`
	Effects        *Effects                            `json:"effects,omitempty"`
}

const (
	PushInstrumentLocalUpstream = "local-upstream"
	PushInstrumentLSRemote      = "ls-remote"
	PushInstrumentPayload       = "payload"
	PushInstrumentUnavailable   = "unavailable"
)

// Effects are observed repository outcomes attached to the coalesced R1
// warning. Nil booleans mean the observer could not determine the fact; they
// are deliberately serialized as null rather than omitted or collapsed to
// false.
type Effects struct {
	CheckoutDirty          *bool  `json:"checkout_dirty"`
	BranchPushed           *bool  `json:"branch_pushed"`
	BranchPushedInstrument string `json:"branch_pushed_instrument"`
	PROpened               bool   `json:"pr_opened"`
}

type EffectGit interface {
	StatusPorcelain(context.Context) (string, error)
	BehindCount(context.Context, string) (int, error)
	RemoteBranches(context.Context, []string) (map[string]struct{}, error)
}

type StaticProvider struct {
	Property runtime.PermissionPolicyApplication
}

func (p StaticProvider) PermissionPolicyApplication(runtime.Agent) runtime.PermissionPolicyApplication {
	return p.Property
}

func Resolve(adapter any, agent runtime.Agent) runtime.PermissionPolicyApplication {
	return runtime.ResolvePermissionPolicyApplication(adapter, agent)
}

// Inventory resolves fixable live agent configurations through adapters, never
// through a parallel runtime/policy table. Missing-agent job history is reported
// separately because durable historical rows cannot be remediated by changing a
// live agent configuration.
func Inventory(ctx context.Context, store *db.Store) ([]Config, error) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	configs := make([]Config, 0, len(agents))
	factory := runtime.Factory{}
	for _, stored := range agents {
		agent := runtime.Agent{
			Name: stored.Name, Runtime: stored.Runtime, RuntimeRef: stored.RuntimeRef,
			AutonomyPolicy: stored.AutonomyPolicy,
		}
		adapter, err := factory.Adapter(agent.Runtime)
		if err != nil {
			configs = append(configs, Config{
				Agent: agent.Name, Runtime: agent.Runtime,
				Policy:   runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy),
				Property: runtime.PermissionPolicyUnresolved,
			})
			continue
		}
		property := Resolve(adapter, agent)
		if property == runtime.PermissionPolicyNotApplied {
			configs = append(configs, Config{Agent: agent.Name, Runtime: agent.Runtime, Policy: runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy), Property: property})
		}
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Key() < configs[j].Key() })
	return configs, nil
}

func Keys(configs []Config) []string {
	keys := make([]string, 0, len(configs))
	for _, config := range configs {
		keys = append(keys, config.Key())
	}
	sort.Strings(keys)
	return keys
}

func DecodeKey(key string) string {
	parts := strings.Split(key, "\x1f")
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	return Config{Agent: parts[0], Runtime: parts[1], Policy: parts[2], Property: runtime.PermissionPolicyApplication(parts[3])}.String()
}

func NewSinceBaseline(current []Config, baseline []string) []string {
	known := make(map[string]struct{}, len(baseline))
	for _, key := range baseline {
		known[key] = struct{}{}
	}
	result := make([]string, 0)
	for _, config := range current {
		if _, ok := known[config.Key()]; !ok {
			result = append(result, config.String())
		}
	}
	return result
}

// RemovedFromBaseline describes recorded configurations absent from the current
// live inventory so a lowering event names the remediations it represents.
func RemovedFromBaseline(current []Config, baseline []string) []string {
	present := make(map[string]struct{}, len(current))
	for _, config := range current {
		present[config.Key()] = struct{}{}
	}
	removed := make([]string, 0)
	for _, key := range baseline {
		if _, ok := present[key]; !ok {
			removed = append(removed, DecodeKey(key))
		}
	}
	return removed
}

// RecordWarning records warn-only dispatch evidence. It never changes job state
// or delivery control flow. FOR READ-ONLY WORK, REFUSING THE JOB IS NOT
// ENFORCEMENT; THE DEFECT IS THAT IT RUNS UNENFORCED.
func RecordWarning(ctx context.Context, store *db.Store, job db.Job, agent runtime.Agent, adapter any, now time.Time) (bool, error) {
	property := Resolve(adapter, agent)
	if property != runtime.PermissionPolicyNotApplied && property != runtime.PermissionPolicyUnresolved {
		return false, nil
	}
	policy := ""
	if property != runtime.PermissionPolicyUnresolved {
		policy = runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy)
	}
	warning := Warning{
		Runtime: agent.Runtime, Policy: policy, Capability: job.Type, JobID: job.ID,
		Property: property, Agent: job.Agent, Warning: WarningText,
	}
	if shellRiskAccepted(agent) {
		warning.RiskAcceptance = ShellRiskAcceptance
	}
	raw, err := json.Marshal(warning)
	if err != nil {
		return false, err
	}
	windowStart := now.UTC().Truncate(WarningWindow).Format(time.RFC3339)
	return store.ClaimPermissionPolicyWarning(ctx, warning.Agent, warning.Runtime, warning.Policy, warning.Capability, windowStart, db.JobEvent{
		JobID: job.ID, Kind: WarningEventKind, Message: string(raw),
	})
}

// RecordEffects attaches completion-time repository observations to the
// existing coalesced warning event. It never creates a second observation. A
// partial capture is still persisted with null for unknown facts, and the
// returned error is advisory so callers can log it without changing job state.
func RecordEffects(ctx context.Context, store *db.Store, jobID, checkout, branch string, pullRequest int, git EffectGit) (bool, error) {
	event, ok, err := store.GetEarliestJobEventByKind(ctx, jobID, WarningEventKind)
	if err != nil || !ok {
		return false, err
	}
	var warning Warning
	if err := json.Unmarshal([]byte(event.Message), &warning); err != nil {
		return false, fmt.Errorf("decode permission-policy warning: %w", err)
	}
	if warning.Effects != nil {
		return false, nil
	}

	effects := Effects{PROpened: pullRequest > 0, BranchPushedInstrument: PushInstrumentUnavailable}
	var captureErrs []error
	if git != nil && strings.TrimSpace(checkout) != "" {
		status, statusErr := git.StatusPorcelain(ctx)
		if statusErr != nil {
			captureErrs = append(captureErrs, fmt.Errorf("inspect checkout status: %w", statusErr))
		} else {
			effects.CheckoutDirty = boolPointer(strings.TrimSpace(status) != "")
		}

		branch = strings.TrimSpace(branch)
		if branch == "" {
			effects.BranchPushed = boolPointer(false)
			effects.BranchPushedInstrument = PushInstrumentPayload
		} else if _, localErr := git.BehindCount(ctx, "origin/"+branch); localErr == nil {
			// A successful rev-list proves the local remote-tracking branch exists.
			// It answers whether this branch has been pushed at least once without a
			// network round trip; the record names that local instrument explicitly.
			effects.BranchPushed = boolPointer(true)
			effects.BranchPushedInstrument = PushInstrumentLocalUpstream
		} else if remote, remoteErr := git.RemoteBranches(ctx, []string{branch}); remoteErr != nil {
			captureErrs = append(captureErrs, fmt.Errorf("inspect remote branch after local upstream lookup failed: %w", remoteErr))
		} else {
			_, exists := remote[branch]
			effects.BranchPushed = boolPointer(exists)
			effects.BranchPushedInstrument = PushInstrumentLSRemote
		}
	}

	warning.Effects = &effects
	raw, err := json.Marshal(warning)
	if err != nil {
		return false, errors.Join(append(captureErrs, err)...)
	}
	updated, updateErr := store.UpdateClaimedPermissionPolicyWarning(ctx, jobID, WarningEventKind, string(raw))
	return updated, errors.Join(append(captureErrs, updateErr)...)
}

func boolPointer(value bool) *bool {
	return &value
}

func shellRiskAccepted(agent runtime.Agent) bool {
	if agent.Runtime != runtime.ShellRuntime || invokesModelCLI(agent.RuntimeRef) {
		return false
	}
	return strings.TrimSpace(agent.RuntimeRef) != ""
}

// invokesModelCLI is advisory-only provenance recognition for attaching the
// shell risk-acceptance reference. Wrappers, renamed binaries, and indirect
// launchers can evade it; a miss leaves the warning intact and only omits that
// reference, so this heuristic must never become load-bearing.
func invokesModelCLI(command string) bool {
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, "'\"`;|&(){}[]")
		base := filepath.Base(field)
		switch base {
		case "claude", "codex", "kimi", "kimi-cli", "omp":
			return true
		}
	}
	return false
}
