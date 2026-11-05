package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
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
	if len(payload.ReviewRelatedPRs) != 1 || payload.ReviewRelatedPRs[0] != related[0] || !strings.Contains(job.Payload, `"review_related_prs":[{"pr":7,"head_sha":"`+sha+`","tree_sha256":"`+related[0].TreeSHA256+`"}]`) || len(related[0].TreeSHA256) != 64 {
		t.Fatalf("payload related = %+v (%s)", payload.ReviewRelatedPRs, job.Payload)
	}

	workspace := t.TempDir()
	restoreWriteBitsOnCleanup(t, workspace)
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
	origin, _ := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	pushRelatedPR(t, origin, 7, func(work string) { writeRelatedFile(t, work, "moved.txt", "moved on\n") })
	tree, cleanup, err := fetchReviewRelatedTree(context.Background(), origin, "owner/repo", related[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	archive, err := reviewRelatedPRArchive(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if tree.files != 4 || !bytes.Contains(archive, []byte("CREATE TABLE sibling")) || bytes.Contains(archive, []byte("moved on")) || bytes.Contains(archive, []byte(origin)) {
		t.Fatalf("archive files=%d, want the 4 pinned entries, not the moved head, and no fetch URL", tree.files)
	}
}

func writeRelatedFile(t *testing.T, work, name, body string) {
	t.Helper()
	target := filepath.Join(work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRelatedLink(t *testing.T, work, name, target string) {
	t.Helper()
	link := filepath.Join(work, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// pushRelatedPR commits build's changes on top of origin's main and pushes the
// commit to refs/pull/N/head, returning its sha.
func pushRelatedPR(t *testing.T, origin string, number int, build func(work string)) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "pr")
	run := func(dir string, args ...string) string {
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
	run(filepath.Dir(work), "clone", "-q", "--branch", "main", origin, work)
	build(work)
	run(work, "add", "-A")
	run(work, "commit", "-qm", "pr")
	run(work, "push", "-q", "-f", origin, fmt.Sprintf("HEAD:refs/pull/%d/head", number))
	return run(work, "rev-parse", "HEAD")
}

// #2534 review P2-1: a symlink whose target, following chains, resolves
// outside .gitmoot-related/pr-N/ refuses the tree at dispatch.
func TestReviewRelatedPRRefusesEscapingSymlinks(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	pushRelatedPR(t, origin, 8, func(work string) { writeRelatedLink(t, work, "abs", "/etc/passwd") })
	pushRelatedPR(t, origin, 9, func(work string) { writeRelatedLink(t, work, "sub/up", "../../secret") })
	pushRelatedPR(t, origin, 10, func(work string) {
		// Lexically sub/M stays inside; following sub/L (-> "..") it escapes.
		writeRelatedLink(t, work, "sub/L", "..")
		writeRelatedLink(t, work, "sub/M", "L/../..")
	})
	pushRelatedPR(t, origin, 11, func(work string) { writeRelatedLink(t, work, "loop", "loop/x") })
	pushRelatedPR(t, origin, 13, func(work string) {
		writeRelatedFile(t, work, "a/b/file", "inside\n")
		writeRelatedLink(t, work, "sub/L", "..")
		writeRelatedLink(t, work, "sub/ok", "L/a/b/file")
		writeRelatedLink(t, work, "a/b/sib", "../../sub/ok")
	})
	for number, want := range map[int]string{8: "absolute symlink target", 9: "outside the pull request tree", 10: "outside the pull request tree", 11: "loops"} {
		_, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{number})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("PR #%d: err = %v, want a symlink refusal naming %q", number, err, want)
		}
	}
	if _, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{13}); err != nil {
		t.Fatalf("in-tree symlink chains refused: %v", err)
	}
}

// #2534 review P2-2: a .gitmoot-related entry the reviewed checkout brought is
// refused and left untouched, whether a directory or a symlink elsewhere.
func TestLocalRelatedInstallRefusesACheckoutOwnedRoot(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), RemoteURL: origin, PollInterval: "30s"}); err != nil {
		t.Fatal(err)
	}
	worker := jobWorker{Store: store, Stdout: &bytes.Buffer{}}
	payload := workflow.JobPayload{Repo: "owner/repo", ReviewRelatedPRs: related}

	tracked := t.TempDir()
	restoreWriteBitsOnCleanup(t, tracked)
	writeRelatedFile(t, tracked, ".gitmoot-related/keep.txt", "tracked\n")
	if _, err := worker.installLocalReviewRelatedPRs(context.Background(), "job", payload, tracked); err == nil || !strings.Contains(err.Error(), "already contains .gitmoot-related") {
		t.Fatalf("tracked dir: err = %v, want a refusal", err)
	}
	if body, err := os.ReadFile(filepath.Join(tracked, ".gitmoot-related", "keep.txt")); err != nil || string(body) != "tracked\n" {
		t.Fatalf("tracked file = %q, %v; want it untouched", body, err)
	}
	if _, err := os.Lstat(filepath.Join(tracked, ".gitmoot-related", "pr-7")); !os.IsNotExist(err) {
		t.Fatalf("a tree was written into the checkout's directory: %v", err)
	}

	linked := t.TempDir()
	outside := t.TempDir()
	restoreWriteBitsOnCleanup(t, linked)
	writeRelatedFile(t, outside, "victim.txt", "outside\n")
	writeRelatedLink(t, linked, ".gitmoot-related", outside)
	if _, err := worker.installLocalReviewRelatedPRs(context.Background(), "job", payload, linked); err == nil || !strings.Contains(err.Error(), "already contains .gitmoot-related") {
		t.Fatalf("symlinked root: err = %v, want a refusal", err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 || entries[0].Name() != "victim.txt" {
		t.Fatalf("outside dir = %v, want only victim.txt untouched", entries)
	}
	if target, err := os.Readlink(filepath.Join(linked, ".gitmoot-related")); err != nil || target != outside {
		t.Fatalf("checkout symlink = %q, %v; want it untouched", target, err)
	}
}

// #2534 review P2-2 and P2-3 for E2B: the sandbox scripts refuse a checkout
// entry before creating anything, and the digest they measure equals the pin
// until the sandbox user restores a write bit and edits the tree.
func TestRemoteRelatedScriptsRefuseCheckoutRootAndDetectTampering(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	sh := func(dir string, script string, args ...string) (string, error) {
		cmd := exec.Command("sh", append([]string{"-c", script, "related"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	linked, outside := t.TempDir(), t.TempDir()
	writeRelatedLink(t, linked, ".gitmoot-related", outside)
	if out, err := sh(linked, remoteReviewRelatedPRPrepareScript); err == nil || !strings.Contains(out, "already contains .gitmoot-related") {
		t.Fatalf("prepare over a checkout symlink: err=%v out=%q, want a refusal", err, out)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("prepare wrote through the checkout symlink: %v", entries)
	}

	workspace := t.TempDir()
	restoreWriteBitsOnCleanup(t, workspace)
	if out, err := sh(workspace, remoteReviewRelatedPRPrepareScript); err != nil {
		t.Fatalf("prepare: %v %s", err, out)
	}
	tree, cleanup, err := fetchReviewRelatedTree(context.Background(), origin, "owner/repo", related[0])
	if err != nil {
		t.Fatal(err)
	}
	archive, err := reviewRelatedPRArchive(context.Background(), tree)
	cleanup()
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "pr-7.tar")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := sh(workspace, remoteReviewRelatedPRUnpackScript, related[0].Path(), archivePath)
	if err != nil || strings.TrimSpace(out) != fmt.Sprintf("%d 0 %s", tree.files, related[0].TreeSHA256) {
		t.Fatalf("unpack = %q, %v; want %d read-only files with the pinned digest %s", out, err, tree.files, related[0].TreeSHA256)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive survives unpacking: %v", err)
	}
	measure := func() string {
		out, err := sh(workspace, remoteReviewRelatedPRMeasureScript, related[0].Path())
		if err != nil {
			t.Fatalf("measure: %v %s", err, out)
		}
		return strings.TrimSpace(out)
	}
	if got := measure(); got != related[0].TreeSHA256+" "+related[0].Path() {
		t.Fatalf("measure = %q, want the pinned digest", got)
	}
	schema := filepath.Join(workspace, ".gitmoot-related", "pr-7", "db", "schema.sql")
	if err := os.Chmod(schema, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schema, []byte("DROP TABLE sibling;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := measure(); strings.HasPrefix(got, related[0].TreeSHA256) {
		t.Fatalf("measure after tampering = %q, want a different digest", got)
	}
}

// #2534 review P2-4: the terminal cleanup's pre-disposal hook removes the
// 0555 related trees before the worktree goes to trash, so an unprivileged
// daemon can dispose of it, and nothing of the worktree or trash remains.
func TestRelatedTreesAreRemovedBeforeWorktreeDisposal(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	world := newCleanupCLIWorld(t, "succeeded", "review")
	restoreWriteBitsOnCleanup(t, world.home)
	restoreWriteBitsOnCleanup(t, world.path)
	ctx := context.Background()
	related, err := resolveReviewRelatedPRs(ctx, origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	world.payload.ReviewRelatedPRs = related
	encoded, err := json.Marshal(world.payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpdateJobPayload(ctx, world.job.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if err := world.store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: world.checkout, RemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	worker := jobWorker{Store: world.store, Stdout: &bytes.Buffer{}}
	if _, err := worker.installLocalReviewRelatedPRs(ctx, world.job.ID, world.payload, world.path); err != nil {
		t.Fatal(err)
	}
	engine := daemonWorkflowEngine(world.store, nil, world.checkout, world.paths.Home)
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	hook := engine.BeforeReadOnlyWorktreeCleanup
	presentAtDisposal := true
	engine.BeforeReadOnlyWorktreeCleanup = func(ctx context.Context, jobID, jobType string, payload workflow.JobPayload) error {
		err := hook(ctx, jobID, jobType, payload)
		_, statErr := os.Lstat(filepath.Join(payload.WorktreePath, ".gitmoot-related"))
		presentAtDisposal = !os.IsNotExist(statErr)
		return err
	}
	removed, err := engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.job.ID)
	if err != nil || !removed {
		t.Fatalf("cleanup removed=%t err=%v", removed, err)
	}
	if presentAtDisposal {
		t.Fatal("related trees were still in the worktree when the pre-disposal hook finished")
	}
	if _, err := os.Lstat(world.path); !os.IsNotExist(err) {
		t.Fatalf("worktree survives cleanup: %v", err)
	}
	_ = filepath.WalkDir(world.home, func(name string, d os.DirEntry, err error) error {
		if err == nil && (d.Name() == ".gitmoot-related" || strings.Contains(name, "schema.sql")) {
			t.Fatalf("related tree or trash survives cleanup at %s", name)
		}
		return nil
	})
}

// restoreWriteBitsOnCleanup makes every directory under root writable again
// when the test ends, before t.TempDir's own removal runs (cleanups are LIFO),
// so a read-only related tree never fails the test as a non-root user.
func restoreWriteBitsOnCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() { restoreTreeWriteBits(root) })
}

func restoreTreeWriteBits(root string) {
	_ = filepath.WalkDir(root, func(name string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(name, 0o755)
		}
		return nil
	})
}

// snapshotTree records every entry under root with its mode and content.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(root, func(name string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(name)
			value += " -> " + target
		case info.Mode().IsRegular():
			body, _ := os.ReadFile(name)
			value += " " + string(body)
		}
		snapshot[name] = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// #2534 review round 2, F2: ownership comes from the job's own recorded state,
// never from the reviewed tree. A checkout directory carrying a marker that
// names this very job is refused and left byte-for-byte untouched, and the
// pre-disposal hook does not remove it either.
func TestLocalRelatedInstallRefusesACheckoutRootWithAForgedMarker(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), RemoteURL: origin, PollInterval: "30s"}); err != nil {
		t.Fatal(err)
	}
	worker := jobWorker{Store: store, Stdout: &bytes.Buffer{}}
	payload := workflow.JobPayload{Repo: "owner/repo", ReviewRelatedPRs: related}
	checkout := t.TempDir()
	restoreWriteBitsOnCleanup(t, checkout)
	writeRelatedFile(t, checkout, ".gitmoot-related/.gitmoot-owner", "forged-job\n")
	writeRelatedFile(t, checkout, ".gitmoot-related/pr-7/tracked.sql", "tracked by the checkout\n")
	before := snapshotTree(t, checkout)
	if _, err := worker.installLocalReviewRelatedPRs(context.Background(), "forged-job", payload, checkout); err == nil || !strings.Contains(err.Error(), "already contains .gitmoot-related") {
		t.Fatalf("forged marker: err = %v, want a refusal", err)
	}
	payload.WorktreePath = checkout
	if err := reviewRelatedPRPrecleanupHook(store)(context.Background(), "forged-job", "review", payload); err != nil {
		t.Fatalf("pre-disposal hook: %v", err)
	}
	after := snapshotTree(t, checkout)
	if len(before) != len(after) {
		t.Fatalf("checkout changed: before=%v after=%v", before, after)
	}
	for name, value := range before {
		if after[name] != value {
			t.Fatalf("%s changed from %q to %q", name, value, after[name])
		}
	}
}

// A retry of the same job in the same worktree may replace exactly the
// directory that job recorded creating, and nothing else.
func TestLocalRelatedInstallReplacesOnlyItsOwnRecordedRoot(t *testing.T) {
	origin, _ := relatedPROrigin(t)
	related, err := resolveReviewRelatedPRs(context.Background(), origin, "owner/repo", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	store := daemonWorkerStore(t)
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: "owner", Name: "repo", CheckoutPath: t.TempDir(), RemoteURL: origin, PollInterval: "30s"}); err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{ID: "retry-job", Agent: "reviewer", Type: "review", State: string(workflow.JobRunning), Payload: "{}"}, "running")
	worker := jobWorker{Store: store, Stdout: &bytes.Buffer{}}
	payload := workflow.JobPayload{Repo: "owner/repo", ReviewRelatedPRs: related}
	workspace := t.TempDir()
	restoreWriteBitsOnCleanup(t, workspace)
	if _, err := worker.installLocalReviewRelatedPRs(context.Background(), "retry-job", payload, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.installLocalReviewRelatedPRs(context.Background(), "other-job", payload, workspace); err == nil {
		t.Fatal("another job replaced a root it did not create")
	}
	remove, err := worker.installLocalReviewRelatedPRs(context.Background(), "retry-job", payload, workspace)
	if err != nil {
		t.Fatalf("retry of the recording job refused: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, ".gitmoot-related", "pr-7", "db", "schema.sql")); err != nil || !strings.Contains(string(body), "sibling") {
		t.Fatalf("retry tree = %q, %v", body, err)
	}
	remove()
	if _, err := os.Lstat(filepath.Join(workspace, ".gitmoot-related")); !os.IsNotExist(err) {
		t.Fatalf("recorded root survives removal: %v", err)
	}
}
