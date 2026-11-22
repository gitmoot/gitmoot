package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/hub"
)

// hubOMPReceiptPrefix marks a wake row delivered by a remote machine's gitmoot
// client to its local omp inbox add-on (#2710). The full receipt is
// omp-client:<machine>.
const hubOMPReceiptPrefix = "omp-client:"

// hubOMPClaimed is one claimed row as the client receives it. Attempt names
// this delivery attempt; finish must echo it, so an outcome from an older
// attempt can never change a newer one.
type hubOMPClaimed struct {
	ID        int64  `json:"id"`
	Attempt   string `json:"attempt"`
	MessageID int64  `json:"message_id"`
	Prompt    string `json:"prompt"`
}

// hubOMPInFlight remembers claimed rows between claim and finish so a
// delivered directive gets the same transport receipt the daemon records.
// Like the local turn hook's receipt, it is best effort: a hub restart in
// between costs one extra acknowledgment nudge, nothing more.
type hubOMPInFlight struct {
	mu   sync.Mutex
	rows map[int64]db.WakeOutboxObligation
}

const hubOMPInFlightLimit = 4096

func (f *hubOMPInFlight) put(rows []db.WakeOutboxObligation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil || len(f.rows)+len(rows) > hubOMPInFlightLimit {
		f.rows = make(map[int64]db.WakeOutboxObligation)
	}
	for _, row := range rows {
		f.rows[row.ID] = row
	}
}

func (f *hubOMPInFlight) take(id int64) (db.WakeOutboxObligation, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	delete(f.rows, id)
	return row, ok
}

// registerHubOMPEndpoints adds the two-phase delivery a remote omp seat needs
// (#2710). An omp session is reached only through its machine's local inbox
// add-on socket, so the client on that machine does what the daemon does for
// a local omp seat: claim, notify the add-on, report the outcome.
//
//	POST /v1/notifications/omp/claim                      pending -> attempted
//	POST /v1/notifications/omp/finish {"id","attempt","outcome","reason"}
//
// The claim is one transaction for the whole batch: either every selected
// row becomes attempted and is returned, or none does. outcome "delivered"
// finishes the attempt with receipt omp-client:<machine>; "deferred" returns
// it to pending with the add-on's reason, for an outcome that proves nothing
// reached omp. An uncertain receipt is not reported: the row stays attempted
// and the existing aged-attempt health check surfaces it, so nothing is
// resent blindly.
func registerHubOMPEndpoints(handle func(string, hubHandler), store *db.Store) {
	inFlight := &hubOMPInFlight{}
	handle("POST /v1/notifications/omp/claim", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		// max_age_seconds > 0 retires mail that has waited longer than that
		// instead of delivering it as new work (#2725); a client that sends
		// none (0) gets every waiting notification, as before.
		var input struct {
			MaxAgeSeconds int64 `json:"max_age_seconds"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		if input.MaxAgeSeconds < 0 {
			return http.StatusBadRequest, nil, errors.New("max_age_seconds must not be negative")
		}
		now := turnHookNow()
		// Attribution first: claimed rows leave pending and cannot be put back.
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "notifications.omp.claim", "", now.UTC()); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was claimed")
		}
		var warnings bytes.Buffer
		candidates, more, err := eligibleInboxRows(ctx, store, caller.Role, false, now, &warnings)
		if err != nil {
			return http.StatusInternalServerError, nil, fmt.Errorf("list pending notifications: %w", err)
		}
		var retired int64
		if input.MaxAgeSeconds > 0 {
			maxAge := time.Duration(input.MaxAgeSeconds) * time.Second
			candidates, retired, err = retireStaleCandidates(ctx, store, caller, candidates, maxAge, now)
			if err != nil {
				return http.StatusInternalServerError, nil, fmt.Errorf("retire stale notifications: %w", err)
			}
		}
		if len(candidates) == 0 {
			return http.StatusOK, map[string]any{"claimed": []hubOMPClaimed{}, "retired": retired, "more": more, "warnings": hubWarnings(warnings)}, nil
		}
		ids := make([]int64, len(candidates))
		for index, candidate := range candidates {
			ids[index] = candidate.row.ID
		}
		// All or nothing: a row taken meanwhile by the daemon or another
		// client fails the whole batch, which stays pending for the next pass.
		won, err := store.ClaimWakeOutbox(ctx, ids[0], ids[1:], now)
		if err != nil {
			return http.StatusInternalServerError, nil, fmt.Errorf("claim notifications: %w", err)
		}
		if !won {
			return http.StatusOK, map[string]any{"claimed": []hubOMPClaimed{}, "retired": retired, "more": true, "warnings": hubWarnings(warnings)}, nil
		}
		attempt := db.HubAttemptToken(now)
		claimed := make([]hubOMPClaimed, len(candidates))
		rows := make([]db.WakeOutboxObligation, len(candidates))
		for index, candidate := range candidates {
			event := candidate.event
			inboxWakeDetail([]db.WakeOutboxObligation{candidate.row}, &event)
			claimed[index] = hubOMPClaimed{ID: candidate.row.ID, Attempt: attempt, MessageID: candidate.row.MessageID,
				Prompt: eventRuleWakePrompt(candidate.rule.OnKind, event)}
			rows[index] = candidate.row
		}
		inFlight.put(rows)
		return http.StatusOK, map[string]any{"claimed": claimed, "retired": retired, "more": more, "warnings": hubWarnings(warnings)}, nil
	})
	handle("POST /v1/notifications/omp/finish", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			ID      int64  `json:"id"`
			Attempt string `json:"attempt"`
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		if input.ID <= 0 || strings.TrimSpace(input.Attempt) == "" {
			return http.StatusBadRequest, nil, errors.New("id and the attempt from the claim are required")
		}
		delivered := input.Outcome == "delivered"
		if !delivered && input.Outcome != "deferred" {
			return http.StatusBadRequest, nil, errors.New(`outcome must be "delivered" or "deferred"; an uncertain receipt is not reported`)
		}
		detail := hubOMPReceiptPrefix + caller.Machine.Name
		if !delivered {
			if detail = strings.TrimSpace(input.Reason); detail == "" {
				return http.StatusBadRequest, nil, errors.New("a deferred outcome needs the add-on's reason")
			}
		}
		now := time.Now().UTC()
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "notifications.omp."+input.Outcome, fmt.Sprintf("%d@%s", input.ID, input.Attempt), now); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was finished")
		}
		// One guarded UPDATE: the row must be this role's, still attempted,
		// and still on this attempt. Anything else is refused unchanged.
		err := store.FinishHubWakeAttempt(ctx, caller.Role, input.ID, input.Attempt, delivered, detail, now)
		if errors.Is(err, db.ErrHubAttemptStale) {
			return http.StatusConflict, nil, fmt.Errorf("notification %d: not this role's current attempt %s", input.ID, input.Attempt)
		}
		if err != nil {
			return http.StatusInternalServerError, nil, fmt.Errorf("finish notification %d: %w", input.ID, err)
		}
		if row, ok := inFlight.take(input.ID); ok && delivered {
			var warnings bytes.Buffer
			recordTurnHookDirectiveReceipts(ctx, store, caller.Role, []db.WakeOutboxObligation{row}, &warnings)
		}
		return http.StatusOK, map[string]any{"id": input.ID, "outcome": input.Outcome}, nil
	})
}

// retireStaleCandidates supersedes the candidates that have waited longer
// than maxAge and returns the rest with the retired count. A row whose
// creation time does not parse is kept: its age is unknown, and delivering
// it is the behaviour without a guard.
func retireStaleCandidates(ctx context.Context, store *db.Store, caller hub.Caller, candidates []inboxCandidate, maxAge time.Duration, now time.Time) ([]inboxCandidate, int64, error) {
	cutoff := now.Add(-maxAge)
	fresh := candidates[:0:0]
	var stale []int64
	for _, candidate := range candidates {
		createdAt, err := time.Parse(time.RFC3339Nano, candidate.row.CreatedAt)
		if err == nil && createdAt.Before(cutoff) {
			stale = append(stale, candidate.row.ID)
			continue
		}
		fresh = append(fresh, candidate)
	}
	detail := fmt.Sprintf("not delivered: waited longer than %s before a client claimed it (client %s); still in the inbox", maxAge, caller.Machine.Name)
	retired, err := store.SupersedePendingWakeRows(ctx, caller.Role, stale, detail, now)
	return fresh, retired, err
}
