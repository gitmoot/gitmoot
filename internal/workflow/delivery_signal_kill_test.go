package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// #1726, reproduced on an isolated home before this was written: a daemon
// SIGTERM exits in 0.6s, kills its in-flight child, records
// `delivery failed: signal: terminated`, and nothing ever requeues the row -
// not even the daemon on restart. Historically 38 of 39 signal-shaped deaths
// were never recovered and 36 are still `failed`.
//
// This classifies them. It does not requeue: automatic requeue is deliberately
// refused one layer up, because a killed delivery may already have pushed a
// branch or posted a PR comment (#1817 measured 25 such comments from
// dispatches that then died), so re-running it trades a lost job for a
// double-executed one.
//
// THE TWO RENDERINGS ARE BOTH FIRST-CLASS, and that is the finding this test
// exists to hold. Direct-child runtimes report `signal: terminated`; claude and
// omp report `exit status 143`, because a wrapped CLI translates the signal and
// THE NAME IS LOST. A predicate matching only "signal:" would silently miss
// every claude and omp death, which is most of the reviews this campaign
// protects.
func TestClassifyDeliverySignalKillAcceptsBothRenderings(t *testing.T) {
	for _, tt := range []struct {
		name          string
		err           error
		wantSignal    string
		wantRendering string
	}{
		{
			name:          "signal name, as a direct child reports it",
			err:           errors.New("signal: terminated"),
			wantSignal:    "terminated",
			wantRendering: "signal_name",
		},
		{
			name:          "the SIGHUP reproduction, verbatim",
			err:           errors.New("delivery failed: signal: hangup"),
			wantSignal:    "hangup",
			wantRendering: "signal_name",
		},
		{
			name:          "exit status 143, as claude and omp report a SIGTERM",
			err:           errors.New("delivery failed: exit status 143"),
			wantSignal:    "terminated",
			wantRendering: "exit_status",
		},
		{
			name:          "exit status 129, as claude and omp report a SIGHUP",
			err:           errors.New("delivery failed: exit status 129"),
			wantSignal:    "hangup",
			wantRendering: "exit_status",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kill, ok := classifyDeliverySignalKill(tt.err)
			if !ok {
				t.Fatalf("%v was not classified as a signal kill", tt.err)
			}
			if kill.Signal != tt.wantSignal || kill.Rendering != tt.wantRendering {
				t.Fatalf("kill = %+v, want signal %q rendering %q", kill, tt.wantSignal, tt.wantRendering)
			}
		})
	}
}

// TestClassifyDeliverySignalKillRefusesEverythingElse is the over-classification
// control, and the deadline arms are the ones that matter.
//
// A timeout also ends a delivery non-zero, and on this store 8 of 16
// `exit status 143` deaths WERE timeouts. Calling one an operator kill would
// tell an operator a restart lost work that in fact burned its full 45-minute
// wall, and would be the wrong input to any later requeue policy, because
// re-running a timeout just times out again.
func TestClassifyDeliverySignalKillRefusesEverythingElse(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "an ordinary non-zero exit", err: errors.New("delivery failed: exit status 1")},
		{name: "an exit code that is not a signal encoding", err: errors.New("delivery failed: exit status 7")},
		{name: "a 126 permission failure, which is NOT 128+N", err: errors.New("delivery failed: exit status 126")},
		{name: "the job's own deadline", err: fmt.Errorf("delivery failed: %w", context.DeadlineExceeded)},
		{name: "a cancellation", err: fmt.Errorf("delivery failed: %w", context.Canceled)},
		{
			// The dangerous one: a deadline that ALSO carries a signal rendering,
			// because a job_timeout kills its subprocess. The deadline must win.
			name: "a deadline whose subprocess was then killed",
			err:  fmt.Errorf("delivery failed: signal: killed: %w", context.DeadlineExceeded),
		},
		{name: "prose that merely mentions a signal", err: errors.New("the agent discussed signal handling")},
		{name: "an auth failure", err: errors.New("delivery failed: OAuth session expired")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if kill, ok := classifyDeliverySignalKill(tt.err); ok {
				t.Fatalf("%v was classified as a signal kill (%+v)", tt.err, kill)
			}
		})
	}
}

// TestDeliverySignalKillMessageNamesTheSignalAndItsEvidence pins what an
// operator reads. The two renderings are the reason: when they disagree about
// how a death was reported, the operator needs to know which one produced the
// classification.
func TestDeliverySignalKillMessageNamesTheSignalAndItsEvidence(t *testing.T) {
	kill, ok := classifyDeliverySignalKill(errors.New("delivery failed: exit status 129"))
	if !ok {
		t.Fatal("exit status 129 was not classified")
	}
	message := fmt.Sprintf("delivery was killed by SIG%s (evidence: %s)", strings.ToUpper(kill.Signal), kill.Rendering)
	for _, want := range []string{"SIGHANGUP", "exit_status"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not name %q", message, want)
		}
	}
}
