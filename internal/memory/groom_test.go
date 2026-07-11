package memory

import (
	"os"
	"path/filepath"
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

func TestSplitBrickLosslessFact80Shape(t *testing.T) {
	storyA := "The waveform speaker split shipped in PR #812. " + strings.Repeat("The implementation kept frame timing stable while assigning speakers deterministically. ", 10)
	storyB := "The goal-set workflow shipped in PR #819. " + strings.Repeat("The implementation preserved owner review and made continuation state explicit. ", 10)
	content := "**Waveform speaker split**\n" + storyA + "\n\n**Goal-set workflow**\n" + storyB
	if tokens := EstimateTokens(content); tokens < 400 || tokens > IngestMaxChunkTokens {
		t.Fatalf("fixture tokens = %d, want fact-80-shaped 400..%d", tokens, IngestMaxChunkTokens)
	}

	children := SplitBrick("editor-waveform-speakersplit-goalset", content)
	if len(children) != 2 {
		t.Fatalf("children = %+v, want 2", children)
	}
	if children[0].Key != "editor-waveform-speakersplit-goalset-waveform-speaker-split" ||
		children[1].Key != "editor-waveform-speakersplit-goalset-goal-set-workflow" {
		t.Fatalf("child keys = %q, %q", children[0].Key, children[1].Key)
	}
	if got := concatGroomSplitChildren(children); got != strings.TrimSpace(content) {
		t.Fatalf("lossless invariant failed:\ngot  %q\nwant %q", got, strings.TrimSpace(content))
	}
	for _, child := range children {
		if !strings.Contains(content, child.Content) {
			t.Fatalf("child is not an exact parent substring: %q", child.Content)
		}
		if nested := SplitBrick(child.Key, child.Content); len(nested) != 0 {
			t.Fatalf("split child qualifies again: %+v", nested)
		}
	}
}

func TestSplitBrickStrongSeamsBelowRewriteThreshold(t *testing.T) {
	content := "**First shipped story**\n" + strings.Repeat("The cache invalidation path now preserves live sessions. ", 5) +
		"\n\n**Second shipped story**\n" + strings.Repeat("The retry path now records its durable terminal reason. ", 5)
	if len(content) >= GroomRewriteThreshold {
		t.Fatal("fixture must exercise seam qualification below the length threshold")
	}
	children := SplitBrick("session-note", content)
	if len(children) != 2 {
		t.Fatalf("children = %+v, want 2", children)
	}
	proposal := DetectGroomActions([]GroomCandidate{{ID: 7, Key: "session-note", Content: content}})
	if len(proposal.RewriteFlags) != 1 || proposal.RewriteFlags[0].ID != 7 {
		t.Fatalf("multi-story brick should be flagged even below threshold: %+v", proposal.RewriteFlags)
	}
}

func TestSplitBrickDateAndPRMarkerLines(t *testing.T) {
	content := "2026-07-10\n" + strings.Repeat("The deployment story includes enough substantive detail. ", 5) +
		"\nPR #832\n" + strings.Repeat("The groom split story includes enough substantive detail. ", 5)
	children := SplitBrick("dated-stories", content)
	if len(children) != 2 || !strings.HasSuffix(children[0].Key, "2026-07-10") || !strings.HasSuffix(children[1].Key, "pr-832") {
		t.Fatalf("date/PR seams = %+v", children)
	}
	if concatGroomSplitChildren(children) != strings.TrimSpace(content) {
		t.Fatal("date/PR split lost parent bytes")
	}
}

func TestSplitBrickSeamPoorLongProseFallsBackToFlagOnly(t *testing.T) {
	content := strings.Repeat("This is one continuous substantive narrative without a safe paragraph seam. ", 30)
	if len(content) <= GroomRewriteThreshold {
		t.Fatal("fixture must exceed GroomRewriteThreshold")
	}
	if got := SplitBrick("long-prose", content); len(got) != 0 {
		t.Fatalf("seam-poor prose must not split: %+v", got)
	}
	proposal := DetectGroomActions([]GroomCandidate{{ID: 8, Key: "long-prose", Content: content}})
	if len(proposal.RewriteFlags) != 1 || proposal.RewriteFlags[0].ID != 8 {
		t.Fatalf("seam-poor prose must remain flag-only: %+v", proposal)
	}
}

func TestDetectGroomSplitsDeterministicAndRejectsNonSubstantiveSegments(t *testing.T) {
	good := "**Alpha**\n" + strings.Repeat("A substantive first implementation story remains stable. ", 5) +
		"\n\n**Beta**\n" + strings.Repeat("A substantive second implementation story remains stable. ", 5)
	bad := "**Alpha**\n" + strings.Repeat("A substantive implementation story remains stable. ", 5) + "\n\n**Beta**\nx"
	cands := []GroomCandidate{
		{ID: 20, Key: "bad", Content: bad, UpdatedAt: "u20"},
		{ID: 10, Key: "good", Content: good, UpdatedAt: "u10"},
	}
	got := DetectGroomSplits(cands)
	if len(got) != 1 || got[0].ParentID != 10 || got[0].ExpectedUpdatedAt != "u10" {
		t.Fatalf("splits = %+v", got)
	}
	reversed := DetectGroomSplits([]GroomCandidate{cands[1], cands[0]})
	if len(reversed) != 1 || reversed[0].ParentID != got[0].ParentID ||
		concatGroomSplitChildren(reversed[0].Children) != concatGroomSplitChildren(got[0].Children) {
		t.Fatalf("split output changed with input order: first=%+v reversed=%+v", got, reversed)
	}
}

func TestDetectGroomSplitsAllocatesAroundExistingScopeKeys(t *testing.T) {
	content := "**Alpha story**\n" + strings.Repeat("A substantive first implementation story remains stable. ", 5) +
		"\n\n**Beta story**\n" + strings.Repeat("A substantive second implementation story remains stable. ", 5)
	cands := []GroomCandidate{
		{ID: 1, Key: "parent", Content: content, OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
		{ID: 2, Key: "parent-alpha-story", Content: "an existing fact", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/widget", Scope: "repo"},
	}
	got := DetectGroomSplits(cands)
	if len(got) != 1 || got[0].Children[0].Key != "parent-alpha-story-2" || got[0].Children[1].Key != "parent-beta-story" {
		t.Fatalf("collision-safe keys = %+v", got)
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

// TestSplitBrickBoldLeadInlineSeams covers the fact-80 shape that motivated
// #832: two stories whose bold headers LEAD their lines with prose continuing
// on the same line, and NO blank line between the stories. The headers carry
// date/PR evidence so they are story seams; sub-field leads like "**Why:**"
// must NOT cut.
func TestSplitBrickBoldLeadInlineSeams(t *testing.T) {
	content := "**Waveform refinement (2026-06-19, PR #241, main x):** fixed blocky zoom\n" +
		strings.Repeat("The waveform path preserves exact source timing and stable redraw behavior. ", 4) +
		"\n**Per-tile focus bug fixed (2026-06-21, PR #242):** assigning a person\n**Why:** the event was overloaded\n" +
		strings.Repeat("The tile path preserves the split while updating only the selected track. ", 4)
	children := SplitBrick("editor-goalset", content)
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2 (one per dated bold-lead story; **Why:** must not cut): %+v", len(children), children)
	}
	joined := children[0].Content + "\n" + children[1].Content
	if strings.TrimSpace(joined) == "" || !strings.Contains(children[0].Content, "Waveform refinement") || !strings.Contains(children[1].Content, "Per-tile focus bug") {
		t.Fatalf("unexpected partition: %+v", children)
	}
	if !strings.Contains(children[1].Content, "**Why:**") {
		t.Fatalf("sub-field lead should stay inside story 2: %q", children[1].Content)
	}
}

func TestGroomStrongSeamGuards(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "plain PR marker", line: "PR #842", want: true},
		{name: "dash list PR", line: "- #147 phrasing perturbation", want: false},
		{name: "asterisk list PR", line: "* PR #147 phrasing perturbation", want: false},
		{name: "plus list date", line: "+ 2026-07-11 shipped", want: false},
		{name: "number-dot list date", line: "1. 2026-07-11 shipped", want: false},
		{name: "number-paren list PR", line: "2) PR #147 shipped", want: false},
		{name: "why sub-field", line: "**Why:**", want: false},
		{name: "how-to-apply sub-field", line: "**How to apply:**", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGroomStrongSeam(tc.line); got != tc.want {
				t.Fatalf("isGroomStrongSeam(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestMergeGroomRunts(t *testing.T) {
	tests := []struct {
		name     string
		lengths  []int
		wantEnds []int
	}{
		{name: "first runt merges forward", lengths: []int{100, 250, 250}, wantEnds: []int{350, 600}},
		{name: "middle runt merges backward", lengths: []int{250, 100, 250}, wantEnds: []int{350, 600}},
		{name: "last runt merges backward", lengths: []int{250, 250, 100}, wantEnds: []int{250, 600}},
		{name: "repeat until one remains", lengths: []int{100, 100, 100}, wantEnds: []int{300}},
		{name: "exact threshold survives", lengths: []int{200, 200}, wantEnds: []int{200, 400}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var content strings.Builder
			segments := make([]groomTextUnit, 0, len(tc.lengths))
			start := 0
			for i, n := range tc.lengths {
				content.WriteString(strings.Repeat(string(rune('a'+i)), n))
				segments = append(segments, groomTextUnit{start: start, end: start + n})
				start += n
			}
			got := mergeGroomRunts(segments, content.String())
			if len(got) != len(tc.wantEnds) {
				t.Fatalf("segments = %+v, want ends %v", got, tc.wantEnds)
			}
			for i, wantEnd := range tc.wantEnds {
				if got[i].end != wantEnd {
					t.Fatalf("segment %d end = %d, want %d (%+v)", i, got[i].end, wantEnd, got)
				}
			}
		})
	}
}

func TestSplitBrickRejectsStatusChangelog(t *testing.T) {
	line := strings.Repeat(" release status detail", 14)
	content := "2026-07-11" + line + "\n2026-07-10" + line + "\n2026-07-09" + line
	if !detectStatusChangelog(content) {
		t.Fatal("fixture must be detected as a status changelog")
	}
	if got := SplitBrick("release-status", content); got != nil {
		t.Fatalf("status changelog split = %+v, want nil", got)
	}
}

func TestSplitBrickLiveRegressionFixtures(t *testing.T) {
	read := func(id string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("testdata", "groom", id+".md"))
		if err != nil {
			t.Fatalf("read fixture %s: %v", id, err)
		}
		return strings.TrimSuffix(string(body), "\n")
	}

	children := SplitBrick("editor-waveform-speakersplit-goalset", read("80"))
	if len(children) != 2 {
		t.Fatalf("fact 80 children = %+v, want exactly 2", children)
	}
	if !strings.HasPrefix(children[0].Content, "**Waveform refinement (2026-06-19, PR #241") ||
		!strings.HasPrefix(children[1].Content, "**Per-tile speaker-focus bug fixed (2026-06-21, PR #242") {
		t.Fatalf("fact 80 story heads = %q / %q", groomFirstLine(children[0].Content), groomFirstLine(children[1].Content))
	}
	wantLabels := []string{
		"Waveform refinement (2026-06-19, PR #241, main `40b92f0`)",
		"Per-tile speaker-focus bug fixed (2026-06-21, PR #242, main `f249a12`)",
	}
	for i, child := range children {
		if label := groomSeamLabel(groomFirstNonBlankLine(groomTextUnit{start: 0, end: len(child.Content)}, child.Content)); label != wantLabels[i] {
			t.Fatalf("fact 80 child %d label = %q, want %q", i, label, wantLabels[i])
		}
	}
	if concatGroomSplitChildren(children) != read("80") {
		t.Fatal("fact 80 split lost coverage")
	}

	for _, id := range []string{"152", "204", "264"} {
		if got := SplitBrick("live-fact-"+id, read(id)); got != nil {
			t.Fatalf("fact %s split = %+v, want nil", id, got)
		}
	}
}

func TestDetectGroomLLMCandidatesLargestFirst(t *testing.T) {
	long := func(word string, n int) string { return strings.Repeat(word+" continuous prose. ", n) }
	cands := []GroomCandidate{
		{ID: 4, Key: "short", Content: "short keeper"},
		{ID: 3, Key: "medium", Content: long("medium", 75)},
		{ID: 2, Key: "largest", Content: long("largest", 100)},
		{ID: 1, Key: "same-size-lower-id", Content: long("largest", 100)},
		{ID: 5, Key: "deterministic", Content: "**First story (PR #1)**\n" + long("first", 40) + "\n**Second story (PR #2)**\n" + long("second", 40)},
		{ID: 6, Key: "status", Content: "2026-07-11 shipped " + long("status", 25) + "\n2026-07-10 shipped " + long("status", 25) + "\n2026-07-09 shipped " + long("status", 25)},
	}
	got := DetectGroomLLMCandidates(cands)
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("LLM candidates = %+v, want ids 1,2,3 largest-first with id tie-break", got)
	}
	for _, candidate := range got {
		if candidate.ContentHash != GroomContentHash(candidate.Content) || candidate.Bytes != len(strings.TrimSpace(candidate.Content)) {
			t.Fatalf("candidate metadata = %+v", candidate)
		}
	}
	if GroomContentHash("  same body\n") != GroomContentHash("same body") {
		t.Fatal("content hash must use trimmed coverage")
	}
}

func TestEnumerateGroomLLMBoundariesExcludesFencesAndLists(t *testing.T) {
	content := "Intro paragraph.\n\nSecond paragraph starts here.\nPR #842 evidence line.\n\n- list item is not safe\n\n```go\ninside fence\n\nPR #999 not safe\n```\n\nFinal paragraph starts here."
	menu := EnumerateGroomLLMBoundaries(content)
	want := []struct {
		id     string
		offset int
		text   string
	}{
		{id: "c001", offset: strings.Index(content, "Second paragraph"), text: "Second paragraph starts here."},
		{id: "c002", offset: strings.Index(content, "PR #842"), text: "PR #842 evidence line."},
		{id: "c003", offset: strings.Index(content, "Final paragraph"), text: "Final paragraph starts here."},
	}
	if len(menu) != len(want) {
		t.Fatalf("menu = %+v, want %+v", menu, want)
	}
	for i := range want {
		if menu[i].ID != want[i].id || menu[i].Offset != want[i].offset || menu[i].Text != want[i].text {
			t.Fatalf("menu[%d] = %+v, want %+v", i, menu[i], want[i])
		}
		if content[menu[i].Offset:menu[i].Offset+len(menu[i].Text)] != menu[i].Text {
			t.Fatalf("menu[%d] offset does not map to echoed text", i)
		}
	}
}

func TestBuildGroomSplitFromOffsetsUsesDeterministicTail(t *testing.T) {
	first := strings.Repeat("first substantive segment. ", 12)
	second := strings.Repeat("second substantive segment. ", 12)
	content := first + "\n\nSecond subject\n" + second
	offset := strings.Index(content, "Second subject")
	children := BuildGroomSplitFromOffsets("parent", content, []int{offset})
	if len(children) != 2 || concatGroomSplitChildren(children) != strings.TrimSpace(content) {
		t.Fatalf("offset split = %+v", children)
	}
	if got := BuildGroomSplitFromOffsets("parent", content, []int{1}); got != nil {
		t.Fatalf("runt-only cut should merge to one segment: %+v", got)
	}
}
