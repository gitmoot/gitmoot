package db

import (
	"time"
)

type Repo struct {
	Owner         string
	Name          string
	DefaultBranch string
	RemoteURL     string
	CheckoutPath  string
	// PrimaryCheckoutPath is the durable first non-bare checkout reported by
	// `git worktree list --porcelain`. It lets dispatch recover when a mistakenly
	// registered linked worktree has been removed.
	PrimaryCheckoutPath string
	Enabled             bool
	PollInterval        string
	LastPollAt          string
	LastError           string
}

type Agent struct {
	Name           string
	Role           string
	Runtime        string
	RuntimeRef     string
	RepoScope      string
	TemplateID     string
	Model          string
	Effort         string
	Capabilities   []string
	AutonomyPolicy string
	HealthStatus   string
}

type AgentTemplate struct {
	ID             string
	Name           string
	Description    string
	SourceRepo     string
	SourceRef      string
	SourcePath     string
	ResolvedCommit string
	Content        string
	MetadataJSON   string
	VersionID      string
	VersionNumber  int
	VersionState   string
	ContentHash    string
	CreatedAt      string
	UpdatedAt      string
}

type AgentTemplateVersion struct {
	ID             string
	TemplateID     string
	VersionNumber  int
	State          string
	Name           string
	Description    string
	SourceRepo     string
	SourceRef      string
	SourcePath     string
	ResolvedCommit string
	ContentHash    string
	Content        string
	MetadataJSON   string
	CreatedAt      string
	UpdatedAt      string
	PromotedAt     string
}

type AgentRepo struct {
	AgentName    string
	RepoFullName string
}

type AgentInstance struct {
	Name           string
	Type           string
	Runtime        string
	RuntimeRef     string
	RepoFullName   string
	Role           string
	TemplateID     string
	Model          string
	Effort         string
	Capabilities   []string
	AutonomyPolicy string
	State          string
	CreatedAt      string
	LastUsedAt     string
	ExpiresAt      string
}

type Goal struct {
	ID     string
	Title  string
	Source string
	Status string
}

type Task struct {
	ID                     string
	RepoFullName           string
	GoalID                 string
	Title                  string
	State                  string
	Branch                 string
	WorktreePath           string
	UpdatedAt              string
	DisposalTier           string
	DisposalReason         string
	DisposedAt             string
	DisposalEscalationRole string
}

// TaskEvent is one append-only task lifecycle transition. FromState and
// ToState are empty only for informational events that do not move the task.
type TaskEvent struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"task_id"`
	Kind      string `json:"kind"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
}

// StaleTaskCandidate is the narrow age-aware task projection used by the
// daemon reconciler. Keeping UpdatedAt here avoids widening every Task reader.
type StaleTaskCandidate struct {
	ID           string
	RepoFullName string
	State        string
	Branch       string
	UpdatedAt    string
}

type PullRequest struct {
	RepoFullName   string
	Number         int64
	URL            string
	HeadBranch     string
	BaseBranch     string
	HeadSHA        string
	MergeCommitSHA string
	State          string
}

type Comment struct {
	RepoFullName string
	CommentID    int64
	PullRequest  int64
	Body         string
}

type Job struct {
	ID      string
	Agent   string
	Type    string
	State   string
	Payload string
	// Model is the durable model selected when the job was enqueued. Unlike the
	// payload's per-job override, it also snapshots agent and runtime defaults so
	// historical jobs do not change when mutable configuration changes.
	Model string
	// Runtime and RuntimeRef are optional, best-effort proof projections. The
	// jobs table does not persist a normal job's immutable runtime session: a
	// per-job override is carried in payload.runtime_override(_ref), while an
	// ordinary job can only be enriched from the agent's current registration.
	// General job readers leave both empty.
	Runtime         string
	RuntimeRef      string
	ParentJobID     string
	DelegationID    string
	DelegationDepth int
	DelegatedBy     string
	// RootID is the id of the coordination tree's originating coordinator,
	// denormalized onto the row as an indexed column (idx_jobs_root_id, #420) so
	// root-scoped helpers can answer "which jobs belong to this run?" with one
	// indexed lookup instead of a full-table scan that unmarshals every payload.
	// It mirrors the engine's rootJobID() rule: payload.RootJobID when set, else
	// the job's own id (self-root). payload.RootJobID stays the value source of
	// truth; this column is a write-time denormalized index. It is populated by
	// the ListJobsByRoot projection (other readers may leave it empty).
	RootID string
	// WorkflowID is the externally assigned global workflow label, denormalized
	// from payload.workflow_id for indexed grouping and filtering.
	WorkflowID string `json:"workflow_id,omitempty"`
	// Repo/PullRequest are lightweight workflow-list projections. General job
	// readers leave them empty because their payload already carries these values.
	Repo                   string `json:"repo,omitempty"`
	PullRequest            int    `json:"pull_request,omitempty"`
	BlockerRetryAt         string `json:"blocker_retry_at,omitempty"`
	BlockerSuggestedAction string `json:"blocker_suggested_action,omitempty"`
	// RootKilled is the operator kill-switch flag (#341). When true, the
	// delegation tree rooted at this job has been killed: the engine's next
	// dispatch routes through the graceful finalize continuation instead of
	// dispatching new delegations, and the daemon skips queued children.
	RootKilled bool
	// InputTokens and OutputTokens record the runtime token usage captured for
	// this job (best-effort per runtime; see UpdateJobUsage). They default to 0
	// and are populated by every jobs SELECT so the per-root delegation token
	// budget (#338 Part B) can sum a tree's usage. A job whose runtime does not
	// report usage contributes 0 — the budget still works, it just under-counts.
	InputTokens  int
	OutputTokens int
	// ExternallyDriven marks a job whose execution happens OUTSIDE the engine —
	// the "here"/prompt-import calling session does the real work and clocks it in
	// via `job open`/`record` (#657). It changes exactly three behaviors: the job
	// is created directly in `running` (never queued, so the daemon never claims or
	// Delivers it), the stuck-running reaper skips it (a session may hold it open
	// for minutes), and it is closed via the CLI rather than a runtime result.
	// Defaults false, so every engine-driven job and the whole normal dispatch/
	// reaper path is byte-identical. Populated by GetJob/ListJobs.
	ExternallyDriven bool
	// LifecycleGeneration is a MONOTONIC counter identifying one RUN of this job
	// (#1407). It is advanced by the store's state-writing UPDATEs on every
	// transition INTO queued, never by a caller.
	//
	// It exists because State cannot identify a run: a state is a VALUE that
	// recurs, so failed -> queued -> running -> failed returns State to its old
	// string and a compare-and-swap anchored on State alone succeeds against a
	// lifecycle the writer never observed. Any verdict formed about a SPECIFIC
	// run -- a settlement carrying a result from that run -- must be anchored on
	// this field, via TransitionJobStateWithEventAtGeneration.
	//
	// Rows created before the column existed read 0 and take their first bump on
	// their next re-queue, so a comparison is meaningful from the first retry
	// onward without any backfill.
	LifecycleGeneration int64
	// UpdatedAt is populated by ListJobs (for age display in the dashboard);
	// other readers may leave it zero.
	UpdatedAt string
	// CreatedAt is the job's creation timestamp (SQLite UTC form,
	// "2006-01-02 15:04:05"). Populated by ListJobs/GetJob so the web dashboard
	// can stamp a node's StartedAt; other readers may leave it zero.
	CreatedAt string
	// ResultHash is the SHA-256 hex of the compacted raw payload.result JSON.
	// It is populated by ListJobsByRoot for store-integrity projection; general
	// readers may leave it empty.
	ResultHash string
}

// TranscriptJob is the narrow retention-GC projection. It intentionally omits
// payload and every unrelated job column so a sweep never materializes large
// prompts/results merely to decide whether a log file is protected or expired.
type TranscriptJob struct {
	ID        string
	State     string
	UpdatedAt string
	CreatedAt string
}

type JobEvent struct {
	JobID   string
	Kind    string
	Message string
	// CreatedAt is the event's timestamp (SQLite UTC form). Populated by
	// ListJobEvents so the web dashboard can order a node's timeline by real
	// wall-clock time; other readers may leave it zero.
	CreatedAt string
}

// JobGate is one resumable gate row (#682): a single entry from a blocked job's
// gitmoot_result `needs` list, persisted so the blocker becomes actionable. A
// blocked stage records one gate per need; when every gate for the job is
// satisfied the blocked stage auto-re-runs via RetryJob. Satisfied is 0 (open)
// until an operator clears it; SatisfiedAt is the clear timestamp (empty while
// open).
type JobGate struct {
	ID          int64
	JobID       string
	Need        string
	Satisfied   bool
	CreatedAt   string
	SatisfiedAt string
}

const ()

type BranchLock struct {
	RepoFullName           string
	Branch                 string
	Owner                  string
	SkipNativeReviewFanout bool
	// ActingOrgRole is the org role attributed to the agent that took this branch
	// (#1250). Written ONCE at lock creation and read by both PR-open triggers, so
	// native review fanout children inherit an attribution instead of being
	// enqueued unattributed — an unattributed job's blocked event has no owner to
	// wake (#1347). Empty means unattributed, which is both the legacy value for
	// pre-migration locks and the correct value for an undirected dispatch.
	ActingOrgRole string
}

type BranchLockEvent struct {
	RepoFullName string
	Branch       string
	Owner        string
	Kind         string
	Message      string
}

type ResourceLock struct {
	ResourceKey   string
	OwnerJobID    string
	OwnerToken    string
	OwnerPID      int64
	OwnerHostname string
	// OwnerBootID is the acquiring host's kernel boot identifier (db.BootID) at
	// acquire time (#651). "" means no boot identity was recorded (a non-Linux
	// host, or a lock acquired before this column existed / by a non-pid-stamping
	// acquire); such locks fall back to the existing lease/TTL behavior. A recorded
	// boot id that differs from the current one proves the owning process died with
	// its boot, so the lock is reclaimable regardless of an unexpired lease. It is
	// deliberately kept OUT of the shared 9-column lock SELECTs (targeted reads
	// only) so their scan arity is unchanged.
	OwnerBootID string
	CommandHash string
	AcquiredAt  string
	UpdatedAt   string
	ExpiresAt   string
}

type MergeGate struct {
	RepoFullName string
	PullRequest  int64
	State        string
	Reason       string
	// BlockClass persists the merge gate's own transient-vs-quality classification
	// of a blocked row (#1562). Only the block path sets it; a pending or merged
	// row is not a block and correctly stores the zero value. It is durable
	// precisely so an exit path outside the blocking call stack can read it.
	BlockClass int
}

// PullRequestTerminalReconciliation pins one canonical terminal-effects owner
// to an exact pull request head across daemon restarts.
type PullRequestTerminalReconciliation struct {
	RepoFullName     string
	PullRequest      int64
	HeadSHA          string
	OwnerTaskID      string
	EffectsCompleted bool
}

// MergeGateStatusObservation records the exact PR head most recently reconciled
// with the visible gitmoot/merge-gate context. Kind distinguishes a generic
// marker, an observed policy verdict, and an inactive head that needs no marker.
type MergeGateStatusObservation struct {
	RepoFullName string
	PullRequest  int64
	HeadSHA      string
	Kind         string
}

// NoCIObservation records the first merge-gate evaluation that saw zero external
// CI (no external commit-statuses AND no check-runs) at a given head SHA (#596).
// The merge gate uses it to defer concluding "no CI" until a second consecutive
// zero-external observation at the SAME head, at least a grace window later, so a
// fresh head cannot merge before GitHub Actions has created its check run. A new
// head resets the observation.
type NoCIObservation struct {
	RepoFullName string
	PullRequest  int64
	HeadSHA      string
	// FirstZeroAt is the RFC3339Nano timestamp of the first zero-external
	// observation at HeadSHA.
	FirstZeroAt string
}

// HeartbeatState is the persisted state of one named agent heartbeat schedule
// (#533): when it last ran, when it is next due, and the id/status of its most
// recent job. The next_due + last_status are what make a heartbeat restart-safe
// (a restart re-reads next_due rather than firing immediately).
type HeartbeatState struct {
	Agent      string
	Name       string
	LastRunAt  time.Time
	NextDueAt  time.Time
	LastJobID  string
	LastStatus string
}

// BranchLockInfo is a branch lock plus its acquisition timestamps, surfaced by
// ListBranchLocksWithAge for observability (#617): a stranded lock is only obvious
// when its owner AND age are visible, so `gitmoot lock list` can show how long each
// lock has been held and by whom — turning a silent stale-lock mystery into a
// diagnosable one. CreatedAt/UpdatedAt are parsed best-effort; an unparseable stored
// timestamp yields a zero time rather than an error.
type BranchLockInfo struct {
	BranchLock
	CreatedAt time.Time
	UpdatedAt time.Time
}
