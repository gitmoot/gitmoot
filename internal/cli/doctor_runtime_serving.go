package cli

import (
	"context"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// runtimeServingStatus resolves, in the CLI layer that owns store access, which
// runtimes the ENGINE is currently holding off a provider refusal (#1558).
//
// THE PREDICATE, and it is the whole design: a runtime is reported as not
// serving iff a NON-TERMINAL job attributed to it carries a runtime_quota or
// runtime_auth blocker class AND the BlockerRetryAt the classifier wrote is
// still in the FUTURE. That field is the engine's own forward-looking "I am
// still waiting this out", so doctor reports what the engine believes rather
// than a second opinion derived from a clock.
//
// The alternative - "the newest 403 within N minutes" - was rejected on
// measurement, not taste: it asserts the provider is refusing NOW from a
// timestamp that only says it refused THEN. On this host's store 12 jobs
// succeeded on a runtime AFTER carrying a runtime_quota block, and 4 more after
// runtime_auth; a newest-wins rule would report every one of those runtimes as
// dead. Replacing a false green with a false red is the same defect with the
// sign flipped, and the false green is what #1558 was filed for.
//
// FOUR NEGATIVES, each a case this must NOT report:
//   - a hold whose RetryAt has PASSED: the engine will retry it, and the
//     provider may well serve it;
//   - a job that reached a terminal state: its block is history, and a later
//     success on that runtime is the common case;
//   - a hold with NO RetryAt: absent is not "unknown therefore bad". Measured,
//     42 of 55 succeeded checkout_contention rows carry no retry time, so
//     absence is the COMMON case on at least one path;
//   - a job whose runtime cannot be attributed: excluded, never guessed.
//
// Store failures omit the whole check rather than manufacturing a verdict,
// matching stuckJobsStatus's convention.
// The clock is a parameter so the four negatives are testable at a fixed
// instant; runtimeServingStatus is the production entry point and passes the
// real one.
func runtimeServingStatus(paths config.Paths) *doctor.RuntimeServingStatus {
	return runtimeServingStatusAt(paths, time.Now().UTC())
}

func runtimeServingStatusAt(paths config.Paths, now time.Time) *doctor.RuntimeServingStatus {
	if strings.TrimSpace(paths.Database) == "" {
		return nil
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return nil
	}
	defer store.Close()
	ctx := context.Background()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return nil
	}
	registered := registeredAgentRuntimes(ctx, store)
	status := &doctor.RuntimeServingStatus{}
	seen := make(map[string]struct{})
	for _, job := range jobs {
		if workflow.IsFinalJobState(job.State) {
			// Final means the block is history. `blocked` is deliberately NOT
			// excluded here: it is settled but not final (#632's split), and a
			// blocked job holding a future retry is exactly the live hold this
			// check exists to report.
			continue
		}
		payload, err := jobListPayload(job)
		if err != nil {
			continue
		}
		class := strings.TrimSpace(payload.BlockerClass)
		if class != string(blockerClassRuntimeQuota) && class != string(blockerClassRuntimeAuth) {
			continue
		}
		retryAt, ok := parseBlockerRetryAt(payload.BlockerRetryAt)
		if !ok || !retryAt.After(now) {
			// No hold, or a hold the engine has already aged out.
			continue
		}
		runtimeName := blockedJobRuntime(ctx, store, job, payload, registered)
		if runtimeName == "" {
			// Unattributable: excluded rather than guessed. Three rows of 284 on
			// this host - small, which is exactly why the rule has to be written
			// down: nobody would notice three wrong rows.
			continue
		}
		key := runtimeName + "\x00" + class
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		status.Blocks = append(status.Blocks, doctor.RuntimeServingBlock{
			Runtime: runtimeName,
			Class:   class,
			JobID:   job.ID,
			Detail:  firstLineTrimmed(payload.BlockerSuggestedAction),
			RetryAt: retryAt,
		})
	}
	return status
}

// parseBlockerRetryAt reads the classifier's forward-looking hold. An absent or
// unparseable value is NOT a hold: ok=false, so the caller reports nothing.
func parseBlockerRetryAt(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// registeredAgentRuntimes indexes each registered agent's default runtime, the
// third and last resolution source.
func registeredAgentRuntimes(ctx context.Context, store *db.Store) map[string]string {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return nil
	}
	byName := make(map[string]string, len(agents))
	for _, agent := range agents {
		byName[agent.Name] = strings.TrimSpace(agent.Runtime)
	}
	return byName
}

// blockedJobRuntime attributes a blocked job to the runtime its child would have
// used, in PRIORITY ORDER, and returns "" when none of the sources can answer.
//
// It extends the dashboard's existing resolveJobRuntime (agent registry, then an
// ephemeral worker's own spec) rather than duplicating it: the payload's
// EffectiveRuntime wins when present, and the job's own effective_runtime EVENT
// is consulted before falling back to that shared resolver.
//
// No single source covers the population, which is why all three are needed and
// why the residue is excluded. Measured over the 284 runtime_quota /
// runtime_auth rows on this host: 107 carry payload.effective_runtime, 101 have
// an effective_runtime event, 281 have an agent row that still exists, and 3 are
// attributable by none of them.
func blockedJobRuntime(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload, registered map[string]string) string {
	if name := strings.TrimSpace(payload.EffectiveRuntime); name != "" {
		return name
	}
	if name := effectiveRuntimeFromEvents(ctx, store, job.ID); name != "" {
		return name
	}
	return resolveJobRuntime(job, payload, registered)
}

// effectiveRuntimeFromEvents recovers the runtime from the job's own
// effective_runtime event, whose message names it. The event is the only source
// for jobs dispatched before the payload field existed.
func effectiveRuntimeFromEvents(ctx context.Context, store *db.Store, jobID string) string {
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		return ""
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Kind != "effective_runtime" {
			continue
		}
		return runtimeNameFromEffectiveRuntimeMessage(events[index].Message)
	}
	return ""
}

// runtimeNameFromEffectiveRuntimeMessage extracts the runtime from the event's
// own wording ("job runs on runtime kimi (agent default kimi); ..."). It returns
// "" rather than a guess when the wording does not match, so a message format
// change degrades to "unattributable" - which this check excludes - instead of
// to a wrong runtime.
func runtimeNameFromEffectiveRuntimeMessage(message string) string {
	const marker = "runs on runtime "
	index := strings.Index(message, marker)
	if index < 0 {
		return ""
	}
	rest := strings.TrimSpace(message[index+len(marker):])
	if rest == "" {
		return ""
	}
	name, _, _ := strings.Cut(rest, " ")
	return strings.TrimSpace(strings.Trim(name, ";,"))
}
