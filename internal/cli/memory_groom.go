package cli

import (
	"context"
	"encoding/json"
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

// memory groom (#737 P4.2) — the deterministic grooming pass as an explicit,
// human-gated propose→apply round-trip over confirmed memory.
//
// `groom --propose` reads every ACTIVE confirmed memory (the vault lister path,
// retired rows excluded), computes the CURRENT vault snapshot_hash exactly as
// `vault export` does, runs the deterministic detectors (internal/memory), and
// writes a reviewable plan artifact — it touches nothing in the store.
//
// `groom --yes --plan <file>` recomputes the snapshot_hash, ABORTS AS STALE if it
// differs from the plan's (a vault edit between propose and apply invalidates the
// plan), and applies the plan's retirements in ONE transaction. P4.2 is
// retire-only: no content is edited or rewritten; over-long "bricks" are only
// FLAGGED for a later owner/LLM pass (P4.3).

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

// groomPlan is the reviewable artifact `--propose` writes and `--yes --plan` reads.
type groomPlan struct {
	SchemaVersion       int                    `json:"schema_version"`
	SnapshotHash        string                 `json:"snapshot_hash"`
	ProposedRetirements []groomPlanRetirement  `json:"proposed_retirements"`
	RewriteFlags        []groomPlanRewriteFlag `json:"rewrite_flags"`
	Rekeys              []groomPlanRekey       `json:"rekeys"`
	CrossPool           []groomPlanCrossPool   `json:"cross_pool"`
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

func runMemoryGroom(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory groom", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	propose := fs.Bool("propose", false, "read confirmed memory, run the detectors, and write a reviewable plan (writes nothing to the store)")
	yes := fs.Bool("yes", false, "apply a plan's retirements (requires --plan)")
	plan := fs.String("plan", "", "path to a plan artifact produced by --propose (required with --yes)")
	out := fs.String("out", "", "where --propose writes the plan (default: <home>/evals/groom/groom-<snapshot>.json)")
	jsonOut := fs.Bool("json", false, "print the summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if *propose == *yes {
		fmt.Fprintln(stderr, "memory groom: pass exactly one of --propose or --yes")
		printMemoryGroomUsage(stderr)
		return 2
	}
	if *propose {
		return runMemoryGroomPropose(*home, *out, *jsonOut, stdout, stderr)
	}
	return runMemoryGroomApply(*home, *plan, *jsonOut, stdout, stderr)
}

func printMemoryGroomUsage(w io.Writer) {
	fmt.Fprintln(w, "Deterministically groom confirmed memory (#737 P4.2): propose retirements, apply on confirmation.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot memory groom --propose [--out PLAN.json] [--json]")
	fmt.Fprintln(w, "  gitmoot memory groom --yes --plan PLAN.json [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  --propose  read active confirmed memory, run the deterministic detectors")
	fmt.Fprintln(w, "             (status/changelog/ToC snapshots, bare to-do lists, exact duplicates,")
	fmt.Fprintln(w, "             legacy-key rekeys, cross-pool stale shared editions; over-long bricks")
	fmt.Fprintln(w, "             are FLAGGED, not retired) and write a reviewable plan.")
	fmt.Fprintln(w, "             The store is not touched.")
	fmt.Fprintln(w, "  --yes      apply a plan's actions in one transaction: retirements, legacy-key")
	fmt.Fprintln(w, "             rekeys, and cross-pool promote-and-retire pairs. Recomputes the vault")
	fmt.Fprintln(w, "             snapshot and ABORTS AS STALE if the store changed since --propose.")
	fmt.Fprintln(w, "             Content is never edited or rewritten.")
}

func runMemoryGroomPropose(home, out string, jsonOut bool, stdout, stderr io.Writer) int {
	var plan groomPlan
	err := withReadOnlyStore(home, func(store *db.Store) error {
		// Reuse the exact vault build path: the same active-confirmed lister and the
		// same snapshot_hash the export/import staleness guard uses.
		notes, _, snapshotHash, _, err := buildVault(context.Background(), store, "")
		if err != nil {
			return err
		}
		cands := make([]memory.GroomCandidate, 0, len(notes))
		for _, n := range notes {
			cands = append(cands, memory.GroomCandidate{
				ID:           n.memRecord.ID,
				Key:          n.memRecord.Key,
				Content:      n.memRecord.Content,
				OwnerKind:    n.memRecord.OwnerKind,
				OwnerRef:     n.memRecord.OwnerRef,
				OwnerVersion: n.memRecord.OwnerVersion,
				Repo:         n.memRecord.Repo,
				Scope:        n.memRecord.Scope,
				UpdatedAt:    n.memRecord.UpdatedAt,
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
		rekeys := memory.DetectGroomRekeys(surviving)
		rekeyRetiring := make(map[int64]bool)
		for _, rk := range rekeys {
			for _, r := range rk.Retire {
				rekeyRetiring[r.ID] = true
			}
		}
		crossCands := make([]memory.GroomCandidate, 0, len(surviving))
		for _, c := range surviving {
			if !rekeyRetiring[c.ID] {
				crossCands = append(crossCands, c)
			}
		}
		signals, err := store.ListCrossPoolSharedMatches(context.Background())
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
	fmt.Fprintf(w, "  %d of %d memory(ies) proposed for retirement; %d flagged for rewrite; %d rekey group(s); %d cross-pool pair(s)\n",
		plan.Stats.ProposedRetirements, plan.Stats.TotalMemories, plan.Stats.RewriteFlags, plan.Stats.Rekeys, plan.Stats.CrossPool)
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
	fmt.Fprintf(w, "\nplan written to %s\n", outPath)
	if plan.Stats.ProposedRetirements == 0 && plan.Stats.Rekeys == 0 && plan.Stats.CrossPool == 0 {
		fmt.Fprintln(w, "nothing to do.")
		return
	}
	fmt.Fprintf(w, "review it, then apply with: gitmoot memory groom --yes --plan %s\n", outPath)
}
