package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// eventSinkCache holds the single, process-global webhook Sink the daemon shares
// across every per-tick / per-repo engine construction (#446). The webhook sink
// owns a long-lived drain goroutine, so it MUST be constructed once — building a
// fresh one per engine would leak a goroutine per poll. It is keyed by resolved
// home so a multi-home test process never crosses streams; in the normal single-
// daemon deployment there is exactly one entry. A cached nil entry records "this
// home has [events] OFF" so the disabled path stays a cheap map hit with no
// re-parse and, crucially, no sink/goroutine — the off-by-default guarantee.
var eventSinkCache = struct {
	sync.Mutex
	built map[string]events.Sink
}{built: map[string]events.Sink{}}

// daemonEventSink returns the shared best-effort webhook Sink for this home, or
// nil when the event stream is OFF (no [events].webhook_url, or any config load
// failure — fail-safe to disabled so a malformed config never breaks the daemon
// or silently starts emitting). It is safe for concurrent callers and constructs
// the underlying webhook sink (and its drain goroutine) at most once per home.
//
// When enabled, the sink's OnDrop records a single best-effort event_sink_drop
// job event so a dropped emit (full buffer / dead consumer) is locally
// observable without coupling the events package to the db layer or ever
// blocking the caller.
func daemonEventSink(store *db.Store, home string) events.Sink {
	home = strings.TrimSpace(home)
	eventSinkCache.Lock()
	defer eventSinkCache.Unlock()
	if sink, ok := eventSinkCache.built[home]; ok {
		return sink
	}
	sink := buildDaemonEventSink(store, home)
	eventSinkCache.built[home] = sink
	return sink
}

func buildDaemonEventSink(store *db.Store, home string) events.Sink {
	policy, err := loadEventsPolicy(home)
	if err != nil || !policy.Enabled() {
		return nil
	}
	webhook := events.NewWebhookSink(policy.WebhookURL, policy.ResolvedTimeout())
	if webhook == nil {
		return nil
	}
	if store != nil {
		webhook.OnDrop = func(event events.Event, reason string) {
			// Best-effort local observability for a dropped outbound event; never
			// blocks or fails (the drain goroutine is the only caller). A write
			// error is swallowed.
			_ = store.AddJobEvent(context.Background(), db.JobEvent{
				JobID:   event.JobID,
				Kind:    "event_sink_drop",
				Message: string(event.Type) + ": " + reason,
			})
		}
	}
	return webhook
}

// loadEventsPolicy resolves the [events] policy for a home, fail-safe to the
// disabled default when the home or config cannot be resolved/parsed so the
// event stream stays OFF rather than erroring the daemon.
func loadEventsPolicy(home string) (config.EventsPolicy, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return config.DefaultEventsPolicy(), nil
	}
	// The daemon resolves `home` to two different shapes depending on the call
	// path: jobWorker.workflowHome() yields the already-resolved <home>/.gitmoot
	// ROOT, while the registered-repo supervisor passes the RAW --home value — and
	// both flow into daemonWorkflowEngine -> daemonEventSink. Resolve robustly to
	// the config.toml for EITHER shape, WITHOUT re-running pathsFromFlag/
	// initializedPaths (which appended ".gitmoot" a SECOND time, read a phantom
	// .gitmoot/.gitmoot/config.toml that has no [events] section, and even created
	// that phantom home — leaving the event stream silently always-off even when
	// [events].webhook_url was set; #446 regression caught by a live E2E).
	cfg := filepath.Join(home, config.ConfigName)
	if _, err := os.Stat(cfg); err != nil {
		// `home` was the raw --home (no config.toml directly under it); append the
		// .gitmoot dir as PathsForHome would. Side-effect-free (no Initialize).
		cfg = config.PathsForHome(home).ConfigFile
	}
	return config.LoadEventsPolicy(config.Paths{ConfigFile: cfg})
}

// daemonTerminalEventType maps a daemon-owned terminal JobState to the outbound
// event_type (#446). Only failed/blocked map (the succeeded path is engine-owned
// via the Mailbox chokepoint); any other state returns ok=false.
func daemonTerminalEventType(state workflow.JobState) (events.EventType, bool) {
	switch state {
	case workflow.JobFailed:
		return events.EventJobFailed, true
	case workflow.JobBlocked:
		return events.EventJobBlocked, true
	default:
		return "", false
	}
}

// emitDaemonTerminalEvent emits a best-effort terminal event for a job the
// DAEMON (not the engine) just transitioned to a terminal state — the pre-flight
// queued->terminal cases and the permission-blocked running->blocked case that
// never pass through the engine's Mailbox.finishWithPayload chokepoint (#446). It
// is nil-safe (no sink => no-op), redacts via workflow.RedactCommentText, and
// resolves root_id from the payload (falling back to the job id). It must only be
// called on a GENUINE transition so the engine and daemon never double-emit for
// the same terminal state.
func emitDaemonTerminalEvent(ctx context.Context, sink events.Sink, store *db.Store, jobID string, eventType events.EventType, status, detail string) {
	if sink == nil {
		return
	}
	repo := ""
	rootID := jobID
	if store != nil {
		if job, err := store.GetJob(ctx, jobID); err == nil {
			if payload, perr := daemonJobPayload(job); perr == nil {
				repo = payload.Repo
				if strings.TrimSpace(payload.RootJobID) != "" {
					rootID = payload.RootJobID
				}
			}
		}
	}
	events.EmitEvent(ctx, sink, events.NewEvent(
		eventType,
		jobID,
		rootID,
		repo,
		status,
		detail,
		time.Time{},
		workflow.RedactCommentText,
	))
}
