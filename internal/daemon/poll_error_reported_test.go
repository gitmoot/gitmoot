package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/github"
)

// TestRunReportsPollErrorAndKeepsPolling is the guard that actually protects an unattended
// daemon from a caller defect, and it is deliberately a property of the CALLER.
//
// #1381 changed the merge gate to REFUSE a malformed gate miss by returning an error rather
// than panicking, because PollOnce has no recover() and a panic would end the daemon instead
// of one merge decision. That is only an improvement if somebody can SEE the refusal. Run
// previously discarded PollOnce's error entirely, so the refusal produced no log line and no
// other observable: the task stayed retryable and failed identically every interval, forever,
// with nothing emitted. A panic traded for silence is the worse of the two, because a panic
// at least announces itself.
//
// The two halves are asserted together on purpose -- they are in tension, and satisfying
// either one alone is a real regression:
//
//	REPORTED  - a fix that surfaces the error by ABORTING the loop would satisfy "reported"
//	            and break the watcher, which TestRunContinuesAfterPollError pins.
//	CONTINUES - the pre-existing behaviour, which is exactly what was insufficient.
//
// This is the independent observable that TestWithGateMissDoesNotPanic could not be. That test
// was deleted for being indistinguishable from the constructor's refusal guard; no mutant
// separated them, because both died to the same restored panic. This one dies to a mutant
// NEITHER of them notices: discarding the error at the call site.
func TestRunReportsPollErrorAndKeepsPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := testStore(t)
	pollFailure := errors.New("merge reason: a gate miss requires a gate or a cause; an all-blank miss is a caller defect")
	client := &fakeGitHub{listPullRequestsErrs: []error{pollFailure, nil}}

	var logged []string
	var sleeps int
	daemon := Daemon{
		Repo:         github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		Store:        store,
		GitHub:       client,
		PollInterval: time.Second,
		Logf: func(format string, args ...any) {
			logged = append(logged, strings.TrimSpace(fmt.Sprintf(format, args...)))
		},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			sleeps++
			if sleeps == 1 {
				return nil
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
	}

	err := daemon.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	// CONTINUES: the second poll happened despite the first failing.
	if client.listPullRequestsCalls != 2 {
		t.Fatalf("ListPullRequests calls = %d, want 2 (a reported poll error must not stop the watcher)", client.listPullRequestsCalls)
	}
	// REPORTED: and the failure reached the diagnostic sink, carrying its cause. Asserting
	// only that SOMETHING was logged would pass against a loop that logs every tick; the
	// message must contain the error itself.
	var reported string
	for _, line := range logged {
		if strings.Contains(line, "poll error") {
			reported = line
			break
		}
	}
	if reported == "" {
		t.Fatalf("no poll error was reported; a refusal nobody can observe is a panic traded for silence (logged=%q)", logged)
	}
	if !strings.Contains(reported, pollFailure.Error()) {
		t.Fatalf("reported line = %q, want it to carry the underlying cause %q", reported, pollFailure.Error())
	}
}
