package memory

// Deterministic Obsidian-vault rendering for `gitmoot memory vault export`
// (RFC #737, P1). Everything here is a PURE, DB-free function of its inputs so
// the byte-for-byte determinism the export contract promises can be unit-tested
// in isolation: same inputs -> byte-identical output, every run, forever. The
// db-coupled orchestration (queries, file writes, atomic rename) lives in the
// cli package; this file owns only the string and hashing shapes.
//
// The vault is a DERIVED, DISPOSABLE VIEW, never a second source of truth: the
// SQLite store stays the only writer. Export regenerates the whole tree, so the
// output must depend ONLY on store contents — no timestamps of the run itself
// (deliberately no `exported_at`), no map iteration order, no wall clock.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// VaultMemory is the DB-free projection of one confirmed memory the vault
// renderer needs. It mirrors db.ConfirmedMemory but carries no store coupling so
// the rendering logic is pure.
type VaultMemory struct {
	ID           int64
	OwnerKind    string
	OwnerRef     string
	OwnerVersion string
	Repo         string // "" == general scope
	Scope        string
	Key          string
	Content      string
	Provenance   string
	SourceJob    string
	CreatedAt    string
	UpdatedAt    string
	SupersededBy int64 // 0 == not superseded
}

// VaultLink is one rendered [[wikilink]] from a note to a related memory. Stem
// is the target's filename without the .md extension.
type VaultLink struct {
	TargetID int64
	Stem     string
	Key      string
}

// vaultSlugRun matches runs of characters that are NOT lowercase-alphanumeric.
// Slugs are lowercased first so a case-insensitive filesystem can never collide
// two keys that differ only in case.
var vaultSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

const vaultSlugMaxLen = 60

// Slug normalizes a memory key into a lowercase, filesystem-safe filename
// fragment: lowercase, non-alphanumeric runs collapsed to a single '-', trimmed,
// and length-capped. The result is NEVER relied on for uniqueness — the id
// prefix in VaultFilename guarantees that — so an empty/degenerate key falling
// back to "untitled" is safe.
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = vaultSlugRun.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > vaultSlugMaxLen {
		s = strings.Trim(s[:vaultSlugMaxLen], "-")
	}
	if s == "" {
		return "untitled"
	}
	return s
}

// VaultFilename is the deterministic, collision-proof note filename for a
// memory: a zero-padded id prefix (uniqueness) plus a human-readable slug of the
// key. The id prefix means two keys that slug identically still get distinct
// files, on any filesystem, case-insensitive or not.
func VaultFilename(id int64, key string) string {
	return fmt.Sprintf("%09d-%s.md", id, Slug(key))
}

// VaultStem is VaultFilename without the .md extension — the target used inside a
// [[wikilink]].
func VaultStem(id int64, key string) string {
	return fmt.Sprintf("%09d-%s", id, Slug(key))
}

// yamlScalar renders a string as a double-quoted YAML scalar with the minimal
// escaping needed to stay valid and reversible. Always quoting (rather than
// bare-word heuristics) keeps the frontmatter deterministic regardless of value
// content — empty strings, colons, and leading symbols all render identically.
func yamlScalar(s string) string {
	r := strings.ReplaceAll(s, `\`, `\\`)
	r = strings.ReplaceAll(r, `"`, `\"`)
	r = strings.ReplaceAll(r, "\n", `\n`)
	r = strings.ReplaceAll(r, "\r", `\r`)
	r = strings.ReplaceAll(r, "\t", `\t`)
	return `"` + r + `"`
}

// RenderVaultNote renders one memory as an Obsidian note: sorted-key YAML
// frontmatter (LF endings, no exported_at), the content verbatim, then a Links
// section. links MUST already be the deterministic top-K set; they are sorted by
// target id here so rendering never depends on caller order. The body always
// ends with a trailing newline for a stable, editor-friendly shape.
func RenderVaultNote(m VaultMemory, links []VaultLink) string {
	type field struct{ key, val string }
	fields := make([]field, 0, 13)
	if m.OwnerKind == OwnerKindAgent {
		fields = append(fields, field{"agent", yamlScalar(m.OwnerRef)})
	}
	fields = append(fields,
		field{"created_at", yamlScalar(m.CreatedAt)},
		field{"key", yamlScalar(m.Key)},
		field{"memory_id", strconv.FormatInt(m.ID, 10)},
		field{"owner_kind", yamlScalar(m.OwnerKind)},
		field{"owner_ref", yamlScalar(m.OwnerRef)},
		field{"owner_version", yamlScalar(m.OwnerVersion)},
		field{"provenance", yamlScalar(m.Provenance)},
		field{"repo", yamlScalar(m.Repo)},
		field{"scope", yamlScalar(m.Scope)},
		field{"source_job", yamlScalar(m.SourceJob)},
		field{"updated_at", yamlScalar(m.UpdatedAt)},
	)
	if m.SupersededBy != 0 {
		fields = append(fields, field{"superseded_by", strconv.FormatInt(m.SupersededBy, 10)})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })

	var b strings.Builder
	b.WriteString("---\n")
	for _, f := range fields {
		b.WriteString(f.key)
		b.WriteString(": ")
		b.WriteString(f.val)
		b.WriteByte('\n')
	}
	b.WriteString("---\n")
	b.WriteString(m.Content)
	if !strings.HasSuffix(m.Content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("\n## Links\n")
	ordered := append([]VaultLink(nil), links...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TargetID < ordered[j].TargetID })
	if len(ordered) == 0 {
		b.WriteString("\n_No related memories._\n")
		return b.String()
	}
	b.WriteByte('\n')
	for _, l := range ordered {
		b.WriteString("- [[")
		b.WriteString(l.Stem)
		b.WriteString("|")
		b.WriteString(l.Key)
		b.WriteString("]]\n")
	}
	return b.String()
}

// VaultOwnerKey is the canonical, stable identity of a memory pool owner used to
// group notes under one index and to name the index file.
type VaultOwnerKey struct {
	Kind    string
	Ref     string
	Version string
}

// OwnerKey extracts the grouping key from a memory.
func (m VaultMemory) OwnerKey() VaultOwnerKey {
	return VaultOwnerKey{Kind: m.OwnerKind, Ref: m.OwnerRef, Version: m.OwnerVersion}
}

// VaultIndexFilename is the deterministic, collision-proof filename for an
// owner's index note. The leading underscore sorts index notes ahead of memory
// notes (which start with a digit) and makes them easy to spot. The slugged
// kind/ref/version stay human-readable, but slugging is lossy — two owners whose
// components slug identically (e.g. refs "build_bot" and "build-bot", both
// collapsing separators to '-', or refs differing only in case) would otherwise
// map to the same file, silently overwriting one index. A short stable hash of
// the RAW {Kind,Ref,Version} tuple is appended to guarantee distinct owners
// always get distinct files, mirroring how VaultFilename uses the id prefix.
func VaultIndexFilename(o VaultOwnerKey) string {
	name := "_index-" + Slug(o.Kind) + "-" + Slug(o.Ref)
	if strings.TrimSpace(o.Version) != "" {
		name += "-" + Slug(o.Version)
	}
	return name + "-" + vaultOwnerHash(o) + ".md"
}

// vaultOwnerHash is the first 8 hex chars of a sha256 over the raw owner tuple,
// NUL-separated so distinct component boundaries can't alias. It makes the index
// filename lossless: any change to Kind, Ref, or Version flips the suffix.
func vaultOwnerHash(o VaultOwnerKey) string {
	sum := sha256.Sum256([]byte(o.Kind + "\x00" + o.Ref + "\x00" + o.Version))
	return hex.EncodeToString(sum[:])[:8]
}

// RenderVaultIndex renders an owner's index note: memories grouped by repo (with
// a General section for general-scope facts), repos sorted, and keys sorted
// within each group. mems may be in any order; the output is fully deterministic.
func RenderVaultIndex(o VaultOwnerKey, mems []VaultMemory) string {
	general := make([]VaultMemory, 0)
	byRepo := make(map[string][]VaultMemory)
	for _, m := range mems {
		if m.Scope == ScopeGeneral || strings.TrimSpace(m.Repo) == "" {
			general = append(general, m)
		} else {
			byRepo[m.Repo] = append(byRepo[m.Repo], m)
		}
	}

	var b strings.Builder
	b.WriteString("# Memory index — ")
	b.WriteString(o.Ref)
	b.WriteString(" (")
	b.WriteString(o.Kind)
	if strings.TrimSpace(o.Version) != "" {
		b.WriteString(" ")
		b.WriteString(o.Version)
	}
	b.WriteString(")\n")

	writeSection := func(title string, group []VaultMemory) {
		if len(group) == 0 {
			return
		}
		sort.Slice(group, func(i, j int) bool {
			if group[i].Key != group[j].Key {
				return group[i].Key < group[j].Key
			}
			return group[i].ID < group[j].ID
		})
		b.WriteString("\n## ")
		b.WriteString(title)
		b.WriteString("\n\n")
		for _, m := range group {
			b.WriteString("- [[")
			b.WriteString(VaultStem(m.ID, m.Key))
			b.WriteString("|")
			b.WriteString(m.Key)
			b.WriteString("]]\n")
		}
	}

	writeSection("General", general)
	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, r := range repos {
		writeSection(r, byRepo[r])
	}
	return b.String()
}

// VaultFileDigest is one memory note's contribution to the snapshot hash.
type VaultFileDigest struct {
	ID        int64
	UpdatedAt string
	Sum       string // hex sha256 of the note's bytes
}

// VaultSnapshotHash is the manifest staleness anchor (P2's diff depends on it):
// sha256 over the id-sorted sequence of "<id>\t<updated_at>\t<sha256(file)>\n"
// lines. It captures exactly the confirmed-memory content that produced the
// export, so a fresh export from an unchanged store yields the same hash and any
// store mutation flips it. Index notes and the manifest itself are excluded (the
// manifest cannot hash itself; index notes are pure derivations of the members).
func VaultSnapshotHash(digests []VaultFileDigest) string {
	ordered := append([]VaultFileDigest(nil), digests...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	h := sha256.New()
	for _, d := range ordered {
		fmt.Fprintf(h, "%d\t%s\t%s\n", d.ID, d.UpdatedAt, d.Sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}
