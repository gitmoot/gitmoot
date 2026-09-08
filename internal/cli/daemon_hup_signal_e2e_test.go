//go:build e2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1820, the RECORDING half. The delivery half - that a bare
// `systemctl kill -s HUP` reaches every process in the unit and kills a
// dispatched runtime binary with 129 while the daemon survives - was reproduced
// with a throwaway systemd unit and is not what this test covers.
//
// This covers what the DAEMON does with such a death, on the real worker tick
// and the real store. It was declared unexercised when `daemon reload` shipped:
// #1726's classifier exists in the deployed binary but the store holds ZERO
// delivery_signal_killed rows, so nothing had proven the classifier fires on a
// SIGHUP-shaped death rather than only on the SIGTERM one it was written for.
//
// Deterministic, NO-LLM, offline: the agent's default runtime is shell and its
// RuntimeRef is a script that hangs itself up. From the child's point of view a
// self-directed SIGHUP is indistinguishable from a cgroup-wide one, which is
// exactly the substitution that makes this testable without systemd.
func TestWarmReloadSignalledJobIsRecordedAsKilledNotAPlainFailureE2E(t *testing.T) {
	ctx := context.Background()
	// `kill -HUP $$` from the runtime's own shell: the delivery dies by signal
	// before it can print a gitmoot_result, which is precisely a job that was
	// mid-flight when the reload arrived.
	home, store := effectiveRuntimeE2EHome(t, "kill -HUP $$")

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "work that a warm reload will interrupt",
		"--home", home,
		"--repo", "owner/repo",
		"--background",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent ask --background exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse ask output %q: %v", out.String(), err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}

	job, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", output.JobID, err)
	}
	if job.State == string(workflow.JobSucceeded) {
		t.Fatalf("a signalled delivery reported success: state=%q", job.State)
	}

	events, err := store.ListJobEvents(ctx, output.JobID)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	var killed, killMessage string
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		if event.Kind == workflow.DeliverySignalKilledEvent {
			killed = event.Kind
			killMessage = event.Message
		}
	}
	// THE ASSERTION THAT MATTERS: the job must be findable as killed. Before
	// #1726's classifier this population existed only as `exit status 129`
	// inside a failure message, which no engine-side check can read - the store
	// still holds five such jobs from before it, all review-family.
	if killed == "" {
		t.Fatalf("no %s event recorded for a signalled delivery; kinds=%v", workflow.DeliverySignalKilledEvent, kinds)
	}
	// It must name the SIGNAL, not merely that something died. SIGHUP is the
	// warm-reload case; SIGTERM is the restart case #1726 was written for, and an
	// operator diagnosing lost work needs to know which one took it.
	//
	// The production rendering is "SIGHANGUP": recordDeliverySignalKill prefixes
	// "SIG" onto signalNames' value, which is Go's own word for the signal
	// ("hangup") rather than the POSIX abbreviation. Asserting what the code
	// actually emits, and matching on HANGUP so the POSIX spelling would also
	// satisfy it - the naming is #1726's to change if anyone wants it, and this
	// slice does not touch another slice's operator message.
	upper := strings.ToUpper(killMessage)
	if !strings.Contains(upper, "HANGUP") && !strings.Contains(upper, "SIGHUP") {
		t.Fatalf("kill event does not identify the hangup signal, so a reload and a restart are indistinguishable: %q", killMessage)
	}
	// And it must state the requeue position, because that is the operator's next
	// question and the answer is load-bearing: a killed delivery may already
	// have pushed a branch or posted a comment, so retry is a decision.
	if !strings.Contains(killMessage, "requeue") {
		t.Fatalf("kill event does not state the requeue position: %q", killMessage)
	}
}
