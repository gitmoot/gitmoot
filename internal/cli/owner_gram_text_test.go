package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// renderOwnerGramFromNote stores a workflow note addressed to the owner, then
// renders the Gram the daemon would send for it, through the real drain.
func renderOwnerGramFromNote(t *testing.T, store *db.Store, sink synchronousEventRuleTestSink, body string) ownerGramMessage {
	t.Helper()
	ctx := context.Background()
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "adhoc/estel-review-codex-2026-10-08", Author: "life-crm", Body: body,
		Repo: "jerryfane/estel", AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	var sent []ownerGramMessage
	gram := func(_ context.Context, message ownerGramMessage) ownerGramResult {
		sent = append(sent, message)
		return ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-render"}
	}
	resolve := func(ctx context.Context) (replyWakeDelivery, error) {
		rules, err := store.ListEventRules(ctx)
		return replyWakeDelivery{sink: sink, rules: rules, ownerGram: gram}, err
	}
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("gram sends = %d, want 1", len(sent))
	}
	return sent[0]
}

// The first live owner Gram (message 283687) was cut off mid-sentence and full
// of gate jargon. The same escalation now renders as a plain, complete Gram.
func TestOwnerGramRendersMessage283687Plainly(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	const head = "6ad9eed75402ee3d75b021d2e79ceef6e113ac44"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "jerryfane/estel", Number: 6, URL: "https://github.com/jerryfane/estel/pull/6",
		HeadBranch: "engine-claude", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/owner_gram_283687_note.txt")
	if err != nil {
		t.Fatal(err)
	}
	note := strings.TrimSpace(string(raw))
	_, original, _ := strings.Cut(note, "] ")
	message := renderOwnerGramFromNote(t, store, sink, note)
	want := "life-crm needs you: jerryfane/estel#6 — it's approved, but Gitmoot won't merge it by itself.\n" +
		"This repo has no automated checks (CI), so automatic merging is off for it. " +
		"Gitmoot can't confirm who wrote this change, so it won't merge it automatically. " +
		"Fix: life-crm, the seat that wrote it, records its work in Gitmoot.\n" +
		"What to do: If you want this change, merge it on GitHub. If not, close it.\n" +
		"https://github.com/jerryfane/estel/pull/6\n\n" +
		ownerGramSeparator + "\n" +
		"Original message from life-crm about jerryfane/estel#6, unedited:\n" + original
	if message.Text != want || message.Attachment != "" {
		t.Fatalf("rendered gram:\n%s\n\nwant:\n%s", message.Text, want)
	}
	summary, _, _ := strings.Cut(message.Text, ownerGramSeparator)
	if strings.Contains(summary, "gitmoot ") {
		t.Fatalf("owner-facing summary carries a CLI command:\n%s", summary)
	}
	t.Logf("rendered gram for message 283687:\n%s", message.Text)
}

// A message Gitmoot has no template for keeps the sender's own first sentence
// as the plain line, and its full text below the separator.
func TestOwnerGramRendersGenericMessage(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	body := "[org:escalate to=owner from=life-crm wf=adhoc/estel-review-codex-2026-10-08] " +
		"The staging database is full. I paused the nightly import until someone decides whether to buy more storage or drop last year's logs."
	message := renderOwnerGramFromNote(t, store, sink, body)
	want := "life-crm needs you: jerryfane/estel — The staging database is full.\n" +
		"What to do: Read life-crm's message below and reply to life-crm.\n\n" +
		ownerGramSeparator + "\n" +
		"Original message from life-crm about jerryfane/estel, unedited:\n" +
		"The staging database is full. I paused the nightly import until someone decides whether to buy more storage or drop last year's logs."
	if message.Text != want || message.Attachment != "" {
		t.Fatalf("rendered gram:\n%s\n\nwant:\n%s", message.Text, want)
	}
	t.Logf("rendered generic gram:\n%s", message.Text)
}

// Nothing is ever cut: past Herdr's 8 KiB text limit the unedited originals
// move into an attached file, and the text says so.
func TestOwnerGramAttachesOriginalsPastHerdrLimit(t *testing.T) {
	long := strings.Repeat("The import keeps failing on row 4812 and I need a decision. ", 200)
	items := []ownerAlert{{who: "life-crm", subject: "jerryfane/estel", headline: "The import keeps failing on row 4812 and I need a decision.",
		todo: "Read life-crm's message below and reply to life-crm.", original: strings.TrimSpace(long)}}
	message := renderOwnerGram(items, 0)
	if len(message.Text) > ownerGramMaxTextBytes {
		t.Fatalf("gram text is %d bytes, over Herdr's %d", len(message.Text), ownerGramMaxTextBytes)
	}
	if !strings.Contains(message.Text, "The unedited original messages are in the attached file.") ||
		!strings.Contains(message.Attachment, strings.TrimSpace(long)) {
		t.Fatalf("text=%q attachment has full original=%v", message.Text, strings.Contains(message.Attachment, strings.TrimSpace(long)))
	}
}

// With the attribution gap routed back to the seat, the owner receives only
// the part he can act on for estel#6: the repo has no CI, so it needs a merge
// by hand.
func TestOwnerGramRendersEstel6AfterSeatRouting(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	const head = "6ad9eed75402ee3d75b021d2e79ceef6e113ac44"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "jerryfane/estel", Number: 6, URL: "https://github.com/jerryfane/estel/pull/6",
		HeadBranch: "engine-claude", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	original := "CI gate: low-risk auto-merge left the pull request open for a human merge: head 6ad9eed reports no external CI; " +
		"low-risk auto-merge requires real green CI for head " + head
	message := renderOwnerGramFromNote(t, store, sink, "[org:escalate to=owner from=life-crm wf=adhoc/estel-review-codex-2026-10-08] "+original)
	want := "life-crm needs you: jerryfane/estel#6 — it's approved, but Gitmoot won't merge it by itself.\n" +
		"This repo has no automated checks (CI), so automatic merging is off for it.\n" +
		"What to do: If you want this change, merge it on GitHub. If not, close it.\n" +
		"https://github.com/jerryfane/estel/pull/6\n\n" +
		ownerGramSeparator + "\n" +
		"Original message from life-crm about jerryfane/estel#6, unedited:\n" + original
	if message.Text != want || message.Attachment != "" {
		t.Fatalf("rendered gram:\n%s\n\nwant:\n%s", message.Text, want)
	}
	t.Logf("rendered gram for estel#6 after seat routing:\n%s", message.Text)
}

// The same head can head pull requests in different repositories; the Gram
// must link only the alert's own repository's pull request.
func TestOwnerGramLinksOnlyThePullRequestOfItsOwnRepository(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	const head = "6ad9eed75402ee3d75b021d2e79ceef6e113ac44"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "someone/fork", Number: 99, URL: "https://github.com/someone/fork/pull/99",
		HeadBranch: "engine-claude", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	original := "CI gate: low-risk auto-merge left the pull request open for a human merge: head 6ad9eed reports no external CI; " +
		"low-risk auto-merge requires real green CI for head " + head
	message := renderOwnerGramFromNote(t, store, sink, "[org:escalate to=owner from=life-crm wf=adhoc/estel-review-codex-2026-10-08] "+original)
	if strings.Contains(message.Text, "someone/fork") || !strings.HasPrefix(message.Text, "life-crm needs you: jerryfane/estel — ") {
		t.Fatalf("rendered gram linked another repository's pull request:\n%s", message.Text)
	}
}

// GitHub repository names are case-insensitive: a PR stored as Jerryfane/Estel
// still links an alert about jerryfane/estel.
func TestOwnerGramLinksPullRequestAcrossRepositoryNameCase(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	const head = "6ad9eed75402ee3d75b021d2e79ceef6e113ac44"
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "Jerryfane/Estel", Number: 6, URL: "https://github.com/Jerryfane/Estel/pull/6",
		HeadBranch: "engine-claude", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	original := "CI gate: low-risk auto-merge left the pull request open for a human merge: head 6ad9eed reports no external CI; " +
		"low-risk auto-merge requires real green CI for head " + head
	message := renderOwnerGramFromNote(t, store, sink, "[org:escalate to=owner from=life-crm wf=adhoc/estel-review-codex-2026-10-08] "+original)
	if !strings.HasPrefix(message.Text, "life-crm needs you: jerryfane/estel#6 — ") ||
		!strings.Contains(message.Text, "https://github.com/jerryfane/estel/pull/6") {
		t.Fatalf("rendered gram missed the PR stored with different case:\n%s", message.Text)
	}
}
