package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

type memoryLinkEntry struct {
	SrcID     int64   `json:"src_id"`
	DstID     int64   `json:"dst_id"`
	DstKey    string  `json:"dst_key"`
	Score     float64 `json:"score"`
	Origin    string  `json:"origin"`
	CreatedAt string  `json:"created_at,omitempty"`
}

type memoryLinksBackfillResult struct {
	DryRun          bool              `json:"dry_run"`
	Scanned         int               `json:"scanned"`
	Created         int               `json:"created"`
	Skipped         int               `json:"skipped"`
	SkippedExisting int               `json:"skipped_existing"`
	SkippedWeak     int               `json:"skipped_weak"`
	Links           []memoryLinkEntry `json:"links,omitempty"`
}

type memoryLinksListResult struct {
	ID    int64             `json:"id"`
	Links []memoryLinkEntry `json:"links"`
}

func runMemoryLinks(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printMemoryLinksUsage(stdout)
		return 0
	}
	switch args[0] {
	case "backfill":
		return runMemoryLinksBackfill(args[1:], stdout, stderr)
	case "list":
		return runMemoryLinksList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown memory links command %q\n\n", args[0])
		printMemoryLinksUsage(stderr)
		return 2
	}
}

func printMemoryLinksUsage(w io.Writer) {
	fmt.Fprintln(w, "Inspect and backfill persisted memory links.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot memory links backfill [--dry-run] [--json]")
	fmt.Fprintln(w, "  gitmoot memory links list <id> [--json]")
}

func runMemoryLinksBackfill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory links backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	dryRun := fs.Bool("dry-run", false, "print links that would be created without writing")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory links backfill: no positional arguments are accepted")
		return 2
	}

	result := memoryLinksBackfillResult{DryRun: *dryRun}
	err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		rows, err := store.ListConfirmedMemoriesForVault(ctx, "")
		if err != nil {
			return err
		}
		result.Scanned = len(rows)
		for _, r := range rows {
			enriched, err := store.EnrichConfirmedMemoryLinks(ctx, r.ID, *dryRun)
			if err != nil {
				return fmt.Errorf("link memory %d: %w", r.ID, err)
			}
			result.Created += len(enriched.Created)
			result.SkippedExisting += enriched.SkippedExisting
			result.SkippedWeak += enriched.SkippedWeak
			for _, l := range enriched.Created {
				result.Links = append(result.Links, memoryLinkEntryFromDB(l))
			}
		}
		result.Skipped = result.SkippedExisting + result.SkippedWeak
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory links backfill: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory links backfill: %v\n", err)
			return 1
		}
		return 0
	}
	action := "created"
	if *dryRun {
		action = "would create"
	}
	fmt.Fprintf(stdout, "scanned %d confirmed memory(s); %s %d link(s), skipped %d (%d existing, %d weak)\n",
		result.Scanned, action, result.Created, result.Skipped, result.SkippedExisting, result.SkippedWeak)
	for _, l := range result.Links {
		fmt.Fprintf(stdout, "  %d -> %d (%s) score %.6f\n", l.SrcID, l.DstID, l.DstKey, l.Score)
	}
	return 0
}

func runMemoryLinksList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory links list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOut := fs.Bool("json", false, "print as JSON")
	idArg := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		idArg = args[0]
		flagArgs = args[1:]
	}
	if err := parseMemoryFlags(fs, flagArgs); err != nil {
		return memoryFlagExit(err)
	}
	if idArg == "" && fs.NArg() == 1 {
		idArg = fs.Arg(0)
	} else if idArg != "" && fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: gitmoot memory links list <id> [--json]")
		return 2
	}
	if idArg == "" {
		fmt.Fprintln(stderr, "usage: gitmoot memory links list <id> [--json]")
		return 2
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idArg), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(stderr, "memory links list: invalid memory id %q\n", idArg)
		return 2
	}
	result := memoryLinksListResult{ID: id}
	err = withReadOnlyStore(*home, func(store *db.Store) error {
		links, err := store.ListMemoryLinks(context.Background(), id)
		if err != nil {
			return err
		}
		result.Links = make([]memoryLinkEntry, 0, len(links))
		for _, l := range links {
			result.Links = append(result.Links, memoryLinkEntryFromDB(l))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory links list: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory links list: %v\n", err)
			return 1
		}
		return 0
	}
	if len(result.Links) == 0 {
		fmt.Fprintf(stdout, "memory %d has no persisted links\n", id)
		return 0
	}
	for _, l := range result.Links {
		fmt.Fprintf(stdout, "%d -> %d %-24s score %.6f origin %s\n", l.SrcID, l.DstID, l.DstKey, l.Score, l.Origin)
	}
	return 0
}

func memoryLinkEntryFromDB(l db.MemoryLink) memoryLinkEntry {
	return memoryLinkEntry{
		SrcID:     l.SrcID,
		DstID:     l.DstID,
		DstKey:    l.DstKey,
		Score:     l.Score,
		Origin:    l.Origin,
		CreatedAt: l.CreatedAt,
	}
}
