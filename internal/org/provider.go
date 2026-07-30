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
	States          map[string]RoleLiveState `json:"states"`
	ObservedAt      time.Time                `json:"observed_at"`
	ProviderVersion string                   `json:"provider_version,omitempty"`
	PaneBindings    map[string]PaneBinding   `json:"-"`
	Panes           []LivePane               `json:"-"`
}

// PaneBinding records whether a configured role resolves to a live pane.
type PaneBinding struct {
	PaneID string
	Detail string
}

// LivePane is the identity needed to verify that every labeled Herdr pane is
// claimed by an organization role.
type LivePane struct {
	PaneID string
	Label  string
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
