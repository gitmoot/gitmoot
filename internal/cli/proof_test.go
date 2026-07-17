package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/proof"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunProofGradedTreeAndJSON(t *testing.T) {
	home := t.TempDir()
	seedCLIProof(t, home)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"proof", "--home", home, "root-proof"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("proof exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"root root-proof [verified]", "session root-proof", "implementer · codex · gpt-5.6",
		"lineage:", "go test ./internal/proof/ [reported; CI verification deferred]",
		"reviewer · approved · findings 1 · independent [observed]",
		"#17 · head abc123", "grades: reported=", "observed=", "verified=0",
		"integrity: all ", "result_hashes matched",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("graded tree missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"proof", "--json", "--home", home, "root-proof"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("proof --json exit code = %d, stderr=%s", code, stderr.String())
	}
	firstJSON := append([]byte(nil), stdout.Bytes()...)
	var manifest proof.Manifest
	if err := json.Unmarshal(bytes.TrimSpace(firstJSON), &manifest); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, firstJSON)
	}
	if err := proof.VerifyManifest(manifest); err != nil {
		t.Fatalf("verify JSON manifest: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"proof", "--json", "--home", home, "root-proof"}, &stdout, &stderr)
	if code != 0 || !bytes.Equal(firstJSON, stdout.Bytes()) {
		t.Fatalf("proof --json is not byte-stable: code=%d\nfirst=%s\nsecond=%s\nstderr=%s", code, firstJSON, stdout.Bytes(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"proof", "--home", home, "unknown-root"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), `unknown root-id "unknown-root"`) {
		t.Fatalf("unknown root code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunProofDoesNotBorrowMergedReceiptAcrossPRs(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	for _, fixture := range []struct {
		id  string
		pr  int
		sha string
	}{
		{id: "merged-pr-root", pr: 17, sha: "aaa111"},
		{id: "open-pr-root", pr: 18, sha: "bbb222"},
	} {
		seedCLIJob(t, store, db.Job{
			ID: fixture.id, Agent: "impl", Type: "implement", State: string(workflow.JobSucceeded),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo: "owner/repo", PullRequest: fixture.pr, HeadSHA: fixture.sha, WorkflowID: "proof/shared",
			}),
		}, "done")
	}
	for _, pr := range []db.PullRequest{
		{RepoFullName: "owner/repo", Number: 17, HeadSHA: "aaa111", MergeCommitSHA: "merged17", State: "merged"},
		// A stale merge SHA on the PR row is not a daemon merge receipt and must
		// not become merged evidence for this still-open PR.
		{RepoFullName: "owner/repo", Number: 18, HeadSHA: "bbb222", MergeCommitSHA: "stale18", State: "open"},
	} {
		if err := store.UpsertPullRequest(ctx, pr); err != nil {
			t.Fatalf("UpsertPullRequest: %v", err)
		}
	}
	for _, body := range []string{
		"[auto:pr:17:merged] PR #17 merged",
		"[auto:pr:18:opened] PR #18 opened",
	} {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "proof/shared", Author: db.WorkflowAutoNoteAuthor, Body: body, Repo: "owner/repo",
		}); err != nil {
			t.Fatalf("InsertWorkflowNote: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"proof", "--json", "--home", home, "open-pr-root"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof --json code=%d stderr=%s", code, stderr.String())
	}
	var manifest proof.Manifest
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	var foundPR, foundOpened bool
	for _, node := range manifest.Nodes {
		if node.Kind != proof.KindPR || node.Attrs["number"] != "18" {
			continue
		}
		foundPR = true
		if node.Attrs["merged_at"] != "" || node.Attrs["merge_commit_sha"] != "" {
			t.Fatalf("open PR borrowed merged attrs: %+v", node.Attrs)
		}
		for _, claim := range node.Claims {
			if claim.Type == "pr.merged" {
				t.Fatalf("open PR borrowed merged claim: %+v", claim)
			}
			if claim.Type == "pr.opened" {
				foundOpened = true
			}
		}
	}
	if !foundPR || !foundOpened {
		t.Fatalf("PR #18 proof missing PR/opened evidence: foundPR=%v foundOpened=%v", foundPR, foundOpened)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"proof", "--home", home, "open-pr-root"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof text code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "#18 · head bbb222") || !strings.Contains(stdout.String(), "· merged - · state open") {
		t.Fatalf("open PR did not render an honest merged gap:\n%s", stdout.String())
	}
}

func TestRunProofToleratesMalformedJobPayload(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	rootResult := &workflow.AgentResult{
		Decision: "implemented", Summary: "root done",
		Delegations: []workflow.Delegation{{ID: "child", Agent: "worker", Action: "ask", Prompt: "inspect"}},
	}
	seedCLIJob(t, store, db.Job{
		ID: "root-corrupt", Agent: "impl", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Result: rootResult}),
	}, "done")
	seedCLIJob(t, store, db.Job{
		ID: "child-corrupt", Agent: "worker", Type: "ask", State: string(workflow.JobSucceeded),
		ParentJobID: "root-corrupt", DelegationID: "child", DelegationDepth: 1, DelegatedBy: "impl",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", RootJobID: "root-corrupt", ParentJobID: "root-corrupt",
			DelegationID: "child", DelegationDepth: 1, DelegatedBy: "impl",
		}),
	}, "done")
	if err := store.UpdateJobPayload(context.Background(), "child-corrupt", "{not-json"); err != nil {
		t.Fatalf("corrupt child payload: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"proof", "--home", home, "root-corrupt"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof text code=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"session child-corrupt", "payload: - (stored payload is not valid JSON)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("malformed-payload proof missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"proof", "--json", "--home", home, "root-corrupt"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof JSON code=%d stderr=%s", code, stderr.String())
	}
	var manifest proof.Manifest
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &manifest); err != nil {
		t.Fatalf("decode malformed-payload manifest: %v", err)
	}
	foundGap := false
	for _, node := range manifest.Nodes {
		if node.Kind == proof.KindSession && node.Ref == "child-corrupt" && node.Attrs["payload_unparseable"] == "true" {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatal("malformed child session lacks payload_unparseable JSON attr")
	}
}

// TestRunProofReadOnlyStoreUnchanged pins the data contract: proof opens SQLite
// mode=ro and leaves the main database bytes untouched. SQLite may create or
// refresh WAL/SHM bookkeeping sidecars while observing a live WAL database;
// those sidecars are allowed and are not store-data mutations.
func TestRunProofReadOnlyStoreUnchanged(t *testing.T) {
	home := t.TempDir()
	seedCLIProof(t, home)
	database := config.PathsForHome(home).Database
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"proof", "--json", "--home", home, "root-proof"}, &stdout, &stderr); code != 0 {
		t.Fatalf("proof --json code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) || beforeInfo.Size() != afterInfo.Size() {
		t.Fatal("proof changed the SQLite store")
	}
}

func seedCLIProof(t *testing.T, home string) {
	t.Helper()
	store := openCLIJobStore(t, home)
	ctx := context.Background()
	for _, agent := range []db.Agent{
		{Name: "implementer", Runtime: runtime.CodexRuntime, RuntimeRef: "session-implementer"},
		{Name: "reviewer", Runtime: runtime.CodexRuntime, RuntimeRef: "session-reviewer"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent: %v", err)
		}
	}
	rootResult := &workflow.AgentResult{
		Decision: "implemented", Summary: "proof implemented",
		ChangesMade: []string{"added proof"}, TestsRun: []string{"go test ./internal/proof/"},
		Delegations: []workflow.Delegation{{
			ID: "review", Agent: "reviewer", Action: "review", Prompt: "review proof", SynthesisRule: "verify",
		}},
	}
	reviewResult := &workflow.AgentResult{
		Decision: "approved", Summary: "clean",
		Findings: []json.RawMessage{json.RawMessage(`{"severity":"note"}`)},
	}
	seedCLIJob(t, store, db.Job{
		ID: "root-proof", Agent: "implementer", Type: "implement", State: string(workflow.JobSucceeded), Model: "gpt-5.6",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 17, HeadSHA: "abc123", WorkflowID: "proof/17", Result: rootResult,
		}),
	}, "implemented")
	seedCLIJob(t, store, db.Job{
		ID: "review-proof", Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded), Model: "gpt-5.6",
		ParentJobID: "root-proof", DelegationID: "review", DelegationDepth: 1, DelegatedBy: "implementer",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", PullRequest: 17, HeadSHA: "abc123", WorkflowID: "proof/17",
			RootJobID: "root-proof", ParentJobID: "root-proof", DelegationID: "review", DelegationDepth: 1,
			DelegatedBy: "implementer", Result: reviewResult,
		}),
	}, "approved")
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 17, HeadSHA: "abc123", MergeCommitSHA: "def456", State: "merged",
	}); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	for _, body := range []string{
		"[auto:pr:17:opened] PR #17 opened",
		"[auto:pr:17:merged] PR #17 merged",
	} {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "proof/17", Author: db.WorkflowAutoNoteAuthor, Body: body, Repo: "owner/repo",
		}); err != nil {
			t.Fatalf("InsertWorkflowNote: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close proof seed store: %v", err)
	}
}
