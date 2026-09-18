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
// approval is manufactured here. The existing ledger writer already supports
// this case - findings_ledger_writer.go skips a failed review's quoted-only
// findings while letting locator-backed and executed ones survive - so salvage
// reuses that path rather than inventing a second one.
func (m Mailbox) salvagePartialReviewFindings(ctx context.Context, job db.Job, payload *JobPayload) {
	if payload == nil {
		return
	}
	if job.Type != "review" {
		return
	}
	// A job that already carries a result has an envelope; there is nothing
	// partial about it and overwriting it would destroy the real verdict.
	if payload.Result != nil {
		return
	}
	path := FindingsLogPath(payload.WorktreePath)
	findings, malformed, err := readFindingsLog(path)
	if err != nil {
		if !os.IsNotExist(err) {
			_ = m.addEvent(ctx, job.ID, "review_findings_salvage_failed",
				fmt.Sprintf("could not read the findings log at %s: %v", path, err))
		}
		return
	}
	if len(findings) == 0 {
		return
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
		return
	}
	_ = m.addEvent(ctx, job.ID, "review_findings_salvaged", summary)
	// The ledger write is separately optional: preserving the findings ON THE
	// PAYLOAD is the durable half and must not depend on the writer being wired.
	// A Mailbox built without the hook (every CLI-constructed one today) still
	// keeps the salvage.
	if m.RecordReviewFindings == nil {
		return
	}
	if err := m.RecordReviewFindings(writeCtx, job.ID); err != nil {
		_ = m.addEvent(ctx, job.ID, "review_findings_salvage_failed",
			fmt.Sprintf("salvaged %d finding(s) but the ledger write failed: %v", len(findings), err))
	}
}
