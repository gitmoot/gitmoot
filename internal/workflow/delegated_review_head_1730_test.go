package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1730. A review delegated by an IMPLEMENT job recorded the parent's payload
// head, which is the commit BEFORE the parent's change, so the row attested a
// verdict at an ancestor of the tree the reviewer actually read.
//
// These tests drive Engine.allocateAndEnqueueDelegationInner, the production
// path, with a ONE-CHILD delegation - the shape of the reproduced job. That
// matters: readOnlyFanoutNeedsWorktree requires two or more read-only siblings
// (worktree.go:941), so a lone review child never reaches the read-only
// worktree branch, and a fix placed there would be unexercised by the very job
// that motivated it.

type fakeHeadResolver struct {
	heads map[string]string
	err   error
	calls []string
}

func (f *fakeHeadResolver) AddWorktree(context.Context, string, string, string) error { return nil }

func (f *fakeHeadResolver) RevParse(_ context.Context, rev string) (string, error) {
	f.calls = append(f.calls, rev)
	if f.err != nil {
		return "", f.err
	}
	sha, ok := f.heads[rev]
	if !ok {
		return "", errors.New("unknown revision " + rev)
	}
	return sha, nil
}

const (
	staleImplementHead = "0e2ce4f150ed8c8b816cc6b0715c8de0dd64f803"
	producedHead       = "0c175d03796f5625a23d1c7c52464f55d7308d0e"
)

func newDelegatedReviewFixture(t *testing.T, resolver WorktreeManager) (Engine, *db.Store) {
	t.Helper()
	store := openEngineStore(t)
	return Engine{Store: store, DelegationWorktrees: resolver}, store
}

// The reviewed head must be WRITTEN, not merely un-inherited: the row has to
// name the commit the reviewer will read.
func TestDelegatedReviewBindsToTheHeadTheImplementParentProduced(t *testing.T) {
	ctx := context.Background()
	resolver := &fakeHeadResolver{heads: map[string]string{"adhoc-be2e7f7f": producedHead}}
	engine, store := newDelegatedReviewFixture(t, resolver)

	parent := db.Job{ID: "impl-1730", Agent: "appkit-omp", Type: "implement", State: string(JobSucceeded)}
	payload := JobPayload{
		Repo: "themartianapp/appkit", Branch: "adhoc-be2e7f7f",
		HeadSHA: staleImplementHead, TaskID: "task-1730",
	}
	request := JobRequest{
		ID:   "impl-1730/delegation/round2-review",
		Repo: payload.Repo, Branch: payload.Branch, Action: "review", Agent: "reviewer",
		HeadSHA: payload.HeadSHA, DelegationID: "round2-review",
	}
	seedMergeGateFixtureAgent(t, store, "reviewer")
	if err := engine.allocateAndEnqueueDelegationInner(ctx, parent, payload, Delegation{ID: "round2-review", Action: "review"}, request, taskRef{}); err != nil {
		t.Fatalf("allocateAndEnqueueDelegationInner: %v", err)
	}

	child, err := store.GetJob(ctx, "impl-1730/delegation/round2-review")
	if err != nil {
		t.Fatalf("child job not enqueued: %v", err)
	}
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if childPayload.HeadSHA == staleImplementHead {
		t.Fatal("the child still records the implement parent's pre-change head; the row attests a verdict at an ancestor of the reviewed tree")
	}
	if childPayload.HeadSHA != producedHead {
		t.Fatalf("child head = %q, want the branch tip %q that the parent produced", childPayload.HeadSHA, producedHead)
	}
	if len(resolver.calls) == 0 || resolver.calls[0] != "adhoc-be2e7f7f" {
		t.Fatalf("the head was not resolved from the parent's branch: calls=%v", resolver.calls)
	}
}

// A REVIEW parent must be untouched: its head IS the commit under review, and
// unpinning it there would remove the exact-head guarantee the merge gate
// depends on. Without this, the fix could be over-applied to every review
// delegation.
func TestDelegatedReviewOfAReviewParentKeepsTheInheritedHead(t *testing.T) {
	ctx := context.Background()
	resolver := &fakeHeadResolver{heads: map[string]string{"task-9": producedHead}}
	engine, store := newDelegatedReviewFixture(t, resolver)

	parent := db.Job{ID: "review-1730", Agent: "coord", Type: "review", State: string(JobSucceeded)}
	payload := JobPayload{Repo: "gitmoot/gitmoot", Branch: "task-9", HeadSHA: staleImplementHead, TaskID: "task-9"}
	request := JobRequest{
		ID:   "review-1730/delegation/lens-a",
		Repo: payload.Repo, Branch: payload.Branch, Action: "review", Agent: "lens",
		HeadSHA: payload.HeadSHA, DelegationID: "lens-a",
	}
	seedMergeGateFixtureAgent(t, store, "lens")
	if err := engine.allocateAndEnqueueDelegationInner(ctx, parent, payload, Delegation{ID: "lens-a", Action: "review"}, request, taskRef{}); err != nil {
		t.Fatalf("allocateAndEnqueueDelegationInner: %v", err)
	}
	child, err := store.GetJob(ctx, "review-1730/delegation/lens-a")
	if err != nil {
		t.Fatalf("child job not enqueued: %v", err)
	}
	childPayload, err := unmarshalPayload(child.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if childPayload.HeadSHA != staleImplementHead {
		t.Fatalf("a LENS child of a review parent lost its inherited exact head: got %q", childPayload.HeadSHA)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("a review parent's child resolved a branch tip it should not have: %v", resolver.calls)
	}
}
