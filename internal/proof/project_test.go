package proof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestProjectManifestOverJobDAG(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	manifest := Project(root, jobs, results, receipts, events)
	if err := VerifyManifest(manifest); err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if manifest.ProofID == "" || manifest.Root.ID != manifest.ProofID || manifest.Root.Kind != KindRoot {
		t.Fatalf("root manifest = %+v", manifest)
	}
	wantKinds := map[Kind]bool{
		KindRoot: true, KindSession: true, KindDelegation: true, KindCommit: true,
		KindTest: true, KindReview: true, KindPR: true,
	}
	for _, node := range manifest.Nodes {
		delete(wantKinds, node.Kind)
	}
	if len(wantKinds) != 0 {
		t.Fatalf("missing node kinds: %v", wantKinds)
	}
	var delegationLinksSession bool
	for _, node := range manifest.Nodes {
		if node.Kind != KindDelegation {
			continue
		}
		for _, child := range node.Children {
			if manifest.Nodes[child].Kind == KindSession {
				delegationLinksSession = true
			}
		}
	}
	if !delegationLinksSession {
		t.Fatal("delegation nodes do not link to child session hashes")
	}
	first, err := Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	secondManifest := Project(root, jobs, results, receipts, events)
	second, err := Marshal(secondManifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProofID != secondManifest.ProofID || !bytes.Equal(first, second) {
		t.Fatalf("projection is not byte-stable:\n%s\n%s", first, second)
	}
}

func TestGradingReportedObservedVerified(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	manifest := Project(root, jobs, results, receipts, events)
	assertClaim(t, manifest, KindTest, "test.run", GradeReported)
	assertClaim(t, manifest, KindReview, "review.independent_approved", GradeObserved)
	assertClaim(t, manifest, KindCommit, "integrity.result_hash", GradeVerified)
	assertClaim(t, manifest, KindRoot, "integrity.delegation_dag", GradeVerified)
	if tally := GradeTally(manifest); tally[GradeVerified] != 0 {
		t.Fatalf("substantive verified tally = %d, want 0; integrity claims must be separate", tally[GradeVerified])
	}
	if nodes, resultHashes := integrityTally(manifest); nodes != len(manifest.Nodes) || resultHashes != 3 {
		t.Fatalf("integrity tally = nodes %d result_hashes %d, want %d and 3", nodes, resultHashes, len(manifest.Nodes))
	}

	selfRoot, selfJobs, selfResults, selfReceipts, selfEvents := proofFixture(t, true)
	selfManifest := Project(selfRoot, selfJobs, selfResults, selfReceipts, selfEvents)
	assertClaim(t, selfManifest, KindReview, "review.self_approved", GradeObserved)
	for _, node := range selfManifest.Nodes {
		if node.Kind != KindReview {
			continue
		}
		if node.Attrs["independent"] != "false" {
			t.Fatalf("self review independent = %q, want false", node.Attrs["independent"])
		}
		for _, claim := range node.Claims {
			if strings.HasPrefix(claim.Type, "review.") && claim.Grade == GradeVerified {
				t.Fatalf("self review received verified review claim: %+v", claim)
			}
		}
	}
}

func TestProjectGradesDiffCorroboratedChangeObserved(t *testing.T) {
	result := &workflow.AgentResult{
		Decision: "implemented", Summary: "changed result checks",
		ChangesMade: []string{"updated internal/workflow/result_checks.go"},
	}
	observation := &workflow.ResultObservation{
		Source:       workflow.ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []workflow.ChangeObservation{{
			Claim:        result.ChangesMade[0],
			ClaimedFiles: []string{"internal/workflow/result_checks.go"},
			Observation:  []string{"internal/workflow/result_checks.go"},
			Grade:        GradeObserved,
			Divergent:    false,
		}},
		ClaimedOnlyFiles: []string{},
		UnclaimedFiles:   []string{},
		UnboundClaims:    []string{},
		Divergent:        false,
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Result: result, ResultObservation: observation}
	root := db.Job{
		ID: "observed-change", RootID: "observed-change", Agent: "implementer",
		Type: "implement", State: "succeeded", UpdatedAt: "2026-07-29 12:00:00",
		Payload: proofPayload(t, payload),
	}
	manifest := Project(root, []db.Job{root}, map[string]*workflow.AgentResult{root.ID: result}, nil, nil)
	assertClaim(t, manifest, KindCommit, "change", GradeObserved)
}

func TestProjectDoesNotPublishMismatchedPathBindingAsObserved(t *testing.T) {
	result := &workflow.AgentResult{
		Decision: "implemented", Summary: "changed result checks",
		ChangesMade: []string{"updated docs/result_checks.go"},
	}
	observation := &workflow.ResultObservation{
		Source:       workflow.ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []workflow.ChangeObservation{{
			Claim:        result.ChangesMade[0],
			ClaimedFiles: []string{"docs/result_checks.go"},
			Observation:  []string{"internal/workflow/result_checks.go"},
			Grade:        GradeObserved,
		}},
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Result: result, ResultObservation: observation}
	root := db.Job{
		ID: "mismatched-observed-change", RootID: "mismatched-observed-change", Agent: "implementer",
		Type: "implement", State: "succeeded", UpdatedAt: "2026-07-29 12:00:00",
		Payload: proofPayload(t, payload),
	}
	manifest := Project(root, []db.Job{root}, map[string]*workflow.AgentResult{root.ID: result}, nil, nil)
	assertClaim(t, manifest, KindCommit, "change", GradeReported)
}

func TestProjectDoesNotPublishObservedBindingAbsentFromTouchedFiles(t *testing.T) {
	result := &workflow.AgentResult{
		Decision: "implemented", Summary: "changed result checks",
		ChangesMade: []string{"updated docs/result_checks.go"},
	}
	observation := &workflow.ResultObservation{
		Source:       workflow.ResultObservationSourceWorktreeDiff,
		TouchedFiles: []string{"internal/workflow/result_checks.go"},
		Changes: []workflow.ChangeObservation{{
			Claim:        result.ChangesMade[0],
			ClaimedFiles: []string{"docs/result_checks.go"},
			Observation:  []string{"docs/result_checks.go"},
			Grade:        GradeObserved,
		}},
	}
	payload := workflow.JobPayload{Repo: "owner/repo", Result: result, ResultObservation: observation}
	root := db.Job{
		ID: "observed-change-absent-from-diff", RootID: "observed-change-absent-from-diff", Agent: "implementer",
		Type: "implement", State: "succeeded", UpdatedAt: "2026-07-29 12:00:00",
		Payload: proofPayload(t, payload),
	}
	manifest := Project(root, []db.Job{root}, map[string]*workflow.AgentResult{root.ID: result}, nil, nil)
	assertClaim(t, manifest, KindCommit, "change", GradeReported)
}

func TestManifestContentAddressStable(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	first := Project(root, jobs, results, receipts, events)
	second := Project(root, jobs, results, receipts, events)
	if first.ProofID != second.ProofID {
		t.Fatalf("same input proof ids differ: %s != %s", first.ProofID, second.ProofID)
	}
	mutated := first.Root
	mutated.Attrs = cloneAttrs(mutated.Attrs)
	mutated.Attrs["root_id"] = "tampered"
	if NodeID(mutated) == first.Root.ID {
		t.Fatal("mutating an attribute did not change the node content address")
	}
	first.Root = mutated
	if err := VerifyManifest(first); err == nil {
		t.Fatal("tampered manifest passed hash verification")
	}

	orphanManifest := second
	orphanManifest.Nodes = cloneNodes(second.Nodes)
	beforeObserved := GradeTally(orphanManifest)[GradeObserved]
	orphan := Node{
		Kind: KindTest, Ref: "orphan", Attrs: map[string]string{"command": "fabricated"},
		Children: []string{}, Claims: []Claim{{
			Type: "test.run", Grade: GradeObserved, Source: "orphan", EvidenceRef: "orphan", AsOf: "2026-07-17 02:00:00",
		}},
	}
	orphan.ID = NodeID(orphan)
	orphanManifest.Nodes[orphan.ID] = orphan
	if orphanManifest.ProofID != second.ProofID {
		t.Fatal("adding an orphan unexpectedly changed ProofID")
	}
	if err := VerifyManifest(orphanManifest); err == nil || !strings.Contains(err.Error(), "unreachable node") {
		t.Fatalf("orphan manifest verification error = %v, want unreachable-node rejection", err)
	}
	if got := GradeTally(orphanManifest)[GradeObserved]; got != beforeObserved {
		t.Fatalf("orphan inflated observed tally from %d to %d", beforeObserved, got)
	}
}

func TestArtifactNodesBindDigestsWithoutInflatingEvidenceGrade(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	base := Project(root, jobs, results, receipts, events)
	beforeTally := GradeTally(base)
	manifest, err := WithArtifactNodes(base, []ArtifactEvidence{
		{Relpath: "artifacts/build/kit.txt", Stage: "build", Size: 9, SHA256: strings.Repeat("ab", 32)},
	}, "2026-07-17T15:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(manifest); err != nil {
		t.Fatalf("artifact manifest verification: %v", err)
	}
	entries, err := ArtifactEntries(manifest)
	if err != nil || len(entries) != 1 || entries[0].Relpath != "artifacts/build/kit.txt" || entries[0].Size != 9 || entries[0].SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("artifact entries=%+v err=%v", entries, err)
	}
	if got := GradeTally(manifest); got[GradeReported] != beforeTally[GradeReported] || got[GradeObserved] != beforeTally[GradeObserved] || got[GradeVerified] != beforeTally[GradeVerified] {
		t.Fatalf("artifact integrity inflated substantive grades: before=%v after=%v", beforeTally, got)
	}
	var artifactNode Node
	for _, node := range manifest.Nodes {
		if node.Kind == KindArtifact {
			artifactNode = node
		}
	}
	if artifactNode.ID == "" {
		t.Fatal("artifact node is absent")
	}
	foundClaim := false
	for _, claim := range artifactNode.Claims {
		if claim.Type == "integrity.artifact_sha256" && claim.Grade == GradeVerified {
			foundClaim = true
		}
	}
	if !foundClaim {
		t.Fatalf("artifact digest integrity claim absent: %+v", artifactNode.Claims)
	}
	var rendered bytes.Buffer
	if err := RenderTree(&rendered, manifest); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "artifacts/build/kit.txt · 9 bytes · sha256:"+strings.Repeat("ab", 32)+" [verified integrity]") {
		t.Fatalf("rendered tree omits artifact digest:\n%s", rendered.String())
	}
	tampered := manifest
	tampered.Nodes = cloneNodes(manifest.Nodes)
	node := tampered.Nodes[artifactNode.ID]
	node.Attrs = cloneAttrs(node.Attrs)
	node.Attrs["sha256"] = strings.Repeat("cd", 32)
	tampered.Nodes[artifactNode.ID] = node
	if err := VerifyManifest(tampered); err == nil {
		t.Fatal("tampered artifact digest passed manifest verification")
	}
}

func TestReviewIndependenceRequiresNormalizedNonEmptyAgents(t *testing.T) {
	for _, tc := range []struct {
		name, reviewer, wantClaim string
		wantIndependent           string
	}{
		{name: "empty reviewer is unknown", reviewer: "", wantClaim: "review.approved", wantIndependent: ""},
		{name: "whitespace difference is self", reviewer: "implementer ", wantClaim: "review.self_approved", wantIndependent: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, jobs, results, receipts, events := proofFixture(t, false)
			for i := range jobs {
				if jobs[i].Type == "review" {
					jobs[i].Agent = tc.reviewer
				}
			}
			manifest := Project(root, jobs, results, receipts, events)
			assertClaim(t, manifest, KindReview, tc.wantClaim, GradeObserved)
			for _, node := range manifest.Nodes {
				if node.Kind != KindReview {
					continue
				}
				if got := node.Attrs["independent"]; got != tc.wantIndependent {
					t.Fatalf("independent attr = %q, want %q", got, tc.wantIndependent)
				}
				for _, claim := range node.Claims {
					if claim.Type == "review.independent_approved" {
						t.Fatalf("reviewer %q was mislabeled independent", tc.reviewer)
					}
				}
			}
		})
	}
}

// #1685. The projector's job is to certify what happened, so a coordinator
// fan-out must not produce an OBSERVED approval claim: that turns an
// announcement nobody answered into a positive audit artifact. The panelists'
// own review jobs are projected separately and carry the real claims.
func TestReviewFanOutProducesNoApprovalClaim(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	for _, result := range results {
		if result.Decision != "approved" {
			continue
		}
		result.Delegations = []workflow.Delegation{
			{ID: "lens-a", Agent: "r1", Action: "review"},
			{ID: "lens-b", Agent: "r2", Action: "review"},
		}
	}
	manifest := Project(root, jobs, results, receipts, events)
	reviewNodes := 0
	for _, node := range manifest.Nodes {
		if node.Kind != KindReview {
			continue
		}
		reviewNodes++
		for _, claim := range node.Claims {
			if strings.HasPrefix(claim.Type, "review.") && strings.Contains(claim.Type, "approved") {
				t.Fatalf("fan-out review produced approval claim %q", claim.Type)
			}
		}
		if node.Attrs["review_outcome"] != "fan-out (no verdict)" {
			t.Fatalf("review_outcome = %q, want the fan-out label", node.Attrs["review_outcome"])
		}
		if node.Attrs["fan_out_delegations"] != "2" {
			t.Fatalf("fan_out_delegations = %q, want 2", node.Attrs["fan_out_delegations"])
		}
	}
	if reviewNodes == 0 {
		t.Fatal("fixture produced no review node, so this test proves nothing")
	}
}

func TestApprovedWithNotesProofUsesDurableOutcomeEvent(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	results["review-job"] = &workflow.AgentResult{
		Decision: "changes_requested",
		Severity: "P2",
		Summary:  "non-blocking polish",
	}

	withoutEvent := Project(root, jobs, results, receipts, events)
	for _, node := range withoutEvent.Nodes {
		if node.Kind != KindReview {
			continue
		}
		if node.Attrs["decision"] != "changes_requested" {
			t.Fatalf("raw review decision = %q, want changes_requested", node.Attrs["decision"])
		}
		if node.Attrs["effective_decision"] != "" {
			t.Fatalf("review without durable outcome event gained effective decision %q", node.Attrs["effective_decision"])
		}
		for _, claim := range node.Claims {
			if claim.Type == "review.independent_approved" {
				t.Fatal("review without durable outcome event gained an approval claim")
			}
		}
	}

	events["review-job"] = []db.JobEvent{{
		JobID: "review-job", Kind: workflow.ReviewApprovedWithNotesEventKind,
		Message:   "review severity P2 is below repository blocking severity P1",
		CreatedAt: "2026-07-17 01:09:30",
	}}
	withEvent := Project(root, jobs, results, receipts, events)
	assertClaim(t, withEvent, KindReview, "review.independent_approved", GradeObserved)
	for _, node := range withEvent.Nodes {
		if node.Kind != KindReview {
			continue
		}
		if node.Attrs["decision"] != "changes_requested" {
			t.Fatalf("approved-with-notes raw decision = %q, want changes_requested", node.Attrs["decision"])
		}
		if node.Attrs["effective_decision"] != "approved" ||
			node.Attrs["review_outcome"] != "approved-with-notes" {
			t.Fatalf("approved-with-notes attrs = %+v", node.Attrs)
		}
		for _, claim := range node.Claims {
			if claim.Type == "review.independent_approved" &&
				claim.Source != "job_event."+workflow.ReviewApprovedWithNotesEventKind {
				t.Fatalf("approved-with-notes claim source = %q", claim.Source)
			}
		}
	}

	// The rendered tree is the surface a human reads before merging, so it must
	// not contradict the claim graph it was built from: an approval claim with a
	// "0 approved" summary is a false negative in exactly that direction.
	var rendered bytes.Buffer
	if err := RenderTree(&rendered, withEvent); err != nil {
		t.Fatalf("RenderTree returned error: %v", err)
	}
	out := rendered.String()
	if !strings.Contains(out, "1 reviews / 1 approved") {
		t.Fatalf("approved-with-notes render did not count the approval:\n%s", out)
	}
	if !strings.Contains(out, "changes_requested (approved-with-notes)") {
		t.Fatalf("approved-with-notes render lost the raw verdict or the outcome:\n%s", out)
	}

	// Without the durable event the same review must still render as raw
	// changes_requested and count zero approvals.
	rendered.Reset()
	if err := RenderTree(&rendered, withoutEvent); err != nil {
		t.Fatalf("RenderTree returned error: %v", err)
	}
	if raw := rendered.String(); !strings.Contains(raw, "1 reviews / 0 approved") ||
		strings.Contains(raw, "approved-with-notes") {
		t.Fatalf("blocking review render = \n%s", raw)
	}
}

func TestProjectHonestGapsRenderDash(t *testing.T) {
	result := &workflow.AgentResult{Decision: "implemented", Summary: "done"}
	root := db.Job{
		ID: "root-gap", RootID: "root-gap", Agent: "impl", Type: "implement",
		State: "succeeded", UpdatedAt: "2026-07-17 01:00:00",
		Payload: proofPayload(t, workflow.JobPayload{Repo: "owner/repo", Result: result}),
	}
	manifest := Project(root, []db.Job{root}, map[string]*workflow.AgentResult{root.ID: result}, nil, nil)
	var out bytes.Buffer
	if err := RenderTree(&out, manifest); err != nil {
		t.Fatalf("RenderTree: %v", err)
	}
	for _, want := range []string{
		"lineage: -", "commits: -", "tests: - (CI verification deferred)",
		"reviews: -", "PR: -", "PR -",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tree missing honest gap %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "#0") {
		t.Fatalf("tree fabricated PR #0:\n%s", out.String())
	}
}

func proofFixture(t *testing.T, selfReview bool) (db.Job, []db.Job, map[string]*workflow.AgentResult, []PRReceipt, map[string][]db.JobEvent) {
	t.Helper()
	rootResult := &workflow.AgentResult{
		Decision: "implemented", Summary: "implemented proof",
		ChangesMade: []string{"added projection"}, TestsRun: []string{"go test ./..."},
		Delegations: []workflow.Delegation{
			{ID: "child", Agent: "child-agent", Action: "implement", Prompt: "implement child", SynthesisRule: "summary"},
			{ID: "review", Agent: "reviewer", Action: "review", Prompt: "review it", Deps: []string{"child"}, SynthesisRule: "verify"},
		},
	}
	childResult := &workflow.AgentResult{Decision: "implemented", Summary: "child done", ChangesMade: []string{"changed child"}}
	reviewResult := &workflow.AgentResult{
		Decision: "approved", Summary: "approved",
		Findings: []json.RawMessage{json.RawMessage(`{"severity":"note"}`)},
	}
	reviewer := "reviewer"
	if selfReview {
		reviewer = "implementer"
	}
	rootPayload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42", Result: rootResult,
	}
	childPayload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42",
		RootJobID: "root", ParentJobID: "root", DelegationID: "child", DelegationDepth: 1, DelegatedBy: "implementer", Result: childResult,
	}
	reviewPayload := workflow.JobPayload{
		Repo: "owner/repo", PullRequest: 42, HeadSHA: "abc123", WorkflowID: "proof/42",
		RootJobID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1, DelegatedBy: "implementer", Result: reviewResult,
	}
	root := db.Job{
		ID: "root", RootID: "root", Agent: "implementer", Type: "implement", State: "succeeded",
		Model: "gpt-5.6", Runtime: "codex", RuntimeRef: "session-root", Repo: "owner/repo", PullRequest: 42,
		InputTokens: 100, OutputTokens: 50, CreatedAt: "2026-07-17 01:00:00", UpdatedAt: "2026-07-17 01:10:00",
		Payload: proofPayload(t, rootPayload),
	}
	root.ResultHash = resultHashForPayload(t, root.Payload)
	child := db.Job{
		ID: "child-job", RootID: "root", ParentJobID: "root", DelegationID: "child", DelegationDepth: 1, DelegatedBy: "implementer",
		Agent: "child-agent", Type: "implement", State: "succeeded", Model: "gpt-5.6-mini", Runtime: "codex",
		Repo: "owner/repo", PullRequest: 42, CreatedAt: "2026-07-17 01:01:00", UpdatedAt: "2026-07-17 01:08:00",
		Payload: proofPayload(t, childPayload),
	}
	child.ResultHash = resultHashForPayload(t, child.Payload)
	review := db.Job{
		ID: "review-job", RootID: "root", ParentJobID: "root", DelegationID: "review", DelegationDepth: 1, DelegatedBy: "implementer",
		Agent: reviewer, Type: "review", State: "succeeded", Model: "gpt-5.6", Runtime: "codex",
		Repo: "owner/repo", PullRequest: 42, CreatedAt: "2026-07-17 01:08:00", UpdatedAt: "2026-07-17 01:09:00",
		Payload: proofPayload(t, reviewPayload),
	}
	review.ResultHash = resultHashForPayload(t, review.Payload)
	receipts := []PRReceipt{{
		Repo: "owner/repo", Number: 42, HeadSHA: "abc123", MergeCommitSHA: "def456", State: "merged",
		OpenedAt: "2026-07-17 01:02:00", MergedAt: "2026-07-17 01:11:00",
	}}
	events := map[string][]db.JobEvent{
		"root": {{JobID: "root", Kind: "succeeded", Message: "done", CreatedAt: "2026-07-17 01:10:00"}},
	}
	return root, []db.Job{review, root, child}, map[string]*workflow.AgentResult{
		"root": rootResult, "child-job": childResult, "review-job": reviewResult,
	}, receipts, events
}

func proofPayload(t *testing.T, payload workflow.JobPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func resultHashForPayload(t *testing.T, payload string) string {
	t.Helper()
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, envelope.Result); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(compact.Bytes())
	return hex.EncodeToString(sum[:])
}

func assertClaim(t *testing.T, manifest Manifest, kind Kind, claimType string, grade Grade) {
	t.Helper()
	for _, node := range manifest.Nodes {
		if node.Kind != kind {
			continue
		}
		for _, claim := range node.Claims {
			if claim.Type == claimType && claim.Grade == grade {
				return
			}
		}
	}
	t.Fatalf("missing %s claim %q with grade %s", kind, claimType, grade)
}

func cloneAttrs(attrs map[string]string) map[string]string {
	out := make(map[string]string, len(attrs))
	for key, value := range attrs {
		out[key] = value
	}
	return out
}

func cloneNodes(nodes map[string]Node) map[string]Node {
	out := make(map[string]Node, len(nodes))
	for id, node := range nodes {
		out[id] = node
	}
	return out
}

// #1685 P2. Suppressing the approval CLAIM was not enough: the renderer counts a
// raw "approved" whenever no effective decision is carried, so the proof summary
// still reported one approved review for a coordinator announcement. Absence is
// not a classification. Asserted end to end, projector THROUGH renderer, because
// the projector-only test passed while the rendered output was wrong.
func TestReviewFanOutRendersAsNoVerdictAndIsNotCounted(t *testing.T) {
	root, jobs, results, receipts, events := proofFixture(t, false)
	for _, result := range results {
		if result.Decision != "approved" {
			continue
		}
		result.Delegations = []workflow.Delegation{
			{ID: "lens-a", Agent: "r1", Action: "review"},
			{ID: "lens-b", Agent: "r2", Action: "review"},
		}
	}
	manifest := Project(root, jobs, results, receipts, events)

	var rendered strings.Builder
	if err := RenderTree(&rendered, manifest); err != nil {
		t.Fatalf("RenderTree: %v", err)
	}
	out := rendered.String()
	if !strings.Contains(out, "no verdict") || !strings.Contains(out, "fan-out") {
		t.Fatalf("rendered proof hides the fan-out classification:\n%s", out)
	}
	if strings.Contains(out, "1 approved") {
		t.Fatalf("a fan-out was counted as an approved review:\n%s", out)
	}
}
