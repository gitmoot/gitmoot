package workflow

import (
	"context"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// presetDeliveryInputs are the pure decision inputs for whether a job prompt
// should send the SHORT preset reference (#33) instead of the full preset body.
// Keeping the decision pure (no store, no clock) makes every mode x state
// combination directly testable.
type presetDeliveryInputs struct {
	// Mode is the agent's preset_delivery setting (full/referenced/auto; empty ==
	// full).
	Mode string
	// Runtime is the effective runtime the job resumes under.
	Runtime string
	// SessionRef is the runtime session reference the job resumes (agent.RuntimeRef
	// at assembly time).
	SessionRef string
	// HasPreset is true when the job actually carries a preset to reference (a
	// non-empty preset id AND snapshotted content). With no preset there is nothing
	// to shorten.
	HasPreset bool
	// HasState is true when an EXACT (runtime, session, preset id, preset commit)
	// marker exists proving the resumed session already loaded this exact preset.
	HasState bool
}

// decidePresetReference is the correctness-first core: it returns true (send the
// short reference) ONLY when every guarantee holds, and false (send the full
// preset) on ANY doubt. Rules:
//   - full / unknown / empty mode  -> full (the default; byte-identical).
//   - no preset, or a non-resumable session (empty, "last", or a fresh: ref)
//     -> full (nothing to reference, or the resumed session is ambiguous).
//   - referenced -> reference iff an exact state marker exists.
//   - auto       -> reference iff an exact state marker exists AND the runtime
//     supports persisted sessions (codex/claude); shell/kimi/custom -> full.
func decidePresetReference(in presetDeliveryInputs) bool {
	mode := normalizePresetDeliveryMode(in.Mode)
	if mode == db.PresetDeliveryFull {
		return false
	}
	if !in.HasPreset {
		return false
	}
	if !isConcreteSessionRef(in.SessionRef) {
		return false
	}
	if !in.HasState {
		return false
	}
	if mode == db.PresetDeliveryAuto && !runtimeSupportsPersistedSessions(in.Runtime) {
		return false
	}
	return true
}

// normalizePresetDeliveryMode lowercases/trims and defaults any unknown or blank
// value to full, so an unrecognized mode can never enable the optimization.
func normalizePresetDeliveryMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !db.ValidPresetDeliveryMode(mode) {
		return db.PresetDeliveryFull
	}
	return mode
}

// isConcreteSessionRef reports whether ref names a specific, resumable session we
// can key loaded-preset state on. Empty, "last" (resolves to whichever session is
// most recent — ambiguous), and a fresh: ref (a brand-new session) are all
// treated as doubt -> full.
func isConcreteSessionRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == runtime.LastRef || runtime.IsFreshRef(ref) {
		return false
	}
	return true
}

// runtimeSupportsPersistedSessions reports whether a runtime keeps session
// context across separate job invocations (the auto-mode gate). Codex and Claude
// resume a persisted thread; shell sessions are one-shot commands, and the kimi
// runtimes start a fresh session per job, so none of those carry a preset forward.
func runtimeSupportsPersistedSessions(runtimeName string) bool {
	switch strings.TrimSpace(runtimeName) {
	case runtime.CodexRuntime, runtime.ClaudeRuntime:
		return true
	default:
		return false
	}
}

// usePresetReference resolves the pure decision against the store: it looks up
// the exact loaded-preset marker only when the mode/session/preset preconditions
// already hold, so a `full` agent (the default) never issues the query and its
// path is byte-identical. A store error degrades to full (correctness-first).
func (m Mailbox) usePresetReference(ctx context.Context, agent runtime.Agent, payload JobPayload) bool {
	presetID := strings.TrimSpace(payload.TemplateID)
	hasPreset := presetID != "" && strings.TrimSpace(payload.TemplateContent) != ""
	base := presetDeliveryInputs{
		Mode:       agent.PresetDelivery,
		Runtime:    agent.Runtime,
		SessionRef: agent.RuntimeRef,
		HasPreset:  hasPreset,
	}
	// Cheap gate: if the decision is full even assuming a marker exists, skip the
	// store query entirely (this is the byte-identical default path).
	if !decidePresetReference(presetDeliveryInputsWithState(base, true)) {
		return false
	}
	if m.Store == nil {
		return false
	}
	has, err := m.Store.HasPresetSessionState(ctx, agent.Runtime, agent.RuntimeRef, presetID, payload.TemplateResolvedCommit)
	if err != nil || !has {
		return false
	}
	return decidePresetReference(presetDeliveryInputsWithState(base, true))
}

func presetDeliveryInputsWithState(in presetDeliveryInputs, hasState bool) presetDeliveryInputs {
	in.HasState = hasState
	return in
}

// recordPresetSessionState marks that THIS delivery loaded the full preset into
// the resumed session, so a later referenced/auto delivery on the same session
// can send the short reference (#33). Off by default and best-effort:
//   - it only fires for a referenced/auto agent (full agents write nothing, so
//     their behavior is byte-identical);
//   - only when the full preset was actually sent this delivery (referenceUsed
//     is false — a referenced delivery already had a marker);
//   - only when the effective resumed session ref is concrete (so the marker can
//     match the next resume);
//   - a write error never fails an otherwise-successful job.
//
// effectiveRef is the session the NEXT job will resume: the refreshed ref when a
// delivery re-pinned the session, else the agent's current ref.
func (m Mailbox) recordPresetSessionState(ctx context.Context, agent runtime.Agent, payload JobPayload, effectiveRef string, referenceUsed bool) {
	if m.Store == nil || referenceUsed {
		return
	}
	mode := normalizePresetDeliveryMode(agent.PresetDelivery)
	if mode == db.PresetDeliveryFull {
		return
	}
	presetID := strings.TrimSpace(payload.TemplateID)
	if presetID == "" || strings.TrimSpace(payload.TemplateContent) == "" {
		return
	}
	effectiveRef = strings.TrimSpace(effectiveRef)
	if !isConcreteSessionRef(effectiveRef) {
		return
	}
	_ = m.Store.RecordPresetSessionState(ctx, agent.Runtime, effectiveRef, presetID, payload.TemplateResolvedCommit)
}
