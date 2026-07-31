package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

// wakeTargetRoleProducer records a production write site and derives every
// event-rule kind it directs from that producer's source-kind definition. An
// AST guard binds this registry to every production
// WakeTargetRole assignment so a new producer cannot silently bypass doctor.
type wakeTargetRoleProducer struct {
	File     string
	Function string
	Kinds    func() []string
}

var wakeTargetRoleProducers = []wakeTargetRoleProducer{
	{
		File:     "internal/cli/event_rule_sink.go",
		Function: "addressBlockedEvent",
		Kinds:    blockedWakeDirectedKinds,
	},
	{
		File:     "internal/cli/event_sink.go",
		Function: "emitDaemonTerminalEvent",
		Kinds:    blockedWakeDirectedKinds,
	},
	{
		File:     "internal/cli/blocked_since.go",
		Function: "emitInputPendingEpisode",
		Kinds:    inputPendingDirectedKinds,
	},
	{
		File:     "internal/cli/reply_wake_outbox.go",
		Function: "wakeOutboxEvent",
		Kinds:    wakeOutboxDirectedKinds,
	},
}

func blockedWakeDirectedKinds() []string {
	return []string{db.WakeOutboxKindBlocked}
}

func inputPendingDirectedKinds() []string {
	return []string{"pane_input_pending"}
}

func eventObserverDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return unreadableEventObserverCheck("database path is empty"), true
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return unreadableEventObserverCheck(fmt.Sprintf("open event rule store: %v", err)), true
	}
	defer store.Close()
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		return unreadableEventObserverCheck(fmt.Sprintf("list event rules: %v", err)), true
	}
	return buildEventObserverDoctorCheck(rules)
}

func unreadableEventObserverCheck(detail string) doctor.Check {
	return doctor.Check{
		Name:     "event observers",
		Required: false,
		Detail:   "observer coverage unverified: " + detail,
	}
}

// buildEventObserverDoctorCheck stays silent only when every event kind with a
// registered addressee producer has at least one enabled observer rule. An
// empty rule set therefore warns rather than vacuously reading as safe.
func buildEventObserverDoctorCheck(rules []db.EventRule) (doctor.Check, bool) {
	covered := make(map[string]bool)
	for _, rule := range rules {
		if rule.Enabled && rule.Scope == db.EventRuleScopeObserver {
			covered[strings.ToLower(strings.TrimSpace(rule.OnKind))] = true
		}
	}
	kinds := make(map[string]struct{})
	for _, producer := range wakeTargetRoleProducers {
		for _, rawKind := range producer.Kinds() {
			kind := strings.ToLower(strings.TrimSpace(rawKind))
			if kind != "" {
				kinds[kind] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(kinds))
	for kind := range kinds {
		if !covered[kind] {
			missing = append(missing, kind)
		}
	}
	if len(missing) == 0 {
		return doctor.Check{}, false
	}
	sort.Strings(missing)
	return doctor.Check{
		Name:     "event observers",
		Required: false,
		Detail: fmt.Sprintf(
			"directed event kind(s) have no enabled observer-scoped rule: %s — run: gitmoot org events rule set-scope <id> observer",
			strings.Join(missing, ", "),
		),
	}, true
}
