package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/memory"
)

const dashboardBrainEventsMaxLimit = 200

type dashboardBrainEvent struct {
	ID        int64           `json:"id"`
	At        string          `json:"at"`
	Kind      string          `json:"kind"`
	MemoryID  int64           `json:"memoryId,omitempty"`
	Key       string          `json:"key"`
	OwnerKind string          `json:"ownerKind"`
	OwnerRef  string          `json:"ownerRef"`
	Repo      string          `json:"repo,omitempty"`
	Scope     string          `json:"scope"`
	Actor     string          `json:"actor"`
	Detail    json.RawMessage `json:"detail"`
}

type dashboardBrainEventsResponse struct {
	Events     []dashboardBrainEvent `json:"events"`
	NextCursor int64                 `json:"nextCursor"`
	Total      int64                 `json:"total"`
}

type dashboardBrainFact struct {
	ID               int64  `json:"id"`
	Key              string `json:"key"`
	Title            string `json:"title"`
	Content          string `json:"content"`
	Status           string `json:"status"`
	Repo             string `json:"repo,omitempty"`
	Scope            string `json:"scope"`
	OwnerKind        string `json:"ownerKind"`
	OwnerRef         string `json:"ownerRef"`
	RetiredAt        string `json:"retiredAt,omitempty"`
	RetiredReason    string `json:"retiredReason,omitempty"`
	SupersededBy     int64  `json:"supersededBy,omitempty"`
	FirstConfirmedAt string `json:"firstConfirmedAt"`
	UpdatedAt        string `json:"updatedAt"`
}

func (d *webDataSource) BrainEvents(ctx context.Context, cursor int64, limit int) (dashboardBrainEventsResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > dashboardBrainEventsMaxLimit {
		limit = dashboardBrainEventsMaxLimit
	}
	out := dashboardBrainEventsResponse{Events: []dashboardBrainEvent{}}
	err := withStore(d.home, func(store *db.Store) error {
		var err error
		out.Total, err = store.MaxMemoryEventID(ctx)
		if err != nil {
			return err
		}
		events, err := store.ListMemoryEvents(ctx, db.MemoryEventFilter{BeforeID: cursor, Limit: limit + 1})
		if err != nil {
			return err
		}
		if len(events) > limit {
			out.NextCursor = events[limit-1].ID
			events = events[:limit]
		}
		for _, event := range events {
			detail := json.RawMessage(event.Detail)
			if len(detail) == 0 || !json.Valid(detail) {
				detail = json.RawMessage(`{}`)
			}
			out.Events = append(out.Events, dashboardBrainEvent{ID: event.ID, At: event.At, Kind: event.Kind,
				MemoryID: event.MemoryID, Key: event.Key, OwnerKind: event.OwnerKind, OwnerRef: event.OwnerRef,
				Repo: event.Repo, Scope: event.Scope, Actor: event.Actor, Detail: detail})
		}
		return nil
	})
	return out, err
}

func (d *webDataSource) BrainFact(ctx context.Context, id int64) (dashboardBrainFact, error) {
	var out dashboardBrainFact
	err := withStore(d.home, func(store *db.Store) error {
		record, err := store.GetConfirmedMemoryByID(ctx, id)
		if err != nil {
			return err
		}
		status := "active"
		if record.SupersededBy != 0 {
			status = "superseded"
		} else if record.RetiredAt != "" {
			status = "retired"
		}
		title := memory.Title(record.Content)
		if title == "" {
			title = record.Key
		}
		out = dashboardBrainFact{
			ID: record.ID, Key: record.Key, Title: title, Content: record.Content, Status: status,
			Repo: record.Repo, Scope: record.Scope, OwnerKind: record.Owner.Kind, OwnerRef: record.Owner.Ref,
			RetiredAt: record.RetiredAt, RetiredReason: record.RetiredReason, SupersededBy: record.SupersededBy,
			FirstConfirmedAt: record.FirstConfirmedAt, UpdatedAt: record.UpdatedAt,
		}
		return nil
	})
	return out, err
}

func (d *webDataSource) handleBrainEvents(w http.ResponseWriter, r *http.Request) {
	cursor, err := dashboardBrainEventIntQuery(r, "cursor", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := dashboardBrainEventIntQuery(r, "limit", 50)
	if err != nil || limit <= 0 {
		http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
		return
	}
	// singleflight-only (#948 conventions): never a stale page, but concurrent
	// identical polls coalesce into one store read per (cursor, limit) variant.
	// Retention is disabled for this endpoint, so the variant must be part of
	// the cache key itself rather than only the entry cursor.
	pageKey := dashboardBrainEventsCacheKey(cursor, limit)
	body, outcome, err := d.cacheForDashboard().get(r.Context(), pageKey, "", dashboardBrainEventsCachePolicy, func(ctx context.Context) ([]byte, error) {
		response, err := d.BrainEvents(ctx, cursor, int(limit))
		if err != nil {
			return nil, err
		}
		return json.Marshal(response)
	})
	if errors.Is(err, db.ErrMemoryEventCursorUnknown) {
		// Stale client state (e.g. a cursor persisted across a store rebuild):
		// a 400 tells the client to restart from the newest page instead of
		// rendering a permanently-empty feed.
		http.Error(w, "unknown cursor; restart from the newest page", http.StatusBadRequest)
		return
	}
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func dashboardBrainEventsCacheKey(cursor, limit int64) string {
	return fmt.Sprintf("brain-events:%d.%d", cursor, limit)
}

func (d *webDataSource) handleBrainFact(w http.ResponseWriter, r *http.Request) {
	id, err := dashboardBrainFactID(r)
	if err != nil {
		writeDashboardBrainError(w, http.StatusBadRequest, err.Error())
		return
	}
	pageKey := dashboardBrainFactCacheKey(id)
	body, outcome, err := d.cacheForDashboard().get(r.Context(), pageKey, "", dashboardBrainFactCachePolicy, func(ctx context.Context) ([]byte, error) {
		fact, err := d.BrainFact(ctx, id)
		if err != nil {
			return nil, err
		}
		return json.Marshal(fact)
	})
	w.Header().Set(dashboardCacheHeader, outcome)
	if errors.Is(err, db.ErrConfirmedMemoryNotFound) {
		writeDashboardBrainError(w, http.StatusNotFound, err.Error())
		return
	}
	d.writeCachedDashboardJSON(w, outcome, body, err)
}

func dashboardBrainFactCacheKey(id int64) string {
	return fmt.Sprintf("brain-fact:%d", id)
}

func dashboardBrainFactID(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	if raw == "" {
		return 0, fmt.Errorf("id is required")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("id must be a non-negative integer")
	}
	return id, nil
}

func writeDashboardBrainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func dashboardBrainEventIntQuery(r *http.Request, name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}
