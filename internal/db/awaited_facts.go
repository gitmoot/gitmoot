package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

const (
	AwaitedFactSubjectReviewVerdict = "review_verdict"

	AwaitedFactStateWaiting   = "waiting"
	AwaitedFactStateSatisfied = "satisfied"
	AwaitedFactStateExpired   = "expired"
)

// AwaitedFact is one bounded durable interest. Terminal rows remain queryable;
// expiration is never represented by deletion or silent omission.
type AwaitedFact struct {
	ID               int64  `json:"id"`
	WaiterRole       string `json:"waiter_role"`
	SubjectKind      string `json:"subject_kind"`
	SubjectKey       string `json:"subject_key"`
	Deadline         string `json:"deadline"`
	State            string `json:"state"`
	ResolutionDetail string `json:"resolution_detail,omitempty"`
	SatisfiedAt      string `json:"satisfied_at,omitempty"`
	ExpiredAt        string `json:"expired_at,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type AwaitedFactSubscription struct {
	WaiterRole  string
	SubjectKind string
	SubjectKey  string
	Deadline    time.Time
}

// AwaitedFactWakePayload is the durable outbox representation. SubjectKey is
// always produced by the same constructor used by subscriptions and review
// producers; delivery never restates it from separate fields.
type AwaitedFactWakePayload struct {
	ID                  int64  `json:"id"`
	WaiterRole          string `json:"waiter_role"`
	SubjectKind         string `json:"subject_kind"`
	SubjectKey          string `json:"subject_key"`
	State               string `json:"state"`
	JobID               string `json:"job_id,omitempty"`
	LifecycleGeneration int64  `json:"lifecycle_generation,omitempty"`
	Detail              string `json:"detail,omitempty"`
}

// ReviewVerdictSubjectKey is the single source of truth for a review wait's
// repo/PR/head identity. The exact head is part of the key, so an old verdict
// cannot satisfy a newer-head subscription.
func ReviewVerdictSubjectKey(repo string, pullRequest int, headSHA string) (string, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	if parts := strings.Split(repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, "#@ \t\r\n") {
		return "", fmt.Errorf("review verdict repo must be owner/repo")
	}
	if pullRequest <= 0 {
		return "", fmt.Errorf("review verdict pull request must be positive")
	}
	if headSHA == "" || strings.ContainsAny(headSHA, "#@ \t\r\n") {
		return "", fmt.Errorf("review verdict head SHA is required")
	}
	return fmt.Sprintf("%s#%d@%s", repo, pullRequest, headSHA), nil
}

func parseReviewVerdictSubjectKey(key string) (repo string, pullRequest int, headSHA string, err error) {
	key = strings.TrimSpace(key)
	at := strings.LastIndexByte(key, '@')
	if at <= 0 {
		return "", 0, "", fmt.Errorf("invalid review verdict subject key %q", key)
	}
	hash := strings.LastIndexByte(key[:at], '#')
	if hash <= 0 || hash >= at-1 {
		return "", 0, "", fmt.Errorf("invalid review verdict subject key %q", key)
	}
	pullRequest, err = strconv.Atoi(key[hash+1 : at])
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid review verdict subject key %q", key)
	}
	repo = key[:hash]
	headSHA = key[at+1:]
	want, err := ReviewVerdictSubjectKey(repo, pullRequest, headSHA)
	if err != nil || want != key {
		return "", 0, "", fmt.Errorf("invalid review verdict subject key %q", key)
	}
	return repo, pullRequest, headSHA, nil
}

func normalizeAwaitedFactSubscription(request AwaitedFactSubscription) (AwaitedFactSubscription, error) {
	request.WaiterRole = strings.ToLower(strings.TrimSpace(request.WaiterRole))
	request.SubjectKind = strings.ToLower(strings.TrimSpace(request.SubjectKind))
	request.SubjectKey = strings.TrimSpace(request.SubjectKey)
	if request.WaiterRole == "" {
		return request, errors.New("awaited fact waiter role is required")
	}
	if request.Deadline.IsZero() {
		return request, errors.New("awaited fact deadline is required")
	}
	request.Deadline = request.Deadline.UTC()
	if !request.Deadline.After(time.Now().UTC()) {
		return request, errors.New("awaited fact deadline must be in the future")
	}
	switch request.SubjectKind {
	case AwaitedFactSubjectReviewVerdict:
		if _, _, _, err := parseReviewVerdictSubjectKey(request.SubjectKey); err != nil {
			return request, err
		}
	default:
		return request, fmt.Errorf("unsupported awaited fact subject kind %q", request.SubjectKind)
	}
	return request, nil
}

// SubscribeAwaitedFact atomically inserts the interest and rechecks canonical
// state before commit. SQLite serializes a concurrent producer against this
// write transaction: either its fact is visible to the recheck, or its commit
// runs afterward and resolves the newly committed waiting row.
// SubscribeAwaitedFact also returns the headless review rows the resolver passed
// over, so the caller - which can reach workflow, as this package cannot - can
// state each exclusion instead of leaving the row silently invisible (#2008).
func (s *Store) SubscribeAwaitedFact(ctx context.Context, request AwaitedFactSubscription) (AwaitedFact, []HeadlessReviewSkip, error) {
	request, err := normalizeAwaitedFactSubscription(request)
	if err != nil {
		return AwaitedFact{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AwaitedFact{}, nil, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
INSERT INTO awaited_facts(waiter_role, subject_kind, subject_key, deadline)
VALUES (?, ?, ?, ?)`, request.WaiterRole, request.SubjectKind, request.SubjectKey, request.Deadline.Format(time.RFC3339Nano))
	if err != nil {
		return AwaitedFact{}, nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AwaitedFact{}, nil, err
	}
	detail, ok, skipped, err := canonicalAwaitedFactTx(ctx, tx, request.SubjectKind, request.SubjectKey, s.blockingSeverityFor)
	if err != nil {
		return AwaitedFact{}, nil, err
	}
	if ok {
		if _, err := satisfyAwaitedFactTx(ctx, tx, id, request.WaiterRole, request.SubjectKind, request.SubjectKey, detail, time.Now().UTC()); err != nil {
			return AwaitedFact{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AwaitedFact{}, nil, err
	}
	fact, err := s.GetAwaitedFact(ctx, id)
	return fact, skipped, err
}

// HeadlessReviewSkip is a succeeded review row a head-keyed resolver passed over
// because it records no head. It is carried RAW - the id and the flag, nothing
// interpreted - because the rule that turns those into a stated reason lives in
// workflow, and workflow depends on db and never the reverse (#2008). A copy of
// that vocabulary here would be the second definition this campaign exists to
// remove.
type HeadlessReviewSkip struct {
	JobID            string
	ExternallyDriven bool
}

// canonicalAwaitedFactTx also reports the headless review rows it passed over,
// so the caller can say it excluded them. It does NOT report rows at a different
// head: those carry an engine-observed head and are simply not this one.
func canonicalAwaitedFactTx(ctx context.Context, tx *sql.Tx, kind, key string, blockingSeverity func(repo string) string) (string, bool, []HeadlessReviewSkip, error) {
	switch kind {
	case AwaitedFactSubjectReviewVerdict:
		repo, pullRequest, headSHA, err := parseReviewVerdictSubjectKey(key)
		if err != nil {
			return "", false, nil, err
		}
		rows, err := tx.QueryContext(ctx, `
SELECT id, agent, payload, externally_driven
FROM jobs
WHERE type = 'review' AND state = 'succeeded' AND lower(repo) = ? AND pull_request = ?
ORDER BY updated_at DESC, id DESC`, repo, pullRequest)
		if err != nil {
			return "", false, nil, err
		}
		defer rows.Close()
		// The scan is keyed to ONE repo, but reviewVerdictFact consults the resolver
		// per row and the resolver re-reads config from disk on every call (it must,
		// to track a live edit). Left unmemoised this is O(history) filesystem reads
		// while holding the transaction. Resolve once here: one fresh read per
		// transaction, which is the same freshness the single-row producer path gets.
		scanSeverity := blockingSeverity(repo)
		memoized := func(string) string { return scanSeverity }
		var skipped []HeadlessReviewSkip
		for rows.Next() {
			var jobID, agent, payload string
			var externallyDriven bool
			if err := rows.Scan(&jobID, &agent, &payload, &externallyDriven); err != nil {
				return "", false, nil, err
			}
			// The headless case is captured BEFORE reviewVerdictFact, because that
			// helper folds an empty head in with three unrelated rejections and
			// returns a bare false, so a row skipped for want of a head is
			// indistinguishable there from malformed junk. It is deliberately not
			// loosened: its job is to decide whether a row IS a verdict.
			//
			// This uses the package's own decode and isFanOut rather than
			// re-stating their conditions, so it adds no second copy of that rule.
			var decoded reviewVerdictPayload
			if err := json.Unmarshal([]byte(payload), &decoded); err == nil &&
				decoded.Result != nil && !decoded.isFanOut() &&
				decoded.Repo != "" && decoded.PullRequest > 0 &&
				strings.TrimSpace(decoded.HeadSHA) == "" {
				skipped = append(skipped, HeadlessReviewSkip{JobID: jobID, ExternallyDriven: externallyDriven})
			}
			fact, ok := reviewVerdictFact(jobID, agent, "succeeded", payload, memoized)
			if ok && fact.headSHA == headSHA {
				return fact.detail, true, skipped, nil
			}
		}
		return "", false, skipped, rows.Err()
	default:
		return "", false, nil, fmt.Errorf("unsupported awaited fact subject kind %q", kind)
	}
}

type reviewVerdictPayload struct {
	Repo        string `json:"repo"`
	PullRequest int    `json:"pull_request"`
	HeadSHA     string `json:"head_sha"`
	// EffectiveRuntime is the runtime the review job actually ran on (#1528),
	// persisted by dispatch for every job. Empty for jobs predating #1528; the
	// review-loop family resolver then falls back to the agent registry default.
	EffectiveRuntime string `json:"effective_runtime"`
	// Sender identifies a pipeline-dispatched review, whose verdict is
	// report-only and is therefore never re-interpreted against repository
	// severity policy.
	Sender string `json:"sender"`
	// ActingOrgRole is read so a session review authored by a role rather than an
	// agent survives to the caller, which resolves the two into one identity
	// (#2008). Folding stays in workflow.ReviewerIdentity, not here.
	ActingOrgRole string `json:"acting_org_role"`
	Result        *struct {
		Decision string `json:"decision"`
		Severity string `json:"severity,omitempty"`
		// Delegations and FanOut are the canonical fan-out classification, decoded
		// here for the same reason every other consumer reads it: a coordinator
		// announcement carries decision "approved" and is not a verdict (#1685).
		// Both are needed — the executable delegations do not survive the pipeline
		// mailbox seam, where normalization records FanOut instead.
		Delegations []json.RawMessage `json:"delegations"`
		FanOut      bool              `json:"fan_out,omitempty"`
	} `json:"result"`
}

// isFanOut reports whether a decoded review result is a coordinator announcement
// rather than a verdict. It mirrors workflow.ResultIsFanOut, which cannot be
// imported here because workflow depends on db and never the reverse — the same
// reason pipelineReviewSender is duplicated above.
func (p reviewVerdictPayload) isFanOut() bool {
	if p.Result == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.Result.Decision)) {
	case "approved", "changes_requested":
	default:
		return false
	}
	return len(p.Result.Delegations) > 0 || p.Result.FanOut
}

type SucceededReviewVerdict struct {
	JobID    string
	Agent    string
	HeadSHA  string
	Decision string
	Severity string
	// EffectiveRuntime is the runtime the review job ran on, when the dispatch
	// recorded it (#1528). Empty for jobs that predate that recording; callers
	// resolving a runtime family fall back to the agent registry default.
	EffectiveRuntime string
	// ActingOrgRole is the role an externally driven session review recorded IN
	// PLACE OF an agent. It is carried RAW and deliberately not folded here:
	// workflow depends on db and never the reverse, so the Agent-first rule lives
	// in workflow.ReviewerIdentity and this layer must not keep a second copy of
	// it (#2008). A caller reading Agent alone silently drops such a row.
	ActingOrgRole string
	// ExternallyDriven is carried RAW for the same reason and with a sharper one:
	// HEADLESS DOES NOT IMPLY SESSION. Measured on this box, 211 review rows have
	// no head - 41 externally driven, 156 failed, 10 cancelled, and 4 ephemeral
	// ask-delegation children (#1962, #1895). A consumer that reports every
	// headless row as a self-rooted session row would commit an over-attribution
	// while trying to improve honesty, so the distinction must reach the caller
	// that phrases the exclusion (#2008).
	ExternallyDriven bool
}

// SucceededReviewVerdicts returns stable approved or changes-requested review
// verdicts for repo/PR, newest first. Head SHA and decision live only in the JSON
// payload, so they are decoded and filtered in Go rather than pretending jobs
// has indexed columns for them. This is a pure read: unlike SubscribeAwaitedFact
// it creates no durable interest or other state. Skipped reviews are abstentions,
// not verdicts, and must remain retryable at the same head.
func (s *Store) SucceededReviewVerdicts(ctx context.Context, repo string, pullRequest int) ([]SucceededReviewVerdict, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return nil, errors.New("review verdict repo is required")
	}
	if pullRequest <= 0 {
		return nil, errors.New("review verdict pull request must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, agent, payload, externally_driven
FROM jobs
WHERE type = 'review' AND state = 'succeeded' AND lower(repo) = ? AND pull_request = ?
ORDER BY updated_at DESC, id DESC`, repo, pullRequest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	verdicts := make([]SucceededReviewVerdict, 0)
	for rows.Next() {
		var jobID, agent, payload string
		var externallyDriven bool
		if err := rows.Scan(&jobID, &agent, &payload, &externallyDriven); err != nil {
			return nil, err
		}
		var decoded reviewVerdictPayload
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.Result == nil {
			continue
		}
		if decoded.isFanOut() {
			// A coordinator announcement is not a verdict, so it must not enter
			// same-head verdict history: doing so let a fan-out suppress the
			// legitimate retry that would have produced a real one (#1685).
			continue
		}
		decision := strings.ToLower(strings.TrimSpace(decoded.Result.Decision))
		if decision != "approved" && decision != "changes_requested" {
			continue
		}
		verdicts = append(verdicts, SucceededReviewVerdict{
			JobID:            strings.TrimSpace(jobID),
			Agent:            strings.TrimSpace(agent),
			HeadSHA:          strings.ToLower(strings.TrimSpace(decoded.HeadSHA)),
			Decision:         decision,
			Severity:         strings.ToUpper(strings.TrimSpace(decoded.Result.Severity)),
			EffectiveRuntime: strings.ToLower(strings.TrimSpace(decoded.EffectiveRuntime)),
			ActingOrgRole:    strings.TrimSpace(decoded.ActingOrgRole),
			ExternallyDriven: externallyDriven,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return verdicts, nil
}

type reviewVerdictObservation struct {
	repo, headSHA, detail string
	pullRequest           int
}

// pipelineReviewSender mirrors workflow.PipelineJobSender. It is duplicated
// rather than imported because workflow depends on db, never the reverse.
const pipelineReviewSender = "pipeline"

// reviewVerdictFact renders the awaited-fact observation for one review job.
// The detail reports the EFFECTIVE decision — the raw verdict folded against
// blockingSeverity exactly as the engine folds it — so a coordinator woken
// through this channel cannot dispatch a fix round the engine suppressed.
// Pipeline-sender reviews are report-only and keep their raw decision.
func reviewVerdictFact(jobID, agent, state, payload string, blockingSeverity func(repo string) string) (reviewVerdictObservation, bool) {
	if state != "succeeded" {
		return reviewVerdictObservation{}, false
	}
	var decoded reviewVerdictPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.Result == nil {
		return reviewVerdictObservation{}, false
	}
	if decoded.isFanOut() {
		// Waking a waiter with "review verdict approved" because a coordinator
		// announced a panel reports an answer nobody gave (#1685). The panel's own
		// synthesized leaf verdict satisfies the fact when it lands.
		return reviewVerdictObservation{}, false
	}
	decoded.Repo = strings.ToLower(strings.TrimSpace(decoded.Repo))
	decoded.HeadSHA = strings.ToLower(strings.TrimSpace(decoded.HeadSHA))
	decision := strings.TrimSpace(decoded.Result.Decision)
	if decoded.Repo == "" || decoded.PullRequest <= 0 || decoded.HeadSHA == "" || decision == "" {
		return reviewVerdictObservation{}, false
	}
	if decision == "changes_requested" &&
		!strings.EqualFold(strings.TrimSpace(decoded.Sender), pipelineReviewSender) &&
		!reviewseverity.Blocks(strings.ToUpper(strings.TrimSpace(decoded.Result.Severity)), blockingSeverity(decoded.Repo)) {
		decision = "approved"
	}
	detail := fmt.Sprintf("review verdict %s from %s job %s at head %s", decision, strings.TrimSpace(agent), strings.TrimSpace(jobID), decoded.HeadSHA)
	return reviewVerdictObservation{repo: decoded.Repo, pullRequest: decoded.PullRequest, headSHA: decoded.HeadSHA, detail: detail}, true
}

func resolveStoredAwaitedReviewFactTx(ctx context.Context, tx *sql.Tx, jobID, state string, blockingSeverity func(repo string) string, now time.Time) error {
	switch state {
	case "succeeded", "failed", "blocked", "cancelled":
	default:
		return nil
	}
	var agent, jobType, payload string
	var lifecycleGeneration int64
	if err := tx.QueryRowContext(ctx, `
SELECT agent, type, payload, lifecycle_generation
FROM jobs
WHERE id = ?`, jobID).Scan(&agent, &jobType, &payload, &lifecycleGeneration); err != nil {
		return err
	}
	return resolveAwaitedReviewFactTx(
		ctx, tx, jobID, agent, jobType, state, payload, lifecycleGeneration,
		blockingSeverity, now,
	)
}

func resolveAwaitedReviewFactTx(ctx context.Context, tx *sql.Tx, jobID, agent, jobType, state, payload string, lifecycleGeneration int64, blockingSeverity func(repo string) string, now time.Time) error {
	if strings.TrimSpace(jobType) != "review" {
		return nil
	}
	var fact reviewVerdictObservation
	noticeState := ""
	var key string
	switch state {
	case "succeeded":
		var ok bool
		fact, ok = reviewVerdictFact(jobID, agent, state, payload, blockingSeverity)
		if !ok {
			return nil
		}
		var err error
		key, err = ReviewVerdictSubjectKey(fact.repo, fact.pullRequest, fact.headSHA)
		if err != nil {
			return nil
		}
	case "failed", "blocked", "cancelled":
		var decoded reviewVerdictPayload
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return nil
		}
		var err error
		key, err = ReviewVerdictSubjectKey(decoded.Repo, decoded.PullRequest, decoded.HeadSHA)
		if err != nil {
			return nil
		}
		noticeState = "review_" + state
	default:
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id, waiter_role
FROM awaited_facts
WHERE state = 'waiting' AND subject_kind = ? AND subject_key = ?
ORDER BY id`, AwaitedFactSubjectReviewVerdict, key)
	if err != nil {
		return err
	}
	type target struct {
		id   int64
		role string
	}
	var targets []target
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.role); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}

	if state == "succeeded" {
		for _, target := range targets {
			if _, err := satisfyAwaitedFactTx(ctx, tx, target.id, target.role, AwaitedFactSubjectReviewVerdict, key, fact.detail, now); err != nil {
				return err
			}
		}
		return nil
	}

	detail := fmt.Sprintf(
		"review job %s %s without an exact-head verdict; the awaited fact remains unresolved",
		strings.TrimSpace(jobID), state,
	)
	for _, target := range targets {
		if err := insertAwaitedFactNoticeWakeTx(
			ctx, tx, target.id, target.role, target.role, AwaitedFactSubjectReviewVerdict, key,
			noticeState, jobID, lifecycleGeneration, detail,
		); err != nil {
			return err
		}
	}
	return nil
}

func satisfyAwaitedFactTx(ctx context.Context, tx *sql.Tx, id int64, waiterRole, subjectKind, subjectKey, detail string, now time.Time) (bool, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE awaited_facts
SET state = 'satisfied', resolution_detail = ?, satisfied_at = ?, updated_at = ?
WHERE id = ? AND state = 'waiting'`, detail, stamp, stamp, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if err := supersedePendingAwaitedFactWakesTx(ctx, tx, id, AwaitedFactStateSatisfied, now); err != nil {
		return false, err
	}
	return true, insertAwaitedFactWakeTx(ctx, tx, id, waiterRole, waiterRole, subjectKind, subjectKey, AwaitedFactStateSatisfied)
}

func supersedePendingAwaitedFactWakesTx(ctx context.Context, tx *sql.Tx, factID int64, terminalState string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, source_id
FROM wake_outbox
WHERE source_kind = ? AND state = ?`,
		WakeOutboxSourceAwaitedFact, WakeOutboxStatePending,
	)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var sourceID string
		if err := rows.Scan(&id, &sourceID); err != nil {
			rows.Close()
			return err
		}
		var payload AwaitedFactWakePayload
		if json.Unmarshal([]byte(sourceID), &payload) == nil && payload.ID == factID {
			stale = append(stale, id)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	reason := fmt.Sprintf("awaited fact %d reached %s before wake delivery", factID, terminalState)
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `
UPDATE wake_outbox
SET state = ?, last_error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND state = ?`,
			WakeOutboxStateSuperseded, reason, stamp, stamp, id, WakeOutboxStatePending,
		); err != nil {
			return err
		}
	}
	return nil
}

func insertAwaitedFactWakeTx(ctx context.Context, tx *sql.Tx, id int64, waiterRole, targetRole, subjectKind, subjectKey, state string) error {
	return insertAwaitedFactNoticeWakeTx(ctx, tx, id, waiterRole, targetRole, subjectKind, subjectKey, state, "", 0, "")
}

func insertAwaitedFactNoticeWakeTx(ctx context.Context, tx *sql.Tx, id int64, waiterRole, targetRole, subjectKind, subjectKey, state, jobID string, lifecycleGeneration int64, detail string) error {
	payload, err := json.Marshal(AwaitedFactWakePayload{
		ID: id, WaiterRole: waiterRole, SubjectKind: subjectKind, SubjectKey: subjectKey,
		State: state, JobID: strings.TrimSpace(jobID), LifecycleGeneration: lifecycleGeneration,
		Detail: strings.TrimSpace(detail),
	})
	if err != nil {
		return err
	}
	return insertWakeOutboxTx(ctx, tx, WakeOutboxSourceAwaitedFact, string(payload), WakeOutboxKindFact, targetRole)
}

func (s *Store) GetAwaitedFact(ctx context.Context, id int64) (AwaitedFact, error) {
	var fact AwaitedFact
	err := s.db.QueryRowContext(ctx, `
SELECT id, waiter_role, subject_kind, subject_key, deadline, state,
	resolution_detail, satisfied_at, expired_at, created_at, updated_at
FROM awaited_facts WHERE id = ?`, id).Scan(
		&fact.ID, &fact.WaiterRole, &fact.SubjectKind, &fact.SubjectKey, &fact.Deadline,
		&fact.State, &fact.ResolutionDetail, &fact.SatisfiedAt, &fact.ExpiredAt,
		&fact.CreatedAt, &fact.UpdatedAt,
	)
	return fact, err
}

func (s *Store) ListAwaitedFacts(ctx context.Context, waiterRole, state string) ([]AwaitedFact, error) {
	waiterRole = strings.ToLower(strings.TrimSpace(waiterRole))
	state = strings.ToLower(strings.TrimSpace(state))
	if state != "" && state != AwaitedFactStateWaiting && state != AwaitedFactStateSatisfied && state != AwaitedFactStateExpired {
		return nil, fmt.Errorf("invalid awaited fact state %q", state)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, waiter_role, subject_kind, subject_key, deadline, state,
	resolution_detail, satisfied_at, expired_at, created_at, updated_at
FROM awaited_facts
WHERE (? = '' OR waiter_role = ?) AND (? = '' OR state = ?)
ORDER BY created_at ASC, id ASC`, waiterRole, waiterRole, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []AwaitedFact
	for rows.Next() {
		var fact AwaitedFact
		if err := rows.Scan(&fact.ID, &fact.WaiterRole, &fact.SubjectKind, &fact.SubjectKey, &fact.Deadline, &fact.State, &fact.ResolutionDetail, &fact.SatisfiedAt, &fact.ExpiredAt, &fact.CreatedAt, &fact.UpdatedAt); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func (s *Store) ListDueAwaitedFacts(ctx context.Context, now time.Time) ([]AwaitedFact, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, waiter_role, subject_kind, subject_key, deadline, state,
	resolution_detail, satisfied_at, expired_at, created_at, updated_at
FROM awaited_facts
WHERE state = 'waiting' AND julianday(deadline) <= julianday(?)
ORDER BY deadline ASC, id ASC`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []AwaitedFact
	for rows.Next() {
		var fact AwaitedFact
		if err := rows.Scan(&fact.ID, &fact.WaiterRole, &fact.SubjectKind, &fact.SubjectKey, &fact.Deadline, &fact.State, &fact.ResolutionDetail, &fact.SatisfiedAt, &fact.ExpiredAt, &fact.CreatedAt, &fact.UpdatedAt); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

// ExpireAwaitedFact stamps a queryable terminal state and durably addresses
// the escalation target in one transaction. A concurrent producer wins or
// loses the same state CAS; it can never emit both satisfaction and expiry.
func (s *Store) ExpireAwaitedFact(ctx context.Context, id int64, escalationRole string, now time.Time) (bool, error) {
	escalationRole = strings.ToLower(strings.TrimSpace(escalationRole))
	if escalationRole == "" {
		return false, errors.New("awaited fact escalation role is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var fact AwaitedFact
	if err := tx.QueryRowContext(ctx, `SELECT waiter_role, subject_kind, subject_key FROM awaited_facts WHERE id = ?`, id).Scan(&fact.WaiterRole, &fact.SubjectKind, &fact.SubjectKey); err != nil {
		return false, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE awaited_facts
SET state = 'expired', resolution_detail = 'deadline expired', expired_at = ?, updated_at = ?
WHERE id = ? AND state = 'waiting'`, stamp, stamp, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if err := supersedePendingAwaitedFactWakesTx(ctx, tx, id, AwaitedFactStateExpired, now); err != nil {
		return false, err
	}
	if err := insertAwaitedFactWakeTx(ctx, tx, id, fact.WaiterRole, escalationRole, fact.SubjectKind, fact.SubjectKey, AwaitedFactStateExpired); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
