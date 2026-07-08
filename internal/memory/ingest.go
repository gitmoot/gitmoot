package memory

// Pure, DB-free logic for `gitmoot memory ingest` (RFC #737, P3): the optional
// YAML-frontmatter stripper, the heading-aware chunker, and the deterministic
// content hash / observation key derivation. Like the rest of this package it
// owns NO database or filesystem coupling so the boundaries (frontmatter present
// vs absent, under vs over the token budget, heading splits) can be unit-tested
// in isolation. The db writes and directory walk live in the cli layer.
//
// Ingested markdown is UNTRUSTED: it is an indirect-prompt-injection vector, so
// every chunk this file produces is gated by PreFilter and born trust_mark=low
// on the way into memory_observations. Nothing here promotes; the human confirm
// gate is the trust boundary.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// IngestMaxChunkTokens is the token budget above which an ingested file body is
// split on '## ' headings instead of stored as a single observation. It uses the
// same cheap EstimateTokens heuristic as the injection budget.
const IngestMaxChunkTokens = 512

// Chunk is one ingestible unit of markdown: a heading label (empty for the
// pre-heading preamble or an un-split whole-file body) and the full chunk Text
// (the '## ' heading line is retained in Text so the stored fact stays
// self-describing). Text is always trimmed and non-empty.
type Chunk struct {
	Heading string
	Text    string
}

// SplitFrontmatter strips a leading YAML frontmatter block ("---\n … \n---")
// when present and returns it alongside the remaining markdown body. When no
// frontmatter is present (or the opening fence has no closing fence) it returns
// an empty frontmatter and the content unchanged. Unlike the agenttemplate
// parser it never errors: for ingest, frontmatter is optional metadata to
// discard, never a hard requirement. CRLF endings are normalized to LF first.
func SplitFrontmatter(content string) (frontmatter, body string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	trimmedLeading := strings.TrimLeft(normalized, "\n")
	lines := strings.Split(trimmedLeading, "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return "", normalized
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			fm := strings.Join(lines[1:i], "\n")
			b := strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n")
			return fm, b
		}
	}
	// Opening fence with no closing fence: not real frontmatter, keep it all.
	return "", normalized
}

// ChunkMarkdown splits a markdown body into ingestible chunks. When the whole
// body estimates at or under maxTokens it is a single chunk (empty Heading).
// Over budget, it splits on lines beginning with '## ': content before the first
// heading becomes a preamble chunk (empty Heading), and each heading starts a new
// chunk whose Text retains the heading line. Chunks whose text is blank after
// trimming are dropped. A blank body yields no chunks.
func ChunkMarkdown(body string, maxTokens int) []Chunk {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if EstimateTokens(body) <= maxTokens {
		return []Chunk{{Heading: "", Text: strings.TrimSpace(body)}}
	}

	var chunks []Chunk
	var curHeading string
	var cur []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(cur, "\n"))
		if text != "" {
			chunks = append(chunks, Chunk{Heading: curHeading, Text: text})
		}
		cur = nil
	}
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "## ") {
			flush()
			curHeading = strings.TrimSpace(strings.TrimPrefix(ln, "## "))
			cur = []string{ln}
			continue
		}
		cur = append(cur, ln)
	}
	flush()
	return chunks
}

// ContentHash is the deterministic full hex SHA-256 of a chunk's content, used
// for exact-match dedup against existing observations and confirmed rows.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// IngestKey derives a stable observation key from the source file stem, the
// chunk heading, and the chunk content: slug(file)-slug(heading)-<8 hex of the
// content hash>. The 8-hex content suffix keeps re-ingesting an unchanged chunk
// idempotent at the key level while still distinguishing edited content. An
// empty heading slugs to "untitled" (see Slug), which is fine — the hash suffix
// carries the uniqueness.
func IngestKey(fileStem, heading, content string) string {
	return Slug(fileStem) + "-" + Slug(heading) + "-" + ContentHash(content)[:8]
}
