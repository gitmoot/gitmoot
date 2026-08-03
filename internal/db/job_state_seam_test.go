package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryJobStateWriteBumpsTheLifecycleGeneration makes the SEAM ENFORCEABLE.
//
// #1407's whole design argument is that the counter belongs in the state-writing SQL rather
// than in callers, BECAUSE the set of callers that re-queue a job is open and grows while the
// set of SQL statements that write jobs.state is closed and lives in one file. Review falsified
// the second half: internal/db/session_job_test.go executed a raw
// `UPDATE jobs SET state = 'queued'` that bypassed the bump, producing running/0 -> queued/0,
// a row no store writer can produce. The seam was closed by CONVENTION, not by mechanism.
//
// A comment asserting the rule would not have helped -- the rule was already stated in the
// migration and in bumpLifecycleGenerationSQL's own doc, and the violation sat one package away
// from both. When a documented rule has call sites that do not follow it, that is a mis-drawn
// boundary rather than an unknown hazard, and the fix is to make it enforceable.
//
// This scans SOURCE rather than behaviour on purpose: the construct being constrained IS text,
// a SQL string literal, so a source scan is the direct measurement and not a proxy. It fails
// for any new statement that assigns jobs.state without the shared fragment, including one
// added in a test.
func TestEveryJobStateWriteBumpsTheLifecycleGeneration(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir returned error: %v", err)
	}

	const marker = "UPDATE jobs SET state"
	total := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		// This guard's own prose contains the marker; scanning it would report itself.
		if entry.Name() == "job_state_seam_test.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s) returned error: %v", entry.Name(), err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, marker) {
				continue
			}
			total++
			if !strings.Contains(line, "bumpLifecycleGenerationSQL") {
				t.Errorf("%s:%d assigns jobs.state without bumpLifecycleGenerationSQL:\n\t%s\n"+
					"Every entry into queued starts a new run and MUST advance lifecycle_generation (#1407). "+
					"A write that skips it produces a row no store writer can produce, and a settlement "+
					"anchored to that generation then cannot tell one run from the next. Route the write "+
					"through an existing Store method, or embed the shared fragment.",
					entry.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}

	// PREMISE. A scan that matched nothing would pass silently and this guard would be inert
	// the moment the SQL is reformatted, renamed, or moved -- the exact decay it exists to
	// prevent. The floor is deliberately loose (it is not a count assertion) but non-zero.
	if total < 5 {
		t.Fatalf("found %d jobs.state writes, want at least 5; this guard is not matching the statements it claims to constrain, so its silence means nothing", total)
	}
}
