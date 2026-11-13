package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// eventSinkCache holds the process-global rule decorator the daemon shares
// across every per-tick / per-repo engine construction. It is keyed by resolved
// home plus database path so a multi-home test process never crosses streams.
var eventSinkCache = struct {
	sync.Mutex
	rules map[string]*eventRuleSink
}{rules: map[string]*eventRuleSink{}}

// daemonEventSink returns the organization event-rule evaluator for the store,
// or nil when there is no store. Store failures fail safe and never break the
// daemon. It is safe for concurrent callers.
func daemonEventSink(store *db.Store, home string) events.Sink {
	sink, _ := resolveDaemonEventSink(context.Background(), store, home)
	return sink
}

// resolveDaemonEventSink exposes event-rule read failures to callers that own
// durable delivery. Ordinary event emission uses daemonEventSink's best-effort
// wrapper; the wake outbox tick must distinguish "rules disabled" from "rules
// unreadable" so pending delivery work never looks healthy.
func resolveDaemonEventSink(ctx context.Context, store *db.Store, home string) (events.Sink, error) {
	if store == nil {
		return resolveDaemonEventSinkWithRules(store, home, nil), nil
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return resolveDaemonEventSinkWithRules(store, home, nil), fmt.Errorf("list event rules: %w", err)
	}
	return resolveDaemonEventSinkWithRules(store, home, rules), nil
}

// resolveDaemonEventSinkWithRules builds the delivery sink from a rules
// snapshot already read by the caller. The durable outbox uses this form so it
// never re-reads routing state after claiming rows.
func resolveDaemonEventSinkWithRules(store *db.Store, home string, rules []db.EventRule) events.Sink {
	if store == nil {
		return nil
	}
	home = strings.TrimSpace(home)
	key := home + "\x00" + store.DatabasePath()
	eventSinkCache.Lock()
	defer eventSinkCache.Unlock()
	if sink := eventSinkCache.rules[key]; sink != nil {
		return sink
	}
	sink := &eventRuleSink{
		store: store,
		home:  home,
		wake:  cockpit.New(cockpit.Options{HerdrBin: "herdr", OMPRuntimeDir: ompRuntimeDir(home)}),
	}
	eventSinkCache.rules[key] = sink
	return sink
}

// ompRuntimeDir is the OMP inbox add-on registry of the home whose config the
// daemon routes by, so a --home daemon only dials add-ons installed for it.
func ompRuntimeDir(home string) string {
	configFile := resolveConfigFile(home)
	if configFile == "" {
		return ""
	}
	return config.Paths{Home: filepath.Dir(configFile)}.OMPRuntimeDir()
}

// resolveConfigFile resolves the gitmoot config.toml for a `home` that may be
// EITHER an already-resolved <home>/.gitmoot ROOT or a RAW --home. An empty
// home resolves through config.DefaultPaths, matching the CLI's omitted --home
// behavior. It is the single, side-effect-free home->config.toml resolver
// shared by the daemon's read-only policy loaders (resolveBlockedTTL).
//
// On main the daemon's engine wiring (daemonWorkflowEngine -> daemonEventSink)
// receives the already-resolved <home>/.gitmoot root on ALL callers
// (jobWorker.workflowHome(), the registered-repo supervisor's paths.Home, and
// local dispatch's paths.Home), so the common case is the resolved-root branch.
// The probe also tolerates a RAW --home (no config.toml directly under it) by
// appending the .gitmoot dir as PathsForHome would — kept as defense in depth so
// a caller mistake can never re-introduce the #446 silent-off bug.
//
// It MUST stay side-effect-free: it never runs pathsFromFlag/initializedPaths
// (which re-appended ".gitmoot" a SECOND time, read a phantom
// .gitmoot/.gitmoot/config.toml with no [events]/[orchestrate] section, and even
// created that phantom home; #446/#459).
func resolveConfigFile(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		paths, err := config.DefaultPaths()
		if err != nil {
			return ""
		}
		return paths.ConfigFile
	}
	cfg := filepath.Join(home, config.ConfigName)
	if _, err := os.Stat(cfg); err != nil {
		// `home` was the raw --home (no config.toml directly under it); append the
		// .gitmoot dir as PathsForHome would. Side-effect-free (no Initialize).
		cfg = config.PathsForHome(home).ConfigFile
	}
	return cfg
}

type daemonTerminalEmissionKind uint8

const (
	daemonTerminalFailed daemonTerminalEmissionKind = iota
	daemonTerminalBlocked
	daemonTerminalPermissionGuard
	daemonTerminalAdvanceBlocked
	daemonTerminalDeferred
	daemonTerminalEmissionKindCount
)

type daemonTerminalEventSpec struct {
	eventType events.EventType
	cause     string
}

func (kind daemonTerminalEmissionKind) spec() (daemonTerminalEventSpec, bool) {
	switch kind {
	case daemonTerminalFailed:
		return daemonTerminalEventSpec{eventType: events.EventJobFailed}, true
	case daemonTerminalBlocked:
		return daemonTerminalEventSpec{eventType: events.EventJobBlocked}, true
	case daemonTerminalPermissionGuard:
		return daemonTerminalEventSpec{eventType: events.EventJobBlocked, cause: "permission_guard"}, true
	case daemonTerminalAdvanceBlocked:
		return daemonTerminalEventSpec{eventType: events.EventJobBlocked, cause: "advance_blocked"}, true
	case daemonTerminalDeferred:
		return daemonTerminalEventSpec{eventType: events.EventJobDeferred}, true
	default:
		return daemonTerminalEventSpec{}, false
	}
}

// daemonTerminalEventKind maps daemon-owned terminal job states onto the
// emission kinds accepted by emitDaemonTerminalEvent. Succeeded jobs remain
// engine-owned.
func daemonTerminalEventKind(state workflow.JobState) (daemonTerminalEmissionKind, bool) {
	switch state {
	case workflow.JobFailed:
		return daemonTerminalFailed, true
	case workflow.JobBlocked:
		return daemonTerminalBlocked, true
	default:
		return 0, false
	}
}

// daemonTerminalWakeDirectedKinds derives doctor coverage from every emission
// kind the producer accepts. The count sentinel makes additions enter this loop,
// while spec and per-kind doctor tests reject unmapped or uncovered kinds.
func daemonTerminalWakeDirectedKinds() []string {
	seen := make(map[string]struct{})
	var kinds []string
	for emission := daemonTerminalEmissionKind(0); emission < daemonTerminalEmissionKindCount; emission++ {
		spec, ok := emission.spec()
		if !ok {
			continue
		}
		for _, kind := range classifyEventRuleKinds(events.Event{Type: spec.eventType, Cause: spec.cause}) {
			if _, exists := seen[kind]; exists {
				continue
			}
			seen[kind] = struct{}{}
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// emitDaemonTerminalEvent emits a best-effort terminal event for a job the
// DAEMON (not the engine) just transitioned to a terminal state — the pre-flight
// queued->terminal cases and the permission-blocked running->blocked case that
// never pass through the engine's Mailbox.finishWithPayload chokepoint (#446). It
// is nil-safe (no sink => no-op), redacts via workflow.RedactCommentText, and
// resolves root_id from the payload (falling back to the job id). It must only be
// called on a GENUINE transition so the engine and daemon never double-emit for
// the same terminal state.
func emitDaemonTerminalEvent(ctx context.Context, sink events.Sink, store *db.Store, jobID string, emission daemonTerminalEmissionKind, status, detail string) {
	spec, ok := emission.spec()
	if sink == nil || !ok {
		return
	}
	repo := ""
	rootID := jobID
	wakeTargetRole := ""
	if store != nil {
		if job, err := store.GetJob(ctx, jobID); err == nil {
			if payload, perr := daemonJobPayload(job); perr == nil {
				repo = payload.Repo
				wakeTargetRole = workflow.NormalizeActingOrgRole(payload.ActingOrgRole)
				if strings.TrimSpace(payload.RootJobID) != "" {
					rootID = payload.RootJobID
				}
			}
		}
	}
	event := events.NewEvent(
		spec.eventType,
		jobID,
		rootID,
		repo,
		status,
		detail,
		time.Time{},
		workflow.RedactCommentText,
	)
	if spec.cause != "" {
		event.Cause = spec.cause
	}
	if wakeTargetRole != "" {
		event.WakeTargetRole = wakeTargetRole
	}
	events.EmitEvent(ctx, sink, event)
}
