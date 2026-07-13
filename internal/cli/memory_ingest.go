package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
)

// This file implements the #737 P3 slice: `gitmoot memory ingest` (markdown →
// pending observations, trust_mark=low, PreFilter-gated) plus the minimal
// human-gated promotion slice — `memory observations` and `memory confirm` —
// because Phase-2 promotion did not exist at all, so without it ingested notes
// would sit inert. Everything here is CLI-EXPLICIT: no daemon path, and nothing
// reads trust_mark for decisions yet. Ingested markdown is UNTRUSTED (an
// indirect-prompt-injection vector); by default the human confirm gate is the
// trust boundary. When [memory].ingest_auto_confirm is enabled, confirmation is
// still private-pool only and never writes shared memory.

// ---- memory ingest --------------------------------------------------------

// memoryIngestResult is the summary of an ingest run (and the --json shape).
type memoryIngestResult struct {
	Agent          string         `json:"agent"`
	Shared         bool           `json:"shared,omitempty"`
	Scope          string         `json:"scope"`
	Repo           string         `json:"repo,omitempty"`
	DryRun         bool           `json:"dry_run"`
	AutoConfirm    bool           `json:"auto_confirm,omitempty"`
	Files          int            `json:"files"`
	Chunks         int            `json:"chunks"`
	Inserted       int            `json:"inserted"`
	Confirmed      int            `json:"confirmed,omitempty"`
	SkippedRetired int            `json:"skipped_retired,omitempty"`
	Deduped        int            `json:"deduped"`
	RejectedN      int            `json:"rejected"`
	RejectedBy     map[string]int `json:"rejected_by,omitempty"`
	InsertedKeys   []string       `json:"inserted_keys,omitempty"`
}

type memoryIngestOptions struct {
	Home        string
	Path        string
	Agent       string
	Shared      bool
	Repo        string
	Tier        string
	DryRun      bool
	JSON        bool
	AutoConfirm bool
}

func runMemoryIngest(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "sweep" {
		return runMemoryIngestSweep(args[1:], stdout, stderr)
	}
	options, code := parseMemoryIngestOptions(args, stderr)
	if code != 0 {
		return code
	}

	var result memoryIngestResult
	err := withStoreAndPaths(options.Home, func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		options.AutoConfirm = settings.IngestAutoConfirm
		var ingestErr error
		result, ingestErr = ingestMemorySource(context.Background(), store, options)
		return ingestErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory ingest: %v\n", err)
		return 1
	}
	if len(result.RejectedBy) == 0 {
		result.RejectedBy = nil
	}

	if options.JSON {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory ingest: %v\n", err)
			return 1
		}
		return 0
	}
	verb := "ingested"
	if options.DryRun {
		verb = "would ingest"
	}
	fmt.Fprintf(stdout, "%s %d observation(s) from %d file(s), %d chunk(s): inserted=%d confirmed=%d deduped=%d rejected=%d skipped_retired=%d\n",
		verb, result.Inserted, result.Files, result.Chunks, result.Inserted, result.Confirmed, result.Deduped, result.RejectedN, result.SkippedRetired)
	for _, reason := range sortedReasonKeys(result.RejectedBy) {
		fmt.Fprintf(stdout, "  rejected[%s]=%d\n", reason, result.RejectedBy[reason])
	}
	if result.AutoConfirm {
		fmt.Fprintln(stdout, "(auto-confirm enabled: observations were confirmed into the authoring agent's private pool only)")
	} else {
		fmt.Fprintln(stdout, "(ingested notes are trust_mark=low pending observations; run `gitmoot memory confirm` to promote)")
	}
	return 0
}

func parseMemoryIngestOptions(args []string, stderr io.Writer) (memoryIngestOptions, int) {
	fs := flag.NewFlagSet("memory ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "owner agent name for the ingested observations (required)")
	shared := fs.Bool("shared", false, "stage observations in the shared pool with --agent as author")
	repo := fs.String("repo", "", "repo (owner/repo) for repo-scoped observations")
	tier := fs.String("tier", "repo", "scope tier: repo|general (general only with this explicit flag)")
	dryRun := fs.Bool("dry-run", false, "report what would be ingested without writing")
	jsonOut := fs.Bool("json", false, "print the ingest summary as JSON")
	// The documented form is `memory ingest <path|dir> [flags]`, but Go's flag
	// parser stops at the first positional, so peel a leading non-flag path off
	// the front before parsing. Flags-then-path (path as the trailing arg) is
	// still accepted via fs.Args() below.
	var pathArg string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pathArg = args[0]
		args = args[1:]
	}
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryIngestOptions{}, memoryFlagExit(err)
	}
	if pathArg == "" {
		switch fs.NArg() {
		case 1:
			pathArg = fs.Arg(0)
		case 0:
			fmt.Fprintln(stderr, "memory ingest: a <path|dir> argument is required")
			return memoryIngestOptions{}, 2
		default:
			fmt.Fprintln(stderr, "memory ingest: exactly one <path|dir> argument is required")
			return memoryIngestOptions{}, 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory ingest: exactly one <path|dir> argument is required")
		return memoryIngestOptions{}, 2
	}
	if strings.TrimSpace(*agent) == "" {
		fmt.Fprintln(stderr, "memory ingest: --agent is required")
		return memoryIngestOptions{}, 2
	}
	scope := strings.TrimSpace(*tier)
	switch scope {
	case memory.ScopeRepo:
		// repo scope keeps whatever --repo the caller passed (may be empty).
	case memory.ScopeGeneral:
		if strings.TrimSpace(*repo) != "" {
			fmt.Fprintln(stderr, "memory ingest: --tier general cannot be combined with --repo")
			return memoryIngestOptions{}, 2
		}
	default:
		fmt.Fprintf(stderr, "memory ingest: invalid --tier %q (want repo|general)\n", *tier)
		return memoryIngestOptions{}, 2
	}
	return memoryIngestOptions{
		Home:   *home,
		Path:   pathArg,
		Agent:  *agent,
		Shared: *shared,
		Repo:   *repo,
		Tier:   scope,
		DryRun: *dryRun,
		JSON:   *jsonOut,
	}, 0
}

func ingestMemorySource(ctx context.Context, store *db.Store, options memoryIngestOptions) (memoryIngestResult, error) {
	files, root, err := collectMarkdownFiles(options.Path)
	if err != nil {
		return memoryIngestResult{}, err
	}
	if len(files) == 0 {
		return memoryIngestResult{}, fmt.Errorf("no .md files under %s", options.Path)
	}

	result := memoryIngestResult{
		Agent:       options.Agent,
		Shared:      options.Shared,
		Scope:       options.Tier,
		Repo:        strings.TrimSpace(options.Repo),
		DryRun:      options.DryRun,
		AutoConfirm: options.AutoConfirm,
		Files:       len(files),
		RejectedBy:  map[string]int{},
	}
	owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: options.Agent}
	authorRef := ""
	if options.Shared {
		owner = db.MemoryOwner{Kind: memory.OwnerKindShared, Ref: memory.SharedOwnerRef}
		authorRef = strings.TrimSpace(options.Agent)
	}

	dedupOwner := owner.Ref
	if options.AutoConfirm {
		dedupOwner = strings.TrimSpace(options.Agent)
	}
	seen, err := store.ObservationDedupKeys(ctx, dedupOwner)
	if err != nil {
		return result, err
	}
	// One allocator per run: stable slug(file)-slug(heading) keys, with ordinal
	// suffixes only when a (file, heading) repeats (split pieces, duplicate
	// headings). Keys are allocated for every chunk — before the pre-filter and
	// dedup drops — so ordinals track document order across sweeps.
	keys := memory.NewIngestKeyAllocator()
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return result, fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		rel = filepath.ToSlash(rel)
		fileStem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		_, body := memory.StripFrontmatter(string(raw))
		for _, chunk := range memory.ChunkMarkdown(body, memory.IngestMaxChunkTokens) {
			result.Chunks++
			key := keys.Next(fileStem, chunk.Heading)
			if ok, reason := memory.PreFilter(chunk.Text, result.Scope); !ok {
				result.RejectedN++
				result.RejectedBy[reason]++
				continue
			}
			// Dedup within the target visibility domain only: identical text
			// under a different repo must still stage (repo-scoped memory
			// injects only for its own repo), so key by (scope, repo, hash).
			dkey := db.MemoryDedupKey(result.Scope, result.Repo, memory.ContentHash(chunk.Text))
			if _, dup := seen[dkey]; dup {
				result.Deduped++
				continue
			}
			seen[dkey] = struct{}{}
			result.Inserted++
			result.InsertedKeys = append(result.InsertedKeys, key)
			if options.DryRun {
				continue
			}
			obs := db.MemoryObservation{
				Owner:      owner,
				AuthorRef:  authorRef,
				Repo:       result.Repo,
				Scope:      result.Scope,
				Key:        key,
				Content:    chunk.Text,
				Provenance: "ingest:" + rel,
				TrustMark:  memory.TrustLow,
			}
			if _, err := store.InsertMemoryObservation(ctx, obs); err != nil {
				return result, fmt.Errorf("insert observation for %s: %w", rel, err)
			}
			confirmed, skipped, err := autoConfirmObservationIfEnabled(ctx, store, obs, options.AutoConfirm)
			if err != nil {
				return result, fmt.Errorf("auto-confirm observation for %s: %w", rel, err)
			}
			if confirmed {
				result.Confirmed++
			}
			if skipped {
				result.SkippedRetired++
			}
		}
	}
	return result, nil
}

func autoConfirmObservationIfEnabled(ctx context.Context, store *db.Store, obs db.MemoryObservation, enabled bool) (confirmed bool, skippedRetired bool, err error) {
	if !enabled || !autoConfirmEligibleProvenance(obs.Provenance) {
		return false, false, nil
	}
	author := observationAuthorRef(obs)
	if strings.TrimSpace(author) == "" {
		return false, false, fmt.Errorf("authoring agent is required")
	}
	owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: author}
	// Auto-confirm is an AUTOMATED writer, so a key-matched in-place update
	// archives the prior edition as a superseded row first (#804): a bad or
	// poisoned note edit can never silently destroy the last edition. Manual
	// confirm paths keep plain overwrite semantics.
	id, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner:      owner,
		Repo:       obs.Repo,
		Scope:      obs.Scope,
		Key:        obs.Key,
		Content:    obs.Content,
		Provenance: obs.Provenance,
		SourceJob:  obs.SourceJob,
	}, db.PreserveSupersededEdition())
	if err != nil {
		if errors.Is(err, db.ErrConfirmedMemoryRetired) {
			return false, true, nil
		}
		return false, false, err
	}
	attachConfirmedFactToCluster(ctx, store, db.ConfirmedMemory{
		ID: id, Owner: owner, Repo: obs.Repo, Scope: obs.Scope, Key: obs.Key, Content: obs.Content,
	})
	return true, false, nil
}

// autoConfirmEligibleProvenance is deliberately fail-closed. Only the three
// existing human/operator-authored ingestion families may cross the optional
// auto-confirm boundary; harvest, distill, and every future producer remain
// pending until this allowlist is explicitly extended.
func autoConfirmEligibleProvenance(provenance string) bool {
	for _, prefix := range []string{"ingest:", "chat:", "workflow:"} {
		if strings.HasPrefix(strings.TrimSpace(provenance), prefix) {
			return true
		}
	}
	return false
}

type memoryIngestSweepSourceResult struct {
	Path           string `json:"path"`
	Agent          string `json:"agent"`
	Repo           string `json:"repo"`
	Tier           string `json:"tier"`
	Files          int    `json:"files"`
	Chunks         int    `json:"chunks"`
	Inserted       int    `json:"inserted"`
	Confirmed      int    `json:"confirmed,omitempty"`
	SkippedRetired int    `json:"skipped_retired,omitempty"`
	Deduped        int    `json:"deduped"`
	Rejected       int    `json:"rejected"`
	Error          string `json:"error"`
}

type memoryIngestSweepTotals struct {
	Sources        int `json:"sources"`
	Succeeded      int `json:"succeeded"`
	Failed         int `json:"failed"`
	Files          int `json:"files"`
	Chunks         int `json:"chunks"`
	Inserted       int `json:"inserted"`
	Confirmed      int `json:"confirmed"`
	SkippedRetired int `json:"skipped_retired"`
	Deduped        int `json:"deduped"`
	Rejected       int `json:"rejected"`
}

type memoryIngestSweepResult struct {
	Totals  memoryIngestSweepTotals         `json:"totals"`
	Sources []memoryIngestSweepSourceResult `json:"sources"`
	Skipped string                          `json:"skipped,omitempty"`
}

func runMemoryIngestSweep(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory ingest sweep", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print the sweep summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory ingest sweep: accepts no positional arguments")
		return 2
	}

	var result memoryIngestSweepResult
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemoryPipelineSettings(paths)
		if err != nil {
			return err
		}
		memorySettings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		result = runConfiguredMemoryIngestSweep(context.Background(), store, settings.IngestSources, memorySettings.IngestAutoConfirm)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory ingest sweep: %v\n", err)
		return 1
	}

	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory ingest sweep: %v\n", err)
			return 1
		}
	} else {
		printMemoryIngestSweepResult(stdout, result)
	}
	if result.Totals.Sources > 0 && result.Totals.Failed == result.Totals.Sources {
		return 1
	}
	return 0
}

func runConfiguredMemoryIngestSweep(ctx context.Context, store *db.Store, sources []config.MemoryIngestSource, autoConfirm bool) memoryIngestSweepResult {
	result := memoryIngestSweepResult{Sources: []memoryIngestSweepSourceResult{}}
	if len(sources) == 0 {
		result.Skipped = "no memory.ingest sources configured"
		return result
	}
	for _, source := range sources {
		ingest, err := ingestMemorySource(ctx, store, memoryIngestOptions{
			Path:        source.Path,
			Agent:       source.Agent,
			Repo:        source.Repo,
			Tier:        source.Tier,
			AutoConfirm: autoConfirm,
		})
		entry := memoryIngestSweepSourceResult{
			Path:           source.Path,
			Agent:          source.Agent,
			Repo:           source.Repo,
			Tier:           source.Tier,
			Files:          ingest.Files,
			Chunks:         ingest.Chunks,
			Inserted:       ingest.Inserted,
			Confirmed:      ingest.Confirmed,
			SkippedRetired: ingest.SkippedRetired,
			Deduped:        ingest.Deduped,
			Rejected:       ingest.RejectedN,
		}
		if err != nil {
			entry.Error = err.Error()
			result.Totals.Failed++
		} else {
			result.Totals.Succeeded++
		}
		result.Totals.Sources++
		result.Totals.Files += entry.Files
		result.Totals.Chunks += entry.Chunks
		result.Totals.Inserted += entry.Inserted
		result.Totals.Confirmed += entry.Confirmed
		result.Totals.SkippedRetired += entry.SkippedRetired
		result.Totals.Deduped += entry.Deduped
		result.Totals.Rejected += entry.Rejected
		result.Sources = append(result.Sources, entry)
	}
	return result
}

func printMemoryIngestSweepResult(stdout io.Writer, result memoryIngestSweepResult) {
	if result.Skipped != "" {
		fmt.Fprintln(stdout, "memory ingest sweep skipped: no sources configured")
		return
	}
	fmt.Fprintf(stdout, "memory ingest sweep: sources=%d succeeded=%d failed=%d inserted=%d confirmed=%d deduped=%d rejected=%d skipped_retired=%d files=%d chunks=%d\n",
		result.Totals.Sources, result.Totals.Succeeded, result.Totals.Failed, result.Totals.Inserted,
		result.Totals.Confirmed, result.Totals.Deduped, result.Totals.Rejected, result.Totals.SkippedRetired, result.Totals.Files, result.Totals.Chunks)
	for _, source := range result.Sources {
		repo := source.Repo
		if repo == "" {
			repo = "-"
		}
		fmt.Fprintf(stdout, "  %s agent=%s repo=%s tier=%s inserted=%d confirmed=%d deduped=%d rejected=%d skipped_retired=%d",
			source.Path, source.Agent, repo, source.Tier, source.Inserted, source.Confirmed, source.Deduped, source.Rejected, source.SkippedRetired)
		if source.Error != "" {
			fmt.Fprintf(stdout, " error=%q", source.Error)
		}
		fmt.Fprintln(stdout)
	}
}

// collectMarkdownFiles returns the sorted set of *.md files at target (a single
// file or a directory walked recursively) and the root the returned files are
// made relative to for provenance. For a single file the root is its parent
// directory so provenance is just the file's basename.
func collectMarkdownFiles(target string) (files []string, root string, err error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		if !isMarkdown(target) {
			return nil, "", fmt.Errorf("%s is not a .md file", target)
		}
		return []string{target}, filepath.Dir(target), nil
	}
	err = filepath.WalkDir(target, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if isMarkdown(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(files)
	return files, target, nil
}

func isMarkdown(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".md")
}

func sortedReasonKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- memory observations --------------------------------------------------

type memoryObservationEntry struct {
	ID         int64  `json:"id"`
	OwnerRef   string `json:"owner_ref"`
	AuthorRef  string `json:"author_ref,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Scope      string `json:"scope"`
	Key        string `json:"key"`
	Content    string `json:"content"`
	Provenance string `json:"provenance,omitempty"`
	TrustMark  string `json:"trust_mark,omitempty"`
	Confirmed  bool   `json:"confirmed"`
	CreatedAt  string `json:"created_at,omitempty"`
}

func runMemoryObservations(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory observations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "filter by owner agent name")
	prefix := fs.String("provenance-prefix", "", "filter to observations whose provenance starts with this prefix")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}

	var entries []memoryObservationEntry
	err := withReadOnlyStore(*home, func(store *db.Store) error {
		rows, err := store.ListMemoryObservationsWithConfirmation(context.Background(), *agent, *prefix)
		if err != nil {
			return err
		}
		for _, r := range rows {
			entries = append(entries, memoryObservationEntry{
				ID: r.ID, OwnerRef: r.Owner.Ref, Repo: r.Repo, Scope: r.Scope, Key: r.Key,
				AuthorRef: r.AuthorRef,
				Content:   r.Content, Provenance: r.Provenance, TrustMark: r.TrustMark,
				Confirmed: r.Confirmed, CreatedAt: r.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory observations: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "memory observations: %v\n", err)
			return 1
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no observations")
		return 0
	}
	for _, e := range entries {
		repo := e.Repo
		if repo == "" {
			repo = "-"
		}
		mark := "pending"
		if e.Confirmed {
			mark = "confirmed"
		}
		fmt.Fprintf(stdout, "%-6d %-9s %-10s %-6s %-28s %s\n", e.ID, mark, repo, e.Scope, e.Key, e.Provenance)
	}
	return 0
}

// ---- memory confirm -------------------------------------------------------

type memoryConfirmResult struct {
	DryRun    bool     `json:"dry_run"`
	ToShared  bool     `json:"to_shared,omitempty"`
	Selected  int      `json:"selected"`
	Confirmed int      `json:"confirmed"`
	Keys      []string `json:"keys,omitempty"`
}

func runMemoryConfirm(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory confirm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "owner agent name (scopes --provenance-prefix selection)")
	prefix := fs.String("provenance-prefix", "", "confirm every pending observation whose provenance starts with this prefix")
	toShared := fs.Bool("to-shared", false, "confirm selected observations into the shared pool")
	yes := fs.Bool("yes", false, "actually promote (without it, prints the plan and makes no writes)")
	jsonOut := fs.Bool("json", false, "print the confirm summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	idArgs := fs.Args()
	if len(idArgs) == 0 && strings.TrimSpace(*prefix) == "" {
		fmt.Fprintln(stderr, "memory confirm: give one or more observation ids, or --provenance-prefix")
		return 2
	}

	ids := make([]int64, 0, len(idArgs))
	for _, a := range idArgs {
		id, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "memory confirm: invalid observation id %q\n", a)
			return 2
		}
		ids = append(ids, id)
	}

	result := memoryConfirmResult{DryRun: !*yes, ToShared: *toShared}
	err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		selected, err := selectObservationsToConfirm(ctx, store, ids, *agent, *prefix)
		if err != nil {
			return err
		}
		result.Selected = len(selected)
		for _, obs := range selected {
			result.Keys = append(result.Keys, obs.Key)
			if !*yes {
				continue
			}
			owner := obs.Owner
			authorRef := observationAuthorRef(obs)
			if !*toShared && owner.Kind == memory.OwnerKindAgent && owner.Ref == authorRef {
				authorRef = ""
			}
			if *toShared {
				owner = db.MemoryOwner{Kind: memory.OwnerKindShared, Ref: memory.SharedOwnerRef}
			}
			// Manual `memory confirm --yes` is an explicit human intervention, so it
			// preserves the pre-existing behavior that can revive a retired keyed row.
			// Automated collectors and ingest auto-confirm do not pass this option.
			newID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
				Owner:      owner,
				AuthorRef:  authorRef,
				Repo:       obs.Repo,
				Scope:      obs.Scope,
				Key:        obs.Key,
				Content:    obs.Content,
				Provenance: obs.Provenance,
				SourceJob:  obs.SourceJob,
			}, db.AllowResurrectConfirmedMemory())
			if err != nil {
				return fmt.Errorf("confirm observation %d: %w", obs.ID, err)
			}
			result.Confirmed++
			// Incremental cluster attach (#763 Track A): a newly confirmed fact joins
			// the cluster of its nearest similarity neighbor. Best-effort and
			// fail-safe — a clustering error must never block the confirmation.
			attachConfirmedFactToCluster(ctx, store, db.ConfirmedMemory{
				ID: newID, Owner: owner, AuthorRef: authorRef, Repo: obs.Repo, Scope: obs.Scope,
				Key: obs.Key, Content: obs.Content,
			})
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory confirm: %v\n", err)
		return 1
	}

	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory confirm: %v\n", err)
			return 1
		}
		return 0
	}
	if result.Selected == 0 {
		fmt.Fprintln(stdout, "no matching observations to confirm")
		return 0
	}
	if !*yes {
		fmt.Fprintf(stdout, "%d observation(s) selected for confirmation (dry run):\n", result.Selected)
		for _, k := range result.Keys {
			fmt.Fprintf(stdout, "  %s\n", k)
		}
		fmt.Fprintln(stdout, "re-run with --yes to promote them to confirmed memory")
		return 0
	}
	fmt.Fprintf(stdout, "confirmed %d observation(s) into confirmed memory\n", result.Confirmed)
	return 0
}

func observationAuthorRef(obs db.MemoryObservation) string {
	if author := strings.TrimSpace(obs.AuthorRef); author != "" {
		return author
	}
	return strings.TrimSpace(obs.Owner.Ref)
}

// selectObservationsToConfirm resolves the explicit id list and/or the
// provenance-prefix selection into a deduplicated, id-ordered set of pending
// observations. An unknown id is a hard error so a typo never silently confirms
// nothing.
func selectObservationsToConfirm(ctx context.Context, store *db.Store, ids []int64, agent, prefix string) ([]db.MemoryObservation, error) {
	byID := map[int64]db.MemoryObservation{}
	for _, id := range ids {
		obs, ok, err := store.GetMemoryObservationByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("no observation with id %d", id)
		}
		byID[obs.ID] = obs
	}
	if strings.TrimSpace(prefix) != "" {
		rows, err := store.ListMemoryObservationsWithConfirmation(ctx, agent, prefix)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			byID[r.ID] = r.MemoryObservation
		}
	}
	out := make([]db.MemoryObservation, 0, len(byID))
	for _, obs := range byID {
		out = append(out, obs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
