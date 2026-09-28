package workflow

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// FindingsLogEnv names the append-only findings log a reviewer writes AS IT
// WORKS, one JSON object per line, before and independent of its final
// envelope. It exists because verdict production is ATOMIC (#2224): findings
// live only in the adapter's return value, so a run that dies mid-review
// returns an error and its findings are unrecoverable. A review that produced
// most of its findings and then lost its runtime leaves the same durable
// artefact as one that never started.
const FindingsLogEnv = "GITMOOT_FINDINGS_LOG"

// findingsLogName is the log's name inside the job worktree. It lives in the
// worktree rather than a host temp dir so the existing transfer machinery
// reaches it: locally a direct read, remotely through the change-set collection
// that already moves a patch out of a sandbox.
const findingsLogName = ".gitmoot-findings.jsonl"

// FindingsLogName is the log's name RELATIVE to the reviewer's working
// directory, which is the job worktree. It is exported because the reviewer has
// to be told where to append, and a relative name is the only form the dispatch
// layer can state: the worktree is allocated by the worker, long after the brief
// is composed.
func FindingsLogName() string { return findingsLogName }

// ReviewFindingsLogBrief is the reviewer-side half of #2224. Without it the
// salvage has nothing to salvage: no template writes the log, so every partial
// is still lost.
//
// IT ASKS FOR APPENDS AS THE REVIEW PROCEEDS, which is the whole point. A
// reviewer that writes the log only at the end has written nothing a crash can
// recover - it would just be a second copy of the envelope it already returns.
//
// The wording is deliberately narrow about what the log is FOR. A salvaged
// partial is not a verdict, satisfies no merge gate, and the reviewer must still
// return its normal envelope; the log is insurance against dying before it can.
func ReviewFindingsLogBrief() string {
	return "\n\nFINDINGS LOG (#2224). As you work, APPEND each finding you are confident about to " +
		findingsLogName + " in your working directory - one JSON object per line, with at least " +
		"severity and title, plus file and line when you have them. Append as you go, NOT at the end: " +
		"the log exists so that findings survive if your run dies before it can return an envelope " +
		"(a deadline, a cancelled job, a lost sandbox). It does NOT replace your result - still return " +
		"your normal verdict envelope, which remains the only thing that can satisfy a merge gate. " +
		"A log recovered from a dead run is recorded as a PARTIAL and approves nothing."
}

// FindingsLogPath is the absolute path handed to the reviewer and read back on
// the failure path. An empty worktree yields an empty path, which the salvage
// treats as "no log" rather than as an error.
func FindingsLogPath(worktree string) string {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return ""
	}
	return filepath.Join(worktree, findingsLogName)
}

// maxSalvagedFindingsBytes bounds what a dead run can push into the store. A
// reviewer that loops writing findings must not be able to grow the ledger
// without limit through the failure path, which is the one path with no
// envelope validation in front of it.
const maxSalvagedFindingsBytes = 1 << 20

// maxSalvagedFindings bounds the row count for the same reason.
const maxSalvagedFindings = 200

// readFindingsLog parses the append-only log into raw findings. Malformed lines
// are SKIPPED rather than failing the salvage: the log is written by a process
// that died, so a truncated final line is the expected case, not a defect. It
// returns the parsed findings and the number of lines it could not use.
func readFindingsLog(path string) ([]json.RawMessage, int, error) {
	if strings.TrimSpace(path) == "" {
		return nil, 0, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	var (
		findings  []json.RawMessage
		malformed int
		consumed  int
	)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		consumed += len(line)
		if consumed > maxSalvagedFindingsBytes || len(findings) >= maxSalvagedFindings {
			break
		}
		var probe any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			// A truncated last line is the NORMAL shape of a log whose writer was
			// killed mid-write. Counting it is how the salvage reports that it
			// happened rather than hiding it.
			malformed++
			continue
		}
		findings = append(findings, json.RawMessage(line))
	}
	if err := scanner.Err(); err != nil {
		return findings, malformed, err
	}
	return findings, malformed, nil
}

// salvagePartialReviewFindings preserves the findings a dead review had already
// produced. It runs on the delivery-error arm, BESIDE storeFailureDiagnostics
// and under the same contract: best-effort, and it must never change the
// failure path. The job stays failed.
//
// THE DECISION STAYS "failed" DELIBERATELY. A salvaged partial is not a verdict
// and must never be able to satisfy a merge gate or suppress a re-review, so no
// approval is manufactured here.
//
// IT DELIBERATELY DOES NOT WRITE THE LEDGER. That is the caller's second step,
// recordSalvagedFindingsToLedger, and it MUST run after the job has reached its
// terminal failed state - see that function for why the ordering is load-bearing
// rather than stylistic. Round 1 review of #2225 caught the original single-step
// version getting this wrong.
//
// It reports whether anything was salvaged, so the caller only performs the
// second step when there is something to record.
func (m Mailbox) salvagePartialReviewFindings(ctx context.Context, job db.Job, payload *JobPayload) bool {
	if payload == nil {
		return false
	}
	if job.Type != "review" {
		return false
	}
	// A job that already carries a result has an envelope; there is nothing
	// partial about it and overwriting it would destroy the real verdict.
	if payload.Result != nil {
		return false
	}
	path := FindingsLogPath(payload.WorktreePath)
	findings, malformed, err := readFindingsLog(path)
	if err != nil {
		if !os.IsNotExist(err) {
			_ = m.addEvent(ctx, job.ID, "review_findings_salvage_failed",
				fmt.Sprintf("could not read the findings log at %s: %v", path, err))
		}
		return false
	}
	if len(findings) == 0 {
		return false
	}

	summary := fmt.Sprintf(
		"PARTIAL: the review died before producing a verdict; %d finding(s) salvaged from the append-only log. This is NOT a verdict and satisfies no merge gate.",
		len(findings))
	if malformed > 0 {
		summary += fmt.Sprintf(" %d log line(s) were unusable, which is the expected shape of a writer killed mid-line.", malformed)
	}
	payload.Result = &AgentResult{
		Decision: "failed",
		Summary:  summary,
		Findings: findings,
	}
	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()
	if err := m.savePayload(writeCtx, job.ID, *payload); err != nil {
		_ = m.addEvent(ctx, job.ID, "review_findings_salvage_failed",
			fmt.Sprintf("salvaged %d finding(s) but the payload write failed: %v", len(findings), err))
		return false
	}
	_ = m.addEvent(ctx, job.ID, "review_findings_salvaged", summary)
	return true
}

// recordSalvagedFindingsToLedger performs the ledger half of the salvage, and it
// MUST be called only after the job has been transitioned to its terminal failed
// state.
//
// THE ORDERING IS THE WHOLE POINT. The ledger writer protects against exactly the
// hazard this feature creates: findings_ledger_writer.go skips a failed review's
// QUOTED-ONLY findings - severity and title with no locator and no execution -
// so unverified text cannot become a merge obligation. That guard is conditioned
// on job.State == JobFailed, and the writer RE-FETCHES the job itself.
//
// The first version of this code called the hook from inside the salvage, before
// m.fail() ran. savePayload writes only the payload column, and the failed
// transition requires from=JobRunning and had not happened yet, so the re-fetched
// job still read "running", the guard's state half was always false, and a
// quoted-only finding recovered from a crashed reviewer fell through to an open
// EvidenceQuoted observation. LedgerObligationsAtHead admits any open row as a
// pending obligation, so unverified crash output - exactly the shape a writer
// killed mid-line leaves - would have become a P1/P2 the merge gate demands an
// answer to on every future round at that head.
//
// That is strictly worse than losing the findings, and it is the precise outcome
// this feature's own contract says must not happen. Calling after the terminal
// transition makes the existing guard work as written; no second filter is
// introduced here, because a copy of that rule is a copy that can drift.
func (m Mailbox) recordSalvagedFindingsToLedger(ctx context.Context, jobID string) {
	// Preserving the findings ON THE PAYLOAD is the durable half and does not
	// depend on the writer being wired. A Mailbox built without the hook - every
	// CLI-constructed one today - still keeps the salvage.
	if m.RecordReviewFindings == nil {
		return
	}
	writeCtx, cancel := terminalWriteContext(ctx)
	defer cancel()
	// RE-CHECK THE STATE HERE RATHER THAN TRUSTING THE CALLER. The caller gates on
	// fail() returning nil, but that is a caller obligation and this function is
	// the thing that must not write. Round 2 review of #2225 showed the race: fail
	// is a CAS from JobRunning with no lease, and CancelJob or the session reaper
	// can transition a running job away with zero coordination, so a lost race
	// would let the write fire while the writer observes a non-failed state - the
	// quoted-only bypass again, probabilistic instead of deterministic.
	//
	// One extra read on an already-failing path is a cheap price for making the
	// invariant local.
	job, err := m.store.GetJob(writeCtx, jobID)
	if err != nil {
		_ = m.addEvent(ctx, jobID, "review_findings_salvage_failed",
			fmt.Sprintf("findings were salvaged onto the payload but the job could not be re-read before the ledger write: %v", err))
		return
	}
	if job.State != string(JobFailed) {
		_ = m.addEvent(ctx, jobID, "review_findings_salvage_failed",
			fmt.Sprintf("findings were salvaged onto the payload but the ledger write was skipped: job is %q, not %q, so the failed-review guard would not apply", job.State, string(JobFailed)))
		return
	}
	if err := m.RecordReviewFindings(writeCtx, jobID); err != nil {
		_ = m.addEvent(ctx, jobID, "review_findings_salvage_failed",
			fmt.Sprintf("findings were salvaged onto the payload but the ledger write failed: %v", err))
	}
}
