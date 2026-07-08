package memory

import (
	"strings"
	"testing"
)

func TestSplitFrontmatterStripsWhenPresent(t *testing.T) {
	in := "---\ntitle: Notes\ntags: [a, b]\n---\n# Body\n\nreal content\n"
	fm, body := SplitFrontmatter(in)
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

func TestSplitFrontmatterPassthroughWhenAbsent(t *testing.T) {
	in := "# Just markdown\n\nno frontmatter here\n"
	fm, body := SplitFrontmatter(in)
	if fm != "" {
		t.Fatalf("expected no frontmatter, got %q", fm)
	}
	if body != in {
		t.Fatalf("body should be unchanged, got %q", body)
	}
}

func TestSplitFrontmatterUnclosedFenceKeepsContent(t *testing.T) {
	in := "---\nlooks like frontmatter but never closes\nmore text\n"
	fm, body := SplitFrontmatter(in)
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
	if len(chunks) != 3 {
		t.Fatalf("expected preamble + 2 heading chunks, got %d", len(chunks))
	}
	if chunks[0].Heading != "" {
		t.Fatalf("first chunk should be the empty-heading preamble, got %q", chunks[0].Heading)
	}
	if chunks[1].Heading != "First Topic" || chunks[2].Heading != "Second Topic" {
		t.Fatalf("headings not captured: %q / %q", chunks[1].Heading, chunks[2].Heading)
	}
	if !strings.HasPrefix(chunks[1].Text, "## First Topic") {
		t.Fatalf("heading line should be retained in chunk text: %q", chunks[1].Text[:20])
	}
}

func TestChunkMarkdownEmptyBody(t *testing.T) {
	if got := ChunkMarkdown("   \n\n  ", IngestMaxChunkTokens); got != nil {
		t.Fatalf("blank body must yield no chunks, got %v", got)
	}
}

func TestIngestKeyStableAndContentSensitive(t *testing.T) {
	k1 := IngestKey("runbook.md", "Deploy", "content A")
	k2 := IngestKey("runbook.md", "Deploy", "content A")
	k3 := IngestKey("runbook.md", "Deploy", "content B")
	if k1 != k2 {
		t.Fatalf("key must be stable for identical inputs: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Fatalf("changed content must change the key: %q", k1)
	}
	if !strings.HasPrefix(k1, "runbook-md-deploy-") {
		t.Fatalf("key shape unexpected: %q", k1)
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
