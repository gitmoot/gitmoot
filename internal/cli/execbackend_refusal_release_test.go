package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// provisionRefusalAttemptState drives a provision that fails with the supplied
// error and returns the ledger state the attempt was left in.
func provisionRefusalAttemptState(t *testing.T, jobID string, provisionErr error) string {
	t.Helper()
	store := openExecBackendLedgerTestStore(t)
	inner := &ledgerTestBackend{provision: func(execbackend.JobScope) (*execbackend.Instance, error) {
		return nil, provisionErr
	}}
	backend := newExecBackendLedgerForTest(t, store, inner, &bytes.Buffer{}, "fencing-token", "boot-id")

	key := db.ExecBackendAttemptKey{JobID: jobID, Attempt: 1, LifecycleGeneration: 0}
	if _, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: jobID, Attempt: 1}); err == nil {
		t.Fatal("Provision succeeded despite the inner backend failing")
	} else if !strings.Contains(err.Error(), "sandbox") && !strings.Contains(err.Error(), "E2B") && !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %v, want the provider failure preserved", err)
	}
	return execBackendAttemptForTest(t, store, key).State
}

// TestProviderRefusalReleasesTheReservation is the regression for the leak that
// took the remote backend offline after one malformed request.
//
// An HTTP 400 is authoritative proof the provider allocated nothing, so the
// reservation must be released at once. Holding it strands both the money and
// the concurrency slot: measured on this box, a 400 left $1.00 reserved in state
// "provisioning" and, with cost_max_concurrent = 1, every later remote job was
// refused with "oldest provisioning is 4m24s old" — for the four hours until the
// row's TTL expired.
//
// MUTATION: delete the errors.As(&refused) branch in execbackend_ledger.go and
// this goes red with the row still in "provisioning".
func TestProviderRefusalReleasesTheReservation(t *testing.T) {
	refusal := &e2b.RequestRefusedError{
		StatusCode: 400,
		// Operation is REQUIRED for the release: round 1 of #2226 showed that a
		// refusal from a cleanup Delete must not free a reservation for a sandbox
		// that may still bill, so only a refused CREATE counts.
		Operation: e2b.OperationCreate,
		Err:       errors.New(`POST /sandboxes: E2B returned HTTP 400: {"code":400,"message":"Timeout cannot be greater than 1 hours"}`),
	}
	if state := provisionRefusalAttemptState(t, "job-refused", refusal); state != string(db.ExecBackendAttemptStateFailed) {
		t.Fatalf("attempt state = %q, want %q: a refused provision must not strand its cost and concurrency slot",
			state, string(db.ExecBackendAttemptStateFailed))
	}
}

// TestAmbiguousProvisionFailureKeepsTheReservation pins the other half, and it is
// the half that must NOT be "fixed" by releasing everything.
//
// A transport failure is not proof of non-allocation: the provider may have
// created a sandbox whose response never arrived. Releasing that reservation
// would let a second attempt run while the first sandbox is still billing, which
// is the double-run the fail-safe design exists to prevent.
func TestAmbiguousProvisionFailureKeepsTheReservation(t *testing.T) {
	err := errors.New("provision remote execution sandbox: dial tcp: i/o timeout")
	if state := provisionRefusalAttemptState(t, "job-ambiguous", err); state != "provisioning" {
		t.Fatalf("attempt state = %q, want %q: an ambiguous failure must keep its fail-safe hold", state, "provisioning")
	}
}

// TestServerErrorKeepsTheReservation pins that a 5xx stays ambiguous. A server
// error can follow a completed allocation, so classifying it as proof-of-nothing
// would reintroduce the double-run hazard through the very mechanism added to
// stop the leak.
func TestServerErrorKeepsTheReservation(t *testing.T) {
	err := errors.New("POST /sandboxes: E2B returned HTTP 503: upstream unavailable")
	if state := provisionRefusalAttemptState(t, "job-5xx", err); state != "provisioning" {
		t.Fatalf("attempt state = %q, want %q: a 5xx may follow a completed allocation", state, "provisioning")
	}
}

// TestNonCreateRefusalKeepsTheReservation pins the narrow hazard round 1
// identified: Provision deletes the sandbox it just made when envd construction
// fails, so a refusal-class error from THAT delete must not release the
// reservation - the sandbox may exist and bill, and freeing the slot invites a
// duplicate run.
func TestNonCreateRefusalKeepsTheReservation(t *testing.T) {
	refusal := &e2b.RequestRefusedError{
		StatusCode: 403,
		Operation:  e2b.OperationDelete,
		Err:        errors.New("DELETE /sandboxes/abc: E2B returned HTTP 403: forbidden"),
	}
	if state := provisionRefusalAttemptState(t, "job-delete-refused", refusal); state != "provisioning" {
		t.Fatalf("attempt state = %q, want %q: only a refused CREATE proves nothing was allocated", state, "provisioning")
	}
}
