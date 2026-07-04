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
}

// enabledFor reports whether memory is active for the given executor agent.
func (c *MemoryController) enabledFor(agentName string) bool {
	if c == nil || c.Store == nil || c.Enabled == nil {
		return false
	}
	return c.Enabled(agentName)
}

// ownerForJob derives the structured memory owner for a job's executor. Phase 1
// scopes memory to REGISTERED agents (owner_kind=agent); the role-pool owner
// (owner_kind=role, template identity + version) is structural-only until the
// Phase-2 ephemeral writers land, so an ephemeral worker's synthetic name is
// simply never enrolled.
func ownerForJob(agent runtime.Agent, _ JobPayload) db.MemoryOwner {
	return db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: agent.Name}
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
	block, _ := memory.RenderBlock(entries, c.TokenBudget)
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
	query := memory.SanitizeFTSQuery(instructions)
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
	entries := make([]memory.Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, memory.Entry{
			Scope:     r.Scope,
			Key:       r.Key,
			Content:   r.Content,
			UpdatedAt: r.UpdatedAt,
		})
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
	rendered, n := memory.RenderBlock(c.PreviewEntries(ctx, agentName, repo, instructions, 0), c.TokenBudget)
	return rendered, n, memory.EstimateTokens(rendered)
}

// record is the Phase-1 WRITE path, run at job terminal. It (a) SHADOW-logs the
// agent's returned learnings to memory_observations after the deterministic
// pre-filters, and (b) writes any gitmoot-authored mechanical fact to
// confirmed_memories (the ONLY Phase-1 confirmed producer — deterministic, no
// LLM). Every write is best-effort: a failure is swallowed so memory can never
// fail an otherwise-successful job.
func (c *MemoryController) record(ctx context.Context, jobID string, agent runtime.Agent, payload JobPayload, result AgentResult) {
	if !c.enabledFor(agent.Name) {
		return
	}
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

	// (b) Gitmoot-authored mechanical fact — deterministic, no LLM. This is the
	// only Phase-1 producer of confirmed (injectable) memories.
	if fact, ok := mechanicalFact(payload, result); ok {
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

// mechanicalFact derives a deterministic, no-LLM repo fact from a terminal job's
// payload. The Phase-1 producer records the FIX-ROUND count: when a job reached
// its terminal decision only after one or more corrective rounds (verify or
// retry), that is durable repo knowledge ("implement jobs here have needed up to
// N fix rounds"). The key is stable per action so repeated jobs UPSERT the
// latest count rather than accumulating rows. A job that needed zero fix rounds
// produces nothing (ok=false), so trivial jobs write no confirmed memory.
func mechanicalFact(payload JobPayload, result AgentResult) (memory.Entry, bool) {
	rounds := memoryFixRounds(payload)
	if rounds <= 0 {
		return memory.Entry{}, false
	}
	action := strings.TrimSpace(result.Decision)
	if action == "" {
		action = "recent"
	}
	key := "fix-rounds:" + action
	content := fmt.Sprintf("Recent %s jobs in this repository needed up to %d corrective fix round(s) before completing.", action, rounds)
	return memory.Entry{Scope: memory.ScopeRepo, Key: key, Content: content}, true
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
