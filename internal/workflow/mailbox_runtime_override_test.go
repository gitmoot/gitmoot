package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestMailboxEnqueuePersistsRuntimeOverride: the payload round-trips the
// override so a background daemon job honors it identically to foreground.
func TestMailboxEnqueuePersistsRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}

	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                 "job-override",
		Agent:              "shipper",
		Action:             "ask",
		Repo:               "gitmoot/gitmoot",
		RuntimeOverride:    runtime.ShellRuntime,
		RuntimeOverrideRef: "printf ok",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.RuntimeOverride != runtime.ShellRuntime || payload.RuntimeOverrideRef != "printf ok" {
		t.Fatalf("payload override = %q/%q", payload.RuntimeOverride, payload.RuntimeOverrideRef)
	}
}

// TestMailboxEnqueueRejectsInvalidRuntimeOverride: an invalid override fails
// at the enqueue chokepoint — BEFORE a job row exists — with an actionable
// error, for every producer.
func TestMailboxEnqueueRejectsInvalidRuntimeOverride(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store}
	freshRef, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef returned error: %v", err)
	}

	for name, request := range map[string]JobRequest{
		"unknown runtime":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"},
		"missing ref":          {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.OmpRuntime},
		"ref without override": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverrideRef: "x"},
		"shell fresh ref":      {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.ShellRuntime, RuntimeOverrideRef: freshRef},
		// "last" names no concrete session, so its lock key could never serialize
		// with a concrete session.
		"last ref on a non-shell runtime": {ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: runtime.OmpRuntime, RuntimeOverrideRef: runtime.LastRef},
	} {
		if _, err := mailbox.Enqueue(ctx, request); err == nil {
			t.Fatalf("%s: Enqueue accepted an invalid runtime override", name)
		}
		if _, err := store.GetJob(ctx, "job-bad"); err == nil {
			t.Fatalf("%s: an invalid override must not enqueue a job", name)
		}
	}

	// The unknown-runtime error enumerates the supported registry.
	_, err = mailbox.Enqueue(ctx, JobRequest{ID: "job-bad", Agent: "a", Action: "ask", Repo: "o/r", RuntimeOverride: "bogus", RuntimeOverrideRef: "x"})
	if err == nil {
		t.Fatal("Enqueue accepted an unknown override runtime")
	}
	for _, supported := range runtime.SupportedRuntimes() {
		if !strings.Contains(err.Error(), supported) {
			t.Fatalf("error %q must enumerate supported runtime %q", err.Error(), supported)
		}
	}
}
