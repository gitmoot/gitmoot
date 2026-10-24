package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

// CleanupPolicy is supplied by the producer, never by a deletion target. Empty
// Mode selects ordinary cleanup for success and retention-bound cleanup otherwise.
type CleanupPolicy struct {
	Mode      string        `json:"mode"`
	Retention time.Duration `json:"retention_ns"`
	cutoff    time.Time
}

type CleanupConsumer struct {
	JobID       string `json:"job_id"`
	Delegation  string `json:"delegation_id"`
	State       string `json:"state"`
	Generation  int64  `json:"lifecycle_generation"`
	PayloadHash string `json:"payload_hash"`
}

type CleanupInspection struct {
	SchemaVersion       int                          `json:"schema_version"`
	Producer            string                       `json:"producer"`
	DecisionOwner       string                       `json:"decision_owner"`
	ObservedAt          string                       `json:"observed_at"`
	ResourceID          string                       `json:"resource_id"`
	ResourceKind        string                       `json:"resource_kind"`
	OwnerJobID          string                       `json:"owner_job_id"`
	OwnerState          string                       `json:"owner_state"`
	LifecycleGeneration int64                        `json:"lifecycle_generation"`
	Repo                string                       `json:"repo"`
	ExpectedPath        string                       `json:"expected_path"`
	ResolvedPath        string                       `json:"resolved_path"`
	OwnedBranch         string                       `json:"owned_branch,omitempty"`
	Eligible            bool                         `json:"eligible"`
	ReasonCodes         []string                     `json:"reason_codes"`
	EligibilityToken    string                       `json:"eligibility_token,omitempty"`
	Consumers           []CleanupConsumer            `json:"consumers"`
	PipelineConsumers   []db.CleanupPipelineConsumer `json:"pipeline_consumers"`
	Policy              CleanupPolicy                `json:"policy"`
	PolicyHash          string                       `json:"policy_hash"`
	RetentionDeadline   string                       `json:"retention_deadline,omitempty"`
	Rebuildable         bool                         `json:"rebuildable"`
	ApprovedSizeBudget  *int64                       `json:"approved_size_budget"`
	Receipt             *db.CleanupReceipt           `json:"receipt,omitempty"`
	Obligation          *db.CleanupObligation        `json:"obligation,omitempty"`
	evidence            cleanupEvidence
}

type cleanupEvidence struct {
	ResourceID        string                       `json:"resource_id"`
	OwnerState        string                       `json:"owner_state"`
	Generation        int64                        `json:"generation"`
	OwnerPayloadHash  string                       `json:"owner_payload_hash"`
	Policy            CleanupPolicy                `json:"policy"`
	PolicyHash        string                       `json:"policy_hash"`
	TerminalAt        string                       `json:"terminal_at"`
	PathIdentity      string                       `json:"path_identity"`
	Git               gitutil.WorktreeIdentity     `json:"git"`
	Ancestors         []CleanupConsumer            `json:"ancestors"`
	Consumers         []CleanupConsumer            `json:"consumers"`
	PipelineConsumers []db.CleanupPipelineConsumer `json:"pipeline_consumers"`
}

type cleanupWorktreeInspector interface {
	InspectWorktree(context.Context, string) (gitutil.WorktreeIdentity, error)
	RevParse(context.Context, string) (string, error)
}

func cleanupHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (d CleanupInspection) refuse(reason string) CleanupInspection {
	d.Eligible = false
	d.EligibilityToken = ""
	d.ReasonCodes = []string{reason}
	return d
}

func cleanupConsumer(job db.Job) CleanupConsumer {
	return CleanupConsumer{JobID: job.ID, Delegation: job.DelegationID, State: job.State,
		Generation: job.LifecycleGeneration, PayloadHash: cleanupHash(job.Payload)}
}

// InspectProducerCleanup is advisory and does not create obligations or receipts.
// ApplyProducerCleanup repeats these checks under the same exclusive fence.
func (e Engine) InspectProducerCleanup(ctx context.Context, jobID string, policy CleanupPolicy) (CleanupInspection, error) {
	if err := e.validate(); err != nil {
		return CleanupInspection{}, err
	}
	ctx, release, err := e.Store.AcquireCleanupLifecycle(ctx)
	if err != nil {
		return CleanupInspection{}, err
	}
	defer release()
	decision, _, err := e.inspectProducerCleanup(ctx, jobID, policy, nil)
	return decision, err
}

func (e Engine) inspectProducerCleanup(ctx context.Context, jobID string, policy CleanupPolicy, collected *cleanupEvidence) (CleanupInspection, JobPayload, error) {
	d := CleanupInspection{SchemaVersion: 1, Producer: "gitmoot", DecisionOwner: "gitmoot",
		OwnerJobID: jobID, ResourceKind: "delegation_worktree", ObservedAt: e.now().UTC().Format(time.RFC3339Nano),
		ReasonCodes: []string{}, Consumers: []CleanupConsumer{}}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return d, payload, err
	}
	d.OwnerState, d.LifecycleGeneration = job.State, job.LifecycleGeneration
	d.Repo, d.ExpectedPath = payload.Repo, strings.TrimSpace(payload.WorktreePath)
	implement := isImplementDelegationWorktree(job.Type, payload)
	readOnly := isReadOnlyDelegationWorktree(job.Type, payload)
	if !implement && !readOnly {
		return d.refuse("unsupported_resource"), payload, nil
	}
	if implement {
		d.OwnedBranch = strings.TrimSpace(payload.Branch)
	}
	d.ResourceID = db.CleanupObligationResourceID(jobID, d.ExpectedPath)
	if collected != nil && (collected.ResourceID != d.ResourceID || collected.Generation != job.LifecycleGeneration || collected.OwnerState != job.State) {
		return d.refuse("generation_changed"), payload, nil
	}
	if !IsFinalJobState(job.State) {
		return d.refuse("owner_resumable"), payload, nil
	}
	validator := ValidateDelegationCleanupTarget
	if e.cleanupTargetValidator != nil {
		validator = e.cleanupTargetValidator
	}
	if err := validator(e.Home, jobID, job.Type, payload); err != nil {
		return d.refuse("identity_or_containment"), payload, nil
	}
	d.ResolvedPath, err = evalPathWithMissingTail(d.ExpectedPath)
	if err != nil {
		return d.refuse("identity_or_containment"), payload, nil
	}
	if strings.TrimSpace(e.DelegationCheckout) == "" {
		return d.refuse("checkout_identity_unknown"), payload, nil
	}
	if _, ok := e.DelegationWorktrees.(ReadOnlyWorktreeManager); !ok {
		return d.refuse("manager_unavailable"), payload, nil
	}
	inspector, ok := e.DelegationWorktrees.(cleanupWorktreeInspector)
	if !ok {
		return d.refuse("git_identity_unknown"), payload, nil
	}
	if implement {
		if _, ok := e.DelegationWorktrees.(BranchDeleter); !ok {
			return d.refuse("branch_cleanup_unavailable"), payload, nil
		}
		if _, ok := e.DelegationWorktrees.(BranchExistenceChecker); !ok {
			return d.refuse("branch_identity_unknown"), payload, nil
		}
		lock, lerr := e.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
		if lerr != nil && !errors.Is(lerr, sql.ErrNoRows) {
			return d.refuse("branch_owner_unknown"), payload, nil
		}
		if lerr == nil && lock.Owner != cleanupBranchOwner(job, payload) {
			return d.refuse("branch_owner_changed"), payload, nil
		}
	}
	if e.CleanupRetention != nil {
		retention, err := e.CleanupRetention()
		if err != nil {
			return d.refuse("policy_unavailable"), payload, err
		}
		policy.Retention = retention
		policy.cutoff = time.Time{}
	}
	if policy.Mode == "" {
		policy.Mode = "ordinary"
		if job.State != string(JobSucceeded) {
			policy.Mode = "aged"
		}
	}
	if policy.Mode != "ordinary" && policy.Mode != "aged" {
		return d.refuse("policy_unknown"), payload, nil
	}
	if policy.Mode == "ordinary" {
		policy.Retention = 0
	}
	d.Policy = policy
	encodedPolicy, _ := json.Marshal(policy)
	d.PolicyHash = cleanupHash(string(encodedPolicy))
	d.evidence = cleanupEvidence{ResourceID: d.ResourceID, OwnerState: job.State,
		Generation: job.LifecycleGeneration, OwnerPayloadHash: cleanupHash(job.Payload), PolicyHash: d.PolicyHash, Policy: policy}
	if receipt, rerr := e.Store.GetCleanupReceipt(ctx, d.ResourceID, job.LifecycleGeneration); rerr == nil {
		d.Receipt = &receipt
		if receipt.RemovedAt != "" {
			return d.refuse("already_removed"), payload, nil
		}
		var saved cleanupEvidence
		if json.Unmarshal([]byte(receipt.Evidence), &saved) != nil || receipt.OwnerPayload != job.Payload ||
			saved.OwnerState != job.State || saved.Generation != job.LifecycleGeneration || saved.ResourceID != d.ResourceID {
			return d.refuse("terminal_evidence_changed"), payload, nil
		}
		collected = &saved
	} else if !errors.Is(rerr, sql.ErrNoRows) {
		return d, payload, rerr
	}
	if obligation, oerr := e.Store.GetCleanupObligation(ctx, d.ResourceID); oerr == nil {
		d.Obligation = &obligation
		if obligation.LifecycleGeneration == job.LifecycleGeneration {
			if obligation.State == db.CleanupObligationQuarantined {
				return d.refuse("quarantined"), payload, nil
			}
			if obligation.State == db.CleanupObligationRemoved && d.Receipt == nil {
				return d.refuse("legacy_cleanup_receipt_missing"), payload, nil
			}
			if next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt); err != nil || next.After(e.now()) {
				return d.refuse("retry_not_due"), payload, nil
			}
		}
	} else if !errors.Is(oerr, sql.ErrNoRows) {
		return d, payload, oerr
	}
	terminalAt, known := parseStoredJobTime(job.UpdatedAt)
	if !known {
		terminalAt, known = parseStoredJobTime(job.CreatedAt)
	}
	if d.Obligation != nil && d.Obligation.LifecycleGeneration == job.LifecycleGeneration && d.Obligation.TerminalAt != "" {
		terminalAt, known = parseStoredJobTime(d.Obligation.TerminalAt)
	}
	if collected != nil {
		terminalAt, known = parseStoredJobTime(collected.TerminalAt)
	}
	if !known {
		return d.refuse("terminal_time_unknown"), payload, nil
	}
	d.evidence.TerminalAt = terminalAt.UTC().Format(time.RFC3339Nano)
	if policy.Mode == "aged" {
		cutoff := policy.cutoff
		if cutoff.IsZero() {
			if policy.Retention <= 0 {
				return d.refuse("aged_cleanup_disabled"), payload, nil
			}
			cutoff = e.now().Add(-policy.Retention)
			d.RetentionDeadline = terminalAt.Add(policy.Retention).UTC().Format(time.RFC3339Nano)
		}
		if terminalAt.After(cutoff) {
			return d.refuse("retention_pending"), payload, nil
		}
	}
	// Only pinned native PR reviews have an existing exact-head reconstruction
	// path. Terminal labels alone do not authorize deleting retryable resources.
	d.Rebuildable = readOnly && job.Type == "review" && payload.PullRequest > 0 && strings.TrimSpace(payload.HeadSHA) != ""
	if job.State != string(JobSucceeded) && (!d.Rebuildable || policy.Mode != "aged") {
		return d.refuse("retry_disposition_unknown"), payload, nil
	}
	if job.State == string(JobSucceeded) && payload.Result == nil {
		return d.refuse("terminal_result_missing"), payload, nil
	}
	if payload.CleanupArtifacts != nil {
		if err := ValidateCleanupArtifactSnapshot(e.Home, d.ResourceID, job.LifecycleGeneration, *payload.CleanupArtifacts); err != nil {
			return d.refuse("artifact_evidence_unavailable"), payload, nil
		}
	}
	refs, err := e.Store.ListJobWorktreeRefs(ctx)
	if err != nil {
		return d, payload, err
	}
	for _, other := range refs {
		if other.ID != jobID && filepath.Clean(strings.TrimSpace(other.WorktreePath)) == filepath.Clean(d.ExpectedPath) {
			return d.refuse("path_referenced"), payload, nil
		}
	}
	if reason, err := e.inspectCleanupConsumers(ctx, payload, &d); err != nil {
		return d, payload, err
	} else if reason != "" {
		return d.refuse(reason), payload, nil
	}
	if reason := e.cleanupRuntimeRefusal(ctx, jobID, d.ResolvedPath); reason != "" {
		return d.refuse(reason), payload, nil
	}
	info, statErr := os.Lstat(d.ExpectedPath)
	if statErr != nil && !os.IsNotExist(statErr) {
		return d.refuse("path_identity_unknown"), payload, nil
	}
	if os.IsNotExist(statErr) {
		if d.Receipt == nil || collected == nil {
			return d.refuse("resource_missing_without_receipt"), payload, nil
		}
		d.evidence.PathIdentity, d.evidence.Git = collected.PathIdentity, collected.Git
	} else {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return d.refuse("path_identity_unknown"), payload, nil
		}
		d.evidence.PathIdentity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
		d.evidence.Git, err = inspector.InspectWorktree(ctx, d.ResolvedPath)
		if err != nil || !d.evidence.Git.Registered || d.evidence.Git.Primary {
			return d.refuse("git_identity_unknown"), payload, nil
		}
		if implement && d.evidence.Git.Branch != payload.Branch || readOnly && d.evidence.Git.Branch != "" {
			return d.refuse("git_identity_changed"), payload, nil
		}
		if collected != nil && (collected.PathIdentity != d.evidence.PathIdentity || collected.Git != d.evidence.Git) {
			return d.refuse("resource_replaced"), payload, nil
		}
	}
	if implement {
		exists, err := e.DelegationWorktrees.(BranchExistenceChecker).BranchExists(ctx, payload.Branch)
		if err != nil {
			return d.refuse("branch_identity_unknown"), payload, nil
		}
		if exists {
			head, err := inspector.RevParse(ctx, "refs/heads/"+payload.Branch)
			if err != nil || strings.TrimSpace(head) != d.evidence.Git.HeadSHA {
				return d.refuse("branch_identity_changed"), payload, nil
			}
		} else if d.Receipt == nil {
			return d.refuse("branch_identity_unknown"), payload, nil
		}
	}
	d.evidence.Consumers = d.Consumers
	encoded, err := json.Marshal(d.evidence)
	if err != nil {
		return d, payload, err
	}
	d.Eligible = true
	d.EligibilityToken = cleanupHash(string(encoded))
	if d.Receipt != nil {
		d.EligibilityToken = d.Receipt.EligibilityToken
	}
	return d, payload, nil
}

func cleanupBranchOwner(job db.Job, payload JobPayload) string {
	if job.Type == "implement" && payload.DelegationReason == "runtime_session_busy" && payload.DelegatedAgent == job.Agent && strings.TrimSpace(payload.OriginalAgent) != "" {
		return payload.OriginalAgent
	}
	return job.Agent
}

func (e Engine) inspectCleanupConsumers(ctx context.Context, payload JobPayload, d *CleanupInspection) (string, error) {
	pipelineConsumers, err := e.Store.ListCleanupPipelineConsumers(ctx, d.OwnerJobID, payload.RootJobID)
	if err != nil {
		return "", err
	}
	d.PipelineConsumers = pipelineConsumers
	d.evidence.PipelineConsumers = pipelineConsumers
	if len(pipelineConsumers) == 0 && (payload.PipelineName != "" || strings.HasPrefix(payload.RootJobID, "prun-")) {
		return "pipeline_consumers_unknown", nil
	}
	for _, consumer := range pipelineConsumers {
		if consumer.RunState != "succeeded" || (consumer.State != "succeeded" && consumer.State != "skipped") {
			return "pipeline_consumer_resumable", nil
		}
	}
	parentID := strings.TrimSpace(payload.ParentJobID)
	if parentID == "" {
		return "", nil
	}
	seenAncestors := map[string]bool{}
	for ancestor := parentID; ancestor != ""; {
		if seenAncestors[ancestor] {
			return "consumer_graph_unknown", nil
		}
		seenAncestors[ancestor] = true
		job, p, err := e.jobPayload(ctx, ancestor)
		if err != nil || p.Result == nil {
			return "consumer_graph_unknown", nil
		}
		d.evidence.Ancestors = append(d.evidence.Ancestors, cleanupConsumer(job))
		if job.State != string(JobSucceeded) {
			return "ancestor_resumable", nil
		}
		ancestor = strings.TrimSpace(p.ParentJobID)
	}
	_, parent, err := e.jobPayload(ctx, parentID)
	if err != nil {
		return "consumer_graph_unknown", nil
	}
	declared := map[string]bool{}
	for _, delegation := range parent.Result.Delegations {
		for _, dep := range delegation.Deps {
			if strings.TrimSpace(dep) == strings.TrimSpace(payload.DelegationID) {
				declared[strings.TrimSpace(delegation.ID)] = false
			}
		}
	}
	children, err := e.Store.ListJobsByParent(ctx, parentID)
	if err != nil {
		return "consumer_graph_unknown", nil
	}
	for _, child := range children {
		if child.ID == d.OwnerJobID {
			continue
		}
		p, err := unmarshalPayload(child.Payload)
		if err != nil {
			return "consumer_graph_unknown", nil
		}
		_, consumes := declared[strings.TrimSpace(child.DelegationID)]
		for _, dep := range p.Deps {
			consumes = consumes || strings.TrimSpace(dep) == strings.TrimSpace(payload.DelegationID)
		}
		// Continuations can consume the completed tree as a whole, rather than
		// naming an individual delegation dependency.
		consumes = consumes || strings.TrimSpace(child.DelegationID) == ""
		if !consumes {
			continue
		}
		d.Consumers = append(d.Consumers, cleanupConsumer(child))
		if _, exists := declared[strings.TrimSpace(child.DelegationID)]; exists {
			declared[strings.TrimSpace(child.DelegationID)] = true
		}
		if child.State != string(JobSucceeded) {
			return "consumer_resumable", nil
		}
	}
	for id, found := range declared {
		if !found {
			d.Consumers = append(d.Consumers, CleanupConsumer{Delegation: id, State: "undispatched"})
			return "consumer_undispatched", nil
		}
	}
	sort.Slice(d.Consumers, func(i, j int) bool { return d.Consumers[i].JobID < d.Consumers[j].JobID })
	return "", nil
}
