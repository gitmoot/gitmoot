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
	ID          int64  `json:"id"`
	WaiterRole  string `json:"waiter_role"`
	SubjectKind string `json:"subject_kind"`
	SubjectKey  string `json:"subject_key"`
	State       string `json:"state"`
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
func (s *Store) SubscribeAwaitedFact(ctx context.Context, request AwaitedFactSubscription) (AwaitedFact, error) {
	request, err := normalizeAwaitedFactSubscription(request)
	if err != nil {
		return AwaitedFact{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AwaitedFact{}, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
INSERT INTO awaited_facts(waiter_role, subject_kind, subject_key, deadline)
VALUES (?, ?, ?, ?)`, request.WaiterRole, request.SubjectKind, request.SubjectKey, request.Deadline.Format(time.RFC3339Nano))
	if err != nil {
		return AwaitedFact{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AwaitedFact{}, err
	}
	if detail, ok, err := canonicalAwaitedFactTx(ctx, tx, request.SubjectKind, request.SubjectKey); err != nil {
		return AwaitedFact{}, err
	} else if ok {
		if _, err := satisfyAwaitedFactTx(ctx, tx, id, request.WaiterRole, request.SubjectKind, request.SubjectKey, detail, time.Now().UTC()); err != nil {
			return AwaitedFact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AwaitedFact{}, err
	}
	return s.GetAwaitedFact(ctx, id)
}

func canonicalAwaitedFactTx(ctx context.Context, tx *sql.Tx, kind, key string) (string, bool, error) {
	switch kind {
	case AwaitedFactSubjectReviewVerdict:
		repo, pullRequest, headSHA, err := parseReviewVerdictSubjectKey(key)
		if err != nil {
			return "", false, err
		}
		rows, err := tx.QueryContext(ctx, `
SELECT id, agent, payload
FROM jobs
WHERE type = 'review' AND state = 'succeeded' AND lower(repo) = ? AND pull_request = ?
ORDER BY updated_at DESC, id DESC`, repo, pullRequest)
		if err != nil {
			return "", false, err
		}
		defer rows.Close()
		for rows.Next() {
			var jobID, agent, payload string
			if err := rows.Scan(&jobID, &agent, &payload); err != nil {
				return "", false, err
			}
			fact, ok := reviewVerdictFact(jobID, agent, "succeeded", payload)
			if ok && fact.headSHA == headSHA {
				return fact.detail, true, nil
			}
		}
		return "", false, rows.Err()
	default:
		return "", false, fmt.Errorf("unsupported awaited fact subject kind %q", kind)
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
	Result           *struct {
		Decision string `json:"decision"`
		Severity string `json:"severity,omitempty"`
	} `json:"result"`
}

// SucceededReviewVerdict is the minimal immutable evidence needed to decide
// whether a review dispatch would repeat a stable verdict at an unchanged head.
// It deliberately carries no result body: callers may use the prior verdict to
// refuse and escalate, but never to serve a cached review result.
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
SELECT id, agent, payload
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
		if err := rows.Scan(&jobID, &agent, &payload); err != nil {
			return nil, err
		}
		var decoded reviewVerdictPayload
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.Result == nil {
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

func reviewVerdictFact(jobID, agent, state, payload string) (reviewVerdictObservation, bool) {
	if state != "succeeded" {
		return reviewVerdictObservation{}, false
	}
	var decoded reviewVerdictPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.Result == nil {
		return reviewVerdictObservation{}, false
	}
	decoded.Repo = strings.ToLower(strings.TrimSpace(decoded.Repo))
	decoded.HeadSHA = strings.ToLower(strings.TrimSpace(decoded.HeadSHA))
	decision := strings.TrimSpace(decoded.Result.Decision)
	if decoded.Repo == "" || decoded.PullRequest <= 0 || decoded.HeadSHA == "" || decision == "" {
		return reviewVerdictObservation{}, false
	}
	detail := fmt.Sprintf("review verdict %s from %s job %s at head %s", decision, strings.TrimSpace(agent), strings.TrimSpace(jobID), decoded.HeadSHA)
	return reviewVerdictObservation{repo: decoded.Repo, pullRequest: decoded.PullRequest, headSHA: decoded.HeadSHA, detail: detail}, true
}

func resolveAwaitedReviewFactTx(ctx context.Context, tx *sql.Tx, jobID, agent, jobType, state, payload string, now time.Time) error {
	if strings.TrimSpace(jobType) != "review" {
		return nil
	}
	fact, ok := reviewVerdictFact(jobID, agent, state, payload)
	if !ok {
		return nil
	}
	key, err := ReviewVerdictSubjectKey(fact.repo, fact.pullRequest, fact.headSHA)
	if err != nil {
		return err
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
	for _, target := range targets {
		if _, err := satisfyAwaitedFactTx(ctx, tx, target.id, target.role, AwaitedFactSubjectReviewVerdict, key, fact.detail, now); err != nil {
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
	return true, insertAwaitedFactWakeTx(ctx, tx, id, waiterRole, waiterRole, subjectKind, subjectKey, AwaitedFactStateSatisfied)
}

func insertAwaitedFactWakeTx(ctx context.Context, tx *sql.Tx, id int64, waiterRole, targetRole, subjectKind, subjectKey, state string) error {
	payload, err := json.Marshal(AwaitedFactWakePayload{ID: id, WaiterRole: waiterRole, SubjectKind: subjectKind, SubjectKey: subjectKey, State: state})
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
	if err := insertAwaitedFactWakeTx(ctx, tx, id, fact.WaiterRole, escalationRole, fact.SubjectKind, fact.SubjectKey, AwaitedFactStateExpired); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
