package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

type memoryRetireResult struct {
	DryRun           bool     `json:"dry_run"`
	ProvenancePrefix string   `json:"provenance_prefix"`
	Agent            string   `json:"agent,omitempty"`
	Selected         int      `json:"selected"`
	Retired          int      `json:"retired"`
	IDs              []int64  `json:"ids,omitempty"`
	Keys             []string `json:"keys,omitempty"`
}

func runMemoryRetire(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory retire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	prefix := fs.String("provenance-prefix", "", "retire active confirmed memories whose provenance starts with this prefix")
	agent := fs.String("agent", "", "filter by owner or author agent name")
	dryRun := fs.Bool("dry-run", false, "report matching rows without writing")
	yes := fs.Bool("yes", false, "actually retire the matching rows")
	jsonOut := fs.Bool("json", false, "print the retire summary as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory retire: accepts no positional arguments")
		return 2
	}
	if strings.TrimSpace(*prefix) == "" {
		fmt.Fprintln(stderr, "memory retire: --provenance-prefix is required")
		return 2
	}
	if *dryRun && *yes {
		fmt.Fprintln(stderr, "memory retire: --dry-run and --yes cannot be combined")
		return 2
	}

	result := memoryRetireResult{
		DryRun:           !*yes,
		ProvenancePrefix: strings.TrimSpace(*prefix),
		Agent:            strings.TrimSpace(*agent),
	}
	collect := func(rows []db.ConfirmedMemory, retired bool) {
		result.Selected = len(rows)
		if retired {
			result.Retired = len(rows)
		}
		for _, row := range rows {
			result.IDs = append(result.IDs, row.ID)
			result.Keys = append(result.Keys, row.Key)
		}
	}

	if result.DryRun {
		err := withReadOnlyStore(*home, func(store *db.Store) error {
			rows, err := store.ListActiveConfirmedMemoriesForRetire(context.Background(), result.ProvenancePrefix, result.Agent)
			if err != nil {
				return err
			}
			collect(rows, false)
			return nil
		})
		if err != nil {
			fmt.Fprintf(stderr, "memory retire: %v\n", err)
			return 1
		}
	} else {
		err := withStore(*home, func(store *db.Store) error {
			rows, err := store.RetireConfirmedMemoriesByProvenancePrefix(context.Background(), result.ProvenancePrefix, result.Agent, "provenance-prefix:"+result.ProvenancePrefix)
			if err != nil {
				return err
			}
			collect(rows, true)
			return nil
		})
		if err != nil {
			fmt.Fprintf(stderr, "memory retire: %v\n", err)
			return 1
		}
	}

	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "memory retire: %v\n", err)
			return 1
		}
		return 0
	}
	if result.Selected == 0 {
		fmt.Fprintln(stdout, "no active confirmed memories match that provenance prefix")
		return 0
	}
	if result.DryRun {
		fmt.Fprintf(stdout, "%d active confirmed memory(s) selected for retirement (dry run):\n", result.Selected)
		for i, id := range result.IDs {
			fmt.Fprintf(stdout, "  %d %s\n", id, result.Keys[i])
		}
		fmt.Fprintln(stdout, "re-run with --yes to retire them")
		return 0
	}
	fmt.Fprintf(stdout, "retired %d confirmed memory(s) by provenance prefix %q\n", result.Retired, result.ProvenancePrefix)
	return 0
}
