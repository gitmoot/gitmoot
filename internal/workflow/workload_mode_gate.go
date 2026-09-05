package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

const (
	operatingModeNotePrefix        = "[operating-mode "
	modeReconciliationNotePrefix   = "[workload-mode-reconciliation "
	workloadModeMarker             = "**Current mode:"
	workloadModeReconciliationScan = 200
)

type modeSensitivePullRequest struct {
	required  bool
	mode      string
	ambiguous bool
	// patchUnavailable records that GitHub omitted the AGENTS.md patch, which is
	// an API limit on large files rather than an author mistake.
	patchUnavailable bool
}

// noteConcernsRepo decides whether an UNPARSEABLE note belongs to this repo: its
// repo column when set, else a lenient scan of the body for a repo value.
//
// The scan tolerates the shapes a hand-written note actually takes. The first
// version matched only a bare `repo=<value>` token, so `repo=owner/name,` and
// `repo = owner/name` still slipped through and a stale row merged (#1783
// round-4 review, F-6). A note that names neither this repo nor nothing is
// another lane's and is skipped, which is the anti-wedge narrowing.
func noteConcernsRepo(note db.WorkflowNote, repo string) bool {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return false
	}
	if column := strings.TrimSpace(note.Repo); column != "" {
		return strings.EqualFold(column, repo)
	}
	if value, ok := leadingRepoValue(note.Body); ok {
		return strings.EqualFold(value, repo)
	}
	return false
}

// leadingRepoValue extracts the first repo value from a note body, tolerating
// `repo=x`, `repo = x`, and trailing punctuation such as `,`, `;` or `]`.
func leadingRepoValue(body string) (string, bool) {
	fields := strings.Fields(body)
	for i, field := range fields {
		key, value, split := strings.Cut(field, "=")
		if !strings.EqualFold(strings.TrimSpace(key), "repo") {
			continue
		}
		if !split || strings.TrimSpace(value) == "" {
			// `repo = x` or `repo =x`: the value is in a later field.
			for _, next := range fields[i+1:] {
				next = strings.TrimPrefix(strings.TrimSpace(next), "=")
				if trimmed := trimNoteFieldValue(next); trimmed != "" {
					return trimmed, true
				}
			}
			return "", false
		}
		if trimmed := trimNoteFieldValue(value); trimmed != "" {
			return trimmed, true
		}
		return "", false
	}
	return "", false
}

func trimNoteFieldValue(value string) string {
	return strings.Trim(strings.TrimSpace(value), "],;\"'")
}

// workloadModeEnforcedOwner is the repository OWNER whose mode-marker changes
// this gate enforces. AGENTS.md writes its workload-mode rules against the
// gitmoot/* scope, but the gate compared FullName to the literal
// "gitmoot/gitmoot", so a mode-marker change in any other gitmoot repository
// merged with no reconciliation at all (#1783 review, P3). The note grammar is
// already repo-parameterized, so widening to the owner matches the policy the
// gate implements without inventing a config knob.
//
// Two deliberate consequences, both raised by the round-2 review and both kept:
// every gitmoot-owned repository is held until a reconciliation row exists, even
// one where no coordinator writes notes today, and a human-requested merge is
// held too. A mode marker is a fleet-wide instruction, so the hold is the point;
// the escape hatch is the documented PR-sourced row (decision_note=none), not a
// per-repo opt-out. If a sibling repo ever needs exemption that is a config
// knob and an AGENTS.md change, not a silent scope narrowing here.
//
// The comparison is case-INSENSITIVE. GitHub treats owner/repo case
// insensitively, so a repo recorded as "Gitmoot/gitmoot" disabled the entire
// gate under a byte comparison — fail-open, and invisible.
const workloadModeEnforcedOwner = "gitmoot"

func inspectModeSensitivePullRequest(ctx context.Context, gh MergeGateGitHub, repo github.Repository, number int64) (modeSensitivePullRequest, error) {
	if !strings.EqualFold(strings.TrimSpace(repo.Owner), workloadModeEnforcedOwner) {
		return modeSensitivePullRequest{}, nil
	}
	files, err := gh.ListPullRequestFiles(ctx, repo, number)
	if err != nil {
		return modeSensitivePullRequest{}, fmt.Errorf("list pull request files for workload-mode reconciliation: %w", err)
	}
	for _, file := range files {
		if strings.TrimSpace(file.Filename) != "AGENTS.md" {
			continue
		}
		patch := strings.TrimSpace(file.Patch)
		if patch == "" {
			// GitHub OMITS `patch` for large files, and AGENTS.md is large, so this
			// is routinely an API limit rather than anything the author did. It
			// still requires reconciliation - the marker may have moved and we
			// cannot see it - but the hold must not tell the author to fix a
			// marker that is not the problem (#1783 round-3 review, F-B).
			return modeSensitivePullRequest{required: true, ambiguous: true, patchUnavailable: true}, nil
		}
		var addedMode string
		changed := false
		ambiguous := false
		for _, line := range strings.Split(patch, "\n") {
			if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
				continue
			}
			text := strings.TrimSpace(line[1:])
			if !strings.HasPrefix(text, workloadModeMarker) {
				continue
			}
			changed = true
			if line[0] != '+' {
				continue
			}
			mode, ok := parseWorkloadModeMarker(text)
			if !ok || (addedMode != "" && addedMode != mode) {
				ambiguous = true
				continue
			}
			addedMode = mode
		}
		if !changed {
			return modeSensitivePullRequest{}, nil
		}
		if addedMode == "" {
			ambiguous = true
		}
		return modeSensitivePullRequest{required: true, mode: addedMode, ambiguous: ambiguous}, nil
	}
	return modeSensitivePullRequest{}, nil
}

func parseWorkloadModeMarker(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, workloadModeMarker) || !strings.HasSuffix(line, ".**") {
		return "", false
	}
	mode := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, workloadModeMarker), ".**"))
	return mode, validWorkloadMode(mode)
}

func validWorkloadMode(mode string) bool {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "THROUGHPUT", "STEADY", "DRAIN":
		return true
	default:
		return false
	}
}

// operatingModeDecision is the newest READABLE owner decision for a repo, or a
// record of why the newest decision-shaped note could not be read.
type operatingModeDecision struct {
	id   int64
	mode string
	// unreadableID names a note that IS an operating-mode note for this repo but
	// whose fields or mode cannot be read. It is not "no decision": the owner
	// decided something the gate cannot determine, so the gate must hold.
	unreadableID  int64
	unreadableWhy string
}

// workloadModeHold is one reconciliation hold, split into the two things a
// caller needs for different reasons.
//
// `detail` is the operator-facing cause and deliberately carries VOLATILE
// context: whichever near-miss row the scan saw last, note ids, row heads, and
// a scan-window notice that appears once the window fills. `key` is the STABLE
// discriminator for the hold's EPISODE - the head and the decision it must
// reconcile against - and nothing else.
//
// They are separate because the pipeline keys a hold episode's budget on the
// cause. Keying it on `detail` let external churn reset the bound: one new
// reconciliation row anywhere in the repo changes the appended near-miss, which
// opened a fresh episode with a full budget and appended another held row, so a
// repo whose notes churn could defer the bound indefinitely - the unbounded
// silent wait this campaign exists to remove, reachable with no code defect at
// all (#1783 round-5 review, F-11).
type workloadModeHold struct {
	key    string
	detail string
}

func ensureWorkloadModeReconciled(ctx context.Context, store *db.Store, gh MergeGateGitHub, repo github.Repository, number int64, headSHA string) (required, reconciled bool, hold workloadModeHold, err error) {
	observed, err := inspectModeSensitivePullRequest(ctx, gh, repo, number)
	if err != nil {
		return false, false, workloadModeHold{}, err
	}
	if !observed.required {
		return false, true, workloadModeHold{}, nil
	}
	if store == nil {
		return true, false, workloadModeHold{}, fmt.Errorf("workload-mode reconciliation requires a store")
	}
	decision, err := latestOperatingModeDecision(ctx, store, repo.FullName())
	if err != nil {
		return true, false, workloadModeHold{}, err
	}
	// An UNREADABLE newest decision is not "no decision". Skipping it returned
	// id==0, which dropped the supersession check AND made decision_note=none
	// satisfiable by a row written before the owner decided — the same fail-open
	// corner the repo-scoped SQL closed, reachable through a typo instead of a
	// truncated window (#1783 review, F1).
	//
	// It must not WEDGE a PR either, which is what directive 110704 asked me to
	// check. Holding unconditionally meant one malformed note froze every
	// mode-marker PR in the repo until someone edited an append-only journal, and
	// the coordinator's remedy — write a fresh exact-head row — did nothing. So an
	// unreadable note is a RECENCY BOUNDARY, not a veto: a row filed AFTER it,
	// which is a coordinator asserting the current head against a decision they
	// can see, still reconciles. Only two states hold: no row newer than the
	// unreadable note, or a PR whose own marker is unreadable too, where nothing
	// remains to check the row against.
	if decision.unreadableID > 0 && strings.TrimSpace(observed.mode) == "" {
		// The remedy must be COMPLETE. Naming only the note left an exit that does
		// not by itself clear the hold: a fresh readable note restores a decision,
		// and the PR still needs an exact-head reconciliation row against it
		// (#1783 round-4 review, F-7).
		remedy := "append a fresh readable operating-mode note AND file an exact-head reconciliation row citing it, or correct the malformed note"
		if observed.patchUnavailable {
			remedy = "GitHub omitted this PR's AGENTS.md patch, so the marker cannot be read here and is not the author's to fix; " + remedy
		}
		return true, false, workloadModeHold{
			key: fmt.Sprintf("head=%s unreadable-note=%d", strings.TrimSpace(headSHA), decision.unreadableID),
			detail: fmt.Sprintf(
				"workload-mode change cannot be reconciled: operating-mode note %d %s and this PR's own mode marker is missing or ambiguous, so no readable decision remains; %s",
				decision.unreadableID, decision.unreadableWhy, remedy,
			),
		}, nil
	}
	expectedMode := strings.ToUpper(strings.TrimSpace(observed.mode))
	if expectedMode == "" && decision.id > 0 {
		expectedMode = decision.mode
	}
	// supersededBy is the note a row must be newer than: the newest readable
	// decision, or an unreadable one, whichever is later.
	supersededBy := decision.id
	if decision.unreadableID > supersededBy {
		supersededBy = decision.unreadableID
	}
	// The episode key must change when the DECISION changes, including when the
	// new decision is unreadable. Leaving decisionID at "none" for every
	// unreadable note gave two different malformed notes at one head the SAME
	// key, and run.go reuses the earliest matching hold timestamp - so a fresh
	// unreadable decision inherited the previous one's 24h budget and could park
	// immediately (#1783 review, P2c). An unreadable decision is still a
	// decision: it gets its own identity, distinguishable from a readable one so
	// the two can never collide on the same id.
	// decisionID is the EPISODE IDENTITY; decisionLabel is what an operator reads.
	// They differ only for an unreadable decision, where the key needs a value
	// that cannot collide with a readable note's id and the message needs a
	// sentence.
	decisionID := "none"
	decisionLabel := "none"
	switch {
	case decision.id > 0:
		decisionID = strconv.FormatInt(decision.id, 10)
		decisionLabel = decisionID
	case decision.unreadableID > 0:
		decisionID = "unreadable-" + strconv.FormatInt(decision.unreadableID, 10)
		decisionLabel = fmt.Sprintf("%d (unreadable: it %s)", decision.unreadableID, decision.unreadableWhy)
	}
	notes, err := store.ListRepoWorkflowNotesByBodyPrefix(ctx, modeReconciliationNotePrefix, repo.FullName(), workloadModeReconciliationScan)
	if err != nil {
		return true, false, workloadModeHold{}, fmt.Errorf("list workload-mode reconciliation notes: %w", err)
	}
	// Two rejections used to be silent, and the #1783 review found both.
	//
	// P2a: the documented `decision_note=none` was unmatchable. decisionID is
	// "none" only while the repo has ZERO operating-mode notes, so the moment the
	// first typed note exists, every row written per the documented PR-sourced
	// instruction was skipped and the PR held forever. `none` is now accepted as
	// what it claims to be: a PR-SOURCED decision, where the PR itself decides
	// and no earlier note has to agree. The row must still be newer than the
	// newest note, so a superseded PR-sourced row cannot ratify anything.
	//
	// P2b: a row citing a note id proved only that someone READ that note, never
	// that the PR agreed with it. With the newest note deciding STEADY, a PR
	// flipping the marker to THROUGHPUT merged cleanly by citing the STEADY note.
	// A row that names a decision must now match that decision's mode.
	//
	// Every rejection reason is recorded and appended to the hold, because the
	// native path holds through g.pending, which retries silently: a coordinator
	// who HAD written a row saw an indefinitely pending PR and no cause.
	nearMiss := ""
	for _, note := range notes {
		fields, ok := parseModeNoteFields(note.Body, modeReconciliationNotePrefix)
		if !ok || !workflowNoteMatchesRepo(note, fields, repo.FullName()) {
			continue
		}
		// The two most likely coordinator mistakes used to `continue` silently, so
		// the hold said only "requires reconciliation": a row written at the
		// PREVIOUS head after a fix-up push, and a row naming another PR (#1783
		// review, F2). Name the row and both values instead.
		if rowPR := strings.TrimSpace(fields["pr"]); rowPR != strconv.FormatInt(number, 10) {
			nearMiss = fmt.Sprintf("; row %d reconciles pr=%s, not this pull request", note.ID, rowPR)
			continue
		}
		if rowHead := strings.TrimSpace(fields["head"]); rowHead != strings.TrimSpace(headSHA) {
			nearMiss = fmt.Sprintf("; row %d reconciles head %s, but the current head is %s", note.ID, shortSHA(rowHead), shortSHA(headSHA))
			continue
		}
		mode := strings.ToUpper(strings.TrimSpace(fields["mode"]))
		if !validWorkloadMode(mode) {
			nearMiss = fmt.Sprintf("; row %d declares mode=%q, which is not a workload mode", note.ID, fields["mode"])
			continue
		}
		// The row must reconcile the mode the PR ADDS. On the decision_note=none
		// path this is the only binding constraint, so without it a PR-sourced row
		// could ratify a marker change it disagreed with (#1783 review, F5a).
		if expectedMode != "" && mode != expectedMode {
			nearMiss = fmt.Sprintf("; row %d reconciles mode=%s but the PR changes the marker to %s", note.ID, mode, expectedMode)
			continue
		}
		if supersededBy > 0 && note.ID <= supersededBy {
			if decision.unreadableID == supersededBy {
				nearMiss = fmt.Sprintf("; row %d predates operating-mode note %d, which %s, so file a new exact-head row acknowledging it", note.ID, supersededBy, decision.unreadableWhy)
			} else {
				nearMiss = fmt.Sprintf("; row %d predates operating-mode note %d, so it reconciles a superseded decision", note.ID, supersededBy)
			}
			continue
		}
		cited := strings.TrimSpace(fields["decision_note"])
		if cited == "none" {
			// PR-sourced: this PR is the decision, so no earlier note must agree.
			return true, true, workloadModeHold{}, nil
		}
		// A row may cite the unreadable note itself: it names what the coordinator
		// saw, and the mode check above already bound it to the PR's marker.
		if decision.unreadableID > 0 && cited == strconv.FormatInt(decision.unreadableID, 10) {
			return true, true, workloadModeHold{}, nil
		}
		if cited != decisionID {
			nearMiss = fmt.Sprintf("; row %d cites decision_note=%s but the newest operating-mode note is %s", note.ID, cited, decisionLabel)
			continue
		}
		if decision.id > 0 && mode != decision.mode {
			nearMiss = fmt.Sprintf("; row %d cites operating-mode note %d, which decided %s, so it cannot ratify %s", note.ID, decision.id, decision.mode, mode)
			continue
		}
		return true, true, workloadModeHold{}, nil
	}
	detail := fmt.Sprintf("workload-mode change requires reconciliation at head %s against operating-mode note %s", shortSHA(headSHA), decisionLabel)
	if observed.ambiguous {
		if observed.patchUnavailable {
			detail += "; GitHub omitted this PR's AGENTS.md patch, so the mode marker could not be read"
		} else {
			detail += "; AGENTS.md mode-marker patch is missing or ambiguous"
		}
	}
	if len(notes) >= workloadModeReconciliationScan {
		// The window is FULL, so a valid row for this head may have been pushed
		// out of it by newer rows from the same repo. Without this line the hold
		// names whichever near-miss the loop happened to see and reads as a
		// verdict on the operator's own row (#1783 round-3 review, F-C). Refiling
		// the row recovers it, which is why this is a message fix.
		detail += fmt.Sprintf("; the newest %d reconciliation rows for this repository were scanned and a valid row for this head may have aged out - refile it if you already wrote one", workloadModeReconciliationScan)
	}
	if nearMiss == "" {
		// The repo-scoped SQL drops a row whose repo COLUMN names another
		// repository before this loop ever sees it, so a coordinator who filed the
		// row against the wrong workflow saw only the generic hold (#1783 review,
		// F2). One unscoped scan, only when nothing else explained the hold, finds
		// it and names both repositories.
		nearMiss = misfiledReconciliationRow(ctx, store, repo.FullName(), number, headSHA)
	}
	detail += nearMiss
	// The key names the head and the decision the row must reconcile against,
	// and stops there. Every string appended above is volatile by design, and an
	// episode keyed on volatile text has no bound (#1783 round-5 review, F-11).
	return true, false, workloadModeHold{
		key:    fmt.Sprintf("head=%s decision-note=%s mode=%s", strings.TrimSpace(headSHA), decisionID, expectedMode),
		detail: detail,
	}, nil
}

// misfiledReconciliationRow finds a row that names THIS repo in its body but was
// recorded under a different repo column, which makes it invisible to the
// repo-scoped lookup. It returns "" when there is nothing to report.
func misfiledReconciliationRow(ctx context.Context, store *db.Store, repo string, number int64, headSHA string) string {
	notes, err := store.ListWorkflowNotesByBodyPrefix(ctx, modeReconciliationNotePrefix, workloadModeReconciliationScan)
	if err != nil {
		return ""
	}
	for _, note := range notes {
		fields, ok := parseModeNoteFields(note.Body, modeReconciliationNotePrefix)
		if !ok || !strings.EqualFold(strings.TrimSpace(fields["repo"]), strings.TrimSpace(repo)) {
			continue
		}
		noteRepo := strings.TrimSpace(note.Repo)
		if noteRepo == "" || strings.EqualFold(noteRepo, strings.TrimSpace(repo)) {
			continue
		}
		if strings.TrimSpace(fields["pr"]) != strconv.FormatInt(number, 10) ||
			strings.TrimSpace(fields["head"]) != strings.TrimSpace(headSHA) {
			continue
		}
		return fmt.Sprintf("; row %d names this repository in its body but was recorded under %s, so the repo-scoped lookup cannot see it", note.ID, noteRepo)
	}
	return ""
}

func latestOperatingModeDecision(ctx context.Context, store *db.Store, repo string) (operatingModeDecision, error) {
	notes, err := store.ListRepoWorkflowNotesByBodyPrefix(ctx, operatingModeNotePrefix, repo, workloadModeReconciliationScan)
	if err != nil {
		return operatingModeDecision{}, fmt.Errorf("list operating-mode notes: %w", err)
	}
	for _, note := range notes {
		fields, parsed := parseModeNoteFields(note.Body, operatingModeNotePrefix)
		if parsed && workflowNoteMatchesRepo(note, fields, repo) {
			mode := strings.ToUpper(strings.TrimSpace(fields["mode"]))
			if validWorkloadMode(mode) {
				return operatingModeDecision{id: note.ID, mode: mode}, nil
			}
			return operatingModeDecision{
				unreadableID:  note.ID,
				unreadableWhy: fmt.Sprintf("declares mode=%q, which is not a workload mode", fields["mode"]),
			}, nil
		}
		// A note whose body PARSED but whose repo FIELD does not confirm this
		// repository is NOT automatically another lane's problem. If the note is
		// demonstrably this repository's - its repo COLUMN says so - then its body
		// omits or contradicts repo=, and that is an unreadable decision for THIS
		// repo, so it must HOLD. Skipping it let the next-oldest row answer and a
		// stale PR-sourced reconciliation merge; the #1783 review reproduced
		// Merged=true through PolicyMergeGate.Evaluate with exactly that shape.
		// Only a note that neither parses for this repo nor belongs to it by column
		// is skipped, so one lane's typo still cannot freeze every repository.
		if parsed && noteConcernsRepo(note, repo) {
			return operatingModeDecision{
				unreadableID: note.ID,
				unreadableWhy: fmt.Sprintf(
					"was recorded for this repository but its body declares repo=%q, so it cannot be read as this repository's decision",
					strings.TrimSpace(fields["repo"])),
			}, nil
		}
		// An unparseable body cannot answer "is this note for this repo?" through
		// its repo FIELD, so fall back to the note's repo COLUMN, and when that is
		// empty to a lenient scan of the body for `repo=<this repo>`. A note that
		// is demonstrably this repo's and unreadable HOLDS; one that names neither
		// this repo nor nothing is another lane's problem and is skipped, so a
		// typo in an unrelated note cannot freeze every gitmoot repository.
		//
		// The lenient body scan closes the residual fail-open the round-3 review
		// measured (F-D): `workflow note` is written by hand, so an omitted --repo
		// left a malformed note invisible and a stale PR-sourced row still
		// reconciled.
		if !parsed && noteConcernsRepo(note, repo) {
			return operatingModeDecision{
				unreadableID:  note.ID,
				unreadableWhy: "has a malformed field list, so its mode cannot be read",
			}, nil
		}
	}
	return operatingModeDecision{}, nil
}

func parseModeNoteFields(body, prefix string) (map[string]string, bool) {
	if !strings.HasPrefix(body, prefix) {
		return nil, false
	}
	end := strings.IndexByte(body, ']')
	if end < len(prefix) {
		return nil, false
	}
	fields := make(map[string]string)
	for _, field := range strings.Fields(body[len(prefix):end]) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return nil, false
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields, true
}

// workflowNoteMatchesRepo compares repositories CASE-INSENSITIVELY. GitHub
// treats owner/repo case insensitively, so a byte comparison made a note
// written as "Gitmoot/gitmoot" invisible on both note streams (#1783 review,
// F3).
func workflowNoteMatchesRepo(note db.WorkflowNote, fields map[string]string, repo string) bool {
	repo = strings.TrimSpace(repo)
	if noteRepo := strings.TrimSpace(note.Repo); noteRepo != "" && !strings.EqualFold(noteRepo, repo) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(fields["repo"]), repo)
}
