package memory

// Frontmatter parsing for the vault round-trip (`memory vault import`, RFC #737
// P2). This is the inverse of RenderVaultNote in vault.go: the exporter renders a
// confirmed memory to an Obsidian note (sorted YAML frontmatter, the content
// verbatim, then a "## Links" section); the importer parses an owner-edited note
// back into its frontmatter map and body so a content edit can be written to the
// exact source row. It is a small, PURE, DB-free helper so the round-trip
// determinism can be unit-tested in isolation.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// vaultLinksMarker is the literal RenderVaultNote emits to open the Links section
// ("\n## Links\n"). Everything before it (after the frontmatter) is the memory
// body; the section itself is a pure derivation of FTS co-occurrence and is
// discarded on import (it is regenerated on the next export).
const vaultLinksMarker = "\n## Links\n"

// SplitFrontmatter separates a leading YAML frontmatter block delimited by `---`
// fences from the remaining body. It is the generalized form of agenttemplate's
// splitFrontmatter, lifted here so the memory vault round-trip can reuse it. CRLF
// is normalized to LF first. The returned body preserves the content after the
// closing fence with only leading blank lines trimmed (mirroring the exporter,
// which writes the content immediately after the closing "---\n").
func SplitFrontmatter(content string) (frontmatter, body string, err error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return "", "", errors.New("note content is empty")
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", errors.New("note must start with YAML frontmatter")
	}
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) != "---" {
			continue
		}
		frontmatter = strings.Join(lines[1:index], "\n")
		body = strings.TrimLeft(strings.Join(lines[index+1:], "\n"), "\n")
		return frontmatter, body, nil
	}
	return "", "", errors.New("note frontmatter is missing closing ---")
}

// VaultNoteFile is a parsed vault note: the frontmatter decoded into a generic
// map (values are the yaml.v3 native types — string, int, etc.) and the memory
// body with the derived "## Links" section stripped.
type VaultNoteFile struct {
	Frontmatter map[string]any
	Body        string
}

// ParseVaultNote parses one exported vault note back into its frontmatter map and
// memory body. The body is the content between the closing frontmatter fence and
// the "## Links" section (the section is a regenerated derivation, never part of
// the stored memory). It is the inverse of RenderVaultNote.
func ParseVaultNote(content string) (VaultNoteFile, error) {
	fm, body, err := SplitFrontmatter(content)
	if err != nil {
		return VaultNoteFile{}, err
	}
	values := map[string]any{}
	if strings.TrimSpace(fm) != "" {
		if err := yaml.Unmarshal([]byte(fm), &values); err != nil {
			return VaultNoteFile{}, fmt.Errorf("parse note frontmatter: %w", err)
		}
	}
	// Strip the derived Links section. RenderVaultNote always appends
	// "\n## Links\n…" after the (newline-terminated) content, so the memory body is
	// everything before the LAST occurrence of the marker — using the last match so
	// a body that itself contains the string cannot truncate the content.
	if idx := strings.LastIndex(body, vaultLinksMarker); idx >= 0 {
		body = body[:idx] // the marker's leading "\n" is a separator; the content
		// keeps its own trailing newline, which sits just before it.
	}
	return VaultNoteFile{Frontmatter: values, Body: body}, nil
}

// MemoryID returns the memory_id recorded in the frontmatter and whether it was
// present and a positive integer. A note with no (or non-positive) memory_id is
// treated by the importer as an owner-authored NEW note rather than an edit.
func (f VaultNoteFile) MemoryID() (int64, bool) {
	raw, ok := f.Frontmatter["memory_id"]
	if !ok {
		return 0, false
	}
	id, ok := frontmatterInt(raw)
	if !ok || id <= 0 {
		return 0, false
	}
	return id, true
}

// String returns a frontmatter value as a string (the exporter renders every
// scalar field as a quoted YAML string), or "" if absent.
func (f VaultNoteFile) String(key string) string {
	raw, ok := f.Frontmatter[key]
	if !ok {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	if n, ok := frontmatterInt(raw); ok {
		return strconv.FormatInt(n, 10)
	}
	return fmt.Sprintf("%v", raw)
}

// VaultMemory reconstructs the DB-free projection from a parsed note's frontmatter
// and body. It is used by the round-trip test (render → parse → re-render must be
// byte-identical) and lets an importer recover the full record shape. Content is
// taken from the parsed Body (the human-editable region), not the frontmatter.
func (f VaultNoteFile) VaultMemory() VaultMemory {
	m := VaultMemory{
		OwnerKind:    f.String("owner_kind"),
		OwnerRef:     f.String("owner_ref"),
		OwnerVersion: f.String("owner_version"),
		Repo:         f.String("repo"),
		Scope:        f.String("scope"),
		Key:          f.String("key"),
		Content:      f.Body,
		Provenance:   f.String("provenance"),
		SourceJob:    f.String("source_job"),
		CreatedAt:    f.String("created_at"),
		UpdatedAt:    f.String("updated_at"),
	}
	if id, ok := f.MemoryID(); ok {
		m.ID = id
	}
	if raw, ok := f.Frontmatter["superseded_by"]; ok {
		if n, ok := frontmatterInt(raw); ok {
			m.SupersededBy = n
		}
	}
	return m
}

// frontmatterInt coerces a yaml.v3-decoded scalar to an int64. yaml.v3 decodes a
// bare integer as int; a quoted integer (how the exporter renders string fields)
// decodes as string.
func frontmatterInt(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}
