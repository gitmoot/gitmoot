package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// #2680: on 2026-11-18 E2B answered both POST /sandboxes and envd with HTTP
// 503 ("upstream connect error or disconnect/reset before headers") because
// the owner's E2B account had reached its spending limit. Every remote review
// failed on its first 503, with a message that read like an outage, and seats
// re-requested by hand.
//
// A 5xx while preparing a sandbox that EXISTS (sync-in, before the runtime
// starts) is retried on a fresh sandbox: the failed one is destroyed and its
// reservation settled first, so a retry never holds two. A 5xx on create is
// not: it is ambiguous, so its reservation stays held until TTL, and against
// a spending limit every retry would hold one more and fill gitmoot's own cost
// cap. Either way the final failure names the likely cause, and a cluster of
// them raises one operator alert.

const (
	remoteProviderPhaseProvision = "provision"
	remoteProviderPhaseSyncIn    = "sync_in"

	remoteProviderRetryEventKind   = "remote_provider_retry"
	remoteProviderRefusedEventKind = "remote_provider_refused"

	remoteProviderAlertWindow    = 10 * time.Minute
	remoteProviderAlertThreshold = 3
	remoteProviderAlertInterval  = time.Hour
	// remoteProviderAlertRecipient is the operator role.
	remoteProviderAlertRecipient = "gitmoot"
	remoteProviderAlertJobsShown = 5
)

// remoteProviderRetryBackoffs is the wait before each retry; one retry per
// entry, so a job makes at most len+1 attempts. A variable for tests.
var remoteProviderRetryBackoffs = []time.Duration{20 * time.Second, 60 * time.Second}

// remoteProviderRefusalError is the job failure for a provider that refused
// new sandboxes: a 5xx on every attempt, or E2B's 402 Payment Required. It
// unwraps to the provider error, so ledger and cap-wait classification still
// see the original.
type remoteProviderRefusalError struct {
	provider string
	status   int
	phase    string
	attempts int
	err      error
}

func (e *remoteProviderRefusalError) Error() string {
	label := remoteProviderLabel(e.provider)
	if e.status == http.StatusPaymentRequired {
		return fmt.Sprintf("E2B spending limit reached: E2B refused a new sandbox with HTTP 402 Payment Required; raise the limit on the E2B dashboard. %v", e.err)
	}
	times := "once"
	if e.attempts > 1 {
		times = fmt.Sprintf("%d times", e.attempts)
	}
	message := fmt.Sprintf("%s refused new sandboxes %s (HTTP %d during %s).", label, times, e.status, e.phase)
	if e.provider == config.RemoteExecProviderE2B {
		message += " A common cause is the E2B account spending limit; check the E2B dashboard."
	}
	if e.phase == remoteProviderPhaseProvision {
		message += " The create is not retried: a 5xx does not prove no sandbox was made, so its cost reservation stays held until its TTL."
	}
	return message + " " + e.err.Error()
}

func (e *remoteProviderRefusalError) Unwrap() error { return e.err }

func remoteProviderLabel(provider string) string {
	if provider == config.RemoteExecProviderE2B {
		return "E2B"
	}
	return provider
}

func remoteProviderPhase(provisioned bool) string {
	if provisioned {
		return remoteProviderPhaseSyncIn
	}
	return remoteProviderPhaseProvision
}

// remoteProviderRefusalStatus reports the 502/503/504 a provider answered.
// Before the instance exists only the create counts; after it, only envd,
// because then the sandbox is known and can be destroyed. A 4xx, a cost-cap or
// admission refusal, and a transport failure carry no HTTPStatusError status in
// this set.
func remoteProviderRefusalStatus(err error, provisioned bool) (int, bool) {
	var statusErr *e2b.HTTPStatusError
	if !errors.As(err, &statusErr) {
		return 0, false
	}
	switch statusErr.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return 0, false
	}
	if provisioned {
		return statusErr.StatusCode, statusErr.Envd()
	}
	return statusErr.StatusCode, statusErr.Operation == e2b.OperationCreate
}

// e2bSpendLimitRefusal reports an E2B create refused with 402 Payment
// Required, the one status that names billing. E2B has not been seen to send
// it: on 2026-11-18 its spending limit surfaced as a gateway 503.
func e2bSpendLimitRefusal(err error, provider string) bool {
	var refused *e2b.RequestRefusedError
	return provider == config.RemoteExecProviderE2B && errors.As(err, &refused) &&
		refused.Operation == e2b.OperationCreate && refused.StatusCode == http.StatusPaymentRequired
}

func (w jobWorker) recordRemoteProviderRetry(ctx context.Context, job db.Job, refusal *remoteProviderRefusalError, backoff time.Duration) {
	message := fmt.Sprintf("attempt=%d status=%d phase=%s next_attempt=%d backoff=%s: %v",
		refusal.attempts, refusal.status, refusal.phase, refusal.attempts+1, backoff, refusal.err)
	if err := w.Store.AddJobEvent(context.WithoutCancel(ctx), db.JobEvent{
		JobID: job.ID, Kind: remoteProviderRetryEventKind, Message: message, Provider: refusal.provider,
	}); err != nil {
		writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, remoteProviderRetryEventKind, err)
	}
}

func sleepForRemoteProviderRetry(ctx context.Context, backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// recordRemoteProviderRefusal marks a job that failed because its provider
// refused new sandboxes, and raises the cluster alert when it is due.
func (w jobWorker) recordRemoteProviderRefusal(ctx context.Context, job db.Job, cause error) {
	var refusal *remoteProviderRefusalError
	if !errors.As(cause, &refusal) {
		return
	}
	writeCtx := context.WithoutCancel(ctx)
	if err := w.Store.AddJobEvent(writeCtx, db.JobEvent{
		JobID: job.ID, Kind: remoteProviderRefusedEventKind, Message: refusal.Error(), Provider: refusal.provider,
	}); err != nil {
		writeLine(w.Stdout, "job %s %s event failed: %v", job.ID, remoteProviderRefusedEventKind, err)
		return
	}
	if _, err := alertRemoteProviderRefusalCluster(writeCtx, w.Store, refusal.provider, time.Now().UTC()); err != nil {
		writeLine(w.Stdout, "remote provider refusal alert for %s failed: %v", refusal.provider, err)
	}
}

// alertRemoteProviderRefusalCluster sends the operator ONE message when at
// least remoteProviderAlertThreshold remote jobs failed on a provider refusal
// within remoteProviderAlertWindow, at most once per
// remoteProviderAlertInterval per provider. The throttle is durable, so a
// daemon restart does not resend.
func alertRemoteProviderRefusalCluster(ctx context.Context, store *db.Store, provider string, now time.Time) (bool, error) {
	events, err := store.ListJobEventsOfKindSince(ctx, remoteProviderRefusedEventKind, provider, now.Add(-remoteProviderAlertWindow), 100)
	if err != nil {
		return false, err
	}
	var jobs []string
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		if !seen[event.JobID] {
			seen[event.JobID] = true
			jobs = append(jobs, event.JobID)
		}
	}
	if len(jobs) < remoteProviderAlertThreshold {
		return false, nil
	}
	shown := jobs
	if len(shown) > remoteProviderAlertJobsShown {
		shown = shown[:remoteProviderAlertJobsShown]
	}
	label := remoteProviderLabel(provider)
	body := fmt.Sprintf("%s refused new sandboxes for %d remote jobs in the last %s. Newest: %s.",
		label, len(jobs), remoteProviderAlertWindow, strings.Join(shown, ", "))
	if provider == config.RemoteExecProviderE2B {
		body += " A common cause is the E2B account spending limit; check the E2B dashboard."
	}
	body += fmt.Sprintf(" Each job's failure has the provider's answer. This alert repeats at most once per %s.", remoteProviderAlertInterval)
	return store.SendThrottledSystemAlert(ctx, "provider-refusal:"+provider, remoteProviderAlertRecipient, body, now, remoteProviderAlertInterval)
}
