package permissionpolicy

import (
	"context"
	"encoding/json"
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

// Inventory resolves declarations through adapters, never through a parallel
// runtime/policy table. The unresolved set is durable job evidence whose agent
// row has disappeared, so runtime and policy intentionally remain unknown.
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
			return nil, fmt.Errorf("resolve permission-policy adapter for agent %q: %w", agent.Name, err)
		}
		property := Resolve(adapter, agent)
		if property == runtime.PermissionPolicyNotApplied {
			configs = append(configs, Config{Agent: agent.Name, Runtime: agent.Runtime, Policy: runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy), Property: property})
		}
	}
	unresolved, err := store.ListUnresolvedJobAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, agent := range unresolved {
		configs = append(configs, Config{Agent: agent, Property: runtime.PermissionPolicyUnresolved})
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
	var result []string
	for _, config := range current {
		if _, ok := known[config.Key()]; !ok {
			result = append(result, config.String())
		}
	}
	return result
}

// RecordWarning records warn-only dispatch evidence. It never changes job state
// or delivery control flow. FOR READ-ONLY WORK, REFUSING THE JOB IS NOT
// ENFORCEMENT; THE DEFECT IS THAT IT RUNS UNENFORCED.
func RecordWarning(ctx context.Context, store *db.Store, job db.Job, agent runtime.Agent, adapter any, now time.Time) (bool, error) {
	property, declared := runtime.DeclaredPermissionPolicyApplication(adapter, agent)
	if !declared {
		return false, nil
	}
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
	return store.ClaimPermissionPolicyWarning(ctx, warning.Agent, warning.Runtime, warning.Policy, windowStart, db.JobEvent{
		JobID: job.ID, Kind: WarningEventKind, Message: string(raw),
	})
}

func shellRiskAccepted(agent runtime.Agent) bool {
	if agent.Runtime != runtime.ShellRuntime || invokesModelCLI(agent.RuntimeRef) {
		return false
	}
	return strings.TrimSpace(agent.RuntimeRef) != ""
}

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
