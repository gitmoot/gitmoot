package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestWriteDelegationArtifactsWritesBriefAndManifest(t *testing.T) {
	root := t.TempDir()
	result := &AgentResult{
		Decision:     "approved",
		Summary:      "coordinated",
		ArtifactBody: "# Brief\n\nShared context for the batch.\n",
		Delegations: []Delegation{
			{ID: "api", Agent: "builder-a", Action: "implement", Worktree: "api-work", Artifacts: []string{"brief.md"}},
			{ID: "ui", Agent: "builder-b", Action: "implement"},
		},
	}

	dir, err := writeDelegationArtifacts(root, "parent-job", result)
	if err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}
	wantDir := filepath.Join(root, "delegations", "parent-job")
	if dir != wantDir {
		t.Fatalf("dir = %q, want %q", dir, wantDir)
	}

	brief, err := os.ReadFile(filepath.Join(dir, "brief.md"))
	if err != nil {
		t.Fatalf("read brief.md: %v", err)
	}
	if string(brief) != result.ArtifactBody {
		t.Fatalf("brief.md = %q, want %q", string(brief), result.ArtifactBody)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "context-manifest.json"))
	if err != nil {
		t.Fatalf("read context-manifest.json: %v", err)
	}
	var manifest delegationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.ParentJobID != "parent-job" {
		t.Fatalf("manifest parent_job_id = %q, want %q", manifest.ParentJobID, "parent-job")
	}
	if len(manifest.Delegations) != 2 {
		t.Fatalf("manifest delegations = %d, want 2", len(manifest.Delegations))
	}
	first := manifest.Delegations[0]
	if first.ID != "api" || first.Agent != "builder-a" || first.Action != "implement" || first.Worktree != "api-work" || len(first.Deps) != 0 {
		t.Fatalf("manifest entry[0] = %+v", first)
	}
	second := manifest.Delegations[1]
	if second.ID != "ui" || second.Agent != "builder-b" || second.Action != "implement" || second.Worktree != "" || len(second.Deps) != 0 {
		t.Fatalf("manifest entry[1] = %+v", second)
	}
}

func TestWriteDelegationArtifactsManifestOnlyHasReducedFields(t *testing.T) {
	root := t.TempDir()
	result := &AgentResult{
		ArtifactBody: "brief",
		Delegations: []Delegation{
			{ID: "api", Agent: "builder", Action: "implement", Worktree: "w", Deps: []string{"x"},
				Prompt: "secret prompt", Timeout: "5m", Retry: 3, FailurePolicy: "escalate", Artifacts: []string{"brief.md"}},
		},
	}

	dir, err := writeDelegationArtifacts(root, "parent-job", result)
	if err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "context-manifest.json"))
	if err != nil {
		t.Fatalf("read context-manifest.json: %v", err)
	}
	// The manifest must expose only the coordination fields, never the prompt
	// or lifecycle controls.
	var generic struct {
		Delegations []map[string]json.RawMessage `json:"delegations"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(generic.Delegations) != 1 {
		t.Fatalf("manifest delegations = %d, want 1", len(generic.Delegations))
	}
	entry := generic.Delegations[0]
	for _, want := range []string{"id", "agent", "action", "worktree", "deps"} {
		if _, ok := entry[want]; !ok {
			t.Fatalf("manifest entry missing %q: %v", want, entry)
		}
	}
	// The dispatch-time manifest has only the reduced coordination fields because
	// no child has succeeded yet, so the #438 result-reference fields are
	// omitempty-absent. The invariant under test is that the manifest never leaks
	// the PROMPT or any lifecycle control (prompt/timeout/retry/failure_policy);
	// the additive result-reference keys are allowed when populated and (here)
	// absent when no dep has succeeded.
	for got := range entry {
		switch got {
		case "id", "agent", "action", "worktree", "deps":
			// reduced coordination fields (always present in shape)
		case "decision", "summary_preview", "changes_made", "pull_request", "output_path", "derived_from":
			// #438 additive result-reference fields — must NOT appear here (nothing
			// has succeeded at dispatch), but if a future change populates them this
			// is a legitimate enriched key, not a leak.
			t.Fatalf("dispatch-time manifest unexpectedly carries enriched field %q (nothing succeeded yet): %v", got, entry)
		default:
			t.Fatalf("manifest entry leaked field %q: %v", got, entry)
		}
	}
}

func TestWriteDelegationArtifactsEmptyRootWritesNothing(t *testing.T) {
	dir, err := writeDelegationArtifacts("", "parent-job", &AgentResult{
		ArtifactBody: "brief",
		Delegations:  []Delegation{{ID: "api", Agent: "a", Action: "implement", Artifacts: []string{"brief.md"}}},
	})
	if err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}
	if dir != "" {
		t.Fatalf("dir = %q, want empty", dir)
	}
}

func TestWriteDelegationArtifactsNoArtifactsWritesNothing(t *testing.T) {
	root := t.TempDir()
	dir, err := writeDelegationArtifacts(root, "parent-job", &AgentResult{
		ArtifactBody: "brief",
		Delegations: []Delegation{
			{ID: "api", Agent: "a", Action: "implement"},
			{ID: "ui", Agent: "b", Action: "review"},
		},
	})
	if err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}
	if dir != "" {
		t.Fatalf("dir = %q, want empty", dir)
	}
	if entries, err := os.ReadDir(root); err != nil {
		t.Fatalf("read root: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("root has %d entries, want none", len(entries))
	}
}

func TestWriteDelegationArtifactsSanitizesUnsafeParentID(t *testing.T) {
	root := t.TempDir()
	result := &AgentResult{
		ArtifactBody: "brief",
		Delegations:  []Delegation{{ID: "api", Agent: "a", Action: "implement", Artifacts: []string{"brief.md"}}},
	}

	dir, err := writeDelegationArtifacts(root, "parent-job/delegation/del-1", result)
	if err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}

	delegationsRoot := filepath.Join(root, "delegations")
	rel, err := filepath.Rel(delegationsRoot, dir)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if strings.Contains(rel, string(filepath.Separator)) || rel == ".." || strings.HasPrefix(rel, "..") {
		t.Fatalf("sanitized dir %q escapes delegations root %q (rel=%q)", dir, delegationsRoot, rel)
	}
	if _, err := os.Stat(filepath.Join(dir, "brief.md")); err != nil {
		t.Fatalf("brief.md not written into sanitized dir: %v", err)
	}
	// The manifest must still carry the original (unsanitized) parent job id.
	raw, err := os.ReadFile(filepath.Join(dir, "context-manifest.json"))
	if err != nil {
		t.Fatalf("read context-manifest.json: %v", err)
	}
	var manifest delegationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.ParentJobID != "parent-job/delegation/del-1" {
		t.Fatalf("manifest parent_job_id = %q, want original id", manifest.ParentJobID)
	}
}

// --- #438: structured upstream dep references in context-manifest.json ---

// TestBuildEnrichedManifestEntrySucceededDepPopulatesFields pins that a
// delegation with a SUCCEEDED child yields the additive result-reference fields
// (decision/summary_preview/changes_made/pull_request/output_path/derived_from),
// mirroring the #419 prose channel but JSON-encoded.
func TestBuildEnrichedManifestEntrySucceededDepPopulatesFields(t *testing.T) {
	e := Engine{InjectUpstreamDepContext: true, ArtifactRoot: "/home/.gitmoot"}
	children := map[string]db.Job{
		"research": childJobWith(t, "parent/delegation/research", "researcher", "review", JobPayload{
			Repo:        "jerryfane/gitmoot",
			PullRequest: 42,
			Result: &AgentResult{
				Decision:     "approved",
				Summary:      "found three relevant prior arts",
				ChangesMade:  []string{"noted A", "noted B"},
				ArtifactBody: "RESEARCH BRIEF BODY",
			},
		}),
	}
	d := Delegation{ID: "research", Agent: "researcher", Action: "review", Deps: []string{"seed"}}
	entry := buildEnrichedManifestEntry(d, children, nil, e)

	if entry.ID != "research" || entry.Agent != "researcher" || entry.Action != "review" {
		t.Fatalf("reduced fields wrong: %+v", entry)
	}
	if entry.Decision != "approved" {
		t.Fatalf("decision = %q, want approved", entry.Decision)
	}
	if entry.SummaryPreview != "found three relevant prior arts" {
		t.Fatalf("summary_preview = %q", entry.SummaryPreview)
	}
	if entry.ChangesMade != 2 {
		t.Fatalf("changes_made = %d, want 2", entry.ChangesMade)
	}
	if entry.PullRequest != "https://github.com/jerryfane/gitmoot/pull/42" {
		t.Fatalf("pull_request = %q", entry.PullRequest)
	}
	if want := e.inlineBriefPath("parent/delegation/research"); entry.OutputPath != want {
		t.Fatalf("output_path = %q, want %q", entry.OutputPath, want)
	}
	if len(entry.DerivedFrom) != 1 || entry.DerivedFrom[0] != "seed" {
		t.Fatalf("derived_from = %v, want [seed]", entry.DerivedFrom)
	}
}

// TestBuildEnrichedManifestEntryPendingOrFailedStaysReduced pins succeeded-only
// enrichment: a delegation with a pending, failed, or missing child keeps the
// bare reduced shape (all new fields zero/omitted), so omitempty leaves it
// byte-identical to the pre-#438 manifest.
func TestBuildEnrichedManifestEntryPendingOrFailedStaysReduced(t *testing.T) {
	e := Engine{InjectUpstreamDepContext: true, ArtifactRoot: "/home/.gitmoot"}
	cases := map[string]map[string]db.Job{
		"missing child": {},
		"failed child": {
			"x": {ID: "parent/delegation/x", Agent: "a", Type: "review", State: string(JobFailed),
				Payload: mustMarshalPayload(t, JobPayload{Result: &AgentResult{Decision: "failed"}})},
		},
		"queued child": {
			"x": {ID: "parent/delegation/x", Agent: "a", Type: "review", State: string(JobQueued)},
		},
	}
	for name, children := range cases {
		t.Run(name, func(t *testing.T) {
			d := Delegation{ID: "x", Agent: "a", Action: "review", Worktree: "w", Deps: []string{"upstream"}}
			entry := buildEnrichedManifestEntry(d, children, nil, e)
			if entry.ID != "x" || entry.Agent != "a" || entry.Action != "review" || entry.Worktree != "w" {
				t.Fatalf("reduced fields wrong: %+v", entry)
			}
			if len(entry.Deps) != 1 || entry.Deps[0] != "upstream" {
				t.Fatalf("deps = %v, want [upstream]", entry.Deps)
			}
			if entry.Decision != "" || entry.SummaryPreview != "" || entry.ChangesMade != 0 ||
				entry.PullRequest != "" || entry.OutputPath != "" || entry.DerivedFrom != nil {
				t.Fatalf("non-succeeded entry must stay reduced, got: %+v", entry)
			}
		})
	}
}

// TestBuildEnrichedManifestEntryFollowsDedupWinner pins that a delegation deduped
// to a winning sibling (passed via dedupWinners, no child of its own) is enriched
// from the winner's payload, matching buildUpstreamDepBlock's dedup-follow rule.
func TestBuildEnrichedManifestEntryFollowsDedupWinner(t *testing.T) {
	e := Engine{InjectUpstreamDepContext: true, ArtifactRoot: "/home/.gitmoot"}
	winner := childJobWith(t, "parent/delegation/winner", "w", "review", JobPayload{
		Result: &AgentResult{Decision: "approved", Summary: "winner summary"},
	})
	dedupWinners := map[string]db.Job{"dup": winner}
	d := Delegation{ID: "dup", Agent: "w", Action: "review"}
	entry := buildEnrichedManifestEntry(d, map[string]db.Job{}, dedupWinners, e)
	if entry.Decision != "approved" || entry.SummaryPreview != "winner summary" {
		t.Fatalf("deduped entry should be enriched from the winner, got: %+v", entry)
	}
	if want := e.inlineBriefPath("parent/delegation/winner"); entry.OutputPath != want {
		t.Fatalf("output_path = %q, want winner path %q", entry.OutputPath, want)
	}
}

// TestManifestSummaryPreviewTruncatedRuneSafe pins that an oversized multi-byte
// summary is capped at maxUpstreamDepSummaryPreviewBytes, rune-safe, with an
// ellipsis, and the full summary is not present.
func TestManifestSummaryPreviewTruncatedRuneSafe(t *testing.T) {
	e := Engine{InjectUpstreamDepContext: true, ArtifactRoot: "/home/.gitmoot"}
	summary := strings.Repeat("世", 200) // 600 bytes, overruns the 280-byte cap
	children := map[string]db.Job{
		"d1": childJobWith(t, "parent/delegation/d1", "d", "review", JobPayload{
			Result: &AgentResult{Decision: "approved", Summary: summary},
		}),
	}
	d := Delegation{ID: "d1", Agent: "d", Action: "review"}
	entry := buildEnrichedManifestEntry(d, children, nil, e)
	if !utf8.ValidString(entry.SummaryPreview) {
		t.Fatalf("summary_preview split a UTF-8 rune: %q", entry.SummaryPreview)
	}
	if len(entry.SummaryPreview) > maxUpstreamDepSummaryPreviewBytes+len("…") {
		t.Fatalf("summary_preview = %d bytes, want <= cap+ellipsis", len(entry.SummaryPreview))
	}
	if !strings.HasSuffix(entry.SummaryPreview, "…") {
		t.Fatalf("expected ellipsis on a truncated preview: %q", entry.SummaryPreview)
	}
	if entry.SummaryPreview == summary || strings.Contains(entry.SummaryPreview, strings.Repeat("世", 200)) {
		t.Fatalf("full oversized summary leaked into the preview")
	}
}

// TestAugmentDelegationManifestRoundTrip writes the base manifest, then augments
// with a children map containing one succeeded dep, re-reads context-manifest.json
// and asserts the enriched fields appear ONLY on the succeeded entry while the
// pending sibling stays reduced.
func TestAugmentDelegationManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	e := Engine{InjectUpstreamDepContext: true, ArtifactRoot: root}
	result := &AgentResult{
		ArtifactBody: "shared brief",
		Delegations: []Delegation{
			{ID: "research", Agent: "researcher", Action: "review", Artifacts: []string{"brief.md"}},
			{ID: "write", Agent: "writer", Action: "review", Deps: []string{"research"}},
		},
	}
	// Base manifest at dispatch (reduced shape, nothing succeeded yet).
	if _, err := writeDelegationArtifacts(root, "parent-job", result); err != nil {
		t.Fatalf("writeDelegationArtifacts returned error: %v", err)
	}

	children := map[string]db.Job{
		"research": childJobWith(t, "parent-job/delegation/research", "researcher", "review", JobPayload{
			Repo:        "jerryfane/gitmoot",
			PullRequest: 7,
			Result: &AgentResult{
				Decision:    "approved",
				Summary:     "RESEARCH_SUMMARY",
				ChangesMade: []string{"a", "b", "c"},
			},
		}),
		// write is still queued (no entry / not succeeded).
	}
	dir, err := augmentDelegationManifest(root, "parent-job", result, children, nil, e)
	if err != nil {
		t.Fatalf("augmentDelegationManifest returned error: %v", err)
	}
	if dir != filepath.Join(root, "delegations", "parent-job") {
		t.Fatalf("augment dir = %q", dir)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "context-manifest.json"))
	if err != nil {
		t.Fatalf("read context-manifest.json: %v", err)
	}
	var manifest delegationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest not valid JSON: %v", err)
	}
	if len(manifest.Delegations) != 2 {
		t.Fatalf("manifest delegations = %d, want 2", len(manifest.Delegations))
	}
	research, write := manifest.Delegations[0], manifest.Delegations[1]
	if research.ID != "research" || write.ID != "write" {
		t.Fatalf("manifest order changed: %+v", manifest.Delegations)
	}
	// Succeeded entry is enriched.
	if research.Decision != "approved" || research.SummaryPreview != "RESEARCH_SUMMARY" || research.ChangesMade != 3 {
		t.Fatalf("research entry not enriched: %+v", research)
	}
	if research.PullRequest != "https://github.com/jerryfane/gitmoot/pull/7" {
		t.Fatalf("research pull_request = %q", research.PullRequest)
	}
	if want := e.inlineBriefPath("parent-job/delegation/research"); research.OutputPath != want {
		t.Fatalf("research output_path = %q, want %q", research.OutputPath, want)
	}
	// Pending sibling stays reduced.
	if write.Decision != "" || write.SummaryPreview != "" || write.ChangesMade != 0 ||
		write.PullRequest != "" || write.OutputPath != "" {
		t.Fatalf("pending write entry must stay reduced: %+v", write)
	}
}

// TestAugmentDelegationManifestNoOpWhenRootEmptyOrNoArtifacts pins that the
// augmenting writer is a no-op (no dir, no write) when there is no artifact root
// or no delegation requested artifacts, mirroring writeDelegationArtifacts.
func TestAugmentDelegationManifestNoOpWhenRootEmptyOrNoArtifacts(t *testing.T) {
	e := Engine{InjectUpstreamDepContext: true}
	result := &AgentResult{
		ArtifactBody: "b",
		Delegations:  []Delegation{{ID: "a", Agent: "x", Action: "review"}}, // no Artifacts
	}
	if dir, err := augmentDelegationManifest("", "parent", result, nil, nil, e); err != nil || dir != "" {
		t.Fatalf("empty root: dir=%q err=%v, want empty/no error", dir, err)
	}
	root := t.TempDir()
	if dir, err := augmentDelegationManifest(root, "parent", result, nil, nil, e); err != nil || dir != "" {
		t.Fatalf("no-artifacts: dir=%q err=%v, want empty/no error", dir, err)
	}
	if entries, err := os.ReadDir(root); err != nil {
		t.Fatalf("read root: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("no-artifacts augment wrote %d entries, want none", len(entries))
	}
}

// mustMarshalPayload is a test helper for building a db.Job.Payload inline.
func mustMarshalPayload(t *testing.T, payload JobPayload) string {
	t.Helper()
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	return encoded
}
