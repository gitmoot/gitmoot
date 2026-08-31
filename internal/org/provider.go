package org

import (
	"context"
	"time"
)

type LifecycleState string

const (
	StateIdle         LifecycleState = "idle"
	StateWorking      LifecycleState = "working"
	StateBlocked      LifecycleState = "blocked"
	StateInputPending LifecycleState = "input_pending"
	// StateUnavailable is a durable Gitmoot overlay for a role whose runtime
	// provider explicitly refused work until a known boundary. It is distinct
	// from Herdr-observed StateBlocked.
	StateUnavailable LifecycleState = "unavailable"
	StateDone        LifecycleState = "done"
	StateUnknown     LifecycleState = "unknown"
)

type RoleLiveState struct {
	State    LifecycleState `json:"state"`
	Detail   string         `json:"detail,omitempty"`
	Activity *RoleActivity  `json:"activity,omitempty"`
}

// RoleActivity identifies a completed provider turn. A nil *RoleActivity means
// the provider did not report turn activity; callers must not treat that as a
// zero-valued or stale turn.
type RoleActivity struct {
	Turn        int64     `json:"turn"`
	TurnEpoch   int64     `json:"turn_epoch"`
	CompletedAt time.Time `json:"completed_at"`
}

type Snapshot struct {
	States          map[string]RoleLiveState   `json:"states"`
	ObservedAt      time.Time                  `json:"observed_at"`
	ProviderVersion string                     `json:"provider_version,omitempty"`
	PaneBindings    map[string]PaneBinding     `json:"-"`
	Panes           []LivePane                 `json:"-"`
	Sessions        map[string]SessionActivity `json:"-"`
}

// SessionActivity is the live, non-durable session metadata Herdr reports for a
// configured role. It stays separate from RoleLiveState because a live pane can
// exist without an active agent session.
type SessionActivity struct {
	PaneID      string
	Agent       string
	TaskTitle   string
	CurrentTurn *int64
}

// PaneBinding records whether a configured role resolves to a live pane.
// Ambiguous is true only when multiple live labels match and no literal id did.
type PaneBinding struct {
	PaneID    string
	Detail    string
	Ambiguous bool
}

// LivePane carries the live identity used by validation plus the two directories
// Herdr reports for branch-safety checks when retiring a seat.
type LivePane struct {
	PaneID        string
	Label         string
	CWD           string
	ForegroundCWD string
}

type RecycleRequest struct {
	Role       string
	Pane       string
	Kind       string
	AgentName  string
	Model      string
	BootPrompt string
}

type Provider interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	Recycle(ctx context.Context, req RecycleRequest) error
}
