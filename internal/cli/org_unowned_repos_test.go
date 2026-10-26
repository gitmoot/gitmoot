package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestUnownedReposListsTiedAndUnmatchedRecentRepos pins the #2363 diagnostic:
// it lists exactly the recently active repos that routing cannot give a single
// owner, and nothing that routing can.
func TestUnownedReposListsTiedAndUnmatchedRecentRepos(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	const orgConfig = `
[org.roles."owner"]
scope=["*"]
pane="w1:p0"
[org.roles."deimos"]
parent="owner"
scope=["*"]
pane="w1:p1"
[org.roles."jarvis"]
parent="owner"
scope=["*"]
pane="w1:p2"
[org.roles."app"]
parent="jarvis"
scope=["acme/docus"]
pane="w1:p3"
[org.roles."fleet"]
parent="jarvis"
scope=["fleet/*"]
pane="w1:p4"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(orgConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i, repo := range []string{"acme/docus", "fleet/one", "Acme/Unowned", "acme/unowned", "  ", "acme/old"} {
		job := db.Job{ID: "job-" + string(rune('a'+i)), Agent: "worker", Type: "ask", State: string(workflow.JobSucceeded),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: repo})}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ExecForTest(ctx, `UPDATE jobs SET created_at = datetime('now', '-30 days') WHERE id = 'job-f'`); err != nil {
		t.Fatal(err)
	}

	unowned, err := findUnownedRepos(ctx, store, cfg, time.Now().UTC().Add(-orgUnownedReposWindow))
	if err != nil {
		t.Fatal(err)
	}
	if len(unowned) != 1 || unowned[0].Repo != "acme/unowned" || strings.Join(unowned[0].Tied, ",") != "deimos,jarvis" {
		t.Fatalf("unowned = %+v, want only acme/unowned tied between deimos and jarvis", unowned)
	}
	warning := unownedReposWarning(ctx, store, cfg, time.Now().UTC())
	if !strings.Contains(warning, "acme/unowned (tied: deimos, jarvis)") || strings.Contains(warning, "acme/docus") || strings.Contains(warning, "acme/old") {
		t.Fatalf("status warning = %q", warning)
	}
}
