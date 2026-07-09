package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/memory"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// runMemory is the entry point for `gitmoot memory` — the read-only audit +
// measurement surface for agent persistent memory (#626). In Phase 1 there is no
// write/forget/promote command yet (those are Phase 2); this command only reads
// the store and runs the offline measurement harness.
func runMemory(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printMemoryUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runMemoryList(args[1:], stdout, stderr)
	case "recall":
		return runMemoryRecall(args[1:], stdout, stderr)
	case "replay":
		return runMemoryReplay(args[1:], stdout, stderr)
	case "eval":
		return runMemoryEval(args[1:], stdout, stderr)
	case "vault":
		return runMemoryVault(args[1:], stdout, stderr)
	case "ingest":
		return runMemoryIngest(args[1:], stdout, stderr)
	case "observations":
		return runMemoryObservations(args[1:], stdout, stderr)
	case "confirm":
		return runMemoryConfirm(args[1:], stdout, stderr)
	case "links":
		return runMemoryLinks(args[1:], stdout, stderr)
	case "groom":
		return runMemoryGroom(args[1:], stdout, stderr)
	case "clusters":
		return runMemoryClusters(args[1:], stdout, stderr)
	case "cluster":
		if len(args) >= 2 && args[1] == "rename" {
			return runMemoryClusterRename(args[2:], stdout, stderr)
		}
		fmt.Fprintln(stderr, "usage: gitmoot memory cluster rename <cluster-id> <label>")
		return 2
	default:
		fmt.Fprintf(stderr, "unknown memory command %q\n\n", args[0])
		printMemoryUsage(stderr)
		return 2
	}
}

func printMemoryUsage(w io.Writer) {
	fmt.Fprintln(w, "Inspect and measure agent persistent memory (#626).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot memory list [--pending|--confirmed] [--agent NAME] [--repo R] [--json]")
	fmt.Fprintln(w, "  gitmoot memory recall \"<query>\" [--repo R] [--agent NAME] [--limit N] [--json]")
	fmt.Fprintln(w, "  gitmoot memory replay [--agent NAME] [--repo R] [--limit N] [--json]")
	fmt.Fprintln(w, "  gitmoot memory eval --fixtures FILE [--k N] [--json]")
	fmt.Fprintln(w, "  gitmoot memory vault export [--out DIR] [--agent NAME] [--json]")
	fmt.Fprintln(w, "  gitmoot memory ingest <path|dir> --agent NAME [--repo R] [--tier repo|general] [--dry-run] [--json]")
	fmt.Fprintln(w, "  gitmoot memory observations [--agent NAME] [--provenance-prefix P] [--json]")
	fmt.Fprintln(w, "  gitmoot memory confirm <obs-id>... | --provenance-prefix P [--agent NAME] [--yes] [--json]")
	fmt.Fprintln(w, "  gitmoot memory links backfill [--dry-run] [--json]")
	fmt.Fprintln(w, "  gitmoot memory links list <id> [--json]")
	fmt.Fprintln(w, "  gitmoot memory groom --propose [--out PLAN.json] [--json] | --yes --plan PLAN.json [--json]")
	fmt.Fprintln(w, "  gitmoot memory clusters [--json]")
	fmt.Fprintln(w, "  gitmoot memory clusters recompute --propose [--out PLAN.json] [--json] | --apply [--plan PLAN.json] [--json]")
	fmt.Fprintln(w, "  gitmoot memory cluster rename <cluster-id> <label>")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  list          show stored memories (confirmed and/or pending observations)")
	fmt.Fprintln(w, "  recall        retrieve ranked confirmed memories; defaults to all agent pools")
	fmt.Fprintln(w, "  replay        offline A/B: render recent real jobs' prompts with vs without the")
	fmt.Fprintln(w, "                learnings block and report the injection delta (tokens, entries)")
	fmt.Fprintln(w, "  eval          recall/precision@K of retrieval over a labeled fixtures file")
	fmt.Fprintln(w, "  vault         render memory as a disposable Obsidian-compatible vault view")
	fmt.Fprintln(w, "  ingest        stage markdown as trust_mark=low pending observations (PreFilter-gated)")
	fmt.Fprintln(w, "  observations  list pending observations, flagging which keys are already confirmed")
	fmt.Fprintln(w, "  confirm       human-gated promotion of pending observations into confirmed memory")
	fmt.Fprintln(w, "  links         inspect persisted memory links; backfill links for existing facts")
	fmt.Fprintln(w, "  groom         deterministically propose stale-memory retirements, apply on confirmation")
	fmt.Fprintln(w, "  clusters      list emergent memory clusters; recompute them via a propose/apply plan")
	fmt.Fprintln(w, "  cluster       rename a cluster (owner label override)")
}

// ---- memory recall --------------------------------------------------------

type memoryRecallOwner struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

type memoryRecallEntry struct {
	ID         int64             `json:"id"`
	Owner      memoryRecallOwner `json:"owner"`
	Repo       string            `json:"repo"`
	Scope      string            `json:"scope"`
	Key        string            `json:"key"`
	Content    string            `json:"content"`
	Provenance string            `json:"provenance"`
	UpdatedAt  string            `json:"updated_at"`
}

func runMemoryRecall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory recall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "filter to one agent owner pool; default searches all agent pools")
	repo := fs.String("repo", "", "filter by repo (owner/repo); omitted searches all repos")
	limit := fs.Int("limit", 15, "maximum number of matching memories to return")
	jsonOut := fs.Bool("json", false, "print as JSON")
	queryText, err := parseMemoryRecallArgs(fs, args)
	if err != nil {
		return memoryFlagExit(err)
	}
	if strings.TrimSpace(queryText) == "" {
		fmt.Fprintln(stderr, "usage: gitmoot memory recall \"<query>\" [--repo owner/repo] [--agent NAME] [--limit N] [--json]")
		return 2
	}
	query := workflow.BuildMemoryMatchQuery(queryText)

	var rows []db.ConfirmedMemory
	err = withReadOnlyStore(*home, func(store *db.Store) error {
		var err error
		ctx := context.Background()
		if strings.TrimSpace(*agent) != "" {
			owner := db.MemoryOwner{Kind: memory.OwnerKindAgent, Ref: strings.TrimSpace(*agent)}
			if strings.TrimSpace(*repo) != "" {
				rows, err = store.QueryConfirmedMemories(ctx, owner, strings.TrimSpace(*repo), query, *limit)
			} else {
				rows, err = store.QueryConfirmedMemoriesForOwnerAllRepos(ctx, owner, query, *limit)
			}
		} else {
			rows, err = store.QueryConfirmedMemoriesForAllAgents(ctx, strings.TrimSpace(*repo), query, *limit)
		}
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory recall: %v\n", err)
		return 1
	}

	if *jsonOut {
		entries := make([]memoryRecallEntry, 0, len(rows))
		for _, r := range rows {
			entries = append(entries, memoryRecallJSONEntry(r))
		}
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "memory recall: %v\n", err)
			return 1
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no matches")
		return 0
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s repo=%s scope=%s owner=%s:%s\n",
			r.Key, memoryRecallDisplayRepo(r), r.Scope, r.Owner.Kind, r.Owner.Ref)
		fmt.Fprintln(stdout, memory.RenderBullet(memory.Entry{
			Scope:     r.Scope,
			Key:       r.Key,
			Content:   r.Content,
			UpdatedAt: r.UpdatedAt,
		}))
	}
	return 0
}

func parseMemoryRecallArgs(fs *flag.FlagSet, args []string) (string, error) {
	var flagArgs []string
	var queryParts []string
	flagsWithValues := map[string]bool{
		"-home": true, "--home": true,
		"-agent": true, "--agent": true,
		"-repo": true, "--repo": true,
		"-limit": true, "--limit": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			queryParts = append(queryParts, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			if strings.Contains(arg, "=") {
				continue
			}
			if flagsWithValues[arg] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		queryParts = append(queryParts, arg)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Join(queryParts, " ")), nil
}

func memoryRecallJSONEntry(r db.ConfirmedMemory) memoryRecallEntry {
	return memoryRecallEntry{
		ID: r.ID,
		Owner: memoryRecallOwner{
			Kind:    r.Owner.Kind,
			Ref:     r.Owner.Ref,
			Version: r.Owner.Version,
		},
		Repo:       r.Repo,
		Scope:      r.Scope,
		Key:        r.Key,
		Content:    r.Content,
		Provenance: r.Provenance,
		UpdatedAt:  r.UpdatedAt,
	}
}

func memoryRecallDisplayRepo(r db.ConfirmedMemory) string {
	if strings.TrimSpace(r.Repo) != "" {
		return r.Repo
	}
	return "(general)"
}

// ---- memory list ----------------------------------------------------------

type memoryListEntry struct {
	Tier       string `json:"tier"` // "confirmed" | "pending"
	ID         int64  `json:"id"`
	OwnerKind  string `json:"owner_kind"`
	OwnerRef   string `json:"owner_ref"`
	Repo       string `json:"repo,omitempty"`
	Scope      string `json:"scope"`
	Key        string `json:"key"`
	Content    string `json:"content"`
	Provenance string `json:"provenance,omitempty"`
	TrustMark  string `json:"trust_mark,omitempty"`
	SourceJob  string `json:"source_job,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

func runMemoryList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "filter by owner agent name")
	repo := fs.String("repo", "", "filter by repo (owner/repo); also includes general-scope facts")
	pending := fs.Bool("pending", false, "show only pending observations")
	confirmed := fs.Bool("confirmed", false, "show only confirmed memories")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	// Default (neither flag) shows both tiers.
	showConfirmed := *confirmed || !*pending
	showPending := *pending || !*confirmed

	var entries []memoryListEntry
	err := withReadOnlyStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if showConfirmed {
			rows, err := store.ListConfirmedMemories(ctx, *agent, *repo)
			if err != nil {
				return err
			}
			for _, r := range rows {
				entries = append(entries, memoryListEntry{
					Tier: "confirmed", ID: r.ID, OwnerKind: r.Owner.Kind, OwnerRef: r.Owner.Ref,
					Repo: r.Repo, Scope: r.Scope, Key: r.Key, Content: r.Content,
					Provenance: r.Provenance, SourceJob: r.SourceJob, UpdatedAt: r.UpdatedAt,
				})
			}
		}
		if showPending {
			rows, err := store.ListMemoryObservations(ctx, *agent, *repo)
			if err != nil {
				return err
			}
			for _, r := range rows {
				entries = append(entries, memoryListEntry{
					Tier: "pending", ID: r.ID, OwnerKind: r.Owner.Kind, OwnerRef: r.Owner.Ref,
					Repo: r.Repo, Scope: r.Scope, Key: r.Key, Content: r.Content,
					Provenance: r.Provenance, TrustMark: r.TrustMark, SourceJob: r.SourceJob, UpdatedAt: r.CreatedAt,
				})
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory list: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "memory list: %v\n", err)
			return 1
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no memories")
		return 0
	}
	for _, e := range entries {
		repo := e.Repo
		if repo == "" {
			repo = "-"
		}
		fmt.Fprintf(stdout, "%-9s %-10s %-14s %-6s %-16s %s\n", e.Tier, e.OwnerRef, repo, e.Scope, e.Key, e.Content)
	}
	return 0
}

// ---- memory replay (offline A/B injection harness) ------------------------

type memoryReplayJob struct {
	JobID         string `json:"job_id"`
	Agent         string `json:"agent"`
	Repo          string `json:"repo"`
	BaseTokens    int    `json:"base_tokens"`
	MemoryTokens  int    `json:"memory_tokens"`
	DeltaTokens   int    `json:"delta_tokens"`
	EntriesInject int    `json:"entries_injected"`
	BlockPreview  string `json:"block_preview,omitempty"`
}

type memoryReplaySummary struct {
	Jobs              int               `json:"jobs"`
	JobsWithInjection int               `json:"jobs_with_injection"`
	TotalDeltaTokens  int               `json:"total_delta_tokens"`
	TotalEntries      int               `json:"total_entries_injected"`
	Details           []memoryReplayJob `json:"details"`
}

func runMemoryReplay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agentFilter := fs.String("agent", "", "only replay jobs for this agent")
	repoFilter := fs.String("repo", "", "only replay jobs for this repo")
	limit := fs.Int("limit", 50, "max number of recent jobs to replay")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}

	var summary memoryReplaySummary
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		controller := &workflow.MemoryController{
			Store:       store,
			TokenBudget: settings.TokenBudget,
			MaxEntries:  settings.MaxEntries,
		}
		jobs, err := store.ListJobs(context.Background())
		if err != nil {
			return err
		}
		// Most-recent-first, capped.
		sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].ID > jobs[j].ID })
		ctx := context.Background()
		count := 0
		for _, job := range jobs {
			if *agentFilter != "" && job.Agent != *agentFilter {
				continue
			}
			payload, perr := daemonJobPayload(job)
			if perr != nil {
				continue
			}
			if *repoFilter != "" && payload.Repo != *repoFilter {
				continue
			}
			base := workflow.RenderBaseJobPrompt(payload, job.Type)
			block, injected, blockTokens := controller.PreviewBlock(ctx, job.Agent, payload.Repo, payload.Instructions)
			detail := memoryReplayJob{
				JobID:         job.ID,
				Agent:         job.Agent,
				Repo:          payload.Repo,
				BaseTokens:    memory.EstimateTokens(base),
				MemoryTokens:  memory.EstimateTokens(base) + blockTokens,
				DeltaTokens:   blockTokens,
				EntriesInject: injected,
			}
			if injected > 0 {
				detail.BlockPreview = block
				summary.JobsWithInjection++
				summary.TotalEntries += injected
				summary.TotalDeltaTokens += blockTokens
			}
			summary.Details = append(summary.Details, detail)
			count++
			if count >= *limit {
				break
			}
		}
		summary.Jobs = count
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory replay: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "memory replay: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "replayed %d job(s); %d with injection; +%d tokens across %d injected entries\n",
		summary.Jobs, summary.JobsWithInjection, summary.TotalDeltaTokens, summary.TotalEntries)
	fmt.Fprintln(stdout, "(Phase 1 measures injection MECHANICS + retrieval quality. Outcome-delta A/B — running")
	fmt.Fprintln(stdout, " real agents twice with and without memory — is the Phase-2 gate, not measured here.)")
	for _, d := range summary.Details {
		if d.EntriesInject == 0 {
			continue
		}
		fmt.Fprintf(stdout, "  %s\t%s\t%s\t+%d tok\t%d entries\n", d.JobID, d.Agent, d.Repo, d.DeltaTokens, d.EntriesInject)
	}
	return 0
}

// ---- memory eval (recall/precision@K over labeled fixtures) ---------------

// memoryEvalFixtures is the on-disk format for labeled retrieval-quality cases:
// each case is a (job → expected memory keys) label, and the harness reports
// recall/precision@K of the sanitized-FTS retrieval against those labels.
type memoryEvalFixtures struct {
	Cases []memoryEvalCase `json:"cases"`
}

type memoryEvalCase struct {
	Agent        string   `json:"agent"`
	Repo         string   `json:"repo"`
	Instructions string   `json:"instructions"`
	ExpectedKeys []string `json:"expected_keys"`
}

type memoryEvalResult struct {
	K             int                    `json:"k"`
	Cases         int                    `json:"cases"`
	MeanRecallAtK float64                `json:"mean_recall_at_k"`
	MeanPrecAtK   float64                `json:"mean_precision_at_k"`
	PerCase       []memoryEvalCaseResult `json:"per_case"`
}

type memoryEvalCaseResult struct {
	Instructions string   `json:"instructions"`
	Expected     []string `json:"expected_keys"`
	Retrieved    []string `json:"retrieved_keys"`
	RecallAtK    float64  `json:"recall_at_k"`
	PrecisionAtK float64  `json:"precision_at_k"`
}

func runMemoryEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	fixturesPath := fs.String("fixtures", "", "path to a labeled fixtures JSON file (required)")
	k := fs.Int("k", 5, "retrieval cutoff K for recall/precision@K")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if *fixturesPath == "" {
		fmt.Fprintln(stderr, "memory eval: --fixtures is required")
		return 2
	}
	fixtures, err := loadMemoryEvalFixtures(*fixturesPath)
	if err != nil {
		fmt.Fprintf(stderr, "memory eval: %v\n", err)
		return 1
	}

	result := memoryEvalResult{K: *k, Cases: len(fixtures.Cases)}
	err = withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		settings, err := config.LoadMemorySettings(paths)
		if err != nil {
			return err
		}
		controller := &workflow.MemoryController{Store: store, TokenBudget: settings.TokenBudget, MaxEntries: settings.MaxEntries}
		ctx := context.Background()
		var sumRecall, sumPrec float64
		for _, c := range fixtures.Cases {
			entries := controller.PreviewEntries(ctx, c.Agent, c.Repo, c.Instructions, *k)
			retrieved := make([]string, 0, len(entries))
			for _, e := range entries {
				retrieved = append(retrieved, e.Key)
			}
			recall, prec := recallPrecisionAtK(retrieved, c.ExpectedKeys, *k)
			sumRecall += recall
			sumPrec += prec
			result.PerCase = append(result.PerCase, memoryEvalCaseResult{
				Instructions: c.Instructions, Expected: c.ExpectedKeys, Retrieved: retrieved,
				RecallAtK: recall, PrecisionAtK: prec,
			})
		}
		if len(fixtures.Cases) > 0 {
			result.MeanRecallAtK = sumRecall / float64(len(fixtures.Cases))
			result.MeanPrecAtK = sumPrec / float64(len(fixtures.Cases))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory eval: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory eval: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "cases=%d K=%d recall@K=%.3f precision@K=%.3f\n", result.Cases, result.K, result.MeanRecallAtK, result.MeanPrecAtK)
	for _, c := range result.PerCase {
		fmt.Fprintf(stdout, "  recall=%.2f prec=%.2f  %q -> %v (expected %v)\n", c.RecallAtK, c.PrecisionAtK, c.Instructions, c.Retrieved, c.Expected)
	}
	return 0
}

func loadMemoryEvalFixtures(path string) (memoryEvalFixtures, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return memoryEvalFixtures{}, err
	}
	var fixtures memoryEvalFixtures
	if err := json.Unmarshal(data, &fixtures); err != nil {
		return memoryEvalFixtures{}, fmt.Errorf("parse fixtures: %w", err)
	}
	return fixtures, nil
}

// recallPrecisionAtK computes recall@K and precision@K of retrieved keys against
// the labeled expected set. Only the top-K retrieved keys are scored. Recall is
// (relevant retrieved)/(relevant total); precision is (relevant retrieved)/(K
// or fewer actually retrieved). A case with no expected keys scores recall 1.0
// (nothing to miss). An empty retrieval earns precision credit only when the
// expected set is also empty (a genuinely correct null retrieval); when keys
// were expected but nothing was retrieved, precision is 0 so a null/empty
// retriever cannot masquerade as perfect precision in the gating harness.
func recallPrecisionAtK(retrieved, expected []string, k int) (recall, precision float64) {
	if k > 0 && len(retrieved) > k {
		retrieved = retrieved[:k]
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e] = struct{}{}
	}
	hits := 0
	for _, r := range retrieved {
		if _, ok := expectedSet[r]; ok {
			hits++
		}
	}
	if len(expected) == 0 {
		recall = 1.0
	} else {
		recall = float64(hits) / float64(len(expected))
	}
	if len(retrieved) == 0 {
		// A null retrieval only deserves precision credit when there was
		// nothing relevant to retrieve; otherwise it earns none (0/0 -> 0),
		// so an empty retriever cannot show "perfect" precision.
		if len(expected) == 0 {
			precision = 1.0
		} else {
			precision = 0.0
		}
	} else {
		precision = float64(hits) / float64(len(retrieved))
	}
	return recall, precision
}

func parseMemoryFlags(fs *flag.FlagSet, args []string) error {
	return fs.Parse(args)
}

func memoryFlagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
