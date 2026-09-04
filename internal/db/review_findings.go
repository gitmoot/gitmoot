package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The #1822 findings ledger. One row per (finding, head) OBSERVATION, never one
// row per finding, so a finding is current only because some round observed it
// at the current head. There is deliberately no column meaning "still true".
//
// IDENTITY IS STORE-OWNED, and that is the whole design (#1822 review 116538).
// Reviewers number findings PER ROUND starting at 1: measured on #1783, F-1
// names four different defects across four rounds. So a reviewer-supplied key
// cannot be identity, because a later round reuses it innocently and an
// obligation keyed on it is discharged by a coincidence of naming. FindingUID is
// minted here; RoundLabel keeps the reviewer's own "F-1" for humans and is NEVER
// used for matching by any consumer.
//
// MINT-BY-DEFAULT puts the fail-safe on the default path: an observation with no
// ContinuesUID mints a NEW finding, so a renumbered F-1 creates a new finding
// and the prior one stays UNOBSERVED. Continuing a prior finding requires citing
// its ContinuesUID, a value obtainable only by reading the ledger. Citing a uid
// is an act of reference; typing F-1 is an act of naming.

// FindingState is the observed state of a finding AT ONE HEAD.
type FindingState string

const (
	FindingOpen       FindingState = "open"
	FindingAnswered   FindingState = "answered"
	FindingWithdrawn  FindingState = "withdrawn"
	FindingSuperseded FindingState = "superseded"
)

// EvidenceKind separates what a reviewer EXECUTED from what it merely REPEATED.
// QUOTED exists because a verdict on #1824 was parsed as reporting 15 mutants
// when its own text said no baseline was established and no mutants ran: it was
// quoting the implementer's unverified claim. A persisted wrong number is worse
// than a transient one, because every later round reads it as established fact.
// QUOTED therefore establishes NOTHING and cannot discharge an obligation.
type EvidenceKind string

const (
	EvidenceExecuted EvidenceKind = "EXECUTED"
	EvidenceStatic   EvidenceKind = "STATIC"
	EvidenceQuoted   EvidenceKind = "QUOTED"
)

// ReviewFindingObservation is one reviewer's observation of one finding at one
// exact head.
type ReviewFindingObservation struct {
	FindingUID   string
	ContinuesUID string
	Repo         string
	PullRequest  int64
	HeadSHA      string
	ObservedAt   string
	ObserverJob  string
	State        FindingState
	Severity     string
	RoundLabel   string
	LabelAbsent  bool
	Title        string
	Detail       string
	File         string
	Line         int64
	// RelevanceKeys widen staleness beyond the named file. A defect's blast
	// radius is not a file: on #1783 a finding in a run.go comment was answered
	// by a change that landed partly in AGENTS.md. Keys are path prefixes, Go
	// package paths, symbols or doc anchors, and they are REVIEWER-SUPPLIED, so
	// the intersection with a diff is mechanical while the key set is judgement.
	RelevanceKeys    []string
	EvidenceKind     EvidenceKind
	ExecutedCommands []string
	ExecutedCount    int64
	EvidenceLocator  string
	Rationale        string
	SourceJob        string
	WithdrawReason   string
}

var (
	// ErrFindingHeadSHA rejects anything that is not a full 40-hex head. A
	// verdict not bound to an exact head is not a verdict about the code, and an
	// abbreviation is worse than a refusal because readers treat length as proof.
	ErrFindingHeadSHA = errors.New("finding observation requires a 40-character hex head sha")
	// ErrFindingEvidence rejects an EXECUTED claim with nothing executed. This is
	// the direct analogue of the verdict that claimed 15 mutants and ran zero.
	ErrFindingEvidence = errors.New("EXECUTED evidence requires a non-empty command list and a non-zero count")
	// ErrFindingDischarge rejects a discharge that carries no evidence. Without
	// it a round facing 27 obligations could submit 27 evidence-free observations
	// and be accepted, which is delta sign-off arriving as cheap discharge.
	ErrFindingDischarge = errors.New("a discharging observation requires EXECUTED evidence, or STATIC evidence with a locator and a rationale")
	// ErrFindingQuotedDischarge follows from QUOTED establishing nothing.
	ErrFindingQuotedDischarge = errors.New("QUOTED evidence cannot discharge an obligation: it establishes nothing")
	// ErrFindingUnknownContinues refuses to silently mint when a reviewer cites a
	// uid that does not exist, because minting there would turn a typo into a new
	// finding and leave the intended one unobserved.
	ErrFindingUnknownContinues = errors.New("continues_uid does not name an existing finding")
)

var (
	headSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	locatorPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+(:[0-9]+)?$`)
)

// RecordReviewFindingObservation validates at the STORE BOUNDARY and inserts.
// Every rejection here is a refusal, never a warning: a warning on a write path
// is indistinguishable from success to the caller that ignores it.
func (s *Store) RecordReviewFindingObservation(ctx context.Context, obs ReviewFindingObservation) (string, error) {
	head := strings.TrimSpace(obs.HeadSHA)
	if !headSHAPattern.MatchString(head) {
		return "", fmt.Errorf("%w: got %q", ErrFindingHeadSHA, obs.HeadSHA)
	}
	repo := strings.TrimSpace(obs.Repo)
	if repo == "" {
		return "", errors.New("finding observation requires a repo")
	}
	switch obs.State {
	case FindingOpen, FindingAnswered, FindingWithdrawn, FindingSuperseded:
	default:
		return "", fmt.Errorf("unknown finding state %q", obs.State)
	}
	switch obs.EvidenceKind {
	case EvidenceExecuted:
		if len(obs.ExecutedCommands) == 0 || obs.ExecutedCount <= 0 {
			return "", fmt.Errorf("%w: commands=%d count=%d", ErrFindingEvidence, len(obs.ExecutedCommands), obs.ExecutedCount)
		}
	case EvidenceStatic:
		locator := strings.TrimSpace(obs.EvidenceLocator)
		if locator == "" || !locatorPattern.MatchString(locator) || strings.TrimSpace(obs.Rationale) == "" {
			return "", fmt.Errorf("%w: locator=%q rationale-empty=%t", ErrFindingDischarge, obs.EvidenceLocator, strings.TrimSpace(obs.Rationale) == "")
		}
	case EvidenceQuoted:
		// Recordable for context, and it discharges nothing. A QUOTED row must
		// name whose claim it repeats so a reader can go and check the source.
		if strings.TrimSpace(obs.SourceJob) == "" {
			return "", errors.New("QUOTED evidence requires source_job naming whose claim it repeats")
		}
	default:
		return "", fmt.Errorf("unknown evidence kind %q", obs.EvidenceKind)
	}
	if obs.EvidenceKind == EvidenceQuoted && obs.State != FindingOpen {
		return "", ErrFindingQuotedDischarge
	}

	uid := strings.TrimSpace(obs.ContinuesUID)
	if uid != "" {
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM review_finding_observations WHERE finding_uid = ?`, uid).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return "", fmt.Errorf("%w: %q", ErrFindingUnknownContinues, uid)
		}
	} else {
		// MINT. Never derived from the reviewer's label, so a renumbered F-1
		// cannot collide with a prior finding.
		var seq int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT finding_uid) FROM review_finding_observations WHERE repo = ? AND pull_request = ?`,
			repo, obs.PullRequest).Scan(&seq); err != nil {
			return "", err
		}
		uid = fmt.Sprintf("%s#%d-f%d", repo, obs.PullRequest, seq+1)
	}

	keys, err := json.Marshal(normaliseKeys(obs.RelevanceKeys, obs.File))
	if err != nil {
		return "", err
	}
	cmds, err := json.Marshal(obs.ExecutedCommands)
	if err != nil {
		return "", err
	}
	labelAbsent := 0
	if strings.TrimSpace(obs.RoundLabel) == "" {
		labelAbsent = 1
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO review_finding_observations(
	finding_uid, repo, pull_request, head_sha, observed_at, observer_job, state,
	severity, round_label, label_absent, title, detail, file, line,
	relevance_keys, evidence_kind, executed_commands, executed_count,
	evidence_locator, rationale, source_job, withdraw_reason)
VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now')), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, repo, obs.PullRequest, head, strings.TrimSpace(obs.ObservedAt), obs.ObserverJob, string(obs.State),
		obs.Severity, obs.RoundLabel, labelAbsent, obs.Title, obs.Detail, obs.File, obs.Line,
		string(keys), string(obs.EvidenceKind), string(cmds), obs.ExecutedCount,
		obs.EvidenceLocator, obs.Rationale, obs.SourceJob, obs.WithdrawReason); err != nil {
		return "", err
	}
	return uid, nil
}

// normaliseKeys seeds relevance from the finding's own file so the set is never
// empty by accident, while leaving the reviewer free to widen it. The seed is a
// floor, not the boundary: see the RelevanceKeys comment.
func normaliseKeys(keys []string, file string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, key := range append(append([]string{}, keys...), strings.TrimSpace(file)) {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// ListReviewFindingObservations returns every observation for a PR, oldest
// first, so a caller can fold them into per-finding latest state itself. The
// store does not fold, because folding is where a "still true" column would
// creep back in.
func (s *Store) ListReviewFindingObservations(ctx context.Context, repo string, pullRequest int64) ([]ReviewFindingObservation, error) {
	return queryList(ctx, s.db, `SELECT finding_uid, repo, pull_request, head_sha, observed_at,
	observer_job, state, severity, round_label, label_absent, title, detail, file, line,
	relevance_keys, evidence_kind, executed_commands, executed_count, evidence_locator,
	rationale, source_job, withdraw_reason
FROM review_finding_observations
WHERE repo = ? AND pull_request = ?
ORDER BY observed_at, rowid`, []any{strings.TrimSpace(repo), pullRequest},
		func(row rowScanner) (ReviewFindingObservation, error) {
			var obs ReviewFindingObservation
			var state, kind, keys, cmds string
			var labelAbsent int
			if err := row.Scan(&obs.FindingUID, &obs.Repo, &obs.PullRequest, &obs.HeadSHA, &obs.ObservedAt,
				&obs.ObserverJob, &state, &obs.Severity, &obs.RoundLabel, &labelAbsent, &obs.Title,
				&obs.Detail, &obs.File, &obs.Line, &keys, &kind, &cmds, &obs.ExecutedCount,
				&obs.EvidenceLocator, &obs.Rationale, &obs.SourceJob, &obs.WithdrawReason); err != nil {
				return ReviewFindingObservation{}, err
			}
			obs.State = FindingState(state)
			obs.EvidenceKind = EvidenceKind(kind)
			obs.LabelAbsent = labelAbsent == 1
			_ = json.Unmarshal([]byte(keys), &obs.RelevanceKeys)
			_ = json.Unmarshal([]byte(cmds), &obs.ExecutedCommands)
			return obs, nil
		})
}
