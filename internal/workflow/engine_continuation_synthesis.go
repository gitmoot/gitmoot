package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// continuationConfig holds the optional inputs threaded into a coordinator
// continuation (#445). It is empty by default so every existing call site builds
// the byte-identical continuation it always did.
type continuationConfig struct {
	// humanAnswer, when non-empty, is the rendered ask-gate answer block injected
	// at the top of the NORMAL continuation prompt so the resumed coordinator reads
	// the human's decision (the answer-driven resume path only ever reaches the
	// normal continuation, never the loop/verify-replan corrective ones).
	humanAnswer string
}

// continuationOption configures a maybeEnqueueContinuation call without changing
// the byte-identical default (no options) path.
type continuationOption func(*continuationConfig)

// withHumanAnswer threads a rendered ask-gate answer block into the continuation
// prompt (#445).
func withHumanAnswer(answer string) continuationOption {
	return func(c *continuationConfig) { c.humanAnswer = answer }
}

// maybeEnqueueContinuation enqueues exactly one coordinator continuation job for
// a parent whose delegations have all finished. Idempotency is enforced by a
// deterministic continuation id plus a one-shot delegation_continuation_enqueued
// event on the parent, so concurrent child completions enqueue it at most once.
// When any delegation declares synthesis_rule "vote", the continuation is gated:
// the parent task is blocked unless every child was approved/succeeded.
func (e Engine) maybeEnqueueContinuation(ctx context.Context, parentJob db.Job, parentPayload JobPayload, parentResult *AgentResult, children map[string]db.Job, ref taskRef, opts ...continuationOption) error {
	var cfg continuationConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	events, err := e.Store.ListJobEvents(ctx, parentJob.ID)
	if err != nil {
		return err
	}
	if continuationEnqueued(events) {
		return nil
	}

	childPayloads := make(map[string]JobPayload, len(children))
	for id, child := range children {
		childPayload, err := unmarshalPayload(child.Payload)
		if err != nil {
			return err
		}
		childPayloads[id] = childPayload
	}

	// Goal-anchor the continuation (#418): resolve the user's ORIGINAL goal from
	// the ROOT coordinator (depth-stable; parentPayload.Instructions is the parent
	// continuation's built prompt for a nested generation) so every variant below
	// restates the same intent. A pruned/empty root falls back to the parent's
	// instructions and, ultimately, an empty goal that omits the header.
	goal := e.originalGoal(ctx, e.rootJobID(parentJob, parentPayload), parentPayload.Instructions)

	// synthesis_rule "vote": block the parent unless every child approved or
	// succeeded. The default ("" / "summary") concatenates child summaries into
	// the continuation prompt below.
	if delegationSynthesisRequiresVote(parentResult.Delegations) && !delegationVoteSatisfied(parentResult.Delegations, children, childPayloads) {
		reason := fmt.Sprintf("delegation synthesis_rule vote failed: not all delegated children for %s were approved/succeeded", parentJob.ID)
		// #758: a pipeline-orchestrate root has no task; a synthesis-gate block would
		// strand its chain with no foldable tail. Route to the finalize continuation
		// so the tail is always foldable (byte-identical e.block for every other tree).
		if e.isPipelineOrchestrateRoot(ctx, parentJob, parentPayload) {
			return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, reason)
		}
		return e.block(ctx, ref, reason)
	}

	// synthesis_rule "quorum": block the parent unless at least K children
	// reached an approving outcome (succeeded state or an approving decision).
	if delegationSynthesisRequiresQuorum(parentResult.Delegations) {
		k := delegationQuorumThreshold(parentResult.Delegations)
		if !delegationQuorumSatisfied(parentResult.Delegations, children, childPayloads, k) {
			reason := fmt.Sprintf("delegation synthesis_rule quorum failed: fewer than %d delegated children for %s were approved/succeeded", k, parentJob.ID)
			if e.isPipelineOrchestrateRoot(ctx, parentJob, parentPayload) {
				return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, reason)
			}
			return e.block(ctx, ref, reason)
		}
	}

	// Result-aware non-progress detection (#339). The structural fast-path
	// (handleDelegationLoop) only catches a coordinator literally re-issuing the
	// same delegation SET; a coordinator that perturbs the set each round to evade
	// the set hash, yet whose children keep returning nothing new, slips past it.
	// Here — after every child has finished and childPayloads carry full results —
	// fold the generation's verifiable side effects into a progressDigest and
	// compare it to the previous generation's digest threaded through the payload.
	// No new durable side effect + an unchanged digest => the streak climbs; any
	// new side effect (a different decision, a new commit/change, a test run, a new
	// PR/HeadSHA, a changed artifact body) resets it to 0 even when the summary
	// text repeats. At the threshold the result-aware path trips the SAME ladder as
	// the structural check: a first trip emits delegation_loop_warning and a
	// corrective continuation; a trip after a corrective nudge has already fired
	// (DelegationRepeatCount >= 1) emits delegation_loop_detected and routes to the
	// #305 graceful finalize continuation. Both reuse the existing once-guards
	// (the continuationEnqueued top-guard above + enqueueFinalizeContinuation's own
	// guard) so re-advance never double-fires.
	digest := progressDigest(parentResult.Delegations, childPayloads)
	nonProgressStreak := 0
	if digest == parentPayload.LastProgressDigest {
		// The previous generation recorded this exact digest and this generation
		// reproduced it: no new durable side effect => the streak climbs.
		nonProgressStreak = parentPayload.NonProgressStreak + 1
	}
	if nonProgressStreak >= e.nonProgressStreakThreshold() {
		if parentPayload.DelegationRepeatCount >= 1 {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "delegation_loop_detected",
				Message: fmt.Sprintf("delegation tree made no new durable side effect for %d consecutive generations after a corrective nudge (digest %s); finalizing instead of continuing", nonProgressStreak, digest),
			})
			// Graceful finalize (#305): give the coordinator one terminal continuation
			// to synthesize a best-effort result rather than stopping silently.
			return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, "delegation tree made no progress after a corrective nudge")
		}

		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_loop_warning",
			Message: fmt.Sprintf("delegation tree made no new durable side effect for %d consecutive generations (digest %s); sending a corrective continuation instead of continuing", nonProgressStreak, digest),
		})
		correctiveRequest := JobRequest{
			ID:     delegationContinuationID(parentJob.ID),
			Agent:  parentJob.Agent,
			Action: "ask",
			Model:  parentPayload.Model,
			Effort: parentPayload.Effort,
			// Per-job runtime override (#531): the corrective continuation stays on
			// the override runtime/ref, like Model (see maybeEnqueueContinuation).
			RuntimeOverride:    parentPayload.RuntimeOverride,
			RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
			Phase:              parentPayload.Phase,
			Repo:               parentPayload.Repo,
			Branch:             parentPayload.Branch,
			PullRequest:        parentPayload.PullRequest,
			HeadSHA:            parentPayload.HeadSHA,
			GoalID:             parentPayload.GoalID,
			TaskID:             parentPayload.TaskID,
			TaskTitle:          parentPayload.TaskTitle,
			LeadAgent:          parentPayload.LeadAgent,
			Reviewers:          parentPayload.Reviewers,
			// Carry the resolved risk tier forward (#650) so a high-risk coordinator's
			// synthesis-only continuation is recognized as such at dispatch (it need not
			// grant the synthetic lead `ask` capability). Empty for every non-risk tree.
			RiskTier:        parentPayload.RiskTier,
			Sender:          parentJob.Agent,
			Instructions:    buildCorrectiveContinuationPrompt(goal, parentResult),
			Constraints:     parentPayload.Constraints,
			ParentJobID:     parentJob.ID,
			DelegationDepth: parentPayload.DelegationDepth + 1,
			DelegatedBy:     parentJob.Agent,
			RootJobID:       e.rootJobID(parentJob, parentPayload),
			WorkflowID:      parentPayload.WorkflowID,
			ThreadID:        parentPayload.ThreadID,
			ChatMessageID:   parentPayload.ChatMessageID,
			// Carry the window forward and mark that a corrective nudge has fired so a
			// further non-progress generation escalates to delegation_loop_detected.
			RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
			DelegationRepeatCount:  parentPayload.DelegationRepeatCount + 1,
			// Thread the non-progress streak forward: if the coordinator's corrective
			// continuation still produces no new side effect, the streak stays at or
			// above the threshold and the next generation escalates.
			NonProgressStreak:  nonProgressStreak,
			LastProgressDigest: digest,
			Cockpit:            parentPayload.Cockpit,
			CockpitSession:     parentPayload.CockpitSession,
			CockpitPaneKey:     parentPayload.CockpitPaneKey,
		}
		if err := e.enqueue(ctx, correctiveRequest); err != nil {
			return fmt.Errorf("enqueue corrective continuation for %q: %w", parentJob.ID, err)
		}
		// The corrective continuation IS the coordinator's single continuation, so it
		// occupies the continuation slot: emit delegation_continuation_enqueued so a
		// re-advance hits the continuationEnqueued top-guard rather than re-running
		// the streak logic.
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_continuation_enqueued",
			Message: fmt.Sprintf("corrective continuation occupies the continuation slot for job %s", correctiveRequest.ID),
		})
		return nil
	}

	// synthesis_rule "verify" (#439): unlike vote/quorum (which BLOCK on failure),
	// verify derives a pass/fail VERDICT from the verify-tagged legs and, on a
	// FAILED verdict, enqueues a BOUNDED corrective "replan" continuation so the
	// coordinator can self-correct — the verify→replan loop is enforced by the
	// engine rather than left to the coordinator. The verdict is mechanical: any
	// verify-tagged leg that did NOT reach an approving outcome (the same test
	// vote/quorum use) fails it (a missing verify child also fails). On a PASSED
	// verdict this falls through to the normal synthesis continuation below,
	// byte-identical to the pre-change path. The loop is bounded by a dedicated
	// per-root VerifyAttempt cap: below the cap a verify_replan_warning + corrective
	// replan continuation fire; at/over the cap it routes to the #305 graceful
	// finalize continuation (verify_replan_exhausted) like every other backstop.
	// This sits AFTER the continuationEnqueued top-guard and the non-progress check
	// and emits delegation_continuation_enqueued for its own request, so it occupies
	// the single continuation slot and a re-advance never double-enqueues.
	// dedupWinners mirrors advanceDelegations: a fingerprint-deduped verify leg
	// never owns its own child, so the verdict must resolve it against its winning
	// sibling rather than reading the absent child as a failed verdict.
	dedupWinners := dedupedDelegationWinners(parentResult.Delegations, children, events)
	if delegationSynthesisRequiresVerify(parentResult.Delegations) && !verifyVerdictPassed(parentResult.Delegations, children, childPayloads, dedupWinners) {
		attemptCap := e.verifyReplanAttemptCap()
		if parentPayload.VerifyAttempt >= attemptCap {
			_ = e.Store.AddJobEvent(ctx, db.JobEvent{
				JobID:   parentJob.ID,
				Kind:    "verify_replan_exhausted",
				Message: fmt.Sprintf("verify→replan attempt cap of %d reached for job %s; finalizing instead of replanning", attemptCap, parentJob.ID),
			})
			// Graceful finalize (#305): give the coordinator one terminal continuation
			// to synthesize a best-effort result rather than looping on a failed verdict.
			return e.enqueueFinalizeContinuation(ctx, parentJob, parentPayload, fmt.Sprintf("verify→replan attempt cap of %d reached", attemptCap))
		}

		attempt := parentPayload.VerifyAttempt + 1
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "verify_replan_warning",
			Message: fmt.Sprintf("independent verification failed (attempt %d/%d); sending a corrective replan continuation for job %s", attempt, attemptCap, parentJob.ID),
		})
		replanRequest := JobRequest{
			ID:     delegationContinuationID(parentJob.ID),
			Agent:  parentJob.Agent,
			Action: "ask",
			Model:  parentPayload.Model,
			Effort: parentPayload.Effort,
			// Per-job runtime override (#531): the replan continuation stays on the
			// override runtime/ref, like Model (see maybeEnqueueContinuation).
			RuntimeOverride:    parentPayload.RuntimeOverride,
			RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
			Phase:              parentPayload.Phase,
			Repo:               parentPayload.Repo,
			Branch:             parentPayload.Branch,
			PullRequest:        parentPayload.PullRequest,
			HeadSHA:            parentPayload.HeadSHA,
			GoalID:             parentPayload.GoalID,
			TaskID:             parentPayload.TaskID,
			TaskTitle:          parentPayload.TaskTitle,
			LeadAgent:          parentPayload.LeadAgent,
			Reviewers:          parentPayload.Reviewers,
			// Carry the resolved risk tier forward (#650) so a high-risk coordinator's
			// synthesis-only continuation is recognized as such at dispatch (it need not
			// grant the synthetic lead `ask` capability). Empty for every non-risk tree.
			RiskTier:        parentPayload.RiskTier,
			Sender:          parentJob.Agent,
			Instructions:    buildVerifyReplanContinuationPrompt(goal, parentResult, children, childPayloads, attempt, attemptCap),
			Constraints:     parentPayload.Constraints,
			ParentJobID:     parentJob.ID,
			DelegationDepth: parentPayload.DelegationDepth + 1,
			DelegatedBy:     parentJob.Agent,
			RootJobID:       e.rootJobID(parentJob, parentPayload),
			WorkflowID:      parentPayload.WorkflowID,
			// Consume one verify attempt so a still-failing verdict next generation
			// climbs toward the cap and eventually finalizes.
			VerifyAttempt: attempt,
			// Carry the non-progress carry-forward fields so a genuine corrective replan
			// is not misclassified as a loop: record this generation's progressDigest and
			// thread the (sub-threshold) streak/window forward exactly like the normal
			// continuation below. A real verdict-driven replan IS a new generation.
			RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
			DelegationRepeatCount:  0,
			NonProgressStreak:      nonProgressStreak,
			LastProgressDigest:     digest,
			Cockpit:                parentPayload.Cockpit,
			CockpitSession:         parentPayload.CockpitSession,
			CockpitPaneKey:         parentPayload.CockpitPaneKey,
		}
		if err := e.enqueue(ctx, replanRequest); err != nil {
			return fmt.Errorf("enqueue verify replan continuation for %q: %w", parentJob.ID, err)
		}
		// The replan continuation IS the coordinator's single continuation, so it
		// occupies the continuation slot: emit delegation_continuation_enqueued so a
		// re-advance hits the continuationEnqueued top-guard rather than re-running
		// the verify gate (and never double-enqueues a normal continuation).
		_ = e.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   parentJob.ID,
			Kind:    "delegation_continuation_enqueued",
			Message: fmt.Sprintf("verify replan continuation occupies the continuation slot for job %s", replanRequest.ID),
		})
		return nil
	}

	request := JobRequest{
		ID:     delegationContinuationID(parentJob.ID),
		Agent:  parentJob.Agent,
		Action: "ask",
		Model:  parentPayload.Model,
		Effort: parentPayload.Effort,
		// Per-job runtime override (#531): a continuation is the SAME logical
		// coordinator run, so it must stay on the override runtime — dropping the
		// override here would run the continuation as the default agent, resuming
		// (and writing into) the agent's default-runtime session AND leaking the
		// override-runtime --model onto the default runtime's CLI. Reusing the
		// parent's ref is safe: continuation generations are strictly sequential
		// (one deterministic continuation id per parent), and adapters treat a
		// fresh ref as "start a brand-new session" on every delivery.
		RuntimeOverride:    parentPayload.RuntimeOverride,
		RuntimeOverrideRef: parentPayload.RuntimeOverrideRef,
		Phase:              parentPayload.Phase,
		Repo:               parentPayload.Repo,
		Branch:             parentPayload.Branch,
		PullRequest:        parentPayload.PullRequest,
		HeadSHA:            parentPayload.HeadSHA,
		GoalID:             parentPayload.GoalID,
		TaskID:             parentPayload.TaskID,
		TaskTitle:          parentPayload.TaskTitle,
		LeadAgent:          parentPayload.LeadAgent,
		Reviewers:          parentPayload.Reviewers,
		// Carry the resolved risk tier forward (#650) so a high-risk coordinator's
		// synthesis-only continuation is recognized as such at dispatch (it need not
		// grant the synthetic lead `ask` capability). Empty for every non-risk tree.
		RiskTier: parentPayload.RiskTier,
		Sender:   parentJob.Agent,
		// Budget-pressure nudge (#418): when the tree is near a depth/job bound, bias
		// the coordinator toward synthesizing now over more fan-out. The job count is
		// best-effort — a lookup error yields 0, which suppresses only the job clause,
		// never the continuation.
		Instructions: e.buildContinuationPrompt(goal, budgetPressureLine(parentPayload.DelegationDepth+1, e.rootJobCountForPressure(ctx, e.rootJobID(parentJob, parentPayload))), parentResult, children, childPayloads, cfg.humanAnswer),
		Constraints:  parentPayload.Constraints,
		ParentJobID:  parentJob.ID,
		// Ask-gate answer (#445): carry the human's answer block on the continuation
		// for durability/observability; buildContinuationPrompt above already rendered
		// it at the top of the prompt. Empty (the default) for every non-answer path,
		// so omitempty keeps the stored payload byte-identical.
		HumanAnswer: cfg.humanAnswer,
		// Chat back-link (#534): a continuation of a chat-promoted (or ask-gate
		// auto-linked) coordinator inherits the thread linkage so its terminal
		// result posts back into the originating thread. Empty for every non-chat
		// coordinator, so omitempty keeps the stored payload byte-identical.
		ThreadID:      parentPayload.ThreadID,
		ChatMessageID: parentPayload.ChatMessageID,
		// Increment depth per continuation generation so a coordinator whose
		// continuation re-delegates is bounded by MaxDelegationDepth instead of
		// looping forever (the continuation reused the parent's depth before).
		DelegationDepth: parentPayload.DelegationDepth + 1,
		DelegatedBy:     parentJob.Agent,
		// Share the originating coordinator's root so the whole continuation
		// chain counts against one per-root budget and is visible to loop detection.
		RootJobID:  e.rootJobID(parentJob, parentPayload),
		WorkflowID: parentPayload.WorkflowID,
		// Record the delegation set that was actually dispatched in the sliding
		// window so the next generation can detect a non-progress repeat. A real
		// dispatch happened => progress, so reset the repeat counter; the
		// corrective-nudge counter only climbs while the coordinator loops.
		RecentDelegationHashes: appendDelegationHashWindow(parentPayload.RecentDelegationHashes, canonicalDelegationSetHash(parentResult.Delegations)),
		DelegationRepeatCount:  0,
		// Result-aware non-progress carry-forward (#339): record this generation's
		// progressDigest so the next generation can detect a non-progress repeat, and
		// thread the (sub-threshold) streak forward. nonProgressStreak is 0 whenever
		// this generation produced a new durable side effect, so genuine progress
		// always resets the streak even when the self-reported summary repeats.
		NonProgressStreak:  nonProgressStreak,
		LastProgressDigest: digest,
		// Inherit the coordinator's cockpit settings so the continuation renders
		// its pane under the same workspace/session as the rest of the tree.
		Cockpit:        parentPayload.Cockpit,
		CockpitSession: parentPayload.CockpitSession,
		CockpitPaneKey: parentPayload.CockpitPaneKey,
	}
	if err := e.enqueue(ctx, request); err != nil {
		return fmt.Errorf("enqueue continuation for %q: %w", parentJob.ID, err)
	}
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{
		JobID:   parentJob.ID,
		Kind:    "delegation_continuation_enqueued",
		Message: fmt.Sprintf("delegation continuation enqueued as job %s", request.ID),
	})
	return nil
}

// childDelegationJobs returns the direct delegation children of a parent job,
// keyed by delegation id. There is no ListJobsByParent store query, so this
// filters ListJobs on ParentJobID (mirroring latestReviewRound's list+filter).
func (e Engine) childDelegationJobs(ctx context.Context, parentJobID string) (map[string]db.Job, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	children := make(map[string]db.Job)
	attempts := make(map[string]int)
	for _, job := range jobs {
		if job.ParentJobID != parentJobID || strings.TrimSpace(job.DelegationID) == "" {
			continue
		}
		// A delegation may have several attempts after retries; keep the latest
		// (highest RetryCount) so the failure/resolution logic always observes the
		// current attempt regardless of ListJobs ordering.
		attempt := delegationJobRetryCount(job)
		if _, ok := children[job.DelegationID]; ok && attempt < attempts[job.DelegationID] {
			continue
		}
		children[job.DelegationID] = job
		attempts[job.DelegationID] = attempt
	}
	return children, nil
}

// delegationJobRetryCount reads a child job's RetryCount from its payload,
// returning 0 when the payload is missing or cannot be parsed.
func delegationJobRetryCount(job db.Job) int {
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return 0
	}
	return payload.RetryCount
}

// delegationFingerprintSeen reports whether a sibling delegation under the same
// parent (other than skipDelegationID) has already been enqueued with the given
// fingerprint. It scans ListJobs filtered by ParentJobID, mirroring
// childDelegationJobs, and compares each child's stored payload.Fingerprint so
// dedup is scoped per the goal's (parentJobID, fingerprint) key.
func (e Engine) delegationFingerprintSeen(ctx context.Context, parentJobID, skipDelegationID, fingerprint string) (bool, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return false, nil
	}
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.ParentJobID != parentJobID || strings.TrimSpace(job.DelegationID) == "" {
			continue
		}
		if job.DelegationID == skipDelegationID {
			continue
		}
		childPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(childPayload.Fingerprint) == fingerprint {
			return true, nil
		}
	}
	return false, nil
}

// delegationFingerprintKey hashes (parentJobID, fingerprint) into a stable,
// parent-scoped dedup key, mirroring jobID's fnv hashing so identical
// fingerprints under different parents never collide.
func delegationFingerprintKey(parentJobID, fingerprint string) string {
	hash := fnv.New64a()
	for _, value := range []string{parentJobID, fingerprint} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "deleg-fp-" + strconv.FormatUint(hash.Sum64(), 36)
}

// dedupedDelegationWinners maps each fingerprint-deduped delegation id to the
// winning sibling's child job: the same-fingerprint delegation that DID get a
// child (and is therefore the canonical attempt the deduped node folds into). A
// deduped delegation produces no child of its own (enqueueDelegation returns
// early after a delegation_deduped event), so without this mapping
// allDelegationsResolved/depsSatisfied would treat it as forever-active and stall
// the coordinator continuation. The deduped set is taken from the parent's
// recorded delegation_deduped events so it reflects exactly what dispatch skipped,
// and each deduped id is resolved to its winner by fingerprint among siblings that
// own a child.
func dedupedDelegationWinners(delegations []Delegation, children map[string]db.Job, events []db.JobEvent) map[string]db.Job {
	deduped := dedupedDelegationIDs(delegations, events)
	if len(deduped) == 0 {
		return nil
	}
	// Map each fingerprint to the winning sibling's child (the same-fingerprint
	// delegation that owns a child). Delegation order is deterministic, so the
	// first such sibling is a stable winner.
	winnerByFingerprint := make(map[string]db.Job)
	for _, d := range delegations {
		fingerprint := strings.TrimSpace(d.Fingerprint)
		if fingerprint == "" {
			continue
		}
		if _, taken := winnerByFingerprint[fingerprint]; taken {
			continue
		}
		if child, ok := children[d.ID]; ok {
			winnerByFingerprint[fingerprint] = child
		}
	}
	winners := make(map[string]db.Job)
	for _, d := range delegations {
		if !deduped[d.ID] {
			continue
		}
		if winner, ok := winnerByFingerprint[strings.TrimSpace(d.Fingerprint)]; ok {
			winners[d.ID] = winner
		}
	}
	if len(winners) == 0 {
		return nil
	}
	return winners
}

// dedupedDelegationIDs returns the set of delegation ids that dispatch skipped
// via fingerprint dedup, by matching each known delegation against the parent's
// recorded delegation_deduped event messages. enqueueDelegation formats those
// messages as `delegation %q skipped: ...`, so a delegation is deduped when an
// event message carries its %q-quoted id as the prefix. Reconstructing the
// quoted prefix per delegation (rather than parsing the id out of the message)
// keeps the match exact even for ids containing quotes or backslashes.
func dedupedDelegationIDs(delegations []Delegation, events []db.JobEvent) map[string]bool {
	var dedupedMessages []string
	for _, event := range events {
		if event.Kind == "delegation_deduped" {
			dedupedMessages = append(dedupedMessages, event.Message)
		}
	}
	if len(dedupedMessages) == 0 {
		return nil
	}
	var ids map[string]bool
	for _, d := range delegations {
		prefix := fmt.Sprintf("delegation %q skipped:", d.ID)
		for _, message := range dedupedMessages {
			if strings.HasPrefix(message, prefix) {
				if ids == nil {
					ids = make(map[string]bool)
				}
				ids[d.ID] = true
				break
			}
		}
	}
	return ids
}

// depsSatisfied reports whether every dependency id maps to a succeeded sibling.
// An unknown dep id (not yet a child, or never created) is never satisfied, so a
// failed or missing dependency keeps the dependent gated rather than enqueuing
// it prematurely. A dep that points at a fingerprint-deduped delegation is
// resolved against its winning sibling: satisfied iff that sibling succeeded.
func depsSatisfied(deps []string, children map[string]db.Job, dedupWinners map[string]db.Job) bool {
	for _, dep := range compactStrings(deps) {
		if winner, ok := dedupWinners[dep]; ok {
			if winner.State != string(JobSucceeded) {
				return false
			}
			continue
		}
		child, ok := children[dep]
		if !ok || child.State != string(JobSucceeded) {
			return false
		}
	}
	return true
}

// allDelegationsResolved reports whether every top-level delegation has reached
// a final disposition: either a terminal child job, or no child at all because a
// dependency failed under a continue policy and the delegation can never run.
// queued/running children, or a not-yet-enqueued delegation whose deps are still
// in flight, mean the batch is still active and no continuation is enqueued yet.
func allDelegationsResolved(delegations []Delegation, children map[string]db.Job, dedupWinners map[string]db.Job) bool {
	byID := delegationsByID(delegations)
	for _, d := range delegations {
		if !delegationResolved(d, children, byID, dedupWinners) {
			return false
		}
	}
	return true
}

func delegationResolved(d Delegation, children map[string]db.Job, byID map[string]Delegation, dedupWinners map[string]db.Job) bool {
	if child, ok := children[d.ID]; ok {
		return IsSettledJobState(child.State)
	}
	// A fingerprint-deduped delegation never gets its own child; it is resolved
	// when its winning sibling (the same-fingerprint delegation that did get a
	// child) reaches a terminal state, so it cannot stall the continuation.
	if winner, ok := dedupWinners[d.ID]; ok {
		return IsSettledJobState(winner.State)
	}
	// No child job yet: the delegation is resolved only if it can never run
	// because one of its dependencies is permanently unrunnable.
	return delegationPermanentlyBlocked(d, children, byID, dedupWinners, map[string]bool{})
}

// delegationPermanentlyBlocked reports whether a not-yet-enqueued delegation can
// never run because a dependency terminally failed (or is itself permanently
// blocked). It guards against cycles via the visiting set, treating a delegation
// caught in a dependency cycle as blocked so the batch cannot deadlock. A dep
// that points at a fingerprint-deduped delegation is resolved against its
// winning sibling: a terminally-failed winner permanently blocks the dependent.
func delegationPermanentlyBlocked(d Delegation, children map[string]db.Job, byID map[string]Delegation, dedupWinners map[string]db.Job, visiting map[string]bool) bool {
	if visiting[d.ID] {
		return true
	}
	visiting[d.ID] = true
	defer delete(visiting, d.ID)
	for _, dep := range compactStrings(d.Deps) {
		if winner, ok := dedupWinners[dep]; ok {
			if IsSettledJobState(winner.State) && winner.State != string(JobSucceeded) {
				return true
			}
			continue
		}
		if child, ok := children[dep]; ok {
			if IsSettledJobState(child.State) && child.State != string(JobSucceeded) {
				return true
			}
			continue
		}
		depDel, ok := byID[dep]
		if !ok {
			// Unknown dependency id can never be satisfied.
			return true
		}
		if delegationPermanentlyBlocked(depDel, children, byID, dedupWinners, visiting) {
			return true
		}
	}
	return false
}

func delegationsByID(delegations []Delegation) map[string]Delegation {
	byID := make(map[string]Delegation, len(delegations))
	for _, d := range delegations {
		byID[d.ID] = d
	}
	return byID
}

func delegationFailurePolicy(d Delegation) string {
	policy := strings.ToLower(strings.TrimSpace(d.FailurePolicy))
	if policy == "" {
		return "block_parent"
	}
	return policy
}

// delegationFailureHandledByPolicy reports whether the named delegation declares
// a continue/escalate/escalate_human failure_policy, meaning a failure of its
// child is governed by the delegation graph (siblings keep running, the
// coordinator continuation absorbs it, or the tree pauses awaiting a human)
// rather than blocking the shared parent task.
func delegationFailureHandledByPolicy(parentResult *AgentResult, delegationID string) bool {
	if parentResult == nil || strings.TrimSpace(delegationID) == "" {
		return false
	}
	for _, d := range parentResult.Delegations {
		if d.ID != delegationID {
			continue
		}
		switch delegationFailurePolicy(d) {
		case "continue", "escalate", "escalate_human":
			return true
		default:
			return false
		}
	}
	return false
}

// delegationRetryPending reports whether the named delegation currently has a
// non-terminal child, meaning the retry pass re-enqueued a fresh attempt and the
// failed attempt's outcome is now superseded. childDelegationJobs already keeps
// the latest attempt per delegation id, so a queued/running retry shows here.
func (e Engine) delegationRetryPending(ctx context.Context, parentJobID, delegationID string) (bool, error) {
	if strings.TrimSpace(delegationID) == "" {
		return false, nil
	}
	children, err := e.childDelegationJobs(ctx, parentJobID)
	if err != nil {
		return false, err
	}
	child, ok := children[delegationID]
	if !ok {
		return false, nil
	}
	return !IsSettledJobState(child.State), nil
}

func childFailureReason(child db.Job) string {
	payload, err := unmarshalPayload(child.Payload)
	if err == nil && payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		return payload.Result.Summary
	}
	return child.State
}

func continuationEnqueued(events []db.JobEvent) bool {
	for _, event := range events {
		if event.Kind == "delegation_continuation_enqueued" {
			return true
		}
	}
	return false
}

func delegationContinuationID(parentJobID string) string {
	return parentJobID + "/continuation"
}

// DelegationContinuationID is the exported view of the deterministic continuation
// id used across a coordinator's continuation chain. The #758 pipeline advancer
// (internal/cli) follows this chain from an orchestrate stage job to its terminal
// tail purely from DB rows — it must derive the very same id the engine mints, so
// the two stay in lockstep by construction rather than by a duplicated string.
func DelegationContinuationID(parentJobID string) string {
	return delegationContinuationID(parentJobID)
}

// isPipelineOrchestrateRoot reports whether parentJob belongs to a #758 pipeline
// orchestrate stage sub-tree — i.e. the tree ROOT is a pipeline stage job carrying
// the OrchestrateStage flag. Such a root has NO task (a pipeline orchestrate stage
// request sets no TaskID, so its taskRef is empty), which means the tree-terminal
// paths that normally e.block(ref) would set no task state AND mint no continuation,
// stranding the stage's continuation chain with no foldable tail. The caller routes
// those paths to enqueueFinalizeContinuation instead so the chain always ends in a
// settled, delegation-less tail the pipeline advancer can fold.
//
// The first generation (the stage job itself) carries OrchestrateStage directly; a
// later continuation generation does not copy the flag onto its payload, so resolve
// the tree root and read the flag there. Best-effort: any lookup/parse failure
// returns false, keeping the e.block path byte-identical for every non-orchestrate
// tree (the overwhelming default).
func (e Engine) isPipelineOrchestrateRoot(ctx context.Context, parentJob db.Job, parentPayload JobPayload) bool {
	if parentPayload.OrchestrateStage {
		return true
	}
	rootID := e.rootJobID(parentJob, parentPayload)
	if strings.TrimSpace(rootID) == "" || rootID == parentJob.ID {
		return false
	}
	root, err := e.Store.GetJob(ctx, rootID)
	if err != nil {
		return false
	}
	rootPayload, err := unmarshalPayload(root.Payload)
	if err != nil {
		return false
	}
	return rootPayload.OrchestrateStage
}

// finalizeOrBlockDispatch converts a DISPATCH-path BlockedError into a foldable
// finalize tail for a #758 pipeline-orchestrate root, mirroring how the
// child-completion terminal sites (block_parent, vote/quorum synthesis gates)
// route to enqueueFinalizeContinuation. A pipeline orchestrate stage job carries
// no task (empty taskRef), so the one-shot dispatch-path blocks — write delegation
// artifacts, and every worktree/branch-lock allocation BlockedError bubbling up
// from allocateAndEnqueueDelegation (implement worktree, unresolved implement deps,
// integration worktree, read-only fan-out worktree, shared-checkout branch lock) —
// would e.block(empty ref): set NO task state and mint NO continuation, stranding
// the stage's chain with no foldable tail (the coordinator stays succeeded with
// delegations>0 and orchestrateStageSettleOutcome never settles). Routing them to a
// finalize continuation guarantees the chain always ends in a settled, delegation-
// less tail the pipeline advancer folds. Returning it (nil on success) also STOPS
// the dispatch loop exactly like the original BlockedError did, so no further child
// is enqueued after the tail is minted.
//
// Byte-identical for every other tree: a nil err returns nil, and a non-orchestrate
// root (isPipelineOrchestrateRoot false) returns the BlockedError unchanged — the
// gate only ever fires for a root whose empty ref made e.block a pure no-op-state
// BlockedError with nothing durable to preserve.
func (e Engine) finalizeOrBlockDispatch(ctx context.Context, job db.Job, payload JobPayload, err error) error {
	if err == nil {
		return nil
	}
	var blocked BlockedError
	if errors.As(err, &blocked) && e.isPipelineOrchestrateRoot(ctx, job, payload) {
		return e.enqueueFinalizeContinuation(ctx, job, payload, blocked.Reason)
	}
	return err
}

// buildContinuationPrompt inlines each finished child's job id, agent, decision,
// summary, and PR link into the coordinator continuation prompt so the
// coordinator can synthesize the results without re-reading every child job. When
// humanAnswer is non-empty (the ask-gate `answer` resume path, #445) it is
// rendered as a clearly-labelled block at the TOP of the prompt so the resumed
// coordinator reads the human's decision before the delegation results; it is ""
// for every non-answer continuation, keeping that prompt byte-identical.
func (e Engine) buildContinuationPrompt(goal, budgetLine string, parentResult *AgentResult, children map[string]db.Job, childPayloads map[string]JobPayload, humanAnswer string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	if block := strings.TrimSpace(humanAnswer); block != "" {
		builder.WriteString("Human answers to your questions (you paused to ask these; use them and proceed):\n")
		builder.WriteString(block)
		builder.WriteString("\n\n")
	}
	builder.WriteString("All delegated jobs have finished. Review the results below.\n\n")
	builder.WriteString(budgetLine)
	builder.WriteString("Delegation results:\n")
	// remainingInline tracks the aggregate ArtifactBody budget across all children
	// for this continuation; only consulted when InlineArtifactBodies is set.
	remainingInline := maxInlineArtifactTotalBytes
	for _, d := range parentResult.Delegations {
		child, ok := children[d.ID]
		if !ok {
			fmt.Fprintf(&builder, "- delegation %q (agent %s): not enqueued (dependencies unmet)\n", d.ID, d.Agent)
			continue
		}
		decision := child.State
		summary := ""
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			if strings.TrimSpace(payload.Result.Decision) != "" {
				decision = payload.Result.Decision
			}
			summary = strings.TrimSpace(payload.Result.Summary)
		}
		fmt.Fprintf(&builder, "- delegation %q (job %s, agent %s): %s", d.ID, child.ID, child.Agent, decision)
		if phase := strings.TrimSpace(d.Phase); phase != "" {
			fmt.Fprintf(&builder, " [phase: %s]", phase)
		}
		if summary != "" {
			fmt.Fprintf(&builder, " — %s", summary)
		}
		if link := childPullRequestLink(childPayloads[d.ID]); link != "" {
			fmt.Fprintf(&builder, " (%s)", link)
		}
		builder.WriteString("\n")
		// Opt-in: inline the child's brief body as a fenced block so a downstream
		// model reads it inline. Guarded entirely behind the flag so the disabled
		// output is byte-identical to the legacy prompt.
		if e.InlineArtifactBodies {
			e.appendInlineArtifactBody(&builder, childPayloads[d.ID], child.ID, &remainingInline)
		}
	}
	// Completion contract: make termination directed. The engine already treats
	// an empty delegations list as terminal, so spell it out for the agent. The
	// closing is reframed (#418) from "decide the next step" to goal-anchored
	// synthesis — but the termination semantics (empty delegations = done) are
	// unchanged.
	builder.WriteString("\n\n")
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// appendInlineArtifactBody writes a fenced block containing the child's
// payload.Result.ArtifactBody, rune-safe truncated to the per-body cap
// (e.MaxInlineArtifactBytes or defaultMaxInlineArtifactBytes) and further bounded
// by the per-continuation aggregate budget (*remaining). The block is fenced so an
// embedded gitmoot_result sentinel inside a body cannot confuse a downstream
// model. When truncation occurs a trailing marker points at the full brief on
// disk. It is only called when Engine.InlineArtifactBodies is true.
func (e Engine) appendInlineArtifactBody(builder *strings.Builder, payload JobPayload, childJobID string, remaining *int) {
	if payload.Result == nil {
		return
	}
	body := payload.Result.ArtifactBody
	if body == "" {
		return
	}
	perBody := e.MaxInlineArtifactBytes
	if perBody <= 0 {
		perBody = defaultMaxInlineArtifactBytes
	}
	limit := perBody
	if *remaining < limit {
		limit = *remaining
	}
	if limit <= 0 {
		return
	}
	truncated, omitted := truncateUTF8Bytes(body, limit)
	*remaining -= len(truncated)
	// Assemble the inner block (body + optional truncation marker) first, then pick
	// a fence longer than the longest backtick run inside it. A plain ``` fence is
	// broken by a body that itself contains ``` (briefs/reviews routinely embed code
	// fences), which would let an embedded gitmoot_result sentinel escape — exactly
	// what fencing must prevent.
	var inner strings.Builder
	inner.WriteString(truncated)
	if !strings.HasSuffix(truncated, "\n") {
		inner.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&inner, "... (%d bytes truncated; full brief at %s)\n", omitted, e.inlineBriefPath(childJobID))
	}
	fence := artifactBodyFence(inner.String())
	builder.WriteString("  artifact_body:\n")
	builder.WriteString(fence)
	builder.WriteString("\n")
	builder.WriteString(inner.String())
	builder.WriteString(fence)
	builder.WriteString("\n")
}

// maxUpstreamDepSummaryPreviewBytes caps the inline summary preview emitted on a
// dependency's header line in the "Upstream dependency results" block (#419). The
// full body travels by reference as the fenced artifact_body below the line, so
// the header preview is intentionally short; it is rune-safe truncated and fenced
// so an embedded gitmoot_result sentinel in a summary cannot escape inline.
const maxUpstreamDepSummaryPreviewBytes = 280

// buildUpstreamDepBlock renders the "Upstream dependency results" block injected
// into a ready dependent leg's prompt when InjectUpstreamDepContext is set (#419):
// deps[] as real dataflow. For each of d's succeeded DIRECT deps (sorted by id for
// stable output), it emits a header line —
//
//   - dep <id> (agent <a>, action <act>): <decision> — <summary preview> (<PR>) [changes_made: N] [head <sha7>]
//
// then the dep's artifact_body as a fenced, byte-budgeted block reusing
// appendInlineArtifactBody (the SAME per-body cap and shared aggregate budget the
// continuation prompt uses). Deps travel by reference: decision + truncated
// summary preview + PR link + changes count + short HeadSHA + the on-disk body
// path in any truncation marker — never the bulk body by value beyond the budget.
//
// Decided defaults (#419): direct-deps only; succeeded only
// (State==JobSucceeded && Result!=nil), defensive on a nil Result (decision/state
// line only, no body); body-only (no Findings). A dep resolved through
// fingerprint dedup is followed to its winning sibling. Returns "" when d has no
// deps or none are succeeded, so the caller appends nothing (and the flag-off
// path is byte-identical). Callers MUST gate the call on InjectUpstreamDepContext.
func (e Engine) buildUpstreamDepBlock(d Delegation, children map[string]db.Job, dedupWinners map[string]db.Job) string {
	deps := compactStrings(d.Deps)
	if len(deps) == 0 {
		return ""
	}
	// Sort by id for stable, order-independent output regardless of how the
	// coordinator listed deps.
	sortedDeps := make([]string, len(deps))
	copy(sortedDeps, deps)
	sort.Strings(sortedDeps)

	// remaining tracks the SAME aggregate artifact-body budget the continuation
	// prompt uses, so a verbose upstream body cannot balloon the dependent's
	// prompt across multiple deps.
	remaining := maxInlineArtifactTotalBytes
	var builder strings.Builder
	wrote := false
	for _, dep := range sortedDeps {
		depJob, ok := children[dep]
		if !ok {
			// A dep that points at a fingerprint-deduped delegation has no child of
			// its own; follow it to the winning sibling that did run.
			depJob, ok = dedupWinners[dep]
		}
		if !ok || depJob.State != string(JobSucceeded) {
			// Succeeded-only: a not-yet-run/failed dep contributes nothing (and a
			// dependent only enqueues once depsSatisfied, so this is defensive).
			continue
		}
		depPayload, err := unmarshalPayload(depJob.Payload)
		if err != nil {
			continue
		}
		if !wrote {
			builder.WriteString("\n\nUpstream dependency results:\n")
			wrote = true
		}
		e.appendUpstreamDepEntry(&builder, dep, depJob, depPayload, &remaining)
	}
	if !wrote {
		return ""
	}
	return builder.String()
}

// appendUpstreamDepEntry writes one dependency's header line and (when present)
// its fenced artifact_body to the upstream block. The header line carries the
// pass-by-reference handle (decision/summary preview/PR/changes count/HeadSHA);
// the body, if any, is fenced + truncated via appendInlineArtifactBody so it
// shares the aggregate budget and cannot break out of its fence. Defensive on a
// nil Result: emits the decision/state line only.
func (e Engine) appendUpstreamDepEntry(builder *strings.Builder, depID string, depJob db.Job, depPayload JobPayload, remaining *int) {
	decision := depJob.State
	summary := ""
	if depPayload.Result != nil {
		if d := strings.TrimSpace(depPayload.Result.Decision); d != "" {
			decision = d
		}
		summary = strings.TrimSpace(depPayload.Result.Summary)
	}
	fmt.Fprintf(builder, "- dep %q (agent %s, action %s): %s", depID, depJob.Agent, depJob.Type, decision)
	if summary != "" {
		fmt.Fprintf(builder, " — %s", upstreamDepSummaryPreview(summary))
	}
	if link := childPullRequestLink(depPayload); link != "" {
		fmt.Fprintf(builder, " (%s)", link)
	}
	if depPayload.Result != nil && len(depPayload.Result.ChangesMade) > 0 {
		fmt.Fprintf(builder, " [changes_made: %d]", len(depPayload.Result.ChangesMade))
	}
	if sha := shortHeadSHA(depPayload.HeadSHA); sha != "" {
		fmt.Fprintf(builder, " [head %s]", sha)
	}
	builder.WriteString("\n")
	// Body by reference: a fenced, truncated artifact_body under the line, sharing
	// the aggregate budget. appendInlineArtifactBody is a no-op when the body is
	// empty or the budget is spent, so a nil/empty Result emits only the line.
	e.appendInlineArtifactBody(builder, depPayload, depJob.ID, remaining)
}

// upstreamDepSummaryPreview caps the summary shown inline on a dep's header line
// to a short, rune-safe preview and fences it when it contains a backtick run so
// an embedded sentinel cannot escape. The full body travels separately as the
// fenced artifact_body, so this preview is deliberately short.
func upstreamDepSummaryPreview(summary string) string {
	preview, omitted := truncateUTF8Bytes(summary, maxUpstreamDepSummaryPreviewBytes)
	if omitted > 0 {
		preview = strings.TrimRight(preview, " \t\n") + "…"
	}
	if strings.Contains(preview, "`") {
		fence := artifactBodyFence(preview)
		return fence + preview + fence
	}
	return preview
}

// shortHeadSHA returns the first 7 hex chars of a commit SHA for compact display,
// or "" when the SHA is empty. It does not validate hex; a shorter SHA is returned
// as-is.
func shortHeadSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// artifactBodyFence returns a backtick fence guaranteed longer than the longest
// run of backticks in content, so an embedded ``` (or a sentinel wrapped in one)
// cannot terminate the fenced block early. Minimum three backticks.
func artifactBodyFence(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// inlineBriefPath renders the on-disk location of the parent's full brief.md, the
// same path writeDelegationArtifacts uses (ArtifactRoot/delegations/<sanitized
// parent job id>/brief.md). The parent job id is recovered from a child job id by
// stripping the trailing "/delegation/<child>" suffix; on any failure it falls
// back to a placeholder segment so the marker is still actionable.
func (e Engine) inlineBriefPath(childJobID string) string {
	root := strings.TrimSpace(e.ArtifactRoot)
	if root == "" {
		root = "<ArtifactRoot>"
	}
	segment := "<parent>"
	parentJobID := parentJobIDFromChild(childJobID)
	if seg, err := safeDelegationPathSegment(parentJobID, "parent job id"); err == nil {
		segment = seg
	}
	return root + "/delegations/" + segment + "/brief.md"
}

// parentJobIDFromChild recovers a parent job id from a delegation child job id of
// the form "<parent>/delegation/<child>". When the marker is absent it returns the
// input unchanged so inlineBriefPath can still sanitize it.
func parentJobIDFromChild(childJobID string) string {
	if idx := strings.LastIndex(childJobID, "/delegation/"); idx >= 0 {
		return childJobID[:idx]
	}
	return childJobID
}

// truncateUTF8Bytes returns s capped to at most maxBytes bytes without splitting a
// multi-byte UTF-8 rune, along with the number of bytes omitted from the original.
// Unlike the truncators in internal/cli it does NOT collapse whitespace; the body
// is preserved verbatim up to the cut point.
func truncateUTF8Bytes(s string, maxBytes int) (string, int) {
	if maxBytes <= 0 {
		return "", len(s)
	}
	if len(s) <= maxBytes {
		return s, 0
	}
	cut := maxBytes
	// Back up to a rune boundary: a continuation byte has the form 10xxxxxx.
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut], len(s) - cut
}

// buildCorrectiveContinuationPrompt is the one-shot nudge sent when a
// coordinator re-issues a delegation set it already issued. It tells the
// coordinator the repeat changed nothing and asks it to change approach or
// finish, then lists the repeated delegations for context. If it repeats again,
// handleDelegationLoop escalates to delegation_loop_detected and stops.
func buildCorrectiveContinuationPrompt(goal string, parentResult *AgentResult) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("You delegated the same set as a previous round; it did not change the outcome. Change your approach or return an EMPTY delegations list to finish.\n\n")
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("Repeated delegation set:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
		builder.WriteString("\n")
	}
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// buildPreflightCorrectiveContinuationPrompt is the corrective continuation sent
// when a delegation set is unroutable (#451): one or more delegations named an
// agent that is not a routable registered agent (unknown / not-allowed /
// uncapable — most often a runtime name where an agent NAME was required). It
// carries the actionable preflight reason (which lists the agents valid for the
// repo and the inline ephemeral alternative) so the coordinator can re-emit a
// corrected set, and it restates that the agent field is a registered agent NAME,
// not a runtime. None of the set was dispatched (all-or-nothing preflight).
func buildPreflightCorrectiveContinuationPrompt(goal string, parentResult *AgentResult, reason string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("Your delegation set could not be dispatched: it named an agent that is not routable, so NONE of it was dispatched (the preflight is all-or-nothing).\n\n")
	fmt.Fprintf(&builder, "%s\n\n", strings.TrimSpace(reason))
	builder.WriteString("A delegation's `agent` field is a registered agent NAME, not a runtime (codex/claude/kimi are runtimes). Re-emit the delegation set using a valid agent name from the list above, or use an inline `ephemeral` spec for an unregistered worker. If you cannot route the work, return an EMPTY delegations list to finish.\n")
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("\nDelegations that were NOT dispatched:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
	}
	return builder.String()
}

// buildVerifyReplanContinuationPrompt is the bounded corrective continuation sent
// when the engine-level verify gate (#439) derives a FAILED verdict from the
// verify-tagged legs. Unlike the finalize prompt it is NOT terminal — it asks the
// coordinator to REPLAN: fix the issues the independent verification surfaced and
// re-run the work. It is goal-anchored (#418), surfaces each verify leg's
// decision/summary so the replan can target the fix, and states the remaining
// attempt budget (after this attempt the loop routes to a best-effort finalize).
func buildVerifyReplanContinuationPrompt(goal string, parentResult *AgentResult, children map[string]db.Job, childPayloads map[string]JobPayload, attempt, attemptCap int) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	builder.WriteString("Independent verification FAILED: at least one verify leg reported the work is not yet correct.\n\n")
	// Surface the verify legs' findings so the coordinator can target the fix
	// rather than re-running blind. Only the verify-tagged legs are listed (the
	// failing verdict is derived from them).
	if parentResult != nil {
		wrote := false
		for _, d := range parentResult.Delegations {
			if delegationSynthesisRule(d) != "verify" {
				continue
			}
			if !wrote {
				builder.WriteString("Verification findings:\n")
				wrote = true
			}
			decision := "missing"
			summary := ""
			if child, ok := children[d.ID]; ok {
				decision = child.State
			}
			if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
				if strings.TrimSpace(payload.Result.Decision) != "" {
					decision = payload.Result.Decision
				}
				summary = strings.TrimSpace(payload.Result.Summary)
			}
			fmt.Fprintf(&builder, "- verify leg %q (agent %s): %s", d.ID, d.Agent, decision)
			if summary != "" {
				fmt.Fprintf(&builder, " — %s", summary)
			}
			builder.WriteString("\n")
		}
		if wrote {
			builder.WriteString("\n")
		}
	}
	fmt.Fprintf(&builder, "This is verify→replan attempt %d of %d. Address the verification findings above, then re-delegate the corrective work. ", attempt, attemptCap)
	if attempt >= attemptCap {
		builder.WriteString("This is the LAST attempt: if verification fails again you will be asked to synthesize a best-effort final result.\n\n")
	} else {
		builder.WriteString("\n\n")
	}
	builder.WriteString(goalSynthesisClosing(goal))
	return builder.String()
}

// buildFinalizeContinuationPrompt is the terminal continuation sent when a
// termination backstop trips. It tells the coordinator it has hit a limit and
// cannot delegate further, asks it to synthesize a best-effort final result from
// the completed work, and states that any delegations it returns now are ignored
// (the engine enforces this via DelegationFinalize). It lists the delegations that
// were not dispatched for context.
func buildFinalizeContinuationPrompt(goal string, parentResult *AgentResult, reason string) string {
	var builder strings.Builder
	builder.WriteString(goalAnchorHeader(goal))
	fmt.Fprintf(&builder, "A termination backstop was reached (%s). You cannot delegate any more work.\n\n", reason)
	// Goal-anchored synthesis (#418): the finalize continuation is already
	// terminal (any delegations are ignored — DelegationFinalize), so it restates
	// the goal and asks for a best-effort answer to it, reconciling child conflicts
	// and flagging gaps, rather than a raw stitch of child outputs.
	if strings.TrimSpace(goal) != "" {
		builder.WriteString("Synthesize a best-effort FINAL answer to the ORIGINAL GOAL above from what has already completed — reconcile any conflicts between children and flag any gaps — and return an EMPTY delegations list. Any delegations you return now will be ignored.\n")
	} else {
		builder.WriteString("Synthesize a best-effort FINAL result from what has already completed — reconcile any conflicts between children and flag any gaps — and return an EMPTY delegations list. Any delegations you return now will be ignored.\n")
	}
	if parentResult != nil && len(parentResult.Delegations) > 0 {
		builder.WriteString("\nDelegations that were NOT dispatched:\n")
		for _, d := range parentResult.Delegations {
			fmt.Fprintf(&builder, "- delegation %q (agent %s, action %s)\n", d.ID, d.Agent, d.Action)
		}
	}
	return builder.String()
}

func childPullRequestLink(payload JobPayload) string {
	if payload.PullRequest <= 0 || strings.TrimSpace(payload.Repo) == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", payload.Repo, payload.PullRequest)
}

func delegationSynthesisRule(d Delegation) string {
	rule := strings.ToLower(strings.TrimSpace(d.SynthesisRule))
	if rule == "" {
		return "summary"
	}
	return rule
}

// delegationSynthesisRequiresVote reports whether any delegation in the batch
// declares synthesis_rule "vote", which gates the coordinator continuation on
// every child being approved/succeeded.
func delegationSynthesisRequiresVote(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "vote" {
			return true
		}
	}
	return false
}

// delegationVoteSatisfied reports whether every delegation's child reached an
// approving outcome: a succeeded job state, or a child result decision of
// approved/succeeded/implemented. A missing or non-approving child fails the
// vote.
func delegationVoteSatisfied(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload) bool {
	for _, d := range delegations {
		child, ok := children[d.ID]
		if !ok {
			return false
		}
		if child.State == string(JobSucceeded) {
			continue
		}
		payload, ok := childPayloads[d.ID]
		if !ok || payload.Result == nil {
			return false
		}
		if !delegationDecisionApproves(payload.Result.Decision) {
			return false
		}
	}
	return true
}

// delegationSynthesisRequiresQuorum reports whether any delegation in the batch
// declares synthesis_rule "quorum", which gates the coordinator continuation on
// at least K children reaching an approving outcome.
func delegationSynthesisRequiresQuorum(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "quorum" {
			return true
		}
	}
	return false
}

// delegationQuorumThreshold returns the quorum K declared on the quorum
// delegation(s). When multiple quorum delegations declare different thresholds,
// the maximum is used.
func delegationQuorumThreshold(delegations []Delegation) int {
	k := 0
	for _, d := range delegations {
		if delegationSynthesisRule(d) != "quorum" {
			continue
		}
		if d.Quorum > k {
			k = d.Quorum
		}
	}
	return k
}

// delegationQuorumSatisfied reports whether at least k children reached an
// approving outcome. It is DECISION-FIRST, mirroring verifyVerdictPassed: whenever
// a child produced a parsed result its decision is the vote
// (approved/succeeded/implemented approve; changes_requested/blocked/failed do
// NOT), and only a child with no parsed result falls back to the succeeded job
// state. Consulting the decision first is load-bearing for the high-risk review
// quorum (#650): a review's changes_requested decision maps to a SUCCEEDED job
// state (stateForDecision), so a succeeded-state short-circuit would count a lens
// that asked for changes as an approving vote and let a high-risk PR clear the
// quorum despite reviewers refuting it.
func delegationQuorumSatisfied(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload, k int) bool {
	approving := 0
	for _, d := range delegations {
		child, ok := children[d.ID]
		if !ok {
			continue
		}
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			if delegationDecisionApproves(payload.Result.Decision) {
				approving++
			}
			continue
		}
		if child.State == string(JobSucceeded) {
			approving++
		}
	}
	return approving >= k
}

// highRiskLensQuorumMet reports whether the high-risk review coordinator that owns
// a lens child (childPayload.ParentJobID) has its cross-lens quorum satisfied
// (#650). It is the gate the native required-reviewer merge path consults for a
// high-risk lens child so an approving lens cannot drive the merge before every
// sibling refutation lens has approved. It fails OPEN (returns true) for a child
// with no coordinator, a missing/pruned coordinator, or a coordinator carrying no
// quorum rule, so it only ever ADDS a wait for a genuine quorum tree and never
// strands a non-quorum review.
func (e Engine) highRiskLensQuorumMet(ctx context.Context, childPayload JobPayload) (bool, error) {
	coordID := strings.TrimSpace(childPayload.ParentJobID)
	if coordID == "" {
		return true, nil
	}
	coord, err := e.Store.GetJob(ctx, coordID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	coordPayload, err := unmarshalPayload(coord.Payload)
	if err != nil {
		return false, err
	}
	if coordPayload.Result == nil || !delegationSynthesisRequiresQuorum(coordPayload.Result.Delegations) {
		return true, nil
	}
	children, err := e.childDelegationJobs(ctx, coordID)
	if err != nil {
		return false, err
	}
	childPayloads := make(map[string]JobPayload, len(children))
	for id, child := range children {
		payload, err := unmarshalPayload(child.Payload)
		if err != nil {
			return false, err
		}
		childPayloads[id] = payload
	}
	k := delegationQuorumThreshold(coordPayload.Result.Delegations)
	return delegationQuorumSatisfied(coordPayload.Result.Delegations, children, childPayloads, k), nil
}

func delegationDecisionApproves(decision string) bool {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approved", "succeeded", "implemented":
		return true
	default:
		return false
	}
}

// delegationSynthesisRequiresVerify reports whether any delegation in the batch
// declares synthesis_rule "verify", which makes the coordinator continuation
// subject to the engine-level verify→replan gate (#439). Unlike vote/quorum it
// does NOT block on failure; it issues a bounded corrective replan continuation.
func delegationSynthesisRequiresVerify(delegations []Delegation) bool {
	for _, d := range delegations {
		if delegationSynthesisRule(d) == "verify" {
			return true
		}
	}
	return false
}

// verifyVerdictPassed derives the v1 verify VERDICT (#439) mechanically from the
// verify-tagged legs (synthesis_rule == "verify"): it returns false iff any such
// leg reached a NON-approving outcome. The verdict is DECISION-driven, reusing the
// approving-outcome test (delegationDecisionApproves) over the #421 convention that
// a verify leg returns approved on a pass and changes_requested on a fail. Because
// a review's changes_requested decision maps to a SUCCEEDED job state
// (stateForDecision), the decision is consulted FIRST whenever the leg produced a
// parsed result — otherwise the succeeded-state short-circuit vote/quorum use would
// read every changes_requested verdict as a pass and defeat the gate. The
// succeeded-state check is kept only as a fallback for a verify leg that finished
// in a succeeded state without a parsed result.
//
// A verify leg with NO child is interpreted by whether it could ever have run.
// A leg whose deps terminally failed (delegationPermanentlyBlocked, the same
// predicate allDelegationsResolved uses) or that was folded into a fingerprint
// dedup winner NEVER RAN: verification was not performed, so its outcome is
// already governed by the upstream failure policy (continue/escalate) and it must
// be SKIPPED here, not read as a failed verdict — otherwise the engine would
// fabricate a failed verdict from an absent verifier and fire a premature
// verify→replan continuation claiming verification failed when it never happened
// (#439 review). Only a verify leg that WAS dispatchable yet has no terminal child
// (a genuinely missing/crashed verification) fails the verdict, so an absent
// verification of work that actually ran is never read as a pass. Non-verify legs
// are ignored here (the conservative ordering runs the vote/quorum gates first).
// No engine-side verify subprocess or second model call is made: the engine reads
// the already-completed verdict the verify leg reported.
func verifyVerdictPassed(delegations []Delegation, children map[string]db.Job, childPayloads map[string]JobPayload, dedupWinners map[string]db.Job) bool {
	byID := delegationsByID(delegations)
	for _, d := range delegations {
		if delegationSynthesisRule(d) != "verify" {
			continue
		}
		child, ok := children[d.ID]
		if !ok {
			// A verify leg that never ran (its deps terminally failed, or it was
			// folded into a dedup winner) is governed by the upstream failure policy,
			// not by this verdict: skip it rather than fabricating a failed verdict.
			if _, deduped := dedupWinners[d.ID]; deduped {
				continue
			}
			if delegationPermanentlyBlocked(d, children, byID, dedupWinners, map[string]bool{}) {
				continue
			}
			// Dispatchable verify leg with no terminal child: a genuinely missing or
			// crashed verification fails the verdict.
			return false
		}
		// Decision-first: when the verify leg produced a parsed result, its decision
		// is the verdict (approved => pass, changes_requested/failed/blocked => fail).
		if payload, ok := childPayloads[d.ID]; ok && payload.Result != nil {
			if !delegationDecisionApproves(payload.Result.Decision) {
				return false
			}
			continue
		}
		// No parsed result: fall back to the job state — a succeeded leg with no
		// verdict is treated as a pass, anything else (failed/blocked/queued) fails.
		if child.State != string(JobSucceeded) {
			return false
		}
	}
	return true
}

func (e Engine) preflightDelegation(ctx context.Context, request JobRequest) error {
	// An ephemeral delegation routes to an on-demand worker that no agent row
	// backs, so the registered-agent existence, repo-access, and capability checks
	// do not apply: the ephemeral child inherits the coordinator's allowed repo
	// scope. Only validate that the spec runtime is a real agent runtime (never
	// shell); the daemon materializes the worker from the spec.
	if request.Ephemeral != nil {
		return validateEphemeralSpec(request.DelegationID, request.Action, request.Ephemeral)
	}
	// Check existence FIRST so a legitimately-named agent literally called
	// "claude" (GetAgent hits) is never mistaken for the runtime-name mixup below.
	agent, err := e.Store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The name resolves to no agent. Only when it is itself a runtime name
			// ({codex,claude,kimi}) is this the common runtime-vs-agent mixup, so
			// flag that explicitly; otherwise it is a typo/unknown agent.
			prefix := fmt.Sprintf("agent %q is not subscribed", request.Agent)
			if _, isRuntime := allowedSet(EphemeralRuntimes)[strings.TrimSpace(request.Agent)]; isRuntime {
				prefix = fmt.Sprintf("%q is a runtime, not a registered agent", request.Agent)
			}
			return fmt.Errorf("%s. %s", prefix, e.delegationAgentHint(ctx, request.Repo))
		}
		return err
	}
	allowed, err := e.Store.AgentCanAccessRepo(ctx, agent.Name, request.Repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q. %s", agent.Name, request.Repo, e.delegationAgentHint(ctx, request.Repo))
	}
	if !contains(agent.Capabilities, request.Action) {
		return fmt.Errorf("agent %q lacks %q capability. %s", agent.Name, request.Action, e.delegationAgentHint(ctx, request.Repo))
	}
	return nil
}

// availableAgentsForRepo returns the names of registered agents that can access
// repo, sorted by name (ListAgents already ORDER BY name). It fails soft: a
// ListAgents error yields nil so the caller's base error is never masked, and a
// per-agent AgentCanAccessRepo error simply drops that agent from the suggestion.
func (e Engine) availableAgentsForRepo(ctx context.Context, repo string) []string {
	agents, err := e.Store.ListAgents(ctx)
	if err != nil {
		return nil
	}
	var names []string
	for _, a := range agents {
		ok, err := e.Store.AgentCanAccessRepo(ctx, a.Name, repo)
		if err != nil || !ok {
			continue
		}
		names = append(names, a.Name)
	}
	return names
}

// delegationAgentHint renders the actionable suffix appended to every
// preflightDelegation error: which registered agents are usable on repo (so a
// coordinator can re-emit a corrected set) plus the inline ephemeral escape hatch
// that needs no pre-registration. The agent field of a delegation is a registered
// agent NAME, not a runtime.
func (e Engine) delegationAgentHint(ctx context.Context, repo string) string {
	var b strings.Builder
	names := e.availableAgentsForRepo(ctx, repo)
	if len(names) > 0 {
		fmt.Fprintf(&b, "Agents allowed on %s: %s. ", repo, strings.Join(names, ", "))
	} else {
		fmt.Fprintf(&b, "No agents are registered for %s. ", repo)
	}
	b.WriteString(`To run an unregistered worker, use an inline ephemeral spec ({runtime: "codex|claude|kimi"}).`)
	return b.String()
}

func (e Engine) implementationNeedsFinalizer(ctx context.Context, payload JobPayload) bool {
	if e.ImplementationFinalizer == nil {
		return false
	}
	taskID := strings.TrimSpace(payload.TaskID)
	if taskID == "" {
		return false
	}
	task, err := e.Store.GetTask(ctx, taskID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(task.WorktreePath) != ""
}
