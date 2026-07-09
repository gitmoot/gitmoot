package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
	"github.com/jerryfane/gitmoot/internal/prompts"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// RenderBaseJobPrompt renders the job prompt for a payload WITHOUT any memory
// block — exactly the prompt the mailbox assembles before the optional #626
// injection. It is exported so the offline A/B replay harness can compute the
// with/without-memory token delta over real stored jobs.
func RenderBaseJobPrompt(payload JobPayload, action string) string {
	return prompts.RenderJob(payload.prompt(action))
}

// MemoryController is the injected, off-by-default seam that wires agent
// persistent memory (#626) into job execution. It is constructed by the cli/
// daemon layer from resolved config and set on Engine.Memory; when nil (every
// path with no enrolled agent, and the global kill switch) the engine builds a
// Mailbox with nil memory hooks, so prompt assembly and the terminal path are
// byte-identical to before this feature existed.
//
// Phase 1 is OBSERVATION MODE: the READ path injects only gitmoot-authored
// confirmed facts, and agent-returned learnings are SHADOW-logged to
// memory_observations (never injected, never promoted). No confirmation
// transaction runs yet (Phase 2).
type MemoryController struct {
	// Store is the memory store (the shared workflow store).
	Store *db.Store
	// Enabled reports whether the given executor agent is enrolled in memory. It
	// folds in both the per-agent [agents.<name>].memory flag and the global
	// [memory].disabled kill switch. A controller with a nil Enabled treats every
	// agent as disabled (defensive: no reads, no writes).
	Enabled func(agentName string) bool
	// TokenBudget caps the estimated tokens of the injected block (0 == unbounded).
	TokenBudget int
	// MaxEntries caps how many confirmed rows are considered for injection.
	MaxEntries int
	// DistillAtTerminal enables the deterministic distill-at-terminal WRITE
	// producers (#737 P4.1). Default false: with it off, record() runs exactly the
	// Phase-1 write path and the terminal is byte-identical (no distilled rows).
	DistillAtTerminal bool
	// DistillMaxPerJob is the hard per-job cap on distill writes; <= 0 falls back
	// to config.DefaultMemoryDistillMaxPerJob so the producers are always bounded.
	DistillMaxPerJob int
	// DistillAllJobs widens distill past the memory-enrolled set to every job. When
	// false (default) distill runs only for enrolled agents, via enabledFor.
	DistillAllJobs bool
}

// enabledFor reports whether memory is active for the given executor agent.
func (c *MemoryController) enabledFor(agentName string) bool {
	if c == nil || c.Store == nil || c.Enabled == nil {
		return false
	}
	return c.Enabled(agentName)
}

// distillEnabledFor reports whether the deterministic distill-at-terminal WRITE
// producers (#737 P4.1) are active for the given executor agent. It is a SEPARATE
// gate from enabledFor: distill runs only when the master switch DistillAtTerminal
// is set, and — unlike the read path and the confirmed mechanical producers — it
// can run for UN-enrolled agents when DistillAllJobs is set (box-wide failure
// harvesting). When DistillAllJobs is false it falls back to the enrolled set.
func (c *MemoryController) distillEnabledFor(agentName string) bool {
	if c == nil || c.Store == nil || !c.DistillAtTerminal {
		return false
	}
	if c.DistillAllJobs {
		return true
	}
	return c.enabledFor(agentName)
}

// ownerForJob derives the structured memory owner for a job's executor. Phase 1
// scopes memory to REGISTERED agents (owner_kind=agent); the role-pool owner
// (owner_kind=role, template identity + version) is structural-only until the
// Phase-2 ephemeral writers land, so an ephemeral worker's synthetic name is
// simply never enrolled.
func ownerForJob(agent runtime.Agent, _ JobPayload) db.MemoryOwner {
	return db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: agent.Name}
}

// BuildMemoryMatchQuery returns the sanitized FTS5 MATCH query used by the
// memory read path. Callers outside workflow use this instead of reaching for
// memory.SanitizeFTSQuery directly so recall surfaces and prompt injection stay
// byte-for-byte aligned.
func BuildMemoryMatchQuery(instructions string) string {
	return memory.SanitizeFTSQuery(instructions)
}

// injectBlock is the READ path (job-prompt assembly). It builds a SANITIZED FTS
// query from the job instructions (never raw text into MATCH), runs the tiered
// confirmed-only retrieval, applies the token budget, and returns the rendered
// "Prior learnings" block — or "" when memory is off, the query is empty, or no
// confirmed fact matches (in which case the caller appends nothing). It never
// errors up: retrieval is best-effort and a query failure yields no block.
func (c *MemoryController) injectBlock(ctx context.Context, agent runtime.Agent, payload JobPayload) string {
	if !c.enabledFor(agent.Name) {
		return ""
	}
	entries := c.retrieve(ctx, ownerForJob(agent, payload), payload.Repo, payload.Instructions, c.MaxEntries)
	if len(entries) == 0 {
		return ""
	}
	block, _ := renderMemoryInjectionBlock(entries, c.TokenBudget, agent.Name)
	return block
}

// retrieve runs the tiered, confirmed-only, sanitized-FTS retrieval and returns
// the ranked entries (best-effort — a query error yields no entries). It does
// NOT check enrollment: the live injectBlock gates on enrollment first; the
// measurement-harness preview methods deliberately run it regardless so the
// mechanics can be measured even for agents not yet enrolled.
func (c *MemoryController) retrieve(ctx context.Context, owner db.MemoryOwner, repo, instructions string, limit int) []memory.Entry {
	if c == nil || c.Store == nil {
		return nil
	}
	query := BuildMemoryMatchQuery(instructions)
	if query == "" {
		return nil
	}
	if limit <= 0 {
		limit = 15
	}
	rows, err := c.Store.QueryConfirmedMemories(ctx, owner, repo, query, limit)
	if err != nil || len(rows) == 0 {
		return nil
	}
	entries := make([]memory.Entry, 0, limit)
	seen := make(map[int64]struct{}, len(rows))
	srcIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, memoryEntryFromConfirmed(r, false))
		seen[r.ID] = struct{}{}
		srcIDs = append(srcIDs, r.ID)
	}
	if len(entries) < limit {
		linked, err := c.Store.ListMemoryLinksForSourcesVisibleToOwner(ctx, owner, repo, srcIDs)
		if err == nil {
			for _, l := range linked {
				if _, dup := seen[l.Memory.ID]; dup {
					continue
				}
				seen[l.Memory.ID] = struct{}{}
				entries = append(entries, memoryEntryFromConfirmed(l.Memory, true))
				if len(entries) >= limit {
					break
				}
			}
		}
	}
	return entries
}

// PreviewEntries returns the ranked confirmed memories that WOULD be considered
// for injection for a job with the given executor agent, repo, and instructions,
// WITHOUT running the job and WITHOUT the enrollment gate. It powers the offline
// measurement harness (A/B replay + recall/precision@K). limit<=0 uses the
// controller's MaxEntries cap.
func (c *MemoryController) PreviewEntries(ctx context.Context, agentName, repo, instructions string, limit int) []memory.Entry {
	if limit <= 0 {
		limit = c.MaxEntries
	}
	return c.retrieve(ctx, db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: agentName}, repo, instructions, limit)
}

// PreviewBlock renders the memory block that WOULD be injected for a job (again
// ungated, for the harness), returning the block text, the entries injected, and
// the block's estimated token cost.
func (c *MemoryController) PreviewBlock(ctx context.Context, agentName, repo, instructions string) (block string, entries int, tokens int) {
	rendered, n := renderMemoryInjectionBlock(c.PreviewEntries(ctx, agentName, repo, instructions, 0), c.TokenBudget, agentName)
	return rendered, n, memory.EstimateTokens(rendered)
}

func memoryEntryFromConfirmed(r db.ConfirmedMemory, linked bool) memory.Entry {
	return memory.Entry{
		Scope:     r.Scope,
		Key:       r.Key,
		Content:   r.Content,
		UpdatedAt: r.UpdatedAt,
		Linked:    linked,
	}
}

func renderMemoryInjectionBlock(entries []memory.Entry, budget int, agentName string) (string, int) {
	block, n := memory.RenderBlock(entries, budget)
	if block == "" {
		return "", 0
	}
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "<agent-name>"
	}
	if !strings.HasSuffix(block, "\n") {
		block += "\n"
	}
	block += fmt.Sprintf("More project memory is searchable: run `gitmoot memory recall \"<query>\" --agent %s`.\n", name)
	return block, n
}

// record is the WRITE path, run at job terminal. The ENROLLED-ONLY Phase-1 body
// (recordEnrolled) (a) SHADOW-logs the agent's returned learnings to
// memory_observations after the deterministic pre-filters, and (b) writes any
// gitmoot-authored mechanical facts to confirmed_memories (the ONLY confirmed
// producers — deterministic, no LLM). On top of that, (c) the #737 P4.1
// distill-at-terminal producer stages deterministic PENDING observations behind
// its own config gate. It takes the job action so the producers can key facts by
// (action, outcome). Every write is best-effort: a failure is swallowed so
// memory can never fail an otherwise-successful job.
func (c *MemoryController) record(ctx context.Context, jobID string, agent runtime.Agent, action string, payload JobPayload, result AgentResult) {
	// The Phase-1 write path (learnings shadow-log + confirmed mechanical facts)
	// is ENROLLED-ONLY and unchanged. When the agent is not enrolled this whole
	// block is skipped exactly as the prior early-return did, so with distill off
	// (the default) the terminal path is byte-identical.
	if c.enabledFor(agent.Name) {
		c.recordEnrolled(ctx, jobID, agent, action, payload, result)
	}
	// (c) #737 P4.1 distill-at-terminal — a SEPARATE, config-gated producer that
	// stages PENDING observations only. It has its own gate (distillEnabledFor),
	// so it is a no-op unless DistillAtTerminal is set, and it can run for
	// un-enrolled agents when DistillAllJobs is set. Fail-safe: any error inside is
	// swallowed and can never affect the job outcome.
	c.distillAtTerminal(ctx, jobID, agent, action, payload, result)
}

// recordEnrolled is the Phase-1 ENROLLED-ONLY write path: shadow-log the agent's
// returned learnings and write gitmoot-authored mechanical confirmed facts. It is
// exactly the body record() ran before #737 P4.1 split out the distill producer.
func (c *MemoryController) recordEnrolled(ctx context.Context, jobID string, agent runtime.Agent, action string, payload JobPayload, result AgentResult) {
	owner := ownerForJob(agent, payload)

	// (a) Shadow-log agent-returned learnings — observations ONLY, with the
	// deterministic pre-filters as the primary gate. Rejected content is dropped
	// silently (Phase 1 is measurement; the rejection stats live in the harness).
	for _, l := range result.Learnings {
		scope := normalizeLearningScope(l.Scope)
		content := strings.TrimSpace(l.Content)
		if ok, _ := memory.PreFilter(content, scope); !ok {
			continue
		}
		repo := payload.Repo
		if scope == memory.ScopeGeneral {
			repo = ""
		}
		_, _ = c.Store.InsertMemoryObservation(ctx, db.MemoryObservation{
			Owner:   owner,
			Repo:    repo,
			Scope:   scope,
			Key:     strings.TrimSpace(l.Key),
			Content: content,
			// Provenance/trust: Phase 1 marks agent-authored returns at normal trust.
			// Marking learnings DERIVED from repo-controlled text (README/issue/PR
			// bodies) as low-trust at birth is a Phase-2 write-path refinement.
			Provenance: "agent-return",
			TrustMark:  memory.TrustNormal,
			SourceJob:  jobID,
		})
	}

	// (b) Gitmoot-authored mechanical facts — deterministic, no LLM. These are the
	// only Phase-1 producers of confirmed (injectable) memories. Each is GATED on a
	// real terminal signal (never one-fact-per-job) and keyed by a BOUNDED category
	// so the pool cannot grow unbounded and repeated jobs UPSERT the keyed row.
	for _, fact := range mechanicalFacts(action, payload, result) {
		_, _ = c.Store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
			Owner:      owner,
			Repo:       payload.Repo,
			Scope:      memory.ScopeRepo,
			Key:        fact.Key,
			Content:    fact.Content,
			Provenance: "gitmoot-mechanical",
			SourceJob:  jobID,
		})
	}
}

// normalizeLearningScope maps an empty/blank scope to the repo default and
// lowercases a set value.
func normalizeLearningScope(scope string) string {
	s := strings.ToLower(strings.TrimSpace(scope))
	if s == memory.ScopeGeneral {
		return memory.ScopeGeneral
	}
	return memory.ScopeRepo
}

// mechanicalFacts derives the deterministic, no-LLM repo facts a terminal job
// yields. It returns ZERO OR MORE confirmed-memory entries — one per genuine
// terminal signal — so a single dispatch point can fan out several small,
// independently-gated producers (#645). The contract every producer obeys:
//
//   - GATED, never one-fact-per-job: a trivial first-try success carrying no
//     notable signal returns nothing, preserving the original "no signal → write
//     nothing" restraint (constraint #1).
//   - BOUNDED keys: every key is a low-cardinality CATEGORY (action × outcome),
//     never free-form content, so the pool cannot grow unbounded and repeated
//     jobs UPSERT the keyed row rather than accumulate (constraint #2).
//   - DURABLE, reference-phrased content that passes the SAME deterministic write
//     filters as agent-returned learnings: every entry is re-checked through
//     memory.PreFilter, so a producer can never emit directive- or secret-shaped
//     content even if a content template later drifts (constraint #3).
func mechanicalFacts(action string, payload JobPayload, result AgentResult) []memory.Entry {
	candidates := make([]memory.Entry, 0, 2)
	if e, ok := fixRoundsFact(payload, result); ok {
		candidates = append(candidates, e)
	}
	if e, ok := terminalOutcomeFact(action, result); ok {
		candidates = append(candidates, e)
	}
	// Deterministic write filter: mechanical content is gitmoot-authored, but
	// running it through the same PreFilter as agent learnings guarantees the
	// directive/secret/executable gates hold for every producer (constraint #3).
	kept := candidates[:0]
	for _, e := range candidates {
		if ok, _ := memory.PreFilter(e.Content, e.Scope); ok {
			kept = append(kept, e)
		}
	}
	return kept
}

// fixRoundsFact records the FIX-ROUND count when a job reached its terminal
// decision only after one or more corrective rounds (verify or retry) — durable
// repo knowledge ("<decision> jobs here needed up to N fix rounds"). The key is
// stable per decision so repeated jobs UPSERT the latest count rather than
// accumulating rows. A job that needed zero fix rounds produces nothing
// (ok=false). This is the ORIGINAL Phase-1 producer, kept unchanged — but note
// it only fires inside verify/retry loops, a path ordinary agent ask/run/review
// jobs never reach, which is exactly the #645 coverage gap the outcome producer
// below closes.
func fixRoundsFact(payload JobPayload, result AgentResult) (memory.Entry, bool) {
	rounds := memoryFixRounds(payload)
	if rounds <= 0 {
		return memory.Entry{}, false
	}
	decision := strings.TrimSpace(result.Decision)
	if decision == "" {
		decision = "recent"
	}
	key := "fix-rounds:" + decision
	content := fmt.Sprintf("Recent %s jobs in this repository needed up to %d corrective fix round(s) before completing.", decision, rounds)
	return memory.Entry{Scope: memory.ScopeRepo, Key: key, Content: content}, true
}

// notableOutcomes maps the terminal decisions the outcome producer AUTO-PROMOTES
// to a reference-phrased fact fragment. The set is deliberately narrow:
//
//   - SUCCESS decisions (approved, implemented) are ABSENT: a routine success is
//     not a signal, so an ordinary first-try success produces nothing (the
//     anti-flood restraint, constraint #1).
//   - The ANOMALOUS terminal states (failed, blocked) are ALSO absent. Those are
//     the outcomes most likely to be a ONE-OFF — a single flaky failure, a single
//     task blocked pending external input — and this producer promotes on the
//     FIRST occurrence with no recurrence threshold and no decay. Auto-promoting
//     them would let ONE anomalous job become a durable, injected repo "fact" that
//     biases every future prompt toward expecting failure, long after the next 500
//     jobs succeed. Until a recurrence gate exists (promote only after the same
//     (owner,repo,action,decision) has recurred N>=2 times) they stay OUT of the
//     auto-promoted set. See #645 review (the misleading-durable-fact finding).
//
// What remains — changes_requested — is a NORMAL, repeatable review conclusion
// (not an anomaly): a review asking for changes is an expected outcome, so its
// hedged "Some ... have" fact is accurate and non-biasing even at a low count.
// Every key is drawn from the validated closed ResultDecisions set, keeping
// outcome-fact cardinality bounded.
var notableOutcomes = map[string]string{
	"changes_requested": "concluded with changes requested rather than approval",
}

// terminalOutcomeFact records a repo fact when an ORDINARY job (the shape
// agent ask/run/review/implement enqueue — no verify/retry loop required, so
// fixRoundsFact above stays silent) terminates on a NOTABLE decision. This is
// the #645 fix: it fires on terminal states ordinary CLI jobs actually reach, so
// the confirmed pool populates under normal usage. It is keyed by
// (action, decision) where BOTH sides are CLOSED categories: the decision is a
// value from the validated ResultDecisions set, and the action is collapsed by
// memoryActionToken to a fixed allowlist plus a single generic bucket. That
// bounding is load-bearing because job.Type for a DELEGATION child is the
// coordinator's free-form, LLM-authored d.Action (validated only as non-empty),
// which must NEVER inflate key cardinality, collide at a length boundary, or leak
// mangled content into a key or the injected prose. A non-notable decision yields
// nothing (ok=false).
func terminalOutcomeFact(action string, result AgentResult) (memory.Entry, bool) {
	decision := strings.TrimSpace(result.Decision)
	phrase, ok := notableOutcomes[decision]
	if !ok {
		return memory.Entry{}, false
	}
	act := memoryActionToken(action)
	key := "outcome:" + act + ":" + decision
	content := fmt.Sprintf("Some %s jobs in this repository have %s.", act, phrase)
	return memory.Entry{Scope: memory.ScopeRepo, Key: key, Content: content}, true
}

// canonicalActions is the CLOSED allowlist of job actions the outcome producer
// keys on. Top-level CLI jobs carry one of these as job.Type; a delegation child
// instead carries its free-form, LLM-authored d.Action (validated only as
// non-empty at result.go), which is precisely why memoryActionToken maps anything
// NOT in this set to a single generic bucket instead of passing it through.
var canonicalActions = map[string]bool{
	"ask":       true,
	"review":    true,
	"implement": true,
}

// memoryActionToken maps a job action to a BOUNDED, key-safe token drawn from a
// CLOSED set. A recognized canonical action passes through (lowercased); ANY other
// value — an empty action, an unknown internal type, or a delegation's free-form
// d.Action such as "review the payment webhook retry logic" — collapses to the
// single generic "recent" bucket. Stripping-and-capping the raw string (the prior
// behavior) was NOT enough: distinct free-form phrasings still produced distinct
// keys (pool bloat, upsert defeated) and injected mangled prose, and long strings
// could collide at the length cap. Collapsing to a closed allowlist keeps the
// outcome key space a genuine closed category (constraint #2): free-form input can
// neither inflate cardinality nor leak mangled content into a key or the prose.
func memoryActionToken(action string) string {
	a := strings.ToLower(strings.TrimSpace(action))
	if canonicalActions[a] {
		return a
	}
	return "recent"
}

// memoryFixRounds is the deterministic corrective-round count for a terminal
// job: the larger of the verify-replan attempt count and the retry count.
func memoryFixRounds(payload JobPayload) int {
	rounds := payload.VerifyAttempt
	if payload.RetryCount > rounds {
		rounds = payload.RetryCount
	}
	return rounds
}
