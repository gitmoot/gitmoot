package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

type memoryLogEntry struct {
	ID        int64           `json:"id"`
	At        string          `json:"at"`
	Kind      string          `json:"kind"`
	MemoryID  int64           `json:"memory_id,omitempty"`
	Key       string          `json:"key"`
	OwnerKind string          `json:"owner_kind"`
	OwnerRef  string          `json:"owner_ref"`
	Repo      string          `json:"repo,omitempty"`
	Scope     string          `json:"scope"`
	Actor     string          `json:"actor"`
	Detail    json.RawMessage `json:"detail"`
}

type memoryLogBackfillOutput struct {
	DryRun  bool             `json:"dry_run"`
	Scanned int              `json:"scanned"`
	Created int              `json:"created"`
	Skipped int              `json:"skipped"`
	Events  []memoryLogEntry `json:"events"`
}

func runMemoryLog(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "backfill" {
		return runMemoryLogBackfill(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("memory log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	id := fs.Int64("id", 0, "show the complete biography of one confirmed memory id")
	key := fs.String("key", "", "filter by exact memory key")
	agent := fs.String("agent", "", "filter by owner agent")
	repo := fs.String("repo", "", "filter by exact repository")
	kinds := fs.String("kind", "", "comma-separated event kinds ("+strings.Join(db.MemoryEventKinds, ", ")+")")
	since := fs.String("since", "", "show events from this duration ago (for example 168h)")
	limit := fs.Int("limit", 50, "maximum events to return")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory log: no positional arguments are accepted")
		return 2
	}
	if *id < 0 || *limit <= 0 {
		fmt.Fprintln(stderr, "memory log: --id must be positive and --limit must be greater than zero")
		return 2
	}
	filter := db.MemoryEventFilter{MemoryID: *id, Key: strings.TrimSpace(*key), Agent: strings.TrimSpace(*agent),
		Repo: strings.TrimSpace(*repo), Limit: *limit}
	limitSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			limitSet = true
		}
	})
	if *id > 0 {
		filter.OldestFirst = true
		filter.Limit = 1_000_000
		// --id is a complete biography: reject every feed flag, including an
		// explicit --limit, instead of silently ignoring some of them.
		if filter.Key != "" || filter.Agent != "" || filter.Repo != "" || strings.TrimSpace(*kinds) != "" || strings.TrimSpace(*since) != "" || limitSet {
			fmt.Fprintln(stderr, "memory log: --id cannot be combined with feed filters (--key/--agent/--repo/--kind/--since/--limit)")
			return 2
		}
	}
	if raw := strings.TrimSpace(*kinds); raw != "" {
		filter.Kinds = strings.Split(raw, ",")
		for _, kind := range filter.Kinds {
			if !memoryEventKindKnown(strings.TrimSpace(kind)) {
				fmt.Fprintf(stderr, "memory log: unknown --kind %q (valid: %s)\n", strings.TrimSpace(kind), strings.Join(db.MemoryEventKinds, ", "))
				return 2
			}
		}
	}
	if raw := strings.TrimSpace(*since); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration < 0 {
			fmt.Fprintf(stderr, "memory log: invalid --since duration %q\n", raw)
			return 2
		}
		filter.Since = time.Now().UTC().Add(-duration).Format(time.RFC3339)
	}
	var events []db.MemoryEvent
	err := withReadOnlyStore(*home, func(store *db.Store) error {
		var err error
		events, err = store.ListMemoryEvents(context.Background(), filter)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory log: %v\n", err)
		return 1
	}
	entries := memoryLogEntries(events)
	if *jsonOut {
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "memory log: %v\n", err)
			return 1
		}
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no memory events")
		return 0
	}
	for _, event := range entries {
		memoryID := "-"
		if event.MemoryID > 0 {
			memoryID = strconv.FormatInt(event.MemoryID, 10)
		}
		fmt.Fprintf(stdout, "%s  %-17s memory=%s key=%s actor=%s\n", event.At, event.Kind, memoryID, event.Key, event.Actor)
		if string(event.Detail) != "{}" {
			fmt.Fprintf(stdout, "  %s\n", event.Detail)
		}
	}
	return 0
}

func runMemoryLogBackfill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory log backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	dryRun := fs.Bool("dry-run", false, "show events without writing them")
	jsonOut := fs.Bool("json", false, "print as JSON")
	if err := parseMemoryFlags(fs, args); err != nil {
		return memoryFlagExit(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "memory log backfill: no positional arguments are accepted")
		return 2
	}
	var result db.MemoryEventBackfillResult
	err := withStore(*home, func(store *db.Store) error {
		var err error
		result, err = store.BackfillMemoryEvents(context.Background(), *dryRun)
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory log backfill: %v\n", err)
		return 1
	}
	out := memoryLogBackfillOutput{DryRun: *dryRun, Scanned: result.Scanned, Created: result.Created,
		Skipped: result.Skipped, Events: memoryLogEntries(result.Events)}
	if *jsonOut {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "memory log backfill: %v\n", err)
			return 1
		}
		return 0
	}
	action := "created"
	if *dryRun {
		action = "would create"
	}
	fmt.Fprintf(stdout, "scanned %d confirmed memory(s); %s %d event(s), skipped %d already backfilled\n",
		out.Scanned, action, out.Created, out.Skipped)
	return 0
}

func memoryEventKindKnown(kind string) bool {
	for _, known := range db.MemoryEventKinds {
		if kind == known {
			return true
		}
	}
	return false
}

func memoryLogEntries(events []db.MemoryEvent) []memoryLogEntry {
	out := make([]memoryLogEntry, 0, len(events))
	for _, event := range events {
		detail := json.RawMessage(event.Detail)
		if len(detail) == 0 || !json.Valid(detail) {
			detail = json.RawMessage(`{}`)
		}
		out = append(out, memoryLogEntry{ID: event.ID, At: event.At, Kind: event.Kind,
			MemoryID: event.MemoryID, Key: event.Key, OwnerKind: event.OwnerKind, OwnerRef: event.OwnerRef,
			Repo: event.Repo, Scope: event.Scope, Actor: event.Actor, Detail: detail})
	}
	return out
}
