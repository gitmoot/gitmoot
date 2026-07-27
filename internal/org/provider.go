package org

import (
	"context"
	"time"
)

type LifecycleState string

const (
	StateIdle    LifecycleState = "idle"
	StateWorking LifecycleState = "working"
	StateBlocked LifecycleState = "blocked"
	// StateUnavailable is a durable Gitmoot overlay for a role whose runtime
	// provider explicitly refused work until a known boundary. It is distinct
	// from Herdr-observed StateBlocked.
	StateUnavailable LifecycleState = "unavailable"
	StateDone        LifecycleState = "done"
	StateUnknown     LifecycleState = "unknown"
)

type RoleLiveState struct {
	State  LifecycleState `json:"state"`
	Detail string         `json:"detail,omitempty"`
}

type Snapshot struct {
	States          map[string]RoleLiveState `json:"states"`
	ObservedAt      time.Time                `json:"observed_at"`
	ProviderVersion string                   `json:"provider_version,omitempty"`
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
