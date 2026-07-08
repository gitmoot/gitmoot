package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// memory vault import (#737 P2) — the human curation gate as an explicit
// round-trip over the P1 export. The flow is strictly: read the vault's manifest →
// regenerate a FRESH export in memory from the current store → abort if they
// disagree (the store moved since export, so the diff would be meaningless) →
// classify every note on disk against the fresh export (unchanged/edited/deleted/
// new) → print the diff. --dry-run (the DEFAULT) writes nothing; --yes applies the
// whole plan in one transaction. The markdown is a disposable view: nothing here
// treats it as a second source of truth — every write still lands through a store
// op (CAS content update, retire, or a pending observation behind the confirm gate).

// vaultImportItem summarizes one planned mutation for the diff/JSON output.
type vaultImportItem struct {
	MemoryID int64  `json:"memory_id,omitempty"`
	Key      string `json:"key,omitempty"`
	File     string `json:"file,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// vaultImportResult is the --json summary (and the backing model for the text diff).
type vaultImportResult struct {
	Dir             string            `json:"dir"`
	Applied         bool              `json:"applied"`
	DryRun          bool              `json:"dry_run"`
	SnapshotHash    string            `json:"snapshot_hash"`
	Updates         []vaultImportItem `json:"updates"`
	Retirements     []vaultImportItem `json:"retirements"`
	NewObservations []vaultImportItem `json:"new_observations"`
	Unchanged       int               `json:"unchanged"`
	Warnings        []string          `json:"warnings"`
}

func runMemoryVaultImport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory vault import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	dryRun := fs.Bool("dry-run", false, "print the diff and write nothing (this is the default)")
	yes := fs.Bool("yes", false, "apply the diff (edits, retirements, and new observations) in one transaction")
	jsonOut := fs.Bool("json", false, "print the import summary as JSON")
	// The directory is a positional and may appear before OR after the flags
	// (`import <dir> --yes` and `import --yes <dir>` are both natural). Go's flag
	// parser stops at the first non-flag argument, so parse the leading flags, lift
	// out the first positional, then parse whatever flags trailed it.
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	dir := strings.TrimSpace(fs.Arg(0))
	if rest := fs.Args(); len(rest) > 1 {
		if err := parseMemoryFlags(fs, rest[1:]); err != nil {
			return memoryFlagExit(err)
		}
		if len(fs.Args()) > 0 {
			fmt.Fprintf(stderr, "memory vault import: unexpected extra argument %q\n", fs.Arg(0))
			return 2
		}
	}
	if *dryRun && *yes {
		fmt.Fprintln(stderr, "memory vault import: pass at most one of --dry-run and --yes")
		return 2
	}
	if dir == "" {
		fmt.Fprintln(stderr, "memory vault import: a vault directory argument is required")
		printMemoryVaultUsage(stderr)
		return 2
	}
	dir = filepath.Clean(dir)
	apply := *yes // dry-run is the default whenever --yes is absent

	var result vaultImportResult
	err := withStore(*home, func(store *db.Store) error {
		res, err := planAndApplyVaultImport(context.Background(), store, dir, apply)
		if err != nil {
			return err
		}
		result = res
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory vault import: %v\n", err)
		return 1
	}

	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory vault import: %v\n", err)
			return 1
		}
		return 0
	}
	printVaultImportDiff(stdout, result)
	return 0
}

// diskNote is one parsed .md file found in the vault directory.
type diskNote struct {
	file   string
	raw    []byte
	parsed memory.VaultNoteFile
}

// planAndApplyVaultImport is the whole import decision, isolated for testing. It
// never writes unless apply is true.
func planAndApplyVaultImport(ctx context.Context, store *db.Store, dir string, apply bool) (vaultImportResult, error) {
	manifest, err := readVaultManifest(dir)
	if err != nil {
		return vaultImportResult{}, err
	}
	if manifest.SchemaVersion != vaultSchemaVersion {
		return vaultImportResult{}, fmt.Errorf("vault schema v%d is not supported by this build (expected v%d); re-run `gitmoot memory vault export`", manifest.SchemaVersion, vaultSchemaVersion)
	}

	// Regenerate a fresh export from the current store and abort if the store moved
	// since the vault was written — a stale diff could silently clobber newer facts.
	// Rebuild with the SAME scope the export used (manifest.Agent, empty == all owners)
	// so a filtered `export --agent NAME` vault compares like-for-like instead of always
	// failing stale against an all-owners rebuild when other owners have memories.
	freshNotes, _, freshHash, _, err := buildVault(ctx, store, manifest.Agent)
	if err != nil {
		return vaultImportResult{}, err
	}
	if freshHash != manifest.SnapshotHash {
		return vaultImportResult{}, fmt.Errorf("vault is stale: the memory store changed since it was exported (manifest snapshot %s, current %s); re-run `gitmoot memory vault export` and re-apply your edits", manifest.SnapshotHash, freshHash)
	}

	disk, warnings, parseFailed, err := readVaultDiskNotes(dir)
	if err != nil {
		return vaultImportResult{}, err
	}
	// A note that fails to parse is DROPPED from `disk`, so its memory_id can no longer
	// be matched to a fresh-export row — the classify loop below would then read the
	// missing counterpart as a deletion and RETIRE a fact whose file is still sitting on
	// disk (an owner's YAML typo silently destroying a memory). Refuse to apply while any
	// note failed to parse; dry-run still previews (writing nothing) so the operator can
	// see the warning and fix the frontmatter before re-running.
	if apply && len(parseFailed) > 0 {
		return vaultImportResult{}, fmt.Errorf("%d vault note(s) failed to parse (%s); refusing to apply because a malformed note would be misread as a deletion — fix the frontmatter (or delete the file to intentionally retire it), then re-export and re-apply", len(parseFailed), strings.Join(parseFailed, ", "))
	}

	result := vaultImportResult{Dir: dir, DryRun: !apply, SnapshotHash: freshHash}
	var plan db.VaultImportPlan

	// Index the disk notes that carry a memory_id so each fresh note can find its
	// on-disk counterpart by id (robust to the human renaming the file).
	diskByID := map[int64]diskNote{}
	var newFiles []diskNote
	for _, dn := range disk {
		id, ok := dn.parsed.MemoryID()
		if !ok {
			newFiles = append(newFiles, dn)
			continue
		}
		if _, dup := diskByID[id]; dup {
			warnings = append(warnings, fmt.Sprintf("%s: duplicate memory_id %d (already claimed by another note); skipping", dn.file, id))
			continue
		}
		diskByID[id] = dn
	}

	// Classify every fresh (current-store) note: edited, unchanged, or deleted.
	for _, note := range freshNotes {
		id := note.memRecord.ID
		dn, ok := diskByID[id]
		if !ok {
			plan.Retirements = append(plan.Retirements, db.VaultImportRetire{ID: id, ExpectedUpdatedAt: note.memRecord.UpdatedAt, Reason: "vault-import: deleted by owner"})
			result.Retirements = append(result.Retirements, vaultImportItem{MemoryID: id, Key: note.memRecord.Key, Detail: "note deleted from vault"})
			continue
		}
		delete(diskByID, id)
		if bytes.Equal(dn.raw, note.bytes) {
			result.Unchanged++
			continue
		}
		if drift := vaultFrontmatterDrift(dn.parsed, note.memRecord); drift != "" {
			warnings = append(warnings, fmt.Sprintf("%s (memory %d): %s — out of scope for import; applying the content edit only", dn.file, id, drift))
		}
		body := dn.parsed.Body
		if strings.TrimSpace(body) == "" {
			warnings = append(warnings, fmt.Sprintf("%s (memory %d): edited note has empty content; skipping (delete the file to retire the memory)", dn.file, id))
			continue
		}
		plan.Updates = append(plan.Updates, db.VaultImportUpdate{
			ID:                id,
			ExpectedUpdatedAt: note.memRecord.UpdatedAt,
			Content:           body,
			Provenance:        "vault-import",
		})
		result.Updates = append(result.Updates, vaultImportItem{MemoryID: id, Key: note.memRecord.Key, File: dn.file, Detail: "content edited"})
	}

	// Any disk memory_id not present in the fresh export refers to a row that is no
	// longer injectable (already retired/superseded); leave it alone but flag it.
	leftover := make([]int64, 0, len(diskByID))
	for id := range diskByID {
		leftover = append(leftover, id)
	}
	sort.Slice(leftover, func(i, j int) bool { return leftover[i] < leftover[j] })
	for _, id := range leftover {
		warnings = append(warnings, fmt.Sprintf("%s: memory_id %d is not in the current export (already retired or superseded); skipping", diskByID[id].file, id))
	}

	// New, owner-authored .md files (no memory_id) stage as pending observations
	// behind the existing confirmation gate. They are owner-authored, so trust_mark
	// is normal (P3's arbitrary-source ingest is the low-trust path).
	for _, dn := range newFiles {
		obs, ok, why := vaultObservationFromNewFile(dn)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: %s; skipping", dn.file, why))
			continue
		}
		plan.Observations = append(plan.Observations, obs)
		result.NewObservations = append(result.NewObservations, vaultImportItem{Key: obs.Key, File: dn.file, Detail: "new pending observation"})
	}

	result.Warnings = warnings

	if apply && (len(plan.Updates) > 0 || len(plan.Retirements) > 0 || len(plan.Observations) > 0) {
		if err := store.ApplyVaultImport(ctx, plan); err != nil {
			return vaultImportResult{}, err
		}
		result.Applied = true
	}
	return result, nil
}

// readVaultManifest reads and validates <dir>/manifest.json.
func readVaultManifest(dir string) (vaultManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vaultManifest{}, fmt.Errorf("%s is not a gitmoot vault: no manifest.json (run `gitmoot memory vault export` first)", dir)
		}
		return vaultManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m vaultManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return vaultManifest{}, fmt.Errorf("%s has an invalid manifest.json: %w", dir, err)
	}
	if m.SchemaVersion <= 0 {
		return vaultManifest{}, fmt.Errorf("%s has an invalid manifest.json (no schema_version)", dir)
	}
	return m, nil
}

// readVaultDiskNotes reads every top-level .md memory note from the vault dir,
// skipping derived/index/manifest/.obsidian artifacts. Parse failures are reported
// as warnings AND returned in parseFailed (the filenames) so the caller can refuse
// to apply — a dropped note whose memory_id can no longer be matched would be
// misread as a deletion and retire a live fact.
func readVaultDiskNotes(dir string) (notes []diskNote, warnings []string, parseFailed []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read vault dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue // flat vault; .obsidian and any nested dirs are ignored
		}
		if name == "manifest.json" || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, "_index-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read note %s: %w", name, err)
		}
		parsed, err := memory.ParseVaultNote(string(raw))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: not a valid vault note (%v); skipping", name, err))
			parseFailed = append(parseFailed, name)
			continue
		}
		notes = append(notes, diskNote{file: name, raw: raw, parsed: parsed})
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].file < notes[j].file })
	return notes, warnings, parseFailed, nil
}

// vaultFrontmatterDrift reports whether the owner edited an identity/classification
// field (out of scope for import — only content edits apply). Empty string == none.
func vaultFrontmatterDrift(f memory.VaultNoteFile, want memory.VaultMemory) string {
	var diffs []string
	check := func(label, got, expected string) {
		if got != expected {
			diffs = append(diffs, fmt.Sprintf("%s %q→%q", label, expected, got))
		}
	}
	check("key", f.String("key"), want.Key)
	check("scope", f.String("scope"), want.Scope)
	check("owner_kind", f.String("owner_kind"), want.OwnerKind)
	check("owner_ref", f.String("owner_ref"), want.OwnerRef)
	check("owner_version", f.String("owner_version"), want.OwnerVersion)
	check("repo", f.String("repo"), want.Repo)
	if len(diffs) == 0 {
		return ""
	}
	return "frontmatter changed (" + strings.Join(diffs, ", ") + ")"
}

// vaultObservationFromNewFile builds a pending observation from an owner-authored
// note that carries no memory_id. It needs an owner ref and non-empty content; the
// key falls back to the filename stem. Returns ok=false with a reason when the file
// lacks the minimum an observation requires.
func vaultObservationFromNewFile(dn diskNote) (db.MemoryObservation, bool, string) {
	f := dn.parsed
	ownerRef := f.String("owner_ref")
	if ownerRef == "" {
		ownerRef = f.String("agent")
	}
	if strings.TrimSpace(ownerRef) == "" {
		return db.MemoryObservation{}, false, "new note has no owner_ref/agent in its frontmatter"
	}
	ownerKind := f.String("owner_kind")
	if strings.TrimSpace(ownerKind) == "" {
		ownerKind = memory.OwnerKindAgent
	}
	content := f.Body
	if strings.TrimSpace(content) == "" {
		return db.MemoryObservation{}, false, "new note has empty content"
	}
	key := f.String("key")
	if strings.TrimSpace(key) == "" {
		key = strings.TrimSuffix(dn.file, ".md")
	}
	return db.MemoryObservation{
		Owner:      db.MemoryOwner{Kind: ownerKind, Ref: ownerRef, Version: f.String("owner_version")},
		Repo:       f.String("repo"),
		Scope:      f.String("scope"),
		Key:        key,
		Content:    content,
		Provenance: "vault-import:" + dn.file,
		TrustMark:  memory.TrustNormal,
	}, true, ""
}

// printVaultImportDiff renders the human-readable diff/summary.
func printVaultImportDiff(w io.Writer, r vaultImportResult) {
	fmt.Fprintf(w, "vault import diff for %s\n", r.Dir)
	fmt.Fprintf(w, "  %d edit(s), %d retirement(s), %d new observation(s), %d unchanged\n",
		len(r.Updates), len(r.Retirements), len(r.NewObservations), r.Unchanged)
	section := func(title string, items []vaultImportItem) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s:\n", title)
		for _, it := range items {
			switch {
			case it.MemoryID != 0:
				fmt.Fprintf(w, "  - memory %d [%s] %s\n", it.MemoryID, it.Key, it.Detail)
			default:
				fmt.Fprintf(w, "  - %s [%s] %s\n", it.File, it.Key, it.Detail)
			}
		}
	}
	section("Edits (content updated)", r.Updates)
	section("Retirements (note deleted)", r.Retirements)
	section("New notes (staged as pending observations)", r.NewObservations)
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, wn := range r.Warnings {
			fmt.Fprintf(w, "  ! %s\n", wn)
		}
	}
	fmt.Fprintln(w)
	if r.Applied {
		fmt.Fprintln(w, "applied: all changes committed in one transaction.")
		return
	}
	if len(r.Updates) == 0 && len(r.Retirements) == 0 && len(r.NewObservations) == 0 {
		fmt.Fprintln(w, "no changes to apply.")
		return
	}
	fmt.Fprintln(w, "(dry run — nothing was written; re-run with --yes to apply)")
}
