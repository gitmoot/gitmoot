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
	StateDone    LifecycleState = "done"
	StateUnknown LifecycleState = "unknown"
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

type Provider interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}
