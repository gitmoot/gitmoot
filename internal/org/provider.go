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
	// Agents lists the live local, unarchived registered agents (#2502), so a
	// seat whose agent was renamed can be recognised by pane and session.
	Agents []LiveAgent `json:"-"`
}

// LiveAgent is one registered agent as Herdr reports it. Session is the
// agent's own session reference (omp's session file); with the pane it is the
// identity that survives a rename, since Herdr has no stable agent id.
type LiveAgent struct {
	Name    string
	PaneID  string
	Session string
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
	// AgentName and Session identify the registered agent an "agent:NAME"
	// binding resolved to (#2502); empty for other bindings.
	AgentName string
	Session   string
	// Unavailable means Herdr still lists the bound agent but could not start
	// its process (#2554); Detail carries Herdr's reason.
	Unavailable bool
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
