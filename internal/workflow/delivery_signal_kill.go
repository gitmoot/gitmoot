package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// DeliverySignalKilledEvent records that a delivery died because its process
// was SIGNALLED, rather than exiting on its own terms.
//
// #1726. A daemon restart kills in-flight work and nothing recovers it:
// reproduced on an isolated home, and measured at 38 of 39 signal-shaped deaths
// never requeued, 36 of them still `failed`. This event does not change that.
// It makes the population FINDABLE, which it currently is not.
//
// WHY IDENTIFICATION AND NOT REQUEUE. Automatic requeue is deliberately refused
// one layer up: recoverForeignBootRunners states that "work consumed before a
// daemon restart is never silently run twice; an operator can inspect the
// durable evidence and explicitly retry". That is sound - a killed delivery may
// already have pushed a branch, posted a PR comment, taken a lock or spent
// tokens (#1817 measured 25 public attributed comments posted by dispatches
// that then died), so re-running it trades a lost job for a double-executed
// one. Whether to make that trade is a policy decision with an owner. Being
// able to SEE which jobs were killed is the prerequisite either way, and it
// cannot double-run anything.
const DeliverySignalKilledEvent = "delivery_signal_killed"

// signalExitStatusPattern matches the 128+N rendering. It is not a fallback: it
// is the ONLY rendering available for the runtimes this campaign is about.
//
// Measured across this store's history: signal-NAMED deaths (`signal: hangup`
// and friends) come from direct-child runtimes such as shell, while claude and
// omp render `exit status 129`, because a wrapped CLI translates the signal into
// an exit code and the NAME IS LOST. A predicate matching only "signal:" would
// silently miss every claude and omp death - which is most of the reviews this
// campaign exists to protect.
var signalExitStatusPattern = regexp.MustCompile(`exit status (129|130|137|143)\b`)

// signalNames maps the 128+N codes this recognises to the signal they encode.
// Deliberately short: only signals actually observed ending a delivery on this
// fleet, so an unrecognised code stays unclassified rather than being guessed.
var signalNames = map[string]string{
	"129": "hangup",
	"130": "interrupt",
	"137": "killed",
	"143": "terminated",
}

// signalNamePattern matches Go's own rendering for a child killed by a signal,
// which is what os/exec produces for a direct child.
var signalNamePattern = regexp.MustCompile(`signal: (hangup|interrupt|killed|terminated|quit)\b`)

// DeliverySignalKill describes a signalled delivery death.
type DeliverySignalKill struct {
	// Signal is the signal name, normalised across both renderings so a consumer
	// never has to know which one it got.
	Signal string
	// Rendering names which form the evidence took, because that is the fact an
	// operator needs when the two disagree: "signal_name" or "exit_status".
	Rendering string
}

// classifyDeliverySignalKill reports whether a delivery error is a signal death.
//
// IT EXCLUDES THE JOB'S OWN DEADLINE, and that exclusion is the point rather
// than a detail. A timeout also ends a delivery non-zero, and on this store 8 of
// 16 `exit status 143` deaths were timeouts, identifiable by a `job_timeout`
// event and a duration equal to an exact configured deadline. Classifying a
// timeout as an operator kill would tell an operator that a restart lost work
// that in fact burned its full 45-minute wall - and would be the wrong input to
// any later requeue policy, since re-running a timeout just times out again.
//
// Deadline and cancellation are therefore refused FIRST, before either pattern
// runs, so the ordering cannot be reversed by a later edit without this comment
// going with it.
func classifyDeliverySignalKill(err error) (DeliverySignalKill, bool) {
	if err == nil {
		return DeliverySignalKill{}, false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return DeliverySignalKill{}, false
	}
	text := err.Error()
	if match := signalNamePattern.FindStringSubmatch(text); len(match) == 2 {
		return DeliverySignalKill{Signal: match[1], Rendering: "signal_name"}, true
	}
	if match := signalExitStatusPattern.FindStringSubmatch(text); len(match) == 2 {
		name, ok := signalNames[match[1]]
		if !ok {
			return DeliverySignalKill{}, false
		}
		return DeliverySignalKill{Signal: name, Rendering: "exit_status"}, true
	}
	return DeliverySignalKill{}, false
}

// recordDeliverySignalKill attaches the classification beside the terminal
// failure. Best effort, like storeFailureDiagnostics at the same seam: losing
// the annotation must never change whether or how a delivery failed.
//
// It consults the job's OWN EVENT STREAM for a timeout before recording, which
// is the discriminator measured on this store: 8 of 16 `exit status 143` deaths
// were timeouts, each carrying a timeout-shaped event and a duration equal to an
// exact configured deadline. The in-process deadline check in the classifier is
// not sufficient by itself, because a job_timeout kills its subprocess and that
// kill can render as `signal: killed` - which would otherwise be read as an
// operator restart. Two independent exclusions for one class, because a
// misclassified timeout tells an operator a restart lost work that in fact
// burned its whole wall.
func (m Mailbox) recordDeliverySignalKill(ctx context.Context, jobID string, err error) {
	kill, ok := classifyDeliverySignalKill(err)
	if !ok || m.store == nil {
		return
	}
	jobID = strings.TrimSpace(jobID)
	if m.jobRecordedATimeout(ctx, jobID) {
		return
	}
	_ = m.store.AddJobEventIfAbsent(ctx, db.JobEvent{
		JobID: jobID,
		Kind:  DeliverySignalKilledEvent,
		Message: fmt.Sprintf("delivery was killed by SIG%s (evidence: %s); this job was abandoned mid-flight and nothing requeues it - inspect and retry explicitly",
			strings.ToUpper(kill.Signal), kill.Rendering),
	})
}

// jobRecordedATimeout reports whether this job already recorded a timeout-shaped
// event. Read failure returns TRUE - it suppresses the annotation rather than
// risking a timeout labelled as an operator kill, because a missing annotation
// costs discoverability while a wrong one costs a wrong diagnosis.
func (m Mailbox) jobRecordedATimeout(ctx context.Context, jobID string) bool {
	events, err := m.store.ListJobEvents(ctx, jobID)
	if err != nil {
		return true
	}
	for _, event := range events {
		if strings.Contains(event.Kind, "timeout") {
			return true
		}
	}
	return false
}
