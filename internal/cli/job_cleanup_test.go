package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestJobCleanupListAndReopen(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	path := managedReclaimTestPath(t, home, "operator-reopen")
	for attempt := 0; attempt < delegationCleanupRetryBudget; attempt++ {
		if _, err := store.RecordCleanupObligationFailure(context.Background(), "operator-job", path, db.CleanupReasonUnknown, errors.New("stuck"), now, now.Add(time.Minute), delegationCleanupRetryBudget); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runJobCleanup([]string{"list", "--home", home, "--state", "quarantined", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("list code=%d stderr=%s", code, stderr.String())
	}
	// Decoded through the stored shape plus the CLI key: a list that still
	// emitted owner_job_id would fill the embedded OwnerJobID (#2601).
	var listed []struct {
		db.CleanupObligation
		OwnerRunID string `json:"owner_run_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].OwnerRunID != "operator-job" || listed[0].OwnerJobID != "" || listed[0].ExpectedPath != path {
		t.Fatalf("listed obligations = %+v, want owner_run_id and no owner_job_id\n%s", listed, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runJobCleanup([]string{"reopen", "--home", home, listed[0].ResourceID}, &stdout, &stderr); code != 0 {
		t.Fatalf("reopen code=%d stderr=%s", code, stderr.String())
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	reopened, err := store.GetCleanupObligation(context.Background(), listed[0].ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != db.CleanupObligationPending || reopened.AttemptCount != 0 {
		t.Fatalf("reopened obligation = %+v", reopened)
	}
}
