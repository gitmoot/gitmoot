package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// relatedPROrigin builds a bare origin whose refs/pull/7/head is a commit with
// files the reviewed PR does not have. It returns the origin path and the
// related head sha.
func relatedPROrigin(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-q", "--bare", origin)
	git(root, "init", "-q", work)
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-qm", "base")
	git(work, "push", "-q", origin, "HEAD:refs/heads/main")
	if err := os.MkdirAll(filepath.Join(work, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "db", "schema.sql"), []byte("CREATE TABLE sibling (id INT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("db/schema.sql", filepath.Join(work, "schema-link")); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-qm", "sibling")
	sha := git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", origin, "HEAD:refs/pull/7/head")
	return origin, sha
}

func TestReviewRelatedPRNumbersAreValidated(t *testing.T) {
	for _, tc := range []struct {
		related []int
		want    string
	}{
		{[]int{0}, "must be positive"},
		{[]int{-3}, "must be positive"},
		{[]int{12}, "is the pull request under review"},
		{[]int{7, 8, 7}, "more than once"},
	} {
		err := workflow.ValidateReviewRelatedPRNumbers(12, tc.related)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("related %v: err = %v, want %q", tc.related, err, tc.want)
		}
	}
	if err := workflow.ValidateReviewRelatedPRNumbers(12, []int{7, 8}); err != nil {
		t.Fatalf("distinct sibling PRs refused: %v", err)
	}
}

// The flag is parsed and refused at the CLI before any store or network work.
func TestReviewRequestRefusesBadRelatedPRFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--related-pr", "12"}, "under review"},
		{[]string{"--related-pr", "0"}, "must be positive"},
		{[]string{"--related-pr", "7", "--related-pr", "7"}, "more than once"},
		{[]string{"--related-pr", "x"}, "want a pull request number"},
	} {
		var stderr bytes.Buffer
		args := append([]string{"--home", t.TempDir(), "--pr", "12", "--role", "r"}, tc.args...)
		if code := runReviewRequest(args, &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("args %v: code=%d stderr=%q, want exit 2 naming %q", tc.args, code, stderr.String(), tc.want)
		}
	}
}

// `agent review --related-pr` is carried to the router verbatim and refused on
// the direct path, which cannot deliver it.
func TestAgentReviewCarriesRelatedPRToTheRouter(t *testing.T) {
	var parseErr bytes.Buffer
	options, ok := parseAgentRunOptions("review", []string{"reviewer", "check it", "--pr", "12", "--related-pr", "7", "--related-pr=8"}, &parseErr)
	if !ok {
		t.Fatalf("agent review refused --related-pr: %s", parseErr.String())
	}
	args := strings.Join(reviewRequestArgsFromAgentReview(options), " ")
	if !strings.Contains(args, "--related-pr 7") || !strings.Contains(args, "--related-pr 8") {
		t.Fatalf("router args = %q, want both related PRs", args)
	}
	var stderr bytes.Buffer
	if _, code := dispatchAgentCommand(options, "review", "test", "agent_review", &bytes.Buffer{}, &stderr); code != 2 || !strings.Contains(stderr.String(), "--related-pr") {
		t.Fatalf("direct path code=%d stderr=%q, want refusal", code, stderr.String())
	}
}

func TestResolveReviewRelatedPRsPinsTheExactHead(t *testing.T) {
	origin, sha := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].PR != 7 || related[0].HeadSHA != sha {
		t.Fatalf("related = %+v, want #7 at %s", related, sha)
	}
	_, err = resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{99})
	if err == nil || !strings.Contains(err.Error(), "--related-pr 99: cannot fetch refs/pull/99/head of owner/repo") {
		t.Fatalf("missing PR err = %v, want a clear fetch refusal", err)
	}
	if strings.Contains(err.Error(), origin) {
		t.Fatalf("refusal leaks the fetch URL: %v", err)
	}
}

// The payload records PR+SHA; the materialized tree has the related PR's files
// at that SHA, is read-only, carries no git metadata or credential, and is
// removed afterwards. The prompt names PR, SHA and path.
func TestLocalReviewRelatedPRTreeIsExactAndReadOnly(t *testing.T) {
	origin, sha := relatedPROrigin(t)
	store := daemonWorkerStore(t)
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), RemoteURL: origin, PollInterval: "30s"}); err != nil {
		t.Fatal(err)
	}
	related := []workflow.ReviewRelatedPR{{PR: 7, HeadSHA: sha}}
	job, err := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("test")).Enqueue(context.Background(), workflow.JobRequest{
		ID: "related-review", Agent: "reviewer", Action: "review", Repo: "owner/repo", PullRequest: 12,
		HeadSHA: strings.Repeat("a", 40), Instructions: "review", ReviewRelatedPRs: related,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.ReviewRelatedPRs) != 1 || payload.ReviewRelatedPRs[0] != related[0] || !strings.Contains(job.Payload, `"review_related_prs":[{"pr":7,"head_sha":"`+sha+`"}]`) {
		t.Fatalf("payload related = %+v (%s)", payload.ReviewRelatedPRs, job.Payload)
	}

	workspace := t.TempDir()
	worker := jobWorker{Store: store, Stdout: &bytes.Buffer{}}
	remove, err := worker.installLocalReviewRelatedPRs(context.Background(), job.ID, payload, workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(workspace, ".gitmoot-related", "pr-7")
	body, err := os.ReadFile(filepath.Join(tree, "db", "schema.sql"))
	if err != nil || string(body) != "CREATE TABLE sibling (id INT);\n" {
		t.Fatalf("schema.sql = %q, %v; want the related head's bytes", body, err)
	}
	if target, err := os.Readlink(filepath.Join(tree, "schema-link")); err != nil || target != "db/schema.sql" {
		t.Fatalf("symlink = %q, %v", target, err)
	}
	if info, err := os.Stat(filepath.Join(tree, "run.sh")); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("run.sh mode = %v, %v; want read-only executable", info.Mode(), err)
	}
	// Root bypasses permission bits, so the write probes run only unprivileged;
	// the mode walk below holds for every user.
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(tree, "db", "schema.sql"), []byte("x"), 0o644); err == nil {
			t.Fatal("related tree file is writable")
		}
		if err := os.WriteFile(filepath.Join(tree, "new"), []byte("x"), 0o644); err == nil {
			t.Fatal("related tree directory is writable")
		}
	}
	_ = filepath.WalkDir(filepath.Join(workspace, ".gitmoot-related"), func(name string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			t.Fatalf("related tree carries git metadata at %s", name)
		}
		if d.Type().IsRegular() {
			data, _ := os.ReadFile(name)
			if bytes.Contains(data, []byte(origin)) || bytes.Contains(data, []byte("extraHeader")) || bytes.Contains(data, []byte("Authorization")) {
				t.Fatalf("%s carries fetch or credential material", name)
			}
		}
		if d.Type().IsRegular() || d.IsDir() {
			if info, _ := d.Info(); info.Mode().Perm()&0o222 != 0 {
				t.Fatalf("%s is writable: %v", name, info.Mode())
			}
		}
		return nil
	})
	events, _ := store.ListJobEvents(context.Background(), job.ID)
	found := false
	for _, event := range events {
		found = found || event.Kind == reviewRelatedPRInstalledEventKind && strings.Contains(event.Message, sha)
	}
	if !found {
		t.Fatalf("no %s event naming the sha: %+v", reviewRelatedPRInstalledEventKind, events)
	}
	remove()
	if _, err := os.Lstat(filepath.Join(workspace, ".gitmoot-related")); !os.IsNotExist(err) {
		t.Fatalf("related trees survive removal: %v", err)
	}

	prompt := reviewRelatedPRInstructions(related)
	for _, want := range []string{"#7", sha, ".gitmoot-related/pr-7/", "read-only"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q does not name %q", prompt, want)
		}
	}
}

// A pinned head is delivered even after the pull request moves on.
func TestReviewRelatedPRArchiveHoldsThePinnedTree(t *testing.T) {
	origin, sha := relatedPROrigin(t)
	gitDir, entries, cleanup, err := fetchReviewRelatedTree(context.Background(), origin, "owner/repo", workflow.ReviewRelatedPR{PR: 7, HeadSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	archive, count, err := reviewRelatedPRArchive(context.Background(), gitDir, entries)
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || !bytes.Contains(archive, []byte("CREATE TABLE sibling")) || bytes.Contains(archive, []byte(origin)) {
		t.Fatalf("archive count=%d, want 4 entries with the sibling bytes and no fetch URL", count)
	}
}
