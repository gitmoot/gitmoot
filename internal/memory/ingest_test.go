package memory

import (
	"strings"
	"testing"
)

func TestStripFrontmatterStripsWhenPresent(t *testing.T) {
	in := "---\ntitle: Notes\ntags: [a, b]\n---\n# Body\n\nreal content\n"
	fm, body := StripFrontmatter(in)
	if !strings.Contains(fm, "title: Notes") {
		t.Fatalf("frontmatter not captured: %q", fm)
	}
	if strings.Contains(body, "title: Notes") {
		t.Fatalf("frontmatter leaked into body: %q", body)
	}
	if !strings.HasPrefix(body, "# Body") {
		t.Fatalf("body should start at the markdown heading: %q", body)
	}
}

func TestStripFrontmatterPassthroughWhenAbsent(t *testing.T) {
	in := "# Just markdown\n\nno frontmatter here\n"
	fm, body := StripFrontmatter(in)
	if fm != "" {
		t.Fatalf("expected no frontmatter, got %q", fm)
	}
	if body != in {
		t.Fatalf("body should be unchanged, got %q", body)
	}
}

func TestStripFrontmatterUnclosedFenceKeepsContent(t *testing.T) {
	in := "---\nlooks like frontmatter but never closes\nmore text\n"
	fm, body := StripFrontmatter(in)
	if fm != "" {
		t.Fatalf("unclosed fence must not be treated as frontmatter, got %q", fm)
	}
	if !strings.Contains(body, "never closes") {
		t.Fatalf("content lost: %q", body)
	}
}

func TestChunkMarkdownSingleWhenUnderBudget(t *testing.T) {
	body := "## A heading\n\nA short fact about the build system.\n"
	chunks := ChunkMarkdown(body, IngestMaxChunkTokens)
	if len(chunks) != 1 {
		t.Fatalf("under-budget body must be one chunk, got %d", len(chunks))
	}
	if chunks[0].Heading != "" {
		t.Fatalf("under-budget chunk keeps whole body with empty heading, got %q", chunks[0].Heading)
	}
	if !strings.Contains(chunks[0].Text, "A short fact") {
		t.Fatalf("chunk text missing content: %q", chunks[0].Text)
	}
}

func TestChunkMarkdownSplitsOnHeadingsWhenOverBudget(t *testing.T) {
	// Two sections each well over the ~4 chars/token budget so the whole body
	// exceeds it and the splitter engages.
	big := strings.Repeat("fact word here ", 200) // ~3000 chars
	body := "preamble line\n\n## First Topic\n\n" + big + "\n\n## Second Topic\n\n" + big + "\n"
	if EstimateTokens(body) <= IngestMaxChunkTokens {
		t.Fatalf("test body should exceed the budget; est=%d", EstimateTokens(body))
	}
	chunks := ChunkMarkdown(body, IngestMaxChunkTokens)
	// Each section is itself over budget, so it fans out into >=1 bounded piece;
	// the preamble is the first chunk and every chunk stays within budget.
	if len(chunks) < 3 {
		t.Fatalf("expected preamble + split heading sections, got %d", len(chunks))
	}
	if chunks[0].Heading != "" {
		t.Fatalf("first chunk should be the empty-heading preamble, got %q", chunks[0].Heading)
	}
	headings := map[string]bool{}
	for i, c := range chunks {
		headings[c.Heading] = true
		if got := EstimateTokens(c.Text); got > IngestMaxChunkTokens {
			t.Fatalf("chunk %d exceeds budget: est=%d > %d", i, got, IngestMaxChunkTokens)
		}
	}
	if !headings["First Topic"] || !headings["Second Topic"] {
		t.Fatalf("headings not captured: %v", headings)
	}
	if chunks[1].Heading != "First Topic" || !strings.HasPrefix(chunks[1].Text, "## First Topic") {
		t.Fatalf("first section chunk should carry its heading line: %q / %q", chunks[1].Heading, chunks[1].Text[:min(20, len(chunks[1].Text))])
	}
}

// TestChunkMarkdownBoundsHeadinglessBody proves an over-budget body with NO '## '
// headings (the pre-fix single-oversized-chunk case) fans out into multiple
// chunks, each within the token budget. Before the fix flush() emitted the entire
// body as one chunk that RenderBlock would force-inject wholesale.
func TestChunkMarkdownBoundsHeadinglessBody(t *testing.T) {
	body := strings.Repeat("deploy runbook fact about the CI box and arm64 runners\n", 400) // ~22k chars
	if EstimateTokens(body) <= IngestMaxChunkTokens {
		t.Fatalf("body must exceed the budget; est=%d", EstimateTokens(body))
	}
	chunks := ChunkMarkdown(body, IngestMaxChunkTokens)
	if len(chunks) < 2 {
		t.Fatalf("oversized heading-less body must sub-split, got %d chunk(s)", len(chunks))
	}
	for i, c := range chunks {
		if c.Heading != "" {
			t.Fatalf("chunk %d of a heading-less body should keep empty heading, got %q", i, c.Heading)
		}
		if got := EstimateTokens(c.Text); got > IngestMaxChunkTokens {
			t.Fatalf("chunk %d exceeds budget: est=%d > %d", i, got, IngestMaxChunkTokens)
		}
	}
}

// TestIngestedChunkFitsInjectionBudget is the ingest-to-render guard: a confirmed
// memory built from any ingested chunk cannot exceed the configured injection
// budget by more than RenderBlock's small fixed header/line-prefix overhead. It
// exercises a single giant section under one heading (the other over-budget shape
// the old splitter could not bound).
func TestIngestedChunkFitsInjectionBudget(t *testing.T) {
	body := "## Runbook\n\n" + strings.Repeat("a single giant section under one heading. ", 400)
	chunks := ChunkMarkdown(body, IngestMaxChunkTokens)
	if len(chunks) < 2 {
		t.Fatalf("giant single section must sub-split, got %d", len(chunks))
	}
	for i, c := range chunks {
		if got := EstimateTokens(c.Text); got > IngestMaxChunkTokens {
			t.Fatalf("chunk %d over budget: est=%d", i, got)
		}
		// Render the chunk as the sole (always-injected-first) confirmed memory.
		block, injected := RenderBlock([]Entry{{Scope: ScopeRepo, Content: c.Text}}, IngestMaxChunkTokens)
		if injected != 1 {
			t.Fatalf("chunk %d: expected the single memory to inject, got %d", i, injected)
		}
		// content <= budget + block header ("Prior learnings…") + "- <scope> " prefix.
		if got := EstimateTokens(block); got > IngestMaxChunkTokens+32 {
			t.Fatalf("chunk %d: rendered block far exceeds budget: %d", i, got)
		}
	}
}

func TestChunkMarkdownEmptyBody(t *testing.T) {
	if got := ChunkMarkdown("   \n\n  ", IngestMaxChunkTokens); got != nil {
		t.Fatalf("blank body must yield no chunks, got %v", got)
	}
}

// TestIngestKeyStableAcrossContentEdits is the #804 key contract: the key is a
// pure function of (file, heading), so an edited chunk keeps its key and a
// re-sweep can key-match the existing confirmed fact. Content participates only
// in dedup (ContentHash), never in the key.
func TestIngestKeyStableAcrossContentEdits(t *testing.T) {
	k1 := IngestKey("runbook.md", "Deploy")
	k2 := IngestKey("runbook.md", "Deploy")
	if k1 != k2 {
		t.Fatalf("key must be stable for identical inputs: %q vs %q", k1, k2)
	}
	if k1 != "runbook-md-deploy" {
		t.Fatalf("key shape unexpected: %q", k1)
	}
	if IngestKey("runbook.md", "") != "runbook-md-untitled" {
		t.Fatalf("empty heading should slug to untitled: %q", IngestKey("runbook.md", ""))
	}
}

// TestIngestKeyAllocatorOrdinals proves repeated (file, heading) chunks within
// one run get deterministic ordinal suffixes instead of colliding, and that an
// ordinal can never collide with a heading whose slug already ends in "-<n>".
func TestIngestKeyAllocatorOrdinals(t *testing.T) {
	a := NewIngestKeyAllocator()
	if got := a.Next("notes", "Setup"); got != "notes-setup" {
		t.Fatalf("first occurrence must take the bare stable key, got %q", got)
	}
	if got := a.Next("notes", "Setup"); got != "notes-setup-2" {
		t.Fatalf("second occurrence must take the -2 ordinal, got %q", got)
	}
	if got := a.Next("notes", "Setup 2"); got != "notes-setup-2-2" {
		t.Fatalf("a heading slugging to an already-allocated key must re-probe, got %q", got)
	}
	if got := a.Next("notes", "Setup"); got != "notes-setup-3" {
		t.Fatalf("third occurrence must take the -3 ordinal, got %q", got)
	}
	// A fresh allocator (a new sweep of the same document) hands out the same
	// sequence, so keys stay aligned across sweeps.
	b := NewIngestKeyAllocator()
	if b.Next("notes", "Setup") != "notes-setup" || b.Next("notes", "Setup") != "notes-setup-2" {
		t.Fatal("allocator must be deterministic across runs")
	}
}

func TestContentHashDeterministic(t *testing.T) {
	if ContentHash("x") != ContentHash("x") {
		t.Fatal("hash not deterministic")
	}
	if ContentHash("x") == ContentHash("y") {
		t.Fatal("distinct content must hash differently")
	}
	if len(ContentHash("x")) != 64 {
		t.Fatalf("expected full sha256 hex, got len %d", len(ContentHash("x")))
	}
}
