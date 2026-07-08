package workflow

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// This file implements #737 P4.1: deterministic DISTILL-AT-TERMINAL. It is a
// config-gated WRITE producer, separate from the Phase-1 enrolled write path,
// that mines a terminal job's OWN result for two closed-category signals —
// FAILING TESTS (from result.TestsRun) and NAMED ERRORS (from result.Summary and
// the tail of payload.RawOutputs) — and stages them as PENDING observations.
//
// OUTPUT DISCIPLINE (differs from the confirmed mechanical producers): distill
// NEVER writes confirmed_memories. It only ever InsertMemoryObservation()s rows
// with TrustMark=low and Provenance "distill:<job-id>", so the owner's
// `memory confirm` gate stays the ONLY promotion path. Every candidate passes
// memory.PreFilter, is content-hash-deduped against existing rows, and the whole
// producer is bounded by a hard per-job cap.
//
// RECURRENCE (gpt-5.5's catch): a one-off anomalous failure must NOT stage — a
// single flaky failure should not become a durable pending memory. The FIRST time
// a normalized key is seen, distill records only a low-trust WITNESS sentinel
// (provenance "distill-seen:<job-id>"); the actual staged observation is written
// only on the SECOND (recurring) sighting across jobs. Because distill owns its
// key namespace ("distill-test:" / "distill-error:"), CountMemoryObservationsForKey
// is an exact recurrence counter with no schema change. Per distinct key at most
// ONE witness + ONE staged row can ever exist (dedup collapses further repeats),
// so the pending queue a human reviews is never flooded by one-offs.
//
// FAIL-SAFE: every store call here is best-effort. Any error (or a nil field)
// returns early; the job outcome is never affected.

const (
	// distillWitnessProvenancePrefix marks the first-sighting recurrence witness.
	// It is DISTINCT from the staged provenance so a confirm/list surface can tell
	// a bookkeeping sentinel apart from a real distilled candidate.
	distillWitnessProvenancePrefix = "distill-seen:"
	// distillStagedProvenancePrefix marks a genuinely staged distilled observation.
	distillStagedProvenancePrefix = "distill:"
	// distillWitnessSentinel is the FIXED, PreFilter-safe content stored on a
	// recurrence witness. It is deliberately constant (never a candidate's content)
	// so it can never false-collide with a real staged content in the dedup path.
	distillWitnessSentinel = "A failure signal was observed once in this repository and is held pending recurrence before it is recorded."
	// distillRawOutputTailBytes bounds how much of the last raw output distill
	// scans for error lines, so a huge transcript can never balloon the scan.
	distillRawOutputTailBytes = 4000
	// distillMaxScanLines bounds how many lines distill inspects for error tokens
	// before the per-job cap even applies.
	distillMaxScanLines = 200
	// distillMaxErrorLineLen skips over-long lines: a genuine named-error line is
	// short, whereas a minified result envelope or a giant log line is not — so the
	// length guard keeps the structured gitmoot_result JSON (and other noise) out.
	distillMaxErrorLineLen = 300
	// distillContentMaxLen caps the length of a distilled observation's content.
	distillContentMaxLen = 220
)

// distillDecisions is the CLOSED set of terminal decisions that trigger distill:
// the anomalous/notable outcomes worth mining for failure signal. Routine
// successes (approved, implemented) are absent — a first-try success carries no
// signal, matching the anti-flood restraint of the confirmed producers.
var distillDecisions = map[string]bool{
	"failed":            true,
	"blocked":           true,
	"changes_requested": true,
}

// distillObs is one deterministic distill candidate: a bounded closed-category
// key plus stable, reference-phrased content. Content is STABLE per key (it never
// embeds volatile per-job text) so the content-hash dedup path collapses repeats
// deterministically.
type distillObs struct {
	Key     string
	Content string
}

// distillAtTerminal is the #737 P4.1 producer entry point invoked from record().
// It is a no-op unless the master switch is on for this agent (distillEnabledFor),
// so with the feature off the terminal path is byte-identical. It stages at most
// DistillMaxPerJob observations (witness or real), each low-trust and pending.
func (c *MemoryController) distillAtTerminal(ctx context.Context, jobID string, agent runtime.Agent, action string, payload JobPayload, result AgentResult) {
	if !c.distillEnabledFor(agent.Name) {
		return
	}
	decision := strings.TrimSpace(result.Decision)
	if !distillDecisions[decision] {
		return
	}
	candidates := distillCandidates(action, payload, result)
	if len(candidates) == 0 {
		return
	}

	perJobCap := c.DistillMaxPerJob
	if perJobCap <= 0 {
		perJobCap = config.DefaultMemoryDistillMaxPerJob
	}

	owner := ownerForJob(agent, payload)
	repo := payload.Repo

	// Existing content-hash dedup domain (pending + confirmed) for this owner. A
	// load failure is fail-safe: skip distill entirely rather than risk a duplicate.
	seen, err := c.Store.ObservationDedupKeys(ctx, owner.Ref)
	if err != nil {
		return
	}

	written := 0
	for _, cand := range candidates {
		if written >= perJobCap {
			return
		}
		// PreFilter is the primary write gate — a directive/secret/executable-shaped
		// error line is dropped before it can be witnessed OR staged.
		if ok, _ := memory.PreFilter(cand.Content, memory.ScopeRepo); !ok {
			continue
		}
		// Recurrence: count prior distill rows (witness + staged) for this key.
		prior, err := c.Store.CountMemoryObservationsForKey(ctx, owner, repo, cand.Key)
		if err != nil {
			continue
		}
		if prior == 0 {
			// First sighting: record a low-trust WITNESS only — do not stage yet.
			_, _ = c.Store.InsertMemoryObservation(ctx, db.MemoryObservation{
				Owner:      owner,
				Repo:       repo,
				Scope:      memory.ScopeRepo,
				Key:        cand.Key,
				Content:    distillWitnessSentinel,
				Provenance: distillWitnessProvenancePrefix + jobID,
				TrustMark:  memory.TrustLow,
				SourceJob:  jobID,
			})
			written++
			continue
		}
		// Recurrence met: content-hash dedup so a repeat never stages twice.
		dkey := db.MemoryDedupKey(memory.ScopeRepo, repo, memory.ContentHash(cand.Content))
		if _, dup := seen[dkey]; dup {
			continue
		}
		seen[dkey] = struct{}{}
		_, _ = c.Store.InsertMemoryObservation(ctx, db.MemoryObservation{
			Owner:      owner,
			Repo:       repo,
			Scope:      memory.ScopeRepo,
			Key:        cand.Key,
			Content:    cand.Content,
			Provenance: distillStagedProvenancePrefix + jobID,
			TrustMark:  memory.TrustLow,
			SourceJob:  jobID,
		})
		written++
	}
}

// distillCandidates assembles the deterministic candidate set for a terminal job,
// deduped by key and in a stable order: failing tests first, then named errors.
func distillCandidates(action string, payload JobPayload, result AgentResult) []distillObs {
	out := make([]distillObs, 0, 4)
	seenKey := make(map[string]struct{})
	add := func(o distillObs) {
		if o.Key == "" || strings.TrimSpace(o.Content) == "" {
			return
		}
		if _, dup := seenKey[o.Key]; dup {
			return
		}
		seenKey[o.Key] = struct{}{}
		out = append(out, o)
	}
	for _, o := range failingTestCandidates(action, result) {
		add(o)
	}
	for _, o := range namedErrorCandidates(payload, result) {
		add(o)
	}
	return out
}

// failingTestCandidates derives one closed-category candidate per test named in
// result.TestsRun. On a distill terminal (failed/blocked/changes_requested) those
// are the tests the failing job exercised; the normalized test name is the key,
// and the content is a stable reference sentence (NO volatile per-job text, so
// dedup is deterministic).
func failingTestCandidates(action string, result AgentResult) []distillObs {
	act := memoryActionToken(action)
	out := make([]distillObs, 0, len(result.TestsRun))
	for _, raw := range result.TestsRun {
		norm := normalizeTestName(raw)
		if norm == "" {
			continue
		}
		out = append(out, distillObs{
			Key:     "distill-test:" + norm,
			Content: fmt.Sprintf("Test %s was exercised in a %s job that did not complete cleanly in this repository.", norm, act),
		})
	}
	return out
}

// namedErrorCandidates extracts stable error tokens from result.Summary and the
// tail of the last raw output. Each error line is normalized to a closed-category
// signature (lowercased, with hashes/paths/addresses/line-numbers/timestamps
// stripped) so distinct incidental values collapse to one key. Content is the
// cleaned line itself (stable), prefixed with a neutral reference frame.
func namedErrorCandidates(payload JobPayload, result AgentResult) []distillObs {
	sources := []string{result.Summary}
	if n := len(payload.RawOutputs); n > 0 {
		tail := payload.RawOutputs[n-1]
		if len(tail) > distillRawOutputTailBytes {
			tail = tail[len(tail)-distillRawOutputTailBytes:]
		}
		sources = append(sources, tail)
	}
	out := make([]distillObs, 0, 4)
	scanned := 0
	for _, src := range sources {
		for _, line := range strings.Split(src, "\n") {
			if scanned >= distillMaxScanLines {
				return out
			}
			scanned++
			line = strings.TrimSpace(line)
			// Skip blanks, over-long lines, and the structured result envelope
			// (raw outputs carry the gitmoot_result JSON) so distill mines genuine
			// human-readable error lines, never a minified JSON brick.
			if line == "" || len(line) > distillMaxErrorLineLen || strings.Contains(line, "gitmoot_result") {
				continue
			}
			if !errorLineRe.MatchString(line) {
				continue
			}
			cleaned := cleanErrorLine(line)
			sig := memory.Slug(cleaned)
			if sig == "" || sig == "untitled" {
				continue
			}
			out = append(out, distillObs{
				Key:     "distill-error:" + sig,
				Content: truncateForContent("A job in this repository hit the error: " + cleaned),
			})
		}
	}
	return out
}

// errorLineRe matches lines that carry a NAMED error token. It is deliberately
// conservative — anchored on unambiguous error markers — so ordinary prose is not
// swept in.
var errorLineRe = regexp.MustCompile(`(?i)(\bpanic:|\bfatal:|\berror:|\bexception\b|\bexit status \d|\bFAIL\b|\bfailed to\b|\bcannot\b|\bno such\b|\bunable to\b|\btimed out\b|\bnil pointer\b|\bsegfault\b)`)

var (
	// distillHexAddr matches 0x-prefixed addresses.
	distillHexAddr = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	// distillLongHex matches sha-like / long hex runs.
	distillLongHex = regexp.MustCompile(`\b[0-9a-fA-F]{7,}\b`)
	// distillTimestamp matches ISO-ish / clock timestamps.
	distillTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[t ]?\d{2}:\d{2}:\d{2}(\.\d+)?z?\b`)
	// distillDuration matches Go test durations like "(0.03s)" and "1.234s".
	distillDuration = regexp.MustCompile(`\(?\b\d+(\.\d+)?(ms|µs|us|ns|s|m|h)\b\)?`)
	// distillPath matches absolute/relative file paths and path-y line:col suffixes.
	distillPath = regexp.MustCompile(`(?:\.?/[\w.\-/]+)|(?::\d+(?::\d+)?)`)
	// distillNumber matches standalone number runs (line numbers, counts, PIDs).
	distillNumber = regexp.MustCompile(`\b\d+\b`)
	// distillWS collapses whitespace runs.
	distillWS = regexp.MustCompile(`\s+`)
)

// cleanErrorLine normalizes one error line to a stable, closed-category form: it
// lowercases and strips the volatile parts (addresses, hashes, timestamps,
// durations, paths, line numbers) that would otherwise make an identical error
// look different across jobs. The order matters — path/hash stripping runs before
// the generic number strip so a "/x/y:42" turns into "" not a bare "42".
func cleanErrorLine(line string) string {
	s := strings.ToLower(line)
	s = distillHexAddr.ReplaceAllString(s, "")
	s = distillTimestamp.ReplaceAllString(s, "")
	s = distillDuration.ReplaceAllString(s, " ")
	s = distillPath.ReplaceAllString(s, " ")
	s = distillLongHex.ReplaceAllString(s, "")
	s = distillNumber.ReplaceAllString(s, "")
	s = distillWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	// distillTestDuration strips a Go test duration suffix like " (0.03s)".
	distillTestDuration = regexp.MustCompile(`\s*\(\d+(\.\d+)?s\)\s*$`)
	// distillTestStatus strips a trailing PASS/FAIL/SKIP status word.
	distillTestStatus = regexp.MustCompile(`(?i)[\s:-]*\b(pass|fail|skip|ok)\b\s*$`)
	// distillTestLineCol strips a :line[:col] suffix from a test/file identifier.
	distillTestLineCol = regexp.MustCompile(`:\d+(:\d+)?`)
)

// normalizeTestName reduces a raw test identifier to a stable, bounded key
// fragment. It strips the volatile bits a test harness appends (durations, status
// words, line:col, hashes) and slugs the remainder, so the SAME test always maps
// to the SAME key while incidental values can never inflate cardinality.
func normalizeTestName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = distillTestDuration.ReplaceAllString(s, "")
	s = distillTestStatus.ReplaceAllString(s, "")
	s = distillHexAddr.ReplaceAllString(s, "")
	s = distillTimestamp.ReplaceAllString(s, "")
	// Strip a :line[:col] suffix but keep the test/file identity before it.
	s = distillTestLineCol.ReplaceAllString(s, "")
	slug := memory.Slug(s)
	if slug == "untitled" {
		return ""
	}
	return slug
}

// truncateForContent caps a distilled observation's content to a bounded length
// on a rune boundary, appending an ellipsis when it had to cut.
func truncateForContent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= distillContentMaxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= distillContentMaxLen {
		return s
	}
	return strings.TrimSpace(string(r[:distillContentMaxLen])) + "…"
}
