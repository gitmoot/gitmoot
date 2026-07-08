package memory

import (
	"strings"
	"testing"
)

func TestDetectStatusChangelog(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "toc index note",
			content: "- [ci fast lanes](ci.md) — #754 shipped 2026-07-08\n" +
				"- [dashboard redesign](dash.md) — LIVE 2026-07-08\n" +
				"- [feedback machine](fb.md) — full loop validated",
			want: true,
		},
		{
			name:    "status marker single line",
			content: "STATUS: shipped 2026-07-08; deployed live; PR #722 merged",
			want:    true,
		},
		{
			// A single dense fact that merely LEADS with a date is a keeper: the weak
			// date marker must not retire the RFC's #1 use case (date-led one-liners).
			name:    "date-led single-line keeper",
			content: "2026-07-08 root-caused #487: rare claude-CLI transient 401 under sustained concurrency; fix = retry-on-transient.",
			want:    false,
		},
		{
			// One substantive prose line that happens to contain SHIPPED is a keeper.
			name:    "shipped-mention single-line keeper",
			content: "Validated when #754 SHIPPED, but the arm64 flake predates it and needs retry-on-transient.",
			want:    false,
		},
		{
			// A single bracketed-ref line (ToC-shaped) is a keeper on its own.
			name:    "bracketed-ref single-line keeper",
			content: "- [#487] rare claude-CLI transient under concurrency; retry-on-transient",
			want:    false,
		},
		{
			// Two date-led lines are still below the min-line dominance floor, so a
			// short weak-marker note is kept rather than retired.
			name:    "two date-led lines below min-lines",
			content: "2026-07-08 shipped X\n2026-07-07 shipped Y",
			want:    false,
		},
		{
			name: "shipped-and-deployed changelog",
			content: "SHIPPED #717 squash-merged (main 4953994)\n" +
				"shipped and deployed live: rubric-induce, binary-eval, synth\n" +
				"SHIPPED the full loop 2026-07-08",
			want: true,
		},
		{
			name: "date-led changelog",
			content: "2026-07-08 cut the release\n" +
				"2026-07-07 chat V1 shipped\n" +
				"2026-06-25 v0.5.2",
			want: true,
		},
		{
			name: "substantive memory that merely mentions SHIPPED once",
			content: "The arm64 CI runner drops flaky failures intermittently under load.\n" +
				"Root cause is a race in the test harness cache; retry-on-transient fixes it.\n" +
				"This was validated when #754 SHIPPED but the underlying flake predates it.\n" +
				"Keep the single-binary invariant sacred; do not add an embeddings service.",
			want: false,
		},
		{
			name:    "plain prose",
			content: "Killing a foreground agent ask strands a runtime-session lock; clear it via DB delete.",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectStatusChangelog(tc.content); got != tc.want {
				t.Fatalf("detectStatusChangelog(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestDetectTaskListOnly(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "pure checkbox list",
			content: "- [ ] wire the advancer\n- [x] add the config keys\n* [ ] write the tests",
			want:    true,
		},
		{
			name:    "numbered checkboxes",
			content: "1. [ ] first\n2. [x] second",
			want:    true,
		},
		{
			name:    "checkboxes with prose interleaved",
			content: "Plan for the wave:\n- [ ] wire the advancer\n- [x] add the config keys",
			want:    false,
		},
		{
			name:    "toc links are not task lists",
			content: "- [ci fast lanes](ci.md)\n- [dashboard](dash.md)",
			want:    false,
		},
		{
			name:    "empty",
			content: "\n\n  \n",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectTaskListOnly(tc.content); got != tc.want {
				t.Fatalf("detectTaskListOnly(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestDetectGroomActionsPrecedenceAndDuplicates(t *testing.T) {
	brick := strings.Repeat("x", GroomRewriteThreshold+50)
	cands := []GroomCandidate{
		// Out of id order on purpose — detection must be order-independent.
		{ID: 5, Key: "dup-b", Content: "identical duplicate body"},
		{ID: 2, Key: "toc", Content: "- [a](a.md) — one\n- [b](b.md) — two\n- [c](c.md) — three"},
		{ID: 1, Key: "dup-a", Content: "identical duplicate body"},
		{ID: 3, Key: "todo", Content: "- [ ] alpha\n- [x] beta"},
		{ID: 4, Key: "brick", Content: brick},
		{ID: 6, Key: "keep", Content: "a genuine substantive operational memory worth keeping"},
	}

	got := DetectGroomActions(cands)

	// Duplicate: keep lowest id (1), retire the higher (5).
	// ToC (2) and to-do (3) each retire once. Brick (4) is FLAGGED, not retired.
	retireByID := map[int64]string{}
	for _, r := range got.Retirements {
		if _, dup := retireByID[r.ID]; dup {
			t.Fatalf("id %d proposed for retirement twice", r.ID)
		}
		retireByID[r.ID] = r.Reason
	}
	if reason := retireByID[5]; reason != GroomReasonDuplicate {
		t.Fatalf("id 5 reason = %q, want duplicate", reason)
	}
	if _, ok := retireByID[1]; ok {
		t.Fatalf("id 1 (lowest dup) must be kept, not retired")
	}
	if reason := retireByID[2]; reason != GroomReasonStatusChangelog {
		t.Fatalf("id 2 reason = %q, want status-changelog", reason)
	}
	if reason := retireByID[3]; reason != GroomReasonTaskList {
		t.Fatalf("id 3 reason = %q, want task-list", reason)
	}
	if _, ok := retireByID[4]; ok {
		t.Fatalf("id 4 (brick) must be flagged, not retired")
	}
	if _, ok := retireByID[6]; ok {
		t.Fatalf("id 6 (keep) must not be retired")
	}

	if len(got.RewriteFlags) != 1 || got.RewriteFlags[0].ID != 4 {
		t.Fatalf("rewrite flags = %+v, want exactly id 4", got.RewriteFlags)
	}
	if got.RewriteFlags[0].Chars != len(brick) {
		t.Fatalf("brick chars = %d, want %d", got.RewriteFlags[0].Chars, len(brick))
	}

	if got.Stats.TotalMemories != 6 {
		t.Fatalf("total = %d, want 6", got.Stats.TotalMemories)
	}
	if got.Stats.ProposedRetirements != 3 {
		t.Fatalf("proposed = %d, want 3", got.Stats.ProposedRetirements)
	}
	if got.Stats.RewriteFlags != 1 {
		t.Fatalf("rewrite flags stat = %d, want 1", got.Stats.RewriteFlags)
	}
	if got.Stats.ByReason[GroomReasonDuplicate] != 1 || got.Stats.ByReason[GroomReasonStatusChangelog] != 1 || got.Stats.ByReason[GroomReasonTaskList] != 1 {
		t.Fatalf("by-reason = %+v", got.Stats.ByReason)
	}
}

func TestDetectGroomActionsDuplicateScoping(t *testing.T) {
	const body = "identical duplicate content body about arm64 retries"
	t.Run("same scope dedups (lowest id kept)", func(t *testing.T) {
		cands := []GroomCandidate{
			{ID: 1, Key: "a", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
			{ID: 2, Key: "b", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
		}
		got := DetectGroomActions(cands)
		if len(got.Retirements) != 1 || got.Retirements[0].ID != 2 || got.Retirements[0].Reason != GroomReasonDuplicate {
			t.Fatalf("same-scope duplicate not deduped as expected: %+v", got.Retirements)
		}
	})
	t.Run("cross-owner identical content is NOT retired", func(t *testing.T) {
		cands := []GroomCandidate{
			{ID: 1, Key: "a", Content: body, OwnerKind: "agent", OwnerRef: "alice", Repo: "acme/widget", Scope: "repo"},
			{ID: 2, Key: "b", Content: body, OwnerKind: "agent", OwnerRef: "bob", Repo: "acme/widget", Scope: "repo"},
		}
		got := DetectGroomActions(cands)
		if len(got.Retirements) != 0 {
			t.Fatalf("cross-owner duplicate wrongly retired: %+v", got.Retirements)
		}
	})
	t.Run("cross-repo identical content is NOT retired", func(t *testing.T) {
		cands := []GroomCandidate{
			{ID: 1, Key: "a", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
			{ID: 2, Key: "b", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/gadget", Scope: "repo"},
		}
		got := DetectGroomActions(cands)
		if len(got.Retirements) != 0 {
			t.Fatalf("cross-repo duplicate wrongly retired: %+v", got.Retirements)
		}
	})
	t.Run("cross-scope (repo vs general) identical content is NOT retired", func(t *testing.T) {
		cands := []GroomCandidate{
			{ID: 1, Key: "a", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
			{ID: 2, Key: "b", Content: body, OwnerKind: "agent", OwnerRef: "lead", Repo: "", Scope: "general"},
		}
		got := DetectGroomActions(cands)
		if len(got.Retirements) != 0 {
			t.Fatalf("cross-scope duplicate wrongly retired: %+v", got.Retirements)
		}
	})
	t.Run("owner/repo/scope surfaced on the retirement", func(t *testing.T) {
		cands := []GroomCandidate{
			{ID: 1, Key: "a", Content: body, OwnerKind: "agent", OwnerRef: "lead", OwnerVersion: "v2", Repo: "acme/widget", Scope: "repo"},
			{ID: 2, Key: "b", Content: body, OwnerKind: "agent", OwnerRef: "lead", OwnerVersion: "v2", Repo: "acme/widget", Scope: "repo"},
		}
		got := DetectGroomActions(cands)
		if len(got.Retirements) != 1 {
			t.Fatalf("want 1 retirement, got %+v", got.Retirements)
		}
		r := got.Retirements[0]
		if r.Owner != "agent:lead@v2" || r.Repo != "acme/widget" || r.Scope != "repo" {
			t.Fatalf("owner/repo/scope not surfaced: %+v", r)
		}
	})
}

func TestDetectGroomActionsDeterministicOrder(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 3, Key: "c", Content: "- [x](x.md)\n- [y](y.md)\nSHIPPED it"},
		{ID: 1, Key: "a", Content: "STATUS: done and shipped, all green"},
		{ID: 2, Key: "b", Content: "- [ ] todo\n- [x] done"},
	}
	first := DetectGroomActions(cands)
	// Reverse the input; output must be identical (sorted by id internally).
	rev := []GroomCandidate{cands[2], cands[1], cands[0]}
	second := DetectGroomActions(rev)
	if len(first.Retirements) != len(second.Retirements) {
		t.Fatalf("retirement counts differ: %d vs %d", len(first.Retirements), len(second.Retirements))
	}
	for i := range first.Retirements {
		if first.Retirements[i] != second.Retirements[i] {
			t.Fatalf("retirement %d differs: %+v vs %+v", i, first.Retirements[i], second.Retirements[i])
		}
	}
	// Retirements come out id-ascending.
	for i := 1; i < len(first.Retirements); i++ {
		if first.Retirements[i-1].ID > first.Retirements[i].ID {
			t.Fatalf("retirements not id-sorted: %+v", first.Retirements)
		}
	}
}

func TestGroomFirstLine(t *testing.T) {
	if got := groomFirstLine("\n\n  first real line  \nsecond"); got != "first real line" {
		t.Fatalf("first line = %q", got)
	}
	long := strings.Repeat("z", groomFirstLineMax+40)
	if got := groomFirstLine(long); len(got) > groomFirstLineMax {
		t.Fatalf("first line not capped: %d chars", len(got))
	}
	if got := groomFirstLine("   \n  "); got != "" {
		t.Fatalf("blank content first line = %q, want empty", got)
	}
}
