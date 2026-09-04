package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	// ErrFindingDuplicateObservation names a second observation by the SAME
	// observer of the same finding at the same head. Two DIFFERENT reviewers at
	// one head is legitimate and the key admits it (#1850 review F8); a repeat by
	// one observer is the caller's own idempotency question, so it gets a sentinel
	// rather than a raw driver string.
	ErrFindingDuplicateObservation = errors.New("this observer already recorded this finding at this head")
	// ErrFindingRelevanceKey rejects a relevance key the matcher could never match.
	ErrFindingRelevanceKey = errors.New("relevance keys must be path-like: a symbol key can never match a changed path")
	// ErrFindingWithdrawReason rejects a reasonless withdrawal.
	ErrFindingWithdrawReason = errors.New("a withdrawn finding requires a withdraw reason")
	// ErrFindingObservedAt rejects a malformed caller timestamp.
	ErrFindingObservedAt       = errors.New("observed_at must be RFC3339 when supplied")
	ErrFindingUnknownContinues = errors.New("continues_uid does not name an existing finding")
)

var (
	headSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	locatorPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+(:[0-9]+)?$`)
	// relevanceKeyPattern is the CHARACTER-level gate only. Path-likeness is
	// decided by pathLikeRelevanceKey below, because a regexp over this character
	// class cannot express it: my previous version WAS this pattern alone, and a
	// bare Go identifier is entirely inside [A-Za-z0-9_.-]+, so it matched and was
	// accepted. The comment claimed the opposite and its test passed only because
	// its fixture carried parentheses, dying on punctuation rather than on
	// symbol-ness (#1850 round 2 F6, the fourteenth mutant the reviewer built).
	relevanceKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*/?$`)
)

// pathLikeRelevanceKey reports whether a key can ever match a CHANGED PATH,
// which is the only thing relevanceTouched compares against.
//
// WHAT IT REFUSES AND WHY EACH ONE CAN NEVER MATCH:
//   - a bare identifier ("EnsureLedgerObligationsObserved"): no diff path is a
//     bare symbol, and nothing here resolves a symbol to its defining file.
//   - a Go package or URL path ("github.com/gitmoot/gitmoot/internal/db"): diff
//     paths are repo-relative, so a first segment that looks like a hostname
//     can never be the head of one.
//   - an absolute path or one containing "..": not repo-relative.
//
// A single-segment key with an extension ("AGENTS.md") is a FILE and is
// accepted. A multi-segment key without a dotted first segment
// ("internal/db") is a DIRECTORY and is accepted.
func pathLikeRelevanceKey(key string) bool {
	key = strings.TrimSpace(key)
	// A TRAILING SLASH IS AN EXPLICIT DIRECTORY and needs no extension: "docs/"
	// is a legitimate key and my first version rejected it, because trimming the
	// slash turned it into the bare identifier "docs". Measured before shipping.
	explicitDir := strings.HasSuffix(key, "/")
	key = strings.TrimSuffix(key, "/")
	if key == "" || strings.HasPrefix(key, "/") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	first, _, hasSlash := strings.Cut(key, "/")
	if !hasSlash {
		if explicitDir {
			return true
		}
		// One segment: it must name a file, so it must carry an extension.
		// Rejecting a bare identifier here is the whole point.
		dot := strings.LastIndex(first, ".")
		return dot > 0 && dot < len(first)-1
	}
	// Multi-segment: a dotted FIRST segment reads as a host or module path.
	return !strings.Contains(first, ".")
}

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
	case FindingOpen, FindingAnswered, FindingSuperseded:
	case FindingWithdrawn:
		// WITHDRAWAL REQUIRES ITS RECORDED REASON (#1850 second verdict B, and
		// directive 116564 required it before that). Design revision 116549 said
		// withdrawal is a reviewer's act only, WITH a recorded reason, and the
		// store never checked it - so the cheapest permanent exit from the
		// invariant was state=withdrawn with a one-character rationale. Withdrawal
		// is the one state relevance never re-arms, which makes it strictly
		// stronger than answered, so it is the one that most needs a readable
		// reason. I enforce the reason rather than making withdrawal re-armable:
		// a withdrawn finding is one a reviewer says was never a defect, and a
		// later diff cannot re-break something that was never broken.
		if strings.TrimSpace(obs.WithdrawReason) == "" {
			return "", ErrFindingWithdrawReason
		}
	default:
		return "", fmt.Errorf("unknown finding state %q", obs.State)
	}
	// SECOND VERDICT C: observed_at DECIDED the fold and any non-empty caller
	// string was accepted, so a round supplying "9999-..." pinned its own row as
	// latest forever and one supplying "1970-..." lost to an older row, silently
	// preserving a stale answered state. Caller-asserted metadata acting as an
	// authority is the class this repo already quarantines for head_sha. Two
	// changes close it: the fold now orders by rowid, which only the database
	// assigns, and a supplied stamp must parse as RFC3339 so the display value
	// cannot be junk either.
	if stamp := strings.TrimSpace(obs.ObservedAt); stamp != "" {
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			return "", fmt.Errorf("%w: got %q", ErrFindingObservedAt, obs.ObservedAt)
		}
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

	// KEYS ARE VALIDATED BEFORE THE TRANSACTION so a bad key costs no write lock.
	//
	// ASSERTED KEYS ARE REFUSED; DERIVED KEYS ARE SANITISED. That asymmetry is the
	// #1850 round 2 F5 fix and it is proportionality, not laxity: a key the
	// REVIEWER supplied that can never match is a mistake it needs told about,
	// but the File-derived seed is a convenience THIS STORE synthesised, and
	// rejecting a whole observation over the store's own derived key is how a P1
	// finding got dropped for reporting "file": "internal/run.go:800" - the
	// file:line convention this repo asks reviewers to use.
	for _, key := range obs.RelevanceKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if !relevanceKeyPattern.MatchString(key) || !pathLikeRelevanceKey(key) {
			return "", fmt.Errorf("%w: got %q", ErrFindingRelevanceKey, key)
		}
	}
	normalised := normaliseKeys(obs.RelevanceKeys, obs.File)
	kept := make([]string, 0, len(normalised))
	for _, key := range normalised {
		if relevanceKeyPattern.MatchString(key) && pathLikeRelevanceKey(key) {
			kept = append(kept, key)
		}
	}
	normalised = kept

	// THE MINT MUST BE ATOMIC WITH THE INSERT (#1850 review F2, P1). The previous
	// version read COUNT(DISTINCT finding_uid) and then inserted as two separate
	// statements: measured, 12 concurrent observations of 12 DISTINCT defects
	// minted only 4 uids, so 8 real findings became permanently unobservable -
	// the exact identity collision store-minted identity exists to prevent.
	// SetMaxOpenConns(1) serialises statements but NOT the read-then-write pair,
	// and separate daemon processes share the file regardless, so the fix is a
	// transaction and not a mutex. BEGIN IMMEDIATE takes the write lock up front,
	// which makes a concurrent minter wait rather than read a stale count.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM review_finding_observations LIMIT 1`); err != nil {
		return "", err
	}

	uid := strings.TrimSpace(obs.ContinuesUID)
	if uid != "" {
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM review_finding_observations WHERE finding_uid = ?`, uid).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 0 {
			return "", fmt.Errorf("%w: %q", ErrFindingUnknownContinues, uid)
		}
	} else {
		// MINT. Never derived from the reviewer's label, so a renumbered F-1
		// cannot collide with a prior finding. The candidate is then CHECKED for
		// existence inside the same transaction: a count-derived name is only as
		// unique as the count, so a collision must be a loud error and never a
		// silent merge onto somebody else's finding.
		var seq int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT finding_uid) FROM review_finding_observations WHERE repo = ? AND pull_request = ?`,
			repo, obs.PullRequest).Scan(&seq); err != nil {
			return "", err
		}
		candidate := fmt.Sprintf("%s#%d-f%d", repo, obs.PullRequest, seq+1)
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM review_finding_observations WHERE finding_uid = ?`, candidate).Scan(&taken); err != nil {
			return "", err
		}
		if taken > 0 {
			return "", fmt.Errorf("finding uid mint collision on %q: refusing to merge a new finding onto an existing uid", candidate)
		}
		uid = candidate
	}

	keys, err := json.Marshal(normalised)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_finding_observations(
	finding_uid, repo, pull_request, head_sha, observed_at, observer_job, state,
	severity, round_label, label_absent, title, detail, file, line,
	relevance_keys, evidence_kind, executed_commands, executed_count,
	evidence_locator, rationale, source_job, withdraw_reason)
VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now')), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, repo, obs.PullRequest, head, strings.TrimSpace(obs.ObservedAt), obs.ObserverJob, string(obs.State),
		obs.Severity, obs.RoundLabel, labelAbsent, obs.Title, obs.Detail, obs.File, obs.Line,
		string(keys), string(obs.EvidenceKind), string(cmds), obs.ExecutedCount,
		obs.EvidenceLocator, obs.Rationale, obs.SourceJob, obs.WithdrawReason); err != nil {
		// The key is (finding_uid, head_sha, observer_job), so this fires only for a
		// REPEAT by one observer. A second reviewer at the same head is admitted by
		// the key rather than reported here (#1850 review F8).
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", fmt.Errorf("%w: uid=%q head=%s observer=%q", ErrFindingDuplicateObservation, uid, head, obs.ObserverJob)
		}
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return uid, nil
}

// normaliseKeys seeds relevance from the finding's own file so the set is never
// empty by accident, while leaving the reviewer free to widen it. The seed is a
// floor, not the boundary: see the RelevanceKeys comment.
func normaliseKeys(keys []string, file string) []string {
	out := make([]string, 0, len(keys)+1)
	seen := map[string]bool{}
	// A file:line value is SPLIT before seeding (#1850 round 2 F5). The repo's own
	// per-finding convention is "path:line", so seeding it verbatim produced a key
	// carrying a colon, which no changed path ever has.
	if path, _, ok := splitPathLine(file); ok {
		file = path
	}
	for _, key := range append([]string{}, append(keys, file)...) {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// splitPathLine separates "path" or "path:line" into its parts. It is the store
// side of the same split findings_ledger.splitLocator performs on evidence
// locators, and it exists here so a seeded key never carries a colon.
func splitPathLine(value string) (string, int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", 0, false
	}
	path, lineText, found := strings.Cut(value, ":")
	if !found {
		return value, 0, true
	}
	line, err := strconv.Atoi(strings.TrimSpace(lineText))
	if err != nil || line < 0 {
		return value, 0, true
	}
	return path, line, true
}

// ListReviewFindingObservations returns every observation for a PR in INSERTION
// ORDER, so a caller can fold them into per-finding latest state itself. The
// store does not fold, because folding is where a "still true" column would
// creep back in.
//
// ORDERING IS rowid, NOT observed_at (#1850 second verdict C). observed_at is
// caller-supplied and the fold takes the last row per uid as authoritative, so
// ordering by it let a round pin its own observation as permanently latest.
// rowid is assigned by the database in insertion order; no caller can set it.
func (s *Store) ListReviewFindingObservations(ctx context.Context, repo string, pullRequest int64) ([]ReviewFindingObservation, error) {
	return queryList(ctx, s.db, `SELECT finding_uid, repo, pull_request, head_sha, observed_at,
	observer_job, state, severity, round_label, label_absent, title, detail, file, line,
	relevance_keys, evidence_kind, executed_commands, executed_count, evidence_locator,
	rationale, source_job, withdraw_reason
FROM review_finding_observations
WHERE repo = ? AND pull_request = ?
ORDER BY rowid`, []any{strings.TrimSpace(repo), pullRequest},
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
			// DECODE ERRORS ARE RETURNED, NOT SWALLOWED (#1850 review F10). A
			// malformed relevance_keys value used to yield a nil key set, which
			// matches nothing, which silently stops an answered finding being
			// mandatory forever. normaliseKeys guarantees a non-empty array on
			// write, so a decode failure here is real corruption and it is
			// indistinguishable from reviewer judgement unless it is reported.
			if err := json.Unmarshal([]byte(keys), &obs.RelevanceKeys); err != nil {
				return ReviewFindingObservation{}, fmt.Errorf("decode relevance_keys for finding %q at %s: %w", obs.FindingUID, obs.HeadSHA, err)
			}
			if err := json.Unmarshal([]byte(cmds), &obs.ExecutedCommands); err != nil {
				return ReviewFindingObservation{}, fmt.Errorf("decode executed_commands for finding %q at %s: %w", obs.FindingUID, obs.HeadSHA, err)
			}
			return obs, nil
		})
}
