package cli

import (
	"context"
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

// eventSinkCache holds the process-global webhook Sink and rule decorator the
// daemon shares across every per-tick / per-repo engine construction (#446).
// The webhook sink
// owns a long-lived drain goroutine, so it MUST be constructed once — building a
// fresh one per engine would leak a goroutine per poll. It is keyed by resolved
// home so a multi-home test process never crosses streams; in the normal single-
// daemon deployment there is exactly one entry. A cached nil entry records "this
// home has [events] OFF" so the disabled path stays a cheap map hit with no
// re-parse and, crucially, no sink/goroutine — the off-by-default guarantee.
var eventSinkCache = struct {
	sync.Mutex
	webhooks map[string]events.Sink
	rules    map[string]*eventRuleSink
}{webhooks: map[string]events.Sink{}, rules: map[string]*eventRuleSink{}}

// daemonEventSink composes the shared best-effort webhook Sink with the opt-in
// organization rule evaluator. It returns nil when both features are OFF (no
// [events].webhook_url and zero enabled event_rules rows). Config/store failures
// fail safe to disabled and never break the daemon. It is safe for concurrent
// callers and constructs the underlying webhook sink (and its drain goroutine)
// at most once per home.
//
// When enabled, the sink's OnDrop records a single best-effort event_sink_drop
// job event so a dropped emit (full buffer / dead consumer) is locally
// observable without coupling the events package to the db layer or ever
// blocking the caller.
func daemonEventSink(store *db.Store, home string) events.Sink {
	home = strings.TrimSpace(home)
	eventSinkCache.Lock()
	webhook, built := eventSinkCache.webhooks[home]
	if !built {
		webhook = buildDaemonEventSink(store, home)
		eventSinkCache.webhooks[home] = webhook
	}
	eventSinkCache.Unlock()

	// Event rules can be added or removed while the daemon is running. Probe the
	// lightweight table when each per-tick engine is built; zero enabled rows
	// preserve the exact historical sink (including nil when webhooks are off).
	if store == nil {
		return webhook
	}
	rules, err := store.ListEventRules(context.Background())
	if err != nil || !hasEnabledEventRule(rules) {
		return webhook
	}
	key := home + "\x00" + store.DatabasePath()
	eventSinkCache.Lock()
	defer eventSinkCache.Unlock()
	if sink := eventSinkCache.rules[key]; sink != nil {
		return sink
	}
	sink := &eventRuleSink{
		inner: webhook,
		store: store,
		home:  home,
		wake:  cockpit.New(cockpit.Options{HerdrBin: "herdr"}, nil),
	}
	eventSinkCache.rules[key] = sink
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
	cfg := resolveConfigFile(home)
	if cfg == "" {
		return config.DefaultEventsPolicy(), nil
	}
	return config.LoadEventsPolicy(config.Paths{ConfigFile: cfg})
}

// resolveConfigFile resolves the gitmoot config.toml for a `home` that may be
// EITHER an already-resolved <home>/.gitmoot ROOT or a RAW --home, returning ""
// for an empty home. It is the single, side-effect-free home->config.toml
// resolver shared by the daemon's read-only policy loaders (loadEventsPolicy,
// resolveEscalationTTL).
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
		return ""
	}
	cfg := filepath.Join(home, config.ConfigName)
	if _, err := os.Stat(cfg); err != nil {
		// `home` was the raw --home (no config.toml directly under it); append the
		// .gitmoot dir as PathsForHome would. Side-effect-free (no Initialize).
		cfg = config.PathsForHome(home).ConfigFile
	}
	return cfg
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
func emitDaemonTerminalEvent(ctx context.Context, sink events.Sink, store *db.Store, jobID string, eventType events.EventType, status, detail string, cause ...string) {
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
	event := events.NewEvent(
		eventType,
		jobID,
		rootID,
		repo,
		status,
		detail,
		time.Time{},
		workflow.RedactCommentText,
	)
	if len(cause) > 0 {
		event.Cause = strings.TrimSpace(cause[0])
	}
	events.EmitEvent(ctx, sink, event)
}
