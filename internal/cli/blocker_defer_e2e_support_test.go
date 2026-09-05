package cli

import (
	"context"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"io"
	"strings"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

// blockerE2EHome initializes an isolated gitmoot home (no GITMOOT_HOME / live
// home reads) and opens its store.
func blockerE2EHome(t *testing.T) (*db.Store, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store, home
}

// blockerE2EWorker builds a jobWorker on the isolated home with only the
// checkout resolution stubbed (checkout state is not under test); the adapter is
// the REAL ShellAdapter built by the default factory from the agent's runtime.
func blockerE2EWorker(store *db.Store, home string, checkout string) jobWorker {
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(_ context.Context, _ db.Job, payload workflow.JobPayload, _ runtime.Agent) (string, error) {
		if isolated := strings.TrimSpace(payload.WorktreePath); payload.ReadOnlySeat && isolated != "" {
			return isolated, nil
		}
		return checkout, nil
	}
	return worker
}

func blockerE2EJobPayload(t *testing.T, store *db.Store, jobID string) (db.Job, workflow.JobPayload) {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	return job, payload
}

func blockerE2EHasEventKind(t *testing.T, store *db.Store, jobID string, kind string) bool {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}
