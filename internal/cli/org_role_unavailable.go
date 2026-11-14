package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// clearRoleUnavailableOnSuccess clears a role's per-runtime unavailability
// incident after a job attributed to role succeeds on runtimeName: that success
// is the definitive signal that the runtime is available to the role again.
//
// Incidents used to be opened by a Claude-only quota classifier at the delivery
// seams. #2587 removed the Claude job runtime, so nothing opens an incident any
// more; detecting omp quota walls is follow-up #2625. Reading, refusing on and
// clearing incidents stay, so any incident already on disk keeps its meaning
// and still clears.
func clearRoleUnavailableOnSuccess(ctx context.Context, store *db.Store, role, runtimeName string) error {
	role = strings.TrimSpace(role)
	runtimeName = strings.TrimSpace(runtimeName)
	if role == "" || runtimeName == "" || store == nil {
		return nil
	}
	return store.ClearOrgRoleUnavailableForRuntime(ctx, role, runtimeName)
}

func unavailableRoleDispatchError(incident db.OrgRoleUnavailable) error {
	return fmt.Errorf("org role %q is unavailable (reason=%s) until %s; dispatch refused",
		incident.Role, incident.Reason, formatOrgRoleUnavailableUntil(incident.Until))
}

// knownRuntimeName reports whether name resolves to a real runtime adapter.
// This is the same test the per-job --runtime override validation uses
// (resolveJobRuntimeOverride), so an unrecognized stored or selected runtime is
// classified identically at both ends.
func knownRuntimeName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	_, err := (runtime.Factory{}).Adapter(strings.ToLower(strings.TrimSpace(name)))
	return err == nil
}

// orgRoleUnavailableRefusesRuntime decides whether an ACTIVE incident row bars a
// dispatch whose selected runtime is selectedRuntime (#1641).
//
// The row is written per-runtime (UpsertOrgRoleUnavailableForRuntime) because a
// provider wall is a property of one provider, not of the role: a claude quota
// wall says nothing about codex or kimi. Enforcement must therefore compare the
// row's runtime against the runtime this dispatch will ACTUALLY use.
//
// FAIL CLOSED, in both directions:
//   - An empty stored runtime keeps its pre-#1641 whole-role meaning. It is not a
//     corrupt value: UpsertOrgRoleUnavailable still writes "", and the runtime
//     column was added by ALTER TABLE ... DEFAULT ” (#1490), so every row
//     predating runtime attribution is legitimately unattributed.
//   - An empty or unrecognized SELECTED runtime means the caller could not say
//     what this job will run as, so it cannot claim to be a different runtime.
//     This is checked first, before any exemption.
//   - A runtime gitmoot REMOVED (#2587: claude, codex, kimi) is not corrupt: it
//     is a recognized historical identity. Its incident bars only dispatches on
//     that same runtime, which no longer exist, so it never refuses a known
//     selected runtime.
//   - Any other unrecognized stored runtime is corrupt, and corruption is not
//     permission to dispatch.
func orgRoleUnavailableRefusesRuntime(incident db.OrgRoleUnavailable, selectedRuntime string) bool {
	stored := strings.ToLower(strings.TrimSpace(incident.Runtime))
	selected := strings.ToLower(strings.TrimSpace(selectedRuntime))
	if !knownRuntimeName(selected) {
		return true
	}
	if runtime.IsRemovedRuntime(stored) {
		// A removed runtime can never be the selected one (selected is known
		// above), so its historical incident refuses nothing.
		return false
	}
	if !knownRuntimeName(stored) {
		return true
	}
	return stored == selected
}

// refuseUnavailableOrgRole refuses a dispatch only when the role's active
// incident belongs to the runtime this dispatch actually selected. Callers must
// pass the runtime from their own final resolution — the value the job will run
// as, overrides included — never the registered agent's default when an override
// is in play.
//
// The store read is unconditional so its eager expiry of a stale row (and
// #1490's fail-closed error on a malformed until) still happens for every
// dispatch, whatever the runtime decision turns out to be.
func refuseUnavailableOrgRole(ctx context.Context, store *db.Store, role, selectedRuntime string, now time.Time) error {
	role = strings.TrimSpace(role)
	if role == "" || store == nil {
		return nil
	}
	incident, found, err := store.GetActiveOrgRoleUnavailable(ctx, role, now)
	if err != nil {
		return fmt.Errorf("read org role %q unavailability: %w", role, err)
	}
	if found && orgRoleUnavailableRefusesRuntime(incident, selectedRuntime) {
		return unavailableRoleDispatchError(incident)
	}
	return nil
}

// selectedJobDispatchRuntime reports the runtime a QUEUED job will actually run
// as, resolved in the same order the claiming worker resolves it.
//
// THE ORDER IS LOAD-BEARING AND IS NOT THE OBVIOUS ONE (#1641, #1952 review):
//
//  1. An ephemeral job's spec wins. daemon_worker.go materializes the throwaway
//     worker UNCONDITIONALLY when payload.Ephemeral is set, building the agent
//     from spec.Runtime and UpsertAgent-ing it under the job's agent name — so it
//     OVERWRITES any pre-existing row of that name. Consulting the agents table
//     first therefore checks the wall against a runtime that will never execute
//     whenever a same-name row happens to exist.
//  2. Otherwise the registered agent's runtime.
//  3. A per-job runtime override wins over both, because the worker applies it
//     after materialization.
//
// Returning "" means UNKNOWN, not "some other runtime": refuseUnavailableOrgRole
// fails closed on an empty value, so an ephemeral spec with an empty or
// unrecognized runtime refuses rather than dispatches.
//
// Deliberately NOT resolveTranscriptRuntime, and for a sharper reason than the
// first version of this comment gave: that helper answers "which transcript do I
// read" and treats the ephemeral spec as a LAST fallback, the same shape
// dashboard_web.go uses for a rendered column. Read/display paths may rank the
// spec last; write/execute paths must not — mailbox.go assigns the spec's runtime
// onto the agent at enqueue for exactly this reason. A dispatch refusal is an
// execute-path decision.
func selectedJobDispatchRuntime(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) string {
	agent, ok := selectedJobRuntimeAgent(ctx, store, job, payload)
	if !ok {
		return ""
	}
	return agent.Runtime
}

// selectedJobRuntimeAgent resolves the runtime.Agent a QUEUED job will actually
// run as, and is the ONE place the precedence override > registered row is
// expressed. Every execute-path decision about a queued job — refuse, hold,
// probe, reserve, serialize — must resolve through here, because the whole
// #1641/#1952 defect class is a consumer that derived a runtime from the agents
// table while the job ran on something else.
//
// ok=false means UNRESOLVABLE, never "some other runtime": callers must fail
// closed on it, and each one keeps the conservative fallback it already had.
func selectedJobRuntimeAgent(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) (runtime.Agent, bool) {
	if store == nil {
		return runtime.Agent{}, false
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return runtime.Agent{}, false
	}
	return applyJobRuntimeOverride(runtimeAgent(agent), payload), true
}
