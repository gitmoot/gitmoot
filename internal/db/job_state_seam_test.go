package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// jobStateWritePattern matches an assignment to jobs.state with WHITESPACE NORMALIZED.
//
// The first version of this guard scanned single physical lines for the exact substring
// "UPDATE jobs SET state". Review defeated it with a compiling mutant that wrote
// `UPDATE jobs\nSET state = ?` and omitted the bump: the seam guard passed while the
// behavioural generation test failed. A guard that constrains ONE SPELLING of a construct does
// not constrain the construct.
var jobStateWritePattern = regexp.MustCompile(`(?is)UPDATE\s+jobs\s+SET\s+state`)

// jobStateWriteAllowlist is the exact set of files permitted to assign jobs.state, with counts.
//
// It carries EXACT PER-FILE COUNTS, not a floor and not merely a file set. The previous version
// required only `total >= 5`, which review showed cannot detect losing an individual statement.
// A file set alone would not close that either: deleting one statement from a file that still
// has seven leaves the set unchanged.
//
// Counts fail in BOTH directions -- a new bypass adds a file or raises a count, a deleted or
// relocated statement lowers one -- so every change to the seam becomes a deliberate edit here
// with a reason attached. That makes this a change-detector by design; the alternative, measured
// twice on this PR, is a guard that silently stops constraining the thing it names.
var jobStateWriteAllowlist = map[string]int{
	"internal/db/store_jobs.go": 8,
}

// TestEveryJobStateWriteBumpsTheLifecycleGeneration enforces the seam #1407's design rests on.
//
// The argument for putting the counter in SQL rather than in callers is that the set of CALLERS
// that re-queue a job is open and grows, while the set of STATEMENTS that write jobs.state is
// closed. Review falsified that claim twice: the seam was closed only by convention, and this
// guard -- written to close it -- was itself defeatable by reformatting and blind to anything
// outside internal/db.
//
// WHAT THIS GUARD CLOSES, each demonstrated with a compiling mutant:
//   - a statement assigning jobs.state without the shared bump fragment, in ANY package,
//     however the SQL is wrapped across lines or spaced;
//   - a state assignment appearing in a new file (the count map grows);
//   - an existing statement being deleted or relocated (a count drops);
//   - the guard itself going inert (its pattern matching nothing yields an empty map).
//
// WHAT IT CANNOT CLOSE, stated plainly because this file has already over-claimed twice and been
// corrected twice: SQL assembled at runtime defeats any source scan. A statement built by
// concatenation, by fmt.Sprintf, from a const assembled elsewhere, or an
// INSERT ... ON CONFLICT DO UPDATE that sets state will not match this pattern. This is a strong
// guard against the ACCIDENTAL bypass -- someone writing a direct UPDATE, which is exactly what
// happened -- and no guard at all against a determined one. Semantics are constrained by
// TestLifecycleGenerationBumpsOnlyOnEntryToQueued; this constrains the shape of new code.
func TestEveryJobStateWriteBumpsTheLifecycleGeneration(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	// This guard's own prose contains the construct it searches for; scanning it would report
	// itself.
	selfPath := filepath.Join(root, "internal", "db", "job_state_seam_test.go")

	found := map[string]int{}
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || path == selfPath {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		for _, loc := range jobStateWritePattern.FindAllStringIndex(text, -1) {
			found[rel]++
			// The bump is concatenated directly into the SQL immediately after the state
			// assignment, so a tight window is faithful: in every legitimate statement the
			// fragment appears within ~30 characters. 400 keeps multi-line formatting valid
			// without letting an unrelated later statement's fragment vouch for this one.
			end := loc[0] + 400
			if end > len(text) {
				end = len(text)
			}
			if !strings.Contains(text[loc[0]:end], "bumpLifecycleGenerationSQL") {
				excerptEnd := loc[0] + 120
				if excerptEnd > len(text) {
					excerptEnd = len(text)
				}
				t.Errorf("%s: a statement assigns jobs.state without bumpLifecycleGenerationSQL:\n\t%s\n"+
					"Every entry into queued starts a new run and MUST advance lifecycle_generation "+
					"(#1407). A write that skips it produces a row no store writer can produce, and a "+
					"settlement anchored to that generation then cannot tell one run from the next. "+
					"Route the write through an existing Store method, or embed the shared fragment.",
					rel, collapseJobStateWhitespace(text[loc[0]:excerptEnd]))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}

	if !sameJobStateCounts(found, jobStateWriteAllowlist) {
		t.Fatalf("jobs.state assignments = %s, want exactly %s.\n"+
			"A file or count APPEARING here is a new bypass of the lifecycle-generation seam. A count "+
			"DROPPING means a statement was deleted or relocated and this guard has stopped "+
			"constraining it. An EMPTY map means the guard has gone inert. Each is a deliberate "+
			"decision -- update jobStateWriteAllowlist with the reason.",
			renderJobStateCounts(found), renderJobStateCounts(jobStateWriteAllowlist))
	}
}

func sameJobStateCounts(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func renderJobStateCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func collapseJobStateWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
