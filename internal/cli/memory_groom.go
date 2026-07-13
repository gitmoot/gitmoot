package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// memory groom (#737 P4.2, #832) — automatic lossless brick splitting plus the
// human-gated propose→apply round-trip over confirmed memory.
//
// `groom --propose` reads every ACTIVE confirmed memory (the vault lister path,
// retired rows excluded), computes the CURRENT vault snapshot_hash exactly as
// `vault export` does, runs the deterministic detectors (internal/memory), and
// writes a reviewable plan artifact — it touches nothing in the store.
//
// `groom --yes --plan <file>` recomputes the snapshot_hash, ABORTS AS STALE if it
// differs from the plan's (a vault edit between propose and apply invalidates the
// plan), and applies the owner-gated plan in ONE transaction. `groom --split`
// separately auto-applies only the deterministic exact-substring transform.

// groomSchemaVersion is the on-disk plan schema version. Bump on a breaking change
// to the plan shape so a stale `--plan` file is rejected rather than misread.
// v2 (#804) added the rekey and cross-pool action sections; an older binary would
// silently drop them, so the version check rejects cross-version plans outright.
const groomSchemaVersion = 2

// groomPlanRetirement is one proposed retirement in the plan artifact.
type groomPlanRetirement struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Reason    string `json:"reason"`
	FirstLine string `json:"first_line"`
	Owner     string `json:"owner,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

// groomPlanRewriteFlag flags an over-long memory for a later rewrite pass (P4.2
// only lists it; it never rewrites).
type groomPlanRewriteFlag struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Chars int    `json:"chars"`
}

// groomPlanRekeyRetire is one older sibling edition retired by a rekey group.
type groomPlanRekeyRetire struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	FirstLine string `json:"first_line"`
}

// groomPlanRekey is one proposed legacy-key migration (#804): keep the current
// edition under the stable key, retire the older hash-suffixed siblings.
type groomPlanRekey struct {
	KeepID    int64                  `json:"keep_id"`
	KeepKey   string                 `json:"keep_key"`
	NewKey    string                 `json:"new_key"`
	Retire    []groomPlanRekeyRetire `json:"retire"`
	Owner     string                 `json:"owner,omitempty"`
	Repo      string                 `json:"repo,omitempty"`
	Scope     string                 `json:"scope,omitempty"`
	FirstLine string                 `json:"first_line,omitempty"`
}

// groomPlanCrossPool is one proposed promote-and-retire pair (#804): promote the
// newer private edition to the shared pool, retire the stale shared edition.
type groomPlanCrossPool struct {
	PrivateID  int64  `json:"private_id"`
	PrivateKey string `json:"private_key"`
	Owner      string `json:"owner,omitempty"`
	SharedID   int64  `json:"shared_id"`
	SharedKey  string `json:"shared_key"`
	Basis      string `json:"basis"`
	Repo       string `json:"repo,omitempty"`
	Scope      string `json:"scope,omitempty"`
	FirstLine  string `json:"first_line,omitempty"`
}

type groomStaleCandidateSummary struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	ContentHash string `json:"content_hash"`
	NewestDate  string `json:"newest_date"`
}

type groomStaleEntry struct {
	Candidate      groomStaleCandidateSummary `json:"candidate"`
	Verdict        string                     `json:"verdict"`
	Action         string                     `json:"action"`
	Residue        string                     `json:"residue,omitempty"`
	Model          string                     `json:"model,omitempty"`
	Cached         bool                       `json:"cached"`
	FallbackReason string                     `json:"fallback_reason"`
}

type groomQualityCandidateSummary struct {
	ID             int64    `json:"id"`
	Key            string   `json:"key"`
	ContentHash    string   `json:"content_hash"`
	Score          int      `json:"score"`
	SignalFamilies []string `json:"signal_families"`
}

type groomQualityEntry struct {
	Candidate      groomQualityCandidateSummary `json:"candidate"`
	Verdict        string                       `json:"verdict"`
	Confidence     float64                      `json:"confidence"`
	Action         string                       `json:"action"`
	Residue        string                       `json:"residue,omitempty"`
	Model          string                       `json:"model,omitempty"`
	Cached         bool                         `json:"cached"`
	FallbackReason string                       `json:"fallback_reason,omitempty"`
}

type groomQualitySection struct {
	Shadow     bool                `json:"shadow"`
	Candidates []groomQualityEntry `json:"candidates"`
	Retired    []int64             `json:"retired,omitempty"`
	Skipped    []int64             `json:"skipped,omitempty"`
}

// groomPlan is the reviewable artifact `--propose` writes and `--yes --plan` reads.
type groomPlan struct {
	SchemaVersion       int                    `json:"schema_version"`
	SnapshotHash        string                 `json:"snapshot_hash"`
	ProposedRetirements []groomPlanRetirement  `json:"proposed_retirements"`
	RewriteFlags        []groomPlanRewriteFlag `json:"rewrite_flags"`
	Rekeys              []groomPlanRekey       `json:"rekeys"`
	CrossPool           []groomPlanCrossPool   `json:"cross_pool"`
	Quality             groomQualitySection    `json:"quality"`
	Stale               []groomStaleEntry      `json:"stale"`
	Stats               memory.GroomStats      `json:"stats"`
}

// groomApplyResult is the --json summary of an apply run.
type groomApplyResult struct {
	Plan             string  `json:"plan"`
	SnapshotHash     string  `json:"snapshot_hash"`
	Applied          bool    `json:"applied"`
	Retired          []int64 `json:"retired"`
	Skipped          []int64 `json:"skipped"`
	Rekeyed          []int64 `json:"rekeyed,omitempty"`
	RekeySkipped     []int64 `json:"rekey_skipped,omitempty"`
	Promoted         []int64 `json:"promoted,omitempty"`
	CrossPoolSkipped []int64 `json:"cross_pool_skipped,omitempty"`
}

type groomSplitChildSummary struct {
	ID    int64  `json:"id,omitempty"`
	Key   string `json:"key"`
	Chars int    `json:"chars"`
}

type groomSplitSummary struct {
	ParentID  int64                    `json:"parent_id"`
	ParentKey string                   `json:"parent_key"`
	Children  []groomSplitChildSummary `json:"children"`
}

type groomSplitOutput struct {
	DryRun       bool                 `json:"dry_run"`
	Detected     int                  `json:"detected"`
	Applied      int                  `json:"applied"`
	Skipped      []int64              `json:"skipped"`
	Splits       []groomSplitSummary  `json:"splits"`
	LLM          []groomSplitLLMEntry `json:"llm,omitempty"`
	Quality      groomQualitySection  `json:"quality"`
	Stale        []groomStaleEntry    `json:"stale,omitempty"`
	StaleRetired []int64              `json:"stale_retired,omitempty"`
	StaleSkipped []int64              `json:"stale_skipped,omitempty"`
}

type groomSplitLLMEntry struct {
	ParentID       int64    `json:"parent_id"`
	ContentHash    string   `json:"content_hash"`
	Model          string   `json:"model"`
	Decision       string   `json:"decision"`
	CutIDs         []string `json:"cut_ids"`
	FallbackReason string   `json:"fallback_reason"`
	Cached         bool     `json:"cached"`
}

type groomLLMCut struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type groomLLMReply struct {
	Split bool          `json:"split"`
	Cuts  []groomLLMCut `json:"cuts"`
}

type groomStaleReply struct {
	Verdict string `json:"verdict"`
	Residue string `json:"residue"`
}

type groomQualityReply struct {
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
	Residue    string  `json:"residue"`
}

type groomLLMBudget struct {
	used int
	max  int
}

func newGroomLLMBudget(max int) *groomLLMBudget {
	return &groomLLMBudget{max: max}
}

func (b *groomLLMBudget) take() bool {
	if b == nil || b.max <= 0 || b.used >= b.max {
		return false
	}
	b.used++
	return true
}

type groomLLMDeliverFunc func(context.Context, runtime.Agent, string) (string, error)

var memoryGroomLLMDeliver groomLLMDeliverFunc = deliverOneShotRuntimePrompt

type groomSplitRevertOutput struct {
	DryRun   bool                         `json:"dry_run"`
	Matched  int                          `json:"matched"`
	Reverted []db.GroomSplitReverted      `json:"reverted"`
	Skipped  []db.GroomSplitRevertSkipped `json:"skipped"`
}

type groomParentIDs []int64

func (ids *groomParentIDs) String() string {
	parts := make([]string, 0, len(*ids))
	for _, id := range *ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func (ids *groomParentIDs) Set(value string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("parent must be a positive memory id: %q", value)
	}
	*ids = append(*ids, id)
	return nil
}

func runMemoryGroom(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory groom", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	propose := fs.Bool("propose", false, "read confirmed memory, run the detectors, and write a reviewable plan (writes nothing to the store)")
	yes := fs.Bool("yes", false, "apply a plan's retirements (requires --plan)")
	split := fs.Bool("split", false, "automatically split qualifying brick memories into lossless children")
	splitRevert := fs.Bool("split-revert", false, "restore parents from active lossless groom-split children")
	dryRun := fs.Bool("dry-run", false, "with --split or --split-revert, print changes without writing")
	var parentIDs groomParentIDs
	fs.Var(&parentIDs, "parent", "with --split-revert, restore only this parent memory id (repeatable)")
	since := fs.String("since", "", "with --split-revert, restore splits created at or after this RFC3339 timestamp")
	plan := fs.String("plan", "", "path to a plan artifact produced by --propose (required with --yes)")
	out := fs.String("out", "", "where --propose writes the plan (default: <home>/evals/groom/groom-<snapshot>.json)")
	jsonOut := fs.Bool("json", false, "print the summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	modes := 0
	for _, enabled := range []bool{*propose, *yes, *split, *splitRevert} {
		if enabled {
			modes++
		}
	}
	if modes != 1 {
		fmt.Fprintln(stderr, "memory groom: pass exactly one of --propose, --yes, --split, or --split-revert")
		printMemoryGroomUsage(stderr)
		return 2
	}
	if *dryRun && !*split && !*splitRevert {
		fmt.Fprintln(stderr, "memory groom: --dry-run requires --split or --split-revert")
		printMemoryGroomUsage(stderr)
		return 2
	}
	if (len(parentIDs) > 0 || strings.TrimSpace(*since) != "") && !*splitRevert {
		fmt.Fprintln(stderr, "memory groom: --parent and --since require --split-revert")
		printMemoryGroomUsage(stderr)
		return 2
	}
	if value := strings.TrimSpace(*since); value != "" {
		if _, err := time.Parse(time.RFC3339, value); err != nil {
			fmt.Fprintf(stderr, "memory groom: --since must be RFC3339: %v\n", err)
			return 2
		}
	}
	if *propose {
		return runMemoryGroomPropose(*home, *out, *jsonOut, stdout, stderr)
	}
	if *split {
		return runMemoryGroomSplit(*home, *dryRun, *jsonOut, stdout, stderr)
	}
	if *splitRevert {
		return runMemoryGroomSplitRevert(*home, []int64(parentIDs), strings.TrimSpace(*since), *dryRun, *jsonOut, stdout, stderr)
	}
	return runMemoryGroomApply(*home, *plan, *jsonOut, stdout, stderr)
}

func printMemoryGroomUsage(w io.Writer) {
	fmt.Fprintln(w, "Deterministically groom confirmed memory: auto-split bricks; propose other actions for owner approval.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot memory groom --propose [--out PLAN.json] [--json]")
	fmt.Fprintln(w, "  gitmoot memory groom --yes --plan PLAN.json [--json]")
	fmt.Fprintln(w, "  gitmoot memory groom --split [--dry-run] [--json]")
	fmt.Fprintln(w, "  gitmoot memory groom --split-revert [--dry-run] [--parent N]... [--since RFC3339] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  --propose  read active confirmed memory, run the deterministic detectors")
	fmt.Fprintln(w, "             (status/changelog/ToC snapshots, bare to-do lists, exact duplicates,")
	fmt.Fprintln(w, "             legacy-key rekeys, cross-pool stale shared editions; over-long bricks")
	fmt.Fprintln(w, "             are FLAGGED, not retired) and write a reviewable plan. Confirmed")
	fmt.Fprintln(w, "             memory is not changed; enabled stale checks may cache LLM verdicts.")
	fmt.Fprintln(w, "  --yes      apply a plan's actions in one transaction: retirements, legacy-key")
	fmt.Fprintln(w, "             rekeys, and cross-pool promote-and-retire pairs. Recomputes the vault")
	fmt.Fprintln(w, "             snapshot and ABORTS AS STALE if the store changed since --propose.")
	fmt.Fprintln(w, "             Content is never edited or rewritten.")
	fmt.Fprintln(w, "  --split    automatically split qualifying multi-story bricks at deterministic")
	fmt.Fprintln(w, "             seams, then optional host-bounded LLM cuts. Children remain exact")
	fmt.Fprintln(w, "             substrings; the parent is superseded. Expired status batons retire")
	fmt.Fprintln(w, "             only when deterministic shape and an LLM verdict agree.")
	fmt.Fprintln(w, "  --split-revert  retire intact split children and restore their superseded parent.")
	fmt.Fprintln(w, "                  Defaults to all active groom splits; --parent and --since filter.")
	fmt.Fprintln(w, "  --dry-run  with --split or --split-revert, print changes without touching the store.")
}

func runMemoryGroomSplit(home string, dryRun, jsonOut bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	now := time.Now().UTC()
	var splits []memory.GroomSplit
	var applied db.GroomSplitResult
	var qualityResult db.GroomRetireResult
	var staleResult db.GroomRetireResult
	var llmEntries []groomSplitLLMEntry
	var qualitySection groomQualitySection
	var staleEntries []groomStaleEntry
	run := func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
		if err != nil {
			return err
		}
		cands := make([]memory.GroomCandidate, 0, len(rows))
		for _, row := range rows {
			cands = append(cands, memory.GroomCandidate{
				ID: row.ID, Key: row.Key, Content: row.Content, Provenance: row.Provenance, OwnerKind: row.Owner.Kind,
				OwnerRef: row.Owner.Ref, OwnerVersion: row.Owner.Version, Repo: row.Repo,
				Scope: row.Scope, FirstConfirmedAt: row.FirstConfirmedAt, UpdatedAt: row.UpdatedAt,
			})
		}
		budget := newGroomLLMBudget(settings.GroomLLMTotalMaxPerRun)
		splitCands := cands
		var qualityRetirements []db.GroomRetire
		qualitySection, qualityRetirements, err = runMemoryGroomQuality(ctx, store, settings, cands, now, true, dryRun, budget)
		if err != nil {
			return err
		}
		qualityIDs := make(map[int64]struct{})
		for _, entry := range qualitySection.Candidates {
			if entry.Action == "auto_retire" || entry.Action == "would_retire" {
				qualityIDs[entry.Candidate.ID] = struct{}{}
			}
		}
		if len(qualityIDs) > 0 {
			splitCands = make([]memory.GroomCandidate, 0, len(cands)-len(qualityIDs))
			for _, candidate := range cands {
				if _, quality := qualityIDs[candidate.ID]; !quality {
					splitCands = append(splitCands, candidate)
				}
			}
		}
		var staleRetirements []db.GroomRetire
		if settings.GroomStale {
			entries, retirements, err := runMemoryGroomStale(ctx, store, settings, splitCands, now, true, dryRun, budget)
			if err != nil {
				return err
			}
			staleEntries, staleRetirements = entries, retirements
			staleIDs := make(map[int64]struct{}, len(entries))
			for _, entry := range entries {
				staleIDs[entry.Candidate.ID] = struct{}{}
			}
			withoutStale := make([]memory.GroomCandidate, 0, len(splitCands)-len(staleIDs))
			for _, candidate := range splitCands {
				if _, stale := staleIDs[candidate.ID]; !stale {
					withoutStale = append(withoutStale, candidate)
				}
			}
			splitCands = withoutStale
		}
		splits = memory.DetectGroomSplits(splitCands)
		if settings.GroomSplitLLM {
			llmSplits, entries, err := runMemoryGroomLLMSplits(ctx, store, settings, splitCands, splits, dryRun, budget)
			if err != nil {
				return err
			}
			splits = append(splits, llmSplits...)
			llmEntries = entries
		}
		if dryRun {
			return nil
		}
		if len(splits) > 0 {
			items := make([]db.GroomSplitItem, 0, len(splits))
			for _, split := range splits {
				item := db.GroomSplitItem{ParentID: split.ParentID, ExpectedUpdatedAt: split.ExpectedUpdatedAt}
				for _, child := range split.Children {
					item.Children = append(item.Children, db.GroomSplitChild{Key: child.Key, Content: child.Content})
				}
				items = append(items, item)
			}
			applied, err = store.ApplyGroomSplits(ctx, items)
			if err != nil {
				return err
			}
		}
		if len(qualityRetirements) > 0 {
			qualityResult, err = store.ApplyGroomRetirements(ctx, qualityRetirements)
			if err != nil {
				return err
			}
			qualitySection.Retired = qualityResult.Retired
			qualitySection.Skipped = qualityResult.Skipped
		}
		if len(staleRetirements) > 0 {
			staleResult, err = store.ApplyGroomRetirements(ctx, staleRetirements)
			return err
		}
		return nil
	}
	var err error
	if dryRun {
		var paths config.Paths
		paths, err = pathsFromFlag(home)
		if err == nil {
			err = withReadOnlyStore(home, func(store *db.Store) error { return run(paths, store) })
		}
	} else {
		err = withStoreAndPaths(home, run)
	}
	if err != nil {
		fmt.Fprintf(stderr, "memory groom: split: %v\n", err)
		return 1
	}

	idsByParent := make(map[int64][]int64, len(applied.Applied))
	for _, item := range applied.Applied {
		idsByParent[item.ParentID] = item.ChildIDs
	}
	out := groomSplitOutput{
		DryRun: dryRun, Detected: len(splits), Applied: len(applied.Applied), Skipped: applied.Skipped,
		Splits: make([]groomSplitSummary, 0, len(splits)), LLM: llmEntries, Quality: qualitySection, Stale: staleEntries,
		StaleRetired: staleResult.Retired, StaleSkipped: staleResult.Skipped,
	}
	for _, split := range splits {
		summary := groomSplitSummary{ParentID: split.ParentID, ParentKey: split.ParentKey}
		childIDs := idsByParent[split.ParentID]
		for i, child := range split.Children {
			entry := groomSplitChildSummary{Key: child.Key, Chars: len(child.Content)}
			if i < len(childIDs) {
				entry.ID = childIDs[i]
			}
			summary.Children = append(summary.Children, entry)
		}
		out.Splits = append(out.Splits, summary)
	}
	if jsonOut {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "memory groom: %v\n", err)
			return 1
		}
		return 0
	}
	verb := "split"
	if dryRun {
		verb = "would split"
	}
	fmt.Fprintf(stdout, "%s %d brick memory(ies)", verb, len(splits))
	if !dryRun {
		fmt.Fprintf(stdout, "; applied %d, skipped %d", len(applied.Applied), len(applied.Skipped))
	}
	fmt.Fprintln(stdout)
	if len(staleEntries) > 0 {
		fmt.Fprintf(stdout, "stale status candidates %d; retired %d, skipped %d\n", len(staleEntries), len(staleResult.Retired), len(staleResult.Skipped))
	}
	if len(qualitySection.Candidates) > 0 {
		fmt.Fprintf(stdout, "quality candidates %d (shadow=%t); retired %d, skipped %d\n", len(qualitySection.Candidates), qualitySection.Shadow, len(qualitySection.Retired), len(qualitySection.Skipped))
	}
	for _, split := range out.Splits {
		fmt.Fprintf(stdout, "  parent %d %s ->", split.ParentID, split.ParentKey)
		for _, child := range split.Children {
			if child.ID > 0 {
				fmt.Fprintf(stdout, " %d:%s", child.ID, child.Key)
			} else {
				fmt.Fprintf(stdout, " %s", child.Key)
			}
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

func runMemoryGroomQuality(ctx context.Context, store *db.Store, settings config.MemorySettings, cands []memory.GroomCandidate, now time.Time, autoRetire, dryRun bool, budget *groomLLMBudget) (groomQualitySection, []db.GroomRetire, error) {
	candidates := memory.DetectGroomQualityCandidates(cands, now, settings.GroomQualityMinAge)
	section := groomQualitySection{Shadow: !settings.GroomQuality, Candidates: make([]groomQualityEntry, 0, len(candidates))}
	modelLabel := groomLLMModelLabel(settings.GroomSplitLLMRuntime, settings.GroomSplitLLMModel)
	var retirements []db.GroomRetire
	calls := 0
	for _, candidate := range candidates {
		entry := groomQualityEntry{
			Candidate: groomQualityCandidateSummary{
				ID: candidate.ID, Key: candidate.Key, ContentHash: candidate.ContentHash,
				Score: candidate.Score, SignalFamilies: candidate.SignalFamilies,
			},
			Verdict: "unknown", Action: "propose_review", Model: modelLabel,
		}
		cached, found, err := store.GetGroomQualityVerdict(ctx, candidate.ContentHash)
		if err != nil {
			return groomQualitySection{}, nil, err
		}
		var reply groomQualityReply
		if found {
			entry.Cached = true
			entry.Model = cached.Model
			reply = groomQualityReply{Verdict: cached.Verdict, Confidence: cached.Confidence, Residue: cached.Residue}
			if err := validateGroomQualityReply(reply, candidate.Content); err != nil {
				entry.FallbackReason = "cached verdict: " + err.Error()
				section.Candidates = append(section.Candidates, entry)
				continue
			}
		} else {
			if calls >= settings.GroomQualityMaxPerRun {
				entry.FallbackReason = "quality per-run LLM cap reached"
				section.Candidates = append(section.Candidates, entry)
				continue
			}
			if !budget.take() {
				entry.FallbackReason = "shared per-run LLM cap reached"
				section.Candidates = append(section.Candidates, entry)
				continue
			}
			calls++
			agent := groomLLMRuntimeAgent(ctx, store, settings, candidate.GroomCandidate)
			callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			raw, deliverErr := memoryGroomLLMDeliver(callCtx, agent, groomQualityPrompt(candidate))
			cancel()
			if deliverErr != nil {
				entry.FallbackReason = "runtime delivery: " + deliverErr.Error()
				section.Candidates = append(section.Candidates, entry)
				continue
			}
			reply, err = parseGroomQualityReply(raw)
			if err != nil {
				entry.FallbackReason = "invalid reply: " + err.Error()
				section.Candidates = append(section.Candidates, entry)
				continue
			}
			if err := validateGroomQualityReply(reply, candidate.Content); err != nil {
				entry.FallbackReason = "invalid verdict: " + err.Error()
				section.Candidates = append(section.Candidates, entry)
				continue
			}
			if !dryRun {
				if err := store.StoreGroomQualityVerdict(ctx, db.GroomQualityVerdict{
					ContentHash: candidate.ContentHash, Verdict: reply.Verdict, Confidence: reply.Confidence,
					Residue: reply.Residue, Model: modelLabel,
				}); err != nil {
					return groomQualitySection{}, nil, err
				}
			}
		}

		entry.Verdict = reply.Verdict
		entry.Confidence = reply.Confidence
		entry.Residue = reply.Residue
		switch reply.Verdict {
		case "useless":
			if len(candidate.SignalFamilies) < 2 {
				entry.Action = "propose_review"
				entry.FallbackReason = "fewer than two corroborating signal families"
			} else if !settings.GroomQuality {
				entry.Action = "would_retire"
			} else {
				entry.Action = "auto_retire"
				if autoRetire {
					retirements = append(retirements, db.GroomRetire{ID: candidate.ID, Reason: "groom-quality:" + now.Format(time.DateOnly)})
				}
			}
		case "useful":
			entry.Action = "keep"
		case "contains_durable_residue":
			entry.Action = "propose_extract"
		}
		section.Candidates = append(section.Candidates, entry)
	}
	return section, retirements, nil
}

func groomQualityPrompt(candidate memory.GroomQualityCandidate) string {
	return "Classify whether this confirmed memory carries durable, reusable information. Return exactly one JSON object with no markdown: " +
		`{"verdict":"useless|useful|contains_durable_residue","confidence":0.0,"residue":""}. ` +
		"Use useless only when the fact adds no durable value beyond transient/source-controlled history, malformed fragments, or generic observations. " +
		"Use useful for a concrete reusable fact or lesson. Use contains_durable_residue when junk framing contains a timeless kernel; residue must then be one exact verbatim quote from the content. " +
		"For the other verdicts residue must be empty. Confidence must be between 0 and 1.\n\nMemory key: " + candidate.Key +
		"\nDeterministic risk score: " + strconv.Itoa(candidate.Score) +
		"\nSignal families: " + strings.Join(candidate.SignalFamilies, ", ") +
		"\nContent (verbatim):\n<content>\n" + candidate.Content + "\n</content>"
}

func parseGroomQualityReply(raw string) (groomQualityReply, error) {
	object, err := firstJSONObject(raw)
	if err != nil {
		return groomQualityReply{}, err
	}
	var wire struct {
		Verdict    *string  `json:"verdict"`
		Confidence *float64 `json:"confidence"`
		Residue    *string  `json:"residue"`
	}
	decoder := json.NewDecoder(strings.NewReader(object))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return groomQualityReply{}, err
	}
	if wire.Verdict == nil || wire.Confidence == nil || wire.Residue == nil {
		return groomQualityReply{}, fmt.Errorf("reply must include verdict, confidence, and residue")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return groomQualityReply{}, fmt.Errorf("reply contains trailing JSON values")
	}
	return groomQualityReply{
		Verdict: strings.TrimSpace(*wire.Verdict), Confidence: *wire.Confidence, Residue: strings.TrimSpace(*wire.Residue),
	}, nil
}

func validateGroomQualityReply(reply groomQualityReply, content string) error {
	if math.IsNaN(reply.Confidence) || math.IsInf(reply.Confidence, 0) || reply.Confidence < 0 || reply.Confidence > 1 {
		return fmt.Errorf("confidence must be between 0 and 1")
	}
	switch reply.Verdict {
	case "useless", "useful":
		if reply.Residue != "" {
			return fmt.Errorf("%s requires empty residue", reply.Verdict)
		}
	case "contains_durable_residue":
		if reply.Residue == "" {
			return fmt.Errorf("contains_durable_residue requires a residue quote")
		}
		if !strings.Contains(content, reply.Residue) {
			return fmt.Errorf("residue is not an exact content quote")
		}
	default:
		return fmt.Errorf("unknown verdict %q", reply.Verdict)
	}
	return nil
}

func runMemoryGroomStale(ctx context.Context, store *db.Store, settings config.MemorySettings, cands []memory.GroomCandidate, now time.Time, autoRetire, dryRun bool, budget *groomLLMBudget) ([]groomStaleEntry, []db.GroomRetire, error) {
	candidates := memory.DetectStaleStatusCandidates(cands, now, settings.GroomStaleAge)
	modelLabel := groomLLMModelLabel(settings.GroomSplitLLMRuntime, settings.GroomSplitLLMModel)
	entries := make([]groomStaleEntry, 0, len(candidates))
	var retirements []db.GroomRetire
	calls := 0
	for _, candidate := range candidates {
		entry := groomStaleEntry{
			Candidate: groomStaleCandidateSummary{ID: candidate.ID, Key: candidate.Key, ContentHash: candidate.ContentHash, NewestDate: candidate.NewestDate},
			Verdict:   "unknown", Action: "propose_review", Model: modelLabel,
		}
		cached, found, err := store.GetGroomStaleVerdict(ctx, candidate.ContentHash)
		if err != nil {
			return nil, nil, err
		}
		var reply groomStaleReply
		if found {
			entry.Cached = true
			entry.Model = cached.Model
			reply = groomStaleReply{Verdict: cached.Verdict, Residue: cached.Residue}
			if err := validateGroomStaleReply(reply, candidate.Content); err != nil {
				entry.FallbackReason = "cached verdict: " + err.Error()
				entries = append(entries, entry)
				continue
			}
		} else {
			if !settings.GroomSplitLLM {
				entry.FallbackReason = "groom_split_llm is disabled"
				entries = append(entries, entry)
				continue
			}
			if calls >= settings.GroomSplitLLMMaxPerRun {
				entry.FallbackReason = "per-run LLM cap reached"
				entries = append(entries, entry)
				continue
			}
			if !budget.take() {
				entry.FallbackReason = "shared per-run LLM cap reached"
				entries = append(entries, entry)
				continue
			}
			calls++
			agent := groomLLMRuntimeAgent(ctx, store, settings, candidate.GroomCandidate)
			callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			raw, deliverErr := memoryGroomLLMDeliver(callCtx, agent, groomStalePrompt(candidate))
			cancel()
			if deliverErr != nil {
				entry.FallbackReason = "runtime delivery: " + deliverErr.Error()
				entries = append(entries, entry)
				continue
			}
			reply, err = parseGroomStaleReply(raw)
			if err != nil {
				entry.FallbackReason = "invalid reply: " + err.Error()
				entries = append(entries, entry)
				continue
			}
			if err := validateGroomStaleReply(reply, candidate.Content); err != nil {
				entry.FallbackReason = "invalid verdict: " + err.Error()
				entries = append(entries, entry)
				continue
			}
			if !dryRun {
				if err := store.StoreGroomStaleVerdict(ctx, db.GroomStaleVerdict{
					ContentHash: candidate.ContentHash, Verdict: reply.Verdict, Residue: reply.Residue, Model: modelLabel,
				}); err != nil {
					return nil, nil, err
				}
			}
		}

		entry.Verdict = reply.Verdict
		entry.Residue = reply.Residue
		switch reply.Verdict {
		case "expired":
			entry.Action = "auto_retire"
			if autoRetire {
				retirements = append(retirements, db.GroomRetire{ID: candidate.ID, Reason: "groom-stale:" + now.Format(time.DateOnly)})
			}
		case "still_relevant":
			entry.Action = "keep"
		case "contains_durable_residue":
			entry.Action = "propose_extract"
		}
		entries = append(entries, entry)
	}
	return entries, retirements, nil
}

func groomStalePrompt(candidate memory.GroomStaleStatusCandidate) string {
	return "Classify this dated operational-status memory. Return exactly one JSON object with no markdown: " +
		`{"verdict":"expired|still_relevant|contains_durable_residue","residue":""}. ` +
		"Use expired only when the operational baton itself is no longer actionable. Use still_relevant when its current status still matters. " +
		"Use contains_durable_residue when stale status is mixed with a timeless rule or lesson; residue must then be one exact verbatim quote from the content. " +
		"For the other verdicts residue must be empty.\n\nMemory key: " + candidate.Key +
		"\nNewest in-content date: " + candidate.NewestDate + "\nContent (verbatim):\n<content>\n" + candidate.Content + "\n</content>"
}

func parseGroomStaleReply(raw string) (groomStaleReply, error) {
	object, err := firstJSONObject(raw)
	if err != nil {
		return groomStaleReply{}, err
	}
	var wire struct {
		Verdict *string `json:"verdict"`
		Residue *string `json:"residue"`
	}
	decoder := json.NewDecoder(strings.NewReader(object))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return groomStaleReply{}, err
	}
	if wire.Verdict == nil || wire.Residue == nil {
		return groomStaleReply{}, fmt.Errorf("reply must include verdict and residue")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return groomStaleReply{}, fmt.Errorf("reply contains trailing JSON values")
	}
	return groomStaleReply{Verdict: strings.TrimSpace(*wire.Verdict), Residue: strings.TrimSpace(*wire.Residue)}, nil
}

func validateGroomStaleReply(reply groomStaleReply, content string) error {
	switch reply.Verdict {
	case "expired", "still_relevant":
		if reply.Residue != "" {
			return fmt.Errorf("%s requires empty residue", reply.Verdict)
		}
	case "contains_durable_residue":
		if reply.Residue == "" {
			return fmt.Errorf("contains_durable_residue requires a residue quote")
		}
		if !strings.Contains(content, reply.Residue) {
			return fmt.Errorf("residue is not an exact content quote")
		}
	default:
		return fmt.Errorf("unknown verdict %q", reply.Verdict)
	}
	return nil
}

func runMemoryGroomLLMSplits(ctx context.Context, store *db.Store, settings config.MemorySettings, cands []memory.GroomCandidate, deterministic []memory.GroomSplit, dryRun bool, budget *groomLLMBudget) ([]memory.GroomSplit, []groomSplitLLMEntry, error) {
	candidates := memory.DetectGroomLLMCandidates(cands)
	modelLabel := groomLLMModelLabel(settings.GroomSplitLLMRuntime, settings.GroomSplitLLMModel)
	var splits []memory.GroomSplit
	var entries []groomSplitLLMEntry
	calls := 0
	for _, candidate := range candidates {
		cached, found, err := store.GetGroomLLMVerdict(ctx, candidate.ContentHash)
		if err != nil {
			return nil, nil, err
		}
		if found {
			entry := groomSplitLLMEntry{
				ParentID: candidate.ID, ContentHash: candidate.ContentHash, Model: cached.Model,
				Decision: cached.Verdict, CutIDs: []string{}, Cached: true,
			}
			if cached.Verdict == "no_split" {
				entries = append(entries, entry)
				continue
			}
			menu := memory.EnumerateGroomLLMBoundaries(candidate.Content)
			cuts, err := decodeGroomLLMCuts(cached.CutsJSON)
			if err != nil {
				entry.Decision = "fallback"
				entry.FallbackReason = "cached cuts: " + err.Error()
				entries = append(entries, entry)
				continue
			}
			cutIDs, offsets, err := validateGroomLLMReply(groomLLMReply{Split: true, Cuts: cuts}, menu)
			if err != nil {
				entry.Decision = "fallback"
				entry.FallbackReason = "cached cuts: " + err.Error()
				entries = append(entries, entry)
				continue
			}
			children := memory.BuildGroomSplitFromOffsets(candidate.Key, candidate.Content, offsets)
			if len(children) < 2 {
				entry.Decision = "fallback"
				entry.FallbackReason = "cached cuts failed lossless split validation"
				entries = append(entries, entry)
				continue
			}
			reserved := append(append([]memory.GroomSplit(nil), deterministic...), splits...)
			children = memory.AllocateGroomSplitChildKeys(cands, reserved, candidate.GroomCandidate, children)
			splits = append(splits, memory.GroomSplit{
				ParentID: candidate.ID, ParentKey: candidate.Key, ExpectedUpdatedAt: candidate.UpdatedAt, Children: children,
			})
			entry.CutIDs = cutIDs
			entries = append(entries, entry)
			continue
		}

		if calls >= settings.GroomSplitLLMMaxPerRun {
			continue
		}
		entry := groomSplitLLMEntry{
			ParentID: candidate.ID, ContentHash: candidate.ContentHash, Model: modelLabel, CutIDs: []string{}, Cached: false,
		}
		if candidate.Bytes > memory.GroomLLMMaxContentBytes {
			entry.Decision = "skipped"
			entry.FallbackReason = fmt.Sprintf("content exceeds %d-byte limit", memory.GroomLLMMaxContentBytes)
			entries = append(entries, entry)
			continue
		}
		menu := memory.EnumerateGroomLLMBoundaries(candidate.Content)
		if len(menu) == 0 {
			entry.Decision = "fallback"
			entry.FallbackReason = "no safe candidate boundaries"
			entries = append(entries, entry)
			continue
		}
		if !budget.take() {
			entry.Decision = "skipped"
			entry.FallbackReason = "shared per-run LLM cap reached"
			entries = append(entries, entry)
			continue
		}
		calls++
		agent := groomLLMRuntimeAgent(ctx, store, settings, candidate.GroomCandidate)
		callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		raw, deliverErr := memoryGroomLLMDeliver(callCtx, agent, groomLLMPrompt(candidate.Key, candidate.Content, menu))
		cancel()
		if deliverErr != nil {
			entry.Decision = "fallback"
			entry.FallbackReason = "runtime delivery: " + deliverErr.Error()
			entries = append(entries, entry)
			continue
		}
		reply, err := parseGroomLLMReply(raw)
		if err != nil {
			entry.Decision = "fallback"
			entry.FallbackReason = "invalid reply: " + err.Error()
			entries = append(entries, entry)
			continue
		}
		cutIDs, offsets, err := validateGroomLLMReply(reply, menu)
		if err != nil {
			entry.Decision = "fallback"
			entry.FallbackReason = "invalid cuts: " + err.Error()
			entries = append(entries, entry)
			continue
		}
		if !reply.Split {
			entry.Decision = "no_split"
			if !dryRun {
				if err := store.StoreGroomLLMVerdict(ctx, db.GroomLLMVerdict{
					ContentHash: candidate.ContentHash, Verdict: "no_split", Model: modelLabel,
				}); err != nil {
					return nil, nil, err
				}
			}
			entries = append(entries, entry)
			continue
		}
		children := memory.BuildGroomSplitFromOffsets(candidate.Key, candidate.Content, offsets)
		if len(children) < 2 {
			entry.Decision = "fallback"
			entry.FallbackReason = "selected cuts failed lossless split validation"
			entries = append(entries, entry)
			continue
		}
		reserved := append(append([]memory.GroomSplit(nil), deterministic...), splits...)
		children = memory.AllocateGroomSplitChildKeys(cands, reserved, candidate.GroomCandidate, children)
		if !dryRun {
			cutsJSON, err := json.Marshal(reply.Cuts)
			if err != nil {
				return nil, nil, err
			}
			if err := store.StoreGroomLLMVerdict(ctx, db.GroomLLMVerdict{
				ContentHash: candidate.ContentHash, Verdict: "split", CutsJSON: string(cutsJSON), Model: modelLabel,
			}); err != nil {
				return nil, nil, err
			}
		}
		splits = append(splits, memory.GroomSplit{
			ParentID: candidate.ID, ParentKey: candidate.Key, ExpectedUpdatedAt: candidate.UpdatedAt, Children: children,
		})
		entry.Decision = "split"
		entry.CutIDs = cutIDs
		entries = append(entries, entry)
	}
	return splits, entries, nil
}

func groomLLMRuntimeAgent(ctx context.Context, store *db.Store, settings config.MemorySettings, candidate memory.GroomCandidate) runtime.Agent {
	workingDir, _ := os.Getwd()
	if strings.TrimSpace(candidate.Repo) != "" {
		if repo, err := store.GetRepo(ctx, candidate.Repo); err == nil && strings.TrimSpace(repo.CheckoutPath) != "" {
			workingDir = repo.CheckoutPath
		}
	}
	return runtime.Agent{
		Name: "memory-groom-llm-" + strconv.FormatInt(candidate.ID, 10), Role: "ask",
		Runtime: settings.GroomSplitLLMRuntime, RuntimeRef: "", RepoScope: candidate.Repo,
		WorkingDir: workingDir, Capabilities: []string{"ask"}, AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		Model: settings.GroomSplitLLMModel, SingleUseSession: true,
	}
}

func groomLLMModelLabel(runtimeName, model string) string {
	if strings.TrimSpace(model) == "" {
		return strings.TrimSpace(runtimeName)
	}
	return strings.TrimSpace(runtimeName) + "/" + strings.TrimSpace(model)
}

func groomLLMPrompt(key, content string, menu []memory.GroomLLMBoundary) string {
	var b strings.Builder
	b.WriteString("Choose whether this memory contains independent stories that should be split.\n")
	b.WriteString("Return exactly one JSON object with no markdown: {\"split\":bool,\"cuts\":[{\"id\":\"cNNN\",\"text\":\"exact echoed line\"}]}.\n")
	b.WriteString("For keep, return split=false and cuts=[]. For split, choose only ids from the host menu and echo each line exactly. Never rewrite content.\n\n")
	b.WriteString("Memory key: ")
	b.WriteString(key)
	b.WriteString("\nCandidate boundaries:\n")
	for _, boundary := range menu {
		b.WriteString(boundary.ID)
		b.WriteString(" ")
		b.WriteString(strconv.Quote(boundary.Text))
		b.WriteByte('\n')
	}
	b.WriteString("\nContent (verbatim):\n<content>\n")
	b.WriteString(content)
	b.WriteString("\n</content>")
	return b.String()
}

func parseGroomLLMReply(raw string) (groomLLMReply, error) {
	object, err := firstJSONObject(raw)
	if err != nil {
		return groomLLMReply{}, err
	}
	var wire struct {
		Split *bool          `json:"split"`
		Cuts  *[]groomLLMCut `json:"cuts"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(object))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return groomLLMReply{}, err
	}
	if wire.Split == nil || wire.Cuts == nil {
		return groomLLMReply{}, fmt.Errorf("reply must include split and cuts")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return groomLLMReply{}, fmt.Errorf("reply contains trailing JSON values")
	}
	return groomLLMReply{Split: *wire.Split, Cuts: *wire.Cuts}, nil
}

func decodeGroomLLMCuts(raw string) ([]groomLLMCut, error) {
	var cuts []groomLLMCut
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cuts); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("cuts contain trailing JSON values")
	}
	return cuts, nil
}

func firstJSONObject(raw string) (string, error) {
	start := -1
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if start < 0 {
			if ch == '{' {
				start = i
				depth = 1
			}
			continue
		}
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("no complete JSON object found")
}

func validateGroomLLMReply(reply groomLLMReply, menu []memory.GroomLLMBoundary) ([]string, []int, error) {
	if !reply.Split {
		if len(reply.Cuts) != 0 {
			return nil, nil, fmt.Errorf("split=false requires empty cuts")
		}
		return nil, nil, nil
	}
	if len(reply.Cuts) == 0 {
		return nil, nil, fmt.Errorf("split=true requires at least one cut")
	}
	byID := make(map[string]memory.GroomLLMBoundary, len(menu))
	for _, boundary := range menu {
		byID[boundary.ID] = boundary
	}
	type selected struct {
		id     string
		offset int
	}
	chosen := make([]selected, 0, len(reply.Cuts))
	seen := make(map[string]struct{}, len(reply.Cuts))
	for _, cut := range reply.Cuts {
		boundary, ok := byID[cut.ID]
		if !ok {
			return nil, nil, fmt.Errorf("unknown cut id %q", cut.ID)
		}
		if _, duplicate := seen[cut.ID]; duplicate {
			return nil, nil, fmt.Errorf("duplicate cut id %q", cut.ID)
		}
		seen[cut.ID] = struct{}{}
		if cut.Text != boundary.Text {
			return nil, nil, fmt.Errorf("cut %s echoed %q, want %q", cut.ID, cut.Text, boundary.Text)
		}
		chosen = append(chosen, selected{id: cut.ID, offset: boundary.Offset})
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].offset < chosen[j].offset })
	ids := make([]string, len(chosen))
	offsets := make([]int, len(chosen))
	for i, cut := range chosen {
		ids[i], offsets[i] = cut.id, cut.offset
	}
	return ids, offsets, nil
}

func runMemoryGroomSplitRevert(home string, parentIDs []int64, since string, dryRun, jsonOut bool, stdout, stderr io.Writer) int {
	ctx := context.Background()
	var result db.GroomSplitRevertResult
	run := func(store *db.Store) error {
		var err error
		result, err = store.RevertGroomSplits(ctx, db.GroomSplitRevertOptions{
			ParentIDs: parentIDs,
			Since:     since,
			DryRun:    dryRun,
		})
		return err
	}
	var err error
	if dryRun {
		err = withReadOnlyStore(home, run)
	} else {
		err = withStore(home, run)
	}
	if err != nil {
		fmt.Fprintf(stderr, "memory groom: split-revert: %v\n", err)
		return 1
	}
	output := groomSplitRevertOutput{
		DryRun: dryRun, Matched: len(result.Reverted) + len(result.Skipped),
		Reverted: result.Reverted, Skipped: result.Skipped,
	}
	if jsonOut {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "memory groom: %v\n", err)
			return 1
		}
		return 0
	}
	verb := "reverted"
	if dryRun {
		verb = "would revert"
	}
	fmt.Fprintf(stdout, "%s %d groom split(s); skipped %d\n", verb, len(result.Reverted), len(result.Skipped))
	for _, item := range result.Reverted {
		fmt.Fprintf(stdout, "  parent %d <- children", item.ParentID)
		for _, childID := range item.ChildIDs {
			fmt.Fprintf(stdout, " %d", childID)
		}
		fmt.Fprintln(stdout)
	}
	for _, item := range result.Skipped {
		fmt.Fprintf(stdout, "  skipped parent %d: %s\n", item.ParentID, item.Reason)
	}
	return 0
}

func runMemoryGroomPropose(home, out string, jsonOut bool, stdout, stderr io.Writer) int {
	var plan groomPlan
	ctx := context.Background()
	now := time.Now().UTC()
	err := withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		// Reuse the exact vault build path: the same active-confirmed lister and the
		// same snapshot_hash the export/import staleness guard uses.
		notes, _, snapshotHash, _, err := buildVault(ctx, store, "")
		if err != nil {
			return err
		}
		cands := make([]memory.GroomCandidate, 0, len(notes))
		for _, n := range notes {
			cands = append(cands, memory.GroomCandidate{
				ID:               n.memRecord.ID,
				Key:              n.memRecord.Key,
				Content:          n.memRecord.Content,
				Provenance:       n.memRecord.Provenance,
				OwnerKind:        n.memRecord.OwnerKind,
				OwnerRef:         n.memRecord.OwnerRef,
				OwnerVersion:     n.memRecord.OwnerVersion,
				Repo:             n.memRecord.Repo,
				Scope:            n.memRecord.Scope,
				FirstConfirmedAt: n.memRecord.CreatedAt,
				UpdatedAt:        n.memRecord.UpdatedAt,
			})
		}
		proposal := memory.DetectGroomActions(cands)

		// #804 rekey + cross-pool detectors run over the candidates that SURVIVE
		// the plain retirement proposals, so one plan never proposes to both
		// retire a row and keep/promote it. Cross-pool additionally excludes
		// rows a rekey group retires.
		retiring := make(map[int64]bool, len(proposal.Retirements))
		for _, r := range proposal.Retirements {
			retiring[r.ID] = true
		}
		surviving := make([]memory.GroomCandidate, 0, len(cands))
		for _, c := range cands {
			if !retiring[c.ID] {
				surviving = append(surviving, c)
			}
		}
		budget := newGroomLLMBudget(settings.GroomLLMTotalMaxPerRun)
		quality, _, err := runMemoryGroomQuality(ctx, store, settings, surviving, now, false, false, budget)
		if err != nil {
			return err
		}
		qualityRetiring := make(map[int64]bool)
		for _, entry := range quality.Candidates {
			if entry.Action == "auto_retire" || entry.Action == "would_retire" {
				qualityRetiring[entry.Candidate.ID] = true
			}
		}
		postQuality := make([]memory.GroomCandidate, 0, len(surviving)-len(qualityRetiring))
		for _, candidate := range surviving {
			if !qualityRetiring[candidate.ID] {
				postQuality = append(postQuality, candidate)
			}
		}
		stale := make([]groomStaleEntry, 0)
		if settings.GroomStale {
			stale, _, err = runMemoryGroomStale(ctx, store, settings, postQuality, now, false, false, budget)
			if err != nil {
				return err
			}
		}
		rekeys := memory.DetectGroomRekeys(postQuality)
		rekeyRetiring := make(map[int64]bool)
		for _, rk := range rekeys {
			for _, r := range rk.Retire {
				rekeyRetiring[r.ID] = true
			}
		}
		crossCands := make([]memory.GroomCandidate, 0, len(postQuality))
		for _, c := range postQuality {
			if !rekeyRetiring[c.ID] {
				crossCands = append(crossCands, c)
			}
		}
		signals, err := store.ListCrossPoolSharedMatches(ctx)
		if err != nil {
			return err
		}
		memSignals := make([]memory.GroomCrossPoolSignal, 0, len(signals))
		for _, sig := range signals {
			memSignals = append(memSignals, memory.GroomCrossPoolSignal{
				PrivateID: sig.PrivateID, SharedID: sig.SharedID, Score: sig.Score, Linked: sig.Linked,
			})
		}
		crossPool := memory.DetectCrossPoolStaleness(crossCands, memSignals)

		proposal.Stats.Rekeys = len(rekeys)
		proposal.Stats.CrossPool = len(crossPool)
		plan = groomPlan{
			SchemaVersion:       groomSchemaVersion,
			SnapshotHash:        snapshotHash,
			ProposedRetirements: make([]groomPlanRetirement, 0, len(proposal.Retirements)),
			RewriteFlags:        make([]groomPlanRewriteFlag, 0, len(proposal.RewriteFlags)),
			Rekeys:              make([]groomPlanRekey, 0, len(rekeys)),
			CrossPool:           make([]groomPlanCrossPool, 0, len(crossPool)),
			Quality:             quality,
			Stale:               stale,
			Stats:               proposal.Stats,
		}
		for _, r := range proposal.Retirements {
			plan.ProposedRetirements = append(plan.ProposedRetirements, groomPlanRetirement{
				ID: r.ID, Key: r.Key, Reason: r.Reason, FirstLine: r.FirstLine,
				Owner: r.Owner, Repo: r.Repo, Scope: r.Scope,
			})
		}
		for _, f := range proposal.RewriteFlags {
			plan.RewriteFlags = append(plan.RewriteFlags, groomPlanRewriteFlag{ID: f.ID, Key: f.Key, Chars: f.Chars})
		}
		for _, rk := range rekeys {
			entry := groomPlanRekey{
				KeepID: rk.KeepID, KeepKey: rk.KeepKey, NewKey: rk.NewKey,
				Retire: make([]groomPlanRekeyRetire, 0, len(rk.Retire)),
				Owner:  rk.Owner, Repo: rk.Repo, Scope: rk.Scope, FirstLine: rk.FirstLine,
			}
			for _, r := range rk.Retire {
				entry.Retire = append(entry.Retire, groomPlanRekeyRetire{ID: r.ID, Key: r.Key, FirstLine: r.FirstLine})
			}
			plan.Rekeys = append(plan.Rekeys, entry)
		}
		for _, cp := range crossPool {
			plan.CrossPool = append(plan.CrossPool, groomPlanCrossPool{
				PrivateID: cp.PrivateID, PrivateKey: cp.PrivateKey, Owner: cp.Owner,
				SharedID: cp.SharedID, SharedKey: cp.SharedKey, Basis: cp.Basis,
				Repo: cp.Repo, Scope: cp.Scope, FirstLine: cp.FirstLine,
			})
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory groom: %v\n", err)
		return 1
	}

	outPath := out
	if outPath == "" {
		paths, err := pathsFromFlag(home)
		if err != nil {
			fmt.Fprintf(stderr, "memory groom: %v\n", err)
			return 1
		}
		// A snapshot-derived filename is deterministic: the same store state always
		// writes the same plan path (nightly re-runs of an unchanged store overwrite
		// in place rather than piling up plans).
		short := plan.SnapshotHash
		if len(short) > 12 {
			short = short[:12]
		}
		outPath = filepath.Join(paths.Evals, "groom", "groom-"+short+".json")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "memory groom: create plan dir: %v\n", err)
		return 1
	}
	if err := writeJSONFile(outPath, plan); err != nil {
		fmt.Fprintf(stderr, "memory groom: write plan: %v\n", err)
		return 1
	}

	if jsonOut {
		summary := struct {
			groomPlan
			Out string `json:"out"`
		}{groomPlan: plan, Out: outPath}
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "memory groom: %v\n", err)
			return 1
		}
		return 0
	}
	printGroomProposal(stdout, plan, outPath)
	return 0
}

func runMemoryGroomApply(home, planPath string, jsonOut bool, stdout, stderr io.Writer) int {
	if planPath == "" {
		fmt.Fprintln(stderr, "memory groom: --yes requires --plan <file>")
		printMemoryGroomUsage(stderr)
		return 2
	}
	plan, err := readGroomPlan(planPath)
	if err != nil {
		fmt.Fprintf(stderr, "memory groom: %v\n", err)
		return 1
	}

	result := groomApplyResult{Plan: planPath}
	err = withStore(home, func(store *db.Store) error {
		ctx := context.Background()
		// Recompute the CURRENT snapshot_hash and abort if the store moved since the
		// plan was proposed — a vault edit in the propose→apply window would make the
		// plan target the wrong revisions (the gpt-5.5 race catch).
		_, _, freshHash, _, err := buildVault(ctx, store, "")
		if err != nil {
			return err
		}
		result.SnapshotHash = freshHash
		if freshHash != plan.SnapshotHash {
			return fmt.Errorf("plan is stale: the memory store changed since it was proposed (plan snapshot %s, current %s); re-run `gitmoot memory groom --propose` and re-review", plan.SnapshotHash, freshHash)
		}
		items := make([]db.GroomRetire, 0, len(plan.ProposedRetirements))
		for _, r := range plan.ProposedRetirements {
			items = append(items, db.GroomRetire{ID: r.ID, Reason: "groom:" + r.Reason})
		}
		rekeys := make([]db.GroomRekeyItem, 0, len(plan.Rekeys))
		for _, rk := range plan.Rekeys {
			item := db.GroomRekeyItem{KeepID: rk.KeepID, NewKey: rk.NewKey, Reason: memory.GroomReasonRekeySuperseded}
			for _, r := range rk.Retire {
				item.RetireIDs = append(item.RetireIDs, r.ID)
			}
			rekeys = append(rekeys, item)
		}
		crossPool := make([]db.GroomCrossPoolItem, 0, len(plan.CrossPool))
		for _, cp := range plan.CrossPool {
			crossPool = append(crossPool, db.GroomCrossPoolItem{
				PrivateID: cp.PrivateID, SharedID: cp.SharedID, Reason: memory.GroomReasonCrossPoolStale,
			})
		}
		res, err := store.ApplyGroomPlan(ctx, items, rekeys, crossPool)
		if err != nil {
			return err
		}
		result.Retired = res.Retired
		result.Skipped = res.RetireSkipped
		result.Rekeyed = res.Rekeyed
		result.RekeySkipped = res.RekeySkipped
		result.Promoted = res.Promoted
		result.CrossPoolSkipped = res.CrossPoolSkipped
		result.Applied = true
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory groom: %v\n", err)
		return 1
	}

	if jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory groom: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "applied groom plan %s (snapshot %s)\n", result.Plan, result.SnapshotHash)
	fmt.Fprintf(stdout, "  retired %d memory(ies), skipped %d (already retired or missing)\n", len(result.Retired), len(result.Skipped))
	if len(result.Rekeyed)+len(result.RekeySkipped) > 0 {
		fmt.Fprintf(stdout, "  rekeyed %d memory(ies) to stable keys, skipped %d group(s)\n", len(result.Rekeyed), len(result.RekeySkipped))
	}
	if len(result.Promoted)+len(result.CrossPoolSkipped) > 0 {
		fmt.Fprintf(stdout, "  promoted %d private memory(ies) over stale shared editions, skipped %d pair(s)\n", len(result.Promoted), len(result.CrossPoolSkipped))
	}
	return 0
}

// readGroomPlan reads and validates a plan artifact.
func readGroomPlan(path string) (groomPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return groomPlan{}, fmt.Errorf("read plan %s: %w", path, err)
	}
	var plan groomPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return groomPlan{}, fmt.Errorf("%s is not a valid groom plan: %w", path, err)
	}
	if plan.SchemaVersion != groomSchemaVersion {
		return groomPlan{}, fmt.Errorf("groom plan schema v%d is not supported by this build (expected v%d); re-run `gitmoot memory groom --propose`", plan.SchemaVersion, groomSchemaVersion)
	}
	if plan.SnapshotHash == "" {
		return groomPlan{}, fmt.Errorf("%s has no snapshot_hash; re-run `gitmoot memory groom --propose`", path)
	}
	return plan, nil
}

// groomScopeLabel renders a retirement's owner/repo/scope as a compact,
// trailing-space-terminated prefix (e.g. "agent:lead repo:acme/widget ") so the
// owner can tell a same-scope duplicate from a cross-scope keeper at a glance.
// Returns "" when no scope fields are present (older plans), keeping output tidy.
func groomScopeLabel(r groomPlanRetirement) string {
	parts := make([]string, 0, 3)
	if r.Owner != "" {
		parts = append(parts, r.Owner)
	}
	switch {
	case r.Repo != "":
		parts = append(parts, "repo:"+r.Repo)
	case r.Scope != "":
		parts = append(parts, r.Scope)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

func printGroomProposal(w io.Writer, plan groomPlan, outPath string) {
	fmt.Fprintf(w, "groom proposal (snapshot %s)\n", plan.SnapshotHash)
	fmt.Fprintf(w, "  %d of %d memory(ies) proposed for retirement; %d flagged for rewrite; %d rekey group(s); %d cross-pool pair(s); %d quality candidate(s) (shadow=%t); %d stale status candidate(s)\n",
		plan.Stats.ProposedRetirements, plan.Stats.TotalMemories, plan.Stats.RewriteFlags, plan.Stats.Rekeys, plan.Stats.CrossPool, len(plan.Quality.Candidates), plan.Quality.Shadow, len(plan.Stale))
	if len(plan.Stats.ByReason) > 0 {
		reasons := make([]string, 0, len(plan.Stats.ByReason))
		for r := range plan.Stats.ByReason {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		fmt.Fprint(w, "  by reason:")
		for _, r := range reasons {
			fmt.Fprintf(w, " %s=%d", r, plan.Stats.ByReason[r])
		}
		fmt.Fprintln(w)
	}
	if len(plan.ProposedRetirements) > 0 {
		fmt.Fprintln(w, "\nProposed retirements:")
		for _, r := range plan.ProposedRetirements {
			fmt.Fprintf(w, "  - memory %d [%s] (%s) %s%s\n", r.ID, r.Key, r.Reason, groomScopeLabel(r), r.FirstLine)
		}
	}
	if len(plan.Rekeys) > 0 {
		fmt.Fprintln(w, "\nProposed legacy-key rekeys:")
		for _, rk := range plan.Rekeys {
			fmt.Fprintf(w, "  - memory %d [%s -> %s] %s%s\n", rk.KeepID, rk.KeepKey, rk.NewKey,
				groomScopeLabel(groomPlanRetirement{Owner: rk.Owner, Repo: rk.Repo, Scope: rk.Scope}), rk.FirstLine)
			for _, r := range rk.Retire {
				fmt.Fprintf(w, "      retire %d [%s] %s\n", r.ID, r.Key, r.FirstLine)
			}
		}
	}
	if len(plan.CrossPool) > 0 {
		fmt.Fprintln(w, "\nProposed cross-pool promote-and-retire pairs:")
		for _, cp := range plan.CrossPool {
			fmt.Fprintf(w, "  - promote %d [%s] (%s, %s) over stale shared %d [%s] %s\n",
				cp.PrivateID, cp.PrivateKey, cp.Owner, cp.Basis, cp.SharedID, cp.SharedKey, cp.FirstLine)
		}
	}
	if len(plan.RewriteFlags) > 0 {
		fmt.Fprintln(w, "\nFlagged for rewrite (NOT retired — owner review):")
		for _, f := range plan.RewriteFlags {
			fmt.Fprintf(w, "  - memory %d [%s] %d chars\n", f.ID, f.Key, f.Chars)
		}
	}
	if len(plan.Quality.Candidates) > 0 {
		fmt.Fprintf(w, "\nQuality candidates (shadow=%t):\n", plan.Quality.Shadow)
		for _, entry := range plan.Quality.Candidates {
			fmt.Fprintf(w, "  - memory %d [%s] score=%d signals=%s verdict=%s confidence=%.2f action=%s cached=%t",
				entry.Candidate.ID, entry.Candidate.Key, entry.Candidate.Score, strings.Join(entry.Candidate.SignalFamilies, ","),
				entry.Verdict, entry.Confidence, entry.Action, entry.Cached)
			if entry.Residue != "" {
				fmt.Fprintf(w, " residue=%q", entry.Residue)
			}
			if entry.FallbackReason != "" {
				fmt.Fprintf(w, " fallback=%q", entry.FallbackReason)
			}
			fmt.Fprintln(w)
		}
	}
	if len(plan.Stale) > 0 {
		fmt.Fprintln(w, "\nStale operational-status candidates:")
		for _, entry := range plan.Stale {
			fmt.Fprintf(w, "  - memory %d [%s] newest=%s verdict=%s action=%s cached=%t",
				entry.Candidate.ID, entry.Candidate.Key, entry.Candidate.NewestDate, entry.Verdict, entry.Action, entry.Cached)
			if entry.Residue != "" {
				fmt.Fprintf(w, " residue=%q", entry.Residue)
			}
			if entry.FallbackReason != "" {
				fmt.Fprintf(w, " fallback=%q", entry.FallbackReason)
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintf(w, "\nplan written to %s\n", outPath)
	if plan.Stats.ProposedRetirements == 0 && plan.Stats.Rekeys == 0 && plan.Stats.CrossPool == 0 && len(plan.Quality.Candidates) == 0 && len(plan.Stale) == 0 {
		fmt.Fprintln(w, "nothing to do.")
		return
	}
	fmt.Fprintf(w, "review it, then apply with: gitmoot memory groom --yes --plan %s\n", outPath)
}
