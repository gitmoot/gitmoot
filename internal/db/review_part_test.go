package db

import (
	"context"
	"fmt"
	"testing"
)

// TestReviewPartScopesTheReviewClaim (#2372): the parts of one split review at
// one head each get their own claim, so all of them can run; the same part
// twice is still one claim; and a review with no part keeps the key it had
// before parts existed.
func TestReviewPartScopesTheReviewClaim(t *testing.T) {
	ctx := context.Background()
	store := openAwaitedFactTestStore(t)
	const repo, pr, head = "themartianapp/docus", 72, "27fc0a64f470f4987f07c66c3acddbd402fd3945"
	owner := ReviewRequestOwner{}

	key := func(part string) string {
		t.Helper()
		k, err := ReviewRequestSubjectKey(repo, pr, head, ReviewRequestPurpose("code", false, part))
		if err != nil {
			t.Fatalf("subject key for part %q: %v", part, err)
		}
		return k
	}

	if got, want := key(""), mustKey(t, repo, pr, head, "code"); got != want {
		t.Fatalf("no-part key = %q, want the pre-#2372 key %q", got, want)
	}

	// The exact shape Docus PR72 dispatches: 30 ranges plus CROSS.
	parts := []string{"CROSS"}
	for i := 1; i <= 30; i++ {
		parts = append(parts, fmt.Sprintf("MM-%d-MM-%d", i*3, i*3+2))
	}
	seen := map[string]bool{}
	for i, part := range parts {
		if !ValidReviewPart(part) {
			t.Fatalf("part %q rejected", part)
		}
		k := key(part)
		if seen[k] {
			t.Fatalf("part %q collides with another part's key %q", part, k)
		}
		seen[k] = true
		_, won, err := store.ClaimReviewRequest(ctx, k, fmt.Sprintf("job-%d", i), ReviewRequestPurpose("code", false, part), "usmle-app", owner)
		if err != nil || !won {
			t.Fatalf("claim for part %q: won=%v err=%v", part, won, err)
		}
	}
	if _, won, err := store.ClaimReviewRequest(ctx, key("mm-3-mm-5"), "job-dup", ReviewRequestPurpose("code", false, "MM-3-MM-5"), "usmle-app", owner); err != nil || won {
		t.Fatalf("the same part requested again must attach, not win a second claim: won=%v err=%v", won, err)
	}
	if _, won, err := store.ClaimReviewRequest(ctx, key(""), "job-whole", "code", "usmle-app", owner); err != nil || !won {
		t.Fatalf("a whole-head review must not be blocked by its parts: won=%v err=%v", won, err)
	}
}

func mustKey(t *testing.T, repo string, pr int, head, purpose string) string {
	t.Helper()
	k, err := ReviewRequestSubjectKey(repo, pr, head, purpose)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestReviewPartLabelsThatCouldForgeAnotherKeyAreRejected: a label must not be
// able to carry a key separator and impersonate a different purpose or head.
func TestReviewPartLabelsThatCouldForgeAnotherKeyAreRejected(t *testing.T) {
	for _, part := range []string{"a|security", "x@deadbeef", "a#1", "a~b", "with space", "-lead", "trail-", "", " ", string(make([]byte, 65))} {
		if part != "" && ValidReviewPart(part) {
			t.Errorf("part %q accepted", part)
		}
	}
	for _, part := range []string{"CROSS", "mm-1-mm-2", "a", "x9"} {
		if !ValidReviewPart(part) {
			t.Errorf("part %q rejected", part)
		}
	}
}
