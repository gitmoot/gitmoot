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
	"fmt"
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

// StripFrontmatter strips a leading YAML frontmatter block ("---\n … \n---")
// when present and returns it alongside the remaining markdown body. When no
// frontmatter is present (or the opening fence has no closing fence) it returns
// an empty frontmatter and the content unchanged. Unlike the agenttemplate
// parser it never errors: for ingest, frontmatter is optional metadata to
// discard, never a hard requirement. CRLF endings are normalized to LF first.
func StripFrontmatter(content string) (frontmatter, body string) {
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

// ChunkMarkdown splits a markdown body into ingestible chunks each estimating at
// or under maxTokens. When the whole body fits it is a single chunk (empty
// Heading). Over budget, it splits on lines beginning with '## ': content before
// the first heading becomes a preamble chunk (empty Heading), and each heading
// starts a new chunk whose Text retains the heading line. A heading-less body (or
// a single section still over budget) is further sub-split by paragraph/line/rune
// windows so NO emitted chunk can exceed maxTokens — an oversized confirmed
// memory would otherwise be force-injected wholesale by RenderBlock (its
// always-emit-the-first-entry guarantee) and blow the injection budget. Chunks
// whose text is blank after trimming are dropped. A blank body yields no chunks.
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
		cur = nil
		if text == "" {
			return
		}
		// Bound every section to the budget; a heading-less body or a single
		// oversized section still fans out into <=maxTokens pieces here.
		for _, piece := range splitToBudget(text, maxTokens) {
			chunks = append(chunks, Chunk{Heading: curHeading, Text: piece})
		}
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

// splitToBudget breaks text into pieces each estimating at or under maxTokens,
// preferring paragraph, then line, then hard rune-window boundaries so an
// ingested chunk can never exceed the injection budget once confirmed. Every
// returned piece is trimmed and non-empty. Text already under budget is returned
// unchanged (single element).
func splitToBudget(text string, maxTokens int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxTokens <= 0 || EstimateTokens(text) <= maxTokens {
		return []string{text}
	}
	var out []string
	var cur string
	for _, unit := range atomicUnits(text, maxTokens) {
		switch {
		case cur == "":
			cur = unit
		case EstimateTokens(cur+"\n"+unit) <= maxTokens:
			cur = cur + "\n" + unit
		default:
			out = append(out, cur)
			cur = unit
		}
	}
	if strings.TrimSpace(cur) != "" {
		out = append(out, cur)
	}
	return out
}

// atomicUnits breaks text into ordered fragments each estimating at or under
// maxTokens, descending through paragraph (blank-line), then line, then hard
// rune-window boundaries. Blank fragments are dropped.
func atomicUnits(text string, maxTokens int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if EstimateTokens(para) <= maxTokens {
			out = append(out, para)
			continue
		}
		for _, line := range strings.Split(para, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if EstimateTokens(line) <= maxTokens {
				out = append(out, line)
				continue
			}
			out = append(out, hardWindows(line, maxTokens)...)
		}
	}
	return out
}

// hardWindows chops a single over-budget line into consecutive rune windows each
// small enough that EstimateTokens stays at or under maxTokens (EstimateTokens is
// (bytes+3)/4, so a window of 4*maxTokens-3 bytes is the ceiling). Rune-aligned so
// multibyte characters are never split.
func hardWindows(s string, maxTokens int) []string {
	maxBytes := 4*maxTokens - 3
	if maxBytes < 1 {
		maxBytes = 1
	}
	var out []string
	var b strings.Builder
	for _, r := range s {
		if b.Len() > 0 && b.Len()+len(string(r)) > maxBytes {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// ContentHash is the deterministic full hex SHA-256 of a chunk's content, used
// for exact-match dedup against existing observations and confirmed rows.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// IngestKey derives the STABLE observation key for a chunk from the source file
// stem and heading alone: slug(file)-slug(heading). The content hash is
// deliberately NOT part of the key (#804) — it participates only in exact-match
// dedup (ContentHash / db.MemoryDedupKey) — so a re-swept EDITED chunk keeps the
// key of its earlier edition and auto-confirm key-matches the existing confirmed
// fact, updating it in place instead of spawning a hash-suffixed sibling. An
// empty heading slugs to "untitled" (see Slug).
func IngestKey(fileStem, heading string) string {
	return Slug(fileStem) + "-" + Slug(heading)
}

// IngestKeyAllocator hands out ingest keys for ONE sweep. Without the content
// hash, chunks that share a (file, heading) — an over-budget section split into
// pieces, or a repeated heading — would collide on the same stable key and, under
// auto-confirm, clobber each other within a single run. The allocator gives the
// first occurrence the bare stable key and later occurrences a "-2", "-3", …
// ordinal in document order. Allocate for EVERY chunk the chunker emits (before
// pre-filter/dedup drops) so ordinals mirror document structure and stay aligned
// across sweeps of an unchanged file set.
type IngestKeyAllocator struct {
	used map[string]struct{}
}

// NewIngestKeyAllocator returns an empty allocator for one ingest run.
func NewIngestKeyAllocator() *IngestKeyAllocator {
	return &IngestKeyAllocator{used: make(map[string]struct{})}
}

// Next returns the key for the next chunk of (fileStem, heading). It re-probes
// against every key already handed out this run, so an ordinal suffix can never
// collide with another heading whose slug happens to end in "-<n>".
func (a *IngestKeyAllocator) Next(fileStem, heading string) string {
	base := IngestKey(fileStem, heading)
	key := base
	for n := 2; ; n++ {
		if _, taken := a.used[key]; !taken {
			break
		}
		key = fmt.Sprintf("%s-%d", base, n)
	}
	a.used[key] = struct{}{}
	return key
}
