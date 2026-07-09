package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestMemoryLinksBackfillDryRunAndIdempotent(t *testing.T) {
	home, store := memoryTestHome(t)
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	ids := []int64{
		seedConfirmed(t, store, owner, "acme/widget", "repo", "alpha", "aurora quartz vector hnsw planner spill runbook"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "beta", "aurora quartz vector hnsw planner calibration checklist"),
		seedConfirmed(t, store, owner, "acme/widget", "repo", "gamma", "aurora quartz vector hnsw planner spill calibration"),
	}
	clearMemoryLinks(t, home)
	if got := memoryLinksForIDs(t, store, ids); got != 0 {
		t.Fatalf("test setup failed: links remain after clear: %d", got)
	}

	code, out, errOut := runMemoryCapture(t, "links", "backfill", "--home", home, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dry-run backfill exit %d: %s", code, errOut)
	}
	var dry memoryLinksBackfillResult
	if err := json.Unmarshal([]byte(out), &dry); err != nil {
		t.Fatalf("parse dry-run: %v (%s)", err, out)
	}
	if !dry.DryRun || dry.Created == 0 || len(dry.Links) != dry.Created {
		t.Fatalf("dry-run summary wrong: %+v", dry)
	}
	if got := memoryLinksForIDs(t, store, ids); got != 0 {
		t.Fatalf("dry-run wrote %d link(s)", got)
	}

	code, out, errOut = runMemoryCapture(t, "links", "backfill", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("backfill exit %d: %s", code, errOut)
	}
	var first memoryLinksBackfillResult
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("parse first backfill: %v (%s)", err, out)
	}
	if first.Created == 0 {
		t.Fatalf("first backfill created no links: %+v", first)
	}
	if got := memoryLinksForIDs(t, store, ids); got != first.Created {
		t.Fatalf("stored links = %d, want created count %d", got, first.Created)
	}

	code, out, errOut = runMemoryCapture(t, "links", "backfill", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("second backfill exit %d: %s", code, errOut)
	}
	var second memoryLinksBackfillResult
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("parse second backfill: %v (%s)", err, out)
	}
	if second.Created != 0 || second.SkippedExisting == 0 {
		t.Fatalf("second backfill should be idempotent, got %+v", second)
	}

	code, out, errOut = runMemoryCapture(t, "links", "list", strconv.FormatInt(ids[0], 10), "--home", home, "--json")
	if code != 0 {
		t.Fatalf("links list exit %d: %s", code, errOut)
	}
	var listed memoryLinksListResult
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("parse links list: %v (%s)", err, out)
	}
	if listed.ID != ids[0] || len(listed.Links) == 0 {
		t.Fatalf("links list missing persisted links: %+v", listed)
	}
	for _, l := range listed.Links {
		if l.SrcID != ids[0] || l.DstID == ids[0] || l.Score <= 0 {
			t.Fatalf("bad listed link: %+v", l)
		}
	}
}

func clearMemoryLinks(t *testing.T, home string) {
	t.Helper()
	raw, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), `DELETE FROM memory_links`); err != nil {
		t.Fatalf("clear memory links: %v", err)
	}
}

func memoryLinksForIDs(t *testing.T, store *db.Store, ids []int64) int {
	t.Helper()
	total := 0
	for _, id := range ids {
		links, err := store.ListMemoryLinks(context.Background(), id)
		if err != nil {
			t.Fatalf("list links %d: %v", id, err)
		}
		total += len(links)
	}
	return total
}
