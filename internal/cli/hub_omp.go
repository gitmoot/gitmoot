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

// hubOMPClaimed is one claimed row as the client receives it.
type hubOMPClaimed struct {
	ID        int64  `json:"id"`
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

func (f *hubOMPInFlight) take(ids []int64) []db.WakeOutboxObligation {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []db.WakeOutboxObligation
	for _, id := range ids {
		if row, ok := f.rows[id]; ok {
			rows = append(rows, row)
			delete(f.rows, id)
		}
	}
	return rows
}

// registerHubOMPEndpoints adds the two-phase delivery a remote omp seat needs
// (#2710). An omp session is reached only through its machine's local inbox
// add-on socket, so the client on that machine does what the daemon does for
// a local omp seat: claim, notify the add-on, report the outcome.
//
//	POST /v1/notifications/omp/claim                 pending -> attempted
//	POST /v1/notifications/omp/finish {"ids","outcome","reason"}
//
// outcome "delivered" finishes the rows with receipt omp-client:<machine>;
// "deferred" returns them to pending with the add-on's reason, for an outcome
// that proves nothing reached omp. An uncertain receipt is not reported: the
// rows stay attempted and the existing aged-attempt health check surfaces
// them, so nothing is resent blindly.
func registerHubOMPEndpoints(handle func(string, hubHandler), store *db.Store) {
	inFlight := &hubOMPInFlight{}
	handle("POST /v1/notifications/omp/claim", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		if err := decodeHubBody(r, &struct{}{}); err != nil {
			return http.StatusBadRequest, nil, err
		}
		now := turnHookNow()
		// Attribution first: claimed rows leave pending and cannot be put back.
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "notifications.omp.claim", "", now.UTC()); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was claimed")
		}
		var warnings bytes.Buffer
		eligible, more, err := eligibleInboxRows(ctx, store, caller.Role, false, now, &warnings)
		if err != nil {
			return http.StatusInternalServerError, nil, fmt.Errorf("list pending notifications: %w", err)
		}
		rules, err := store.ListEventRules(ctx)
		if err != nil {
			return http.StatusInternalServerError, nil, errors.New("event rules unavailable")
		}
		claimed := make([]hubOMPClaimed, 0, len(eligible))
		claimedRows := make([]db.WakeOutboxObligation, 0, len(eligible))
		for _, row := range eligible {
			event, err := wakeOutboxEvent([]db.WakeOutboxObligation{row}, now)
			if err != nil {
				continue
			}
			matching := matchingWakeRules(rules, event)
			if len(matching) == 0 {
				continue
			}
			inboxWakeDetail([]db.WakeOutboxObligation{row}, &event)
			won, err := store.ClaimWakeOutbox(ctx, row.ID, nil, now)
			if err != nil {
				return http.StatusInternalServerError, nil, fmt.Errorf("claim notification %d: %w", row.ID, err)
			}
			if !won {
				continue // the daemon or another client took it
			}
			claimed = append(claimed, hubOMPClaimed{ID: row.ID, MessageID: row.MessageID, Prompt: eventRuleWakePrompt(matching[0].OnKind, event)})
			claimedRows = append(claimedRows, row)
		}
		inFlight.put(claimedRows)
		return http.StatusOK, map[string]any{"claimed": claimed, "more": more, "warnings": hubWarnings(warnings)}, nil
	})
	handle("POST /v1/notifications/omp/finish", func(ctx context.Context, caller hub.Caller, _ config.OrgConfig, r *http.Request) (int, any, error) {
		var input struct {
			IDs     []int64 `json:"ids"`
			Outcome string  `json:"outcome"`
			Reason  string  `json:"reason"`
		}
		if err := decodeHubBody(r, &input); err != nil {
			return http.StatusBadRequest, nil, err
		}
		if len(input.IDs) == 0 || len(input.IDs) > turnHookMaxItems {
			return http.StatusBadRequest, nil, fmt.Errorf("ids must name 1-%d notifications", turnHookMaxItems)
		}
		owned, err := store.WakeOutboxRowsTargetRole(ctx, caller.Role, input.IDs)
		if err != nil {
			return http.StatusInternalServerError, nil, errors.New("notification lookup failed")
		}
		if !owned {
			return http.StatusForbidden, nil, fmt.Errorf("every id must be a notification for %s", caller.Role)
		}
		now := time.Now().UTC()
		if input.Outcome != "delivered" && input.Outcome != "deferred" {
			return http.StatusBadRequest, nil, errors.New(`outcome must be "delivered" or "deferred"; an uncertain receipt is not reported`)
		}
		if err := store.RecordHubWrite(ctx, caller.Machine.ID, caller.Role, "notifications.omp."+input.Outcome, fmt.Sprint(input.IDs), now); err != nil {
			return http.StatusInternalServerError, nil, errors.New("could not record the hub write; nothing was finished")
		}
		switch input.Outcome {
		case "delivered":
			receipt := hubOMPReceiptPrefix + caller.Machine.Name
			for _, id := range input.IDs {
				// One row per finish: these rows were never coalesced, and a
				// multi-id finish would mark the rest superseded.
				if err := store.FinishWakeOutbox(ctx, []int64{id}, db.WakeOutboxStateDelivered, receipt, now); err != nil {
					return http.StatusConflict, nil, fmt.Errorf("finish notification %d: %w", id, err)
				}
			}
			var warnings bytes.Buffer
			recordTurnHookDirectiveReceipts(ctx, store, caller.Role, inFlight.take(input.IDs), &warnings)
		case "deferred":
			reason := strings.TrimSpace(input.Reason)
			if reason == "" {
				return http.StatusBadRequest, nil, errors.New("a deferred outcome needs the add-on's reason")
			}
			if err := store.DeferAttemptedWakeOutbox(ctx, input.IDs, reason, now); err != nil {
				return http.StatusConflict, nil, fmt.Errorf("defer notifications: %w", err)
			}
			inFlight.take(input.IDs)
		}
		return http.StatusOK, map[string]any{"ids": input.IDs, "outcome": input.Outcome}, nil
	})
}
