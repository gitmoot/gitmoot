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

// wakeTargetRoleProducer records the production write site and the event-rule
// kind it directs. An AST guard binds this registry to every production
// WakeTargetRole assignment so a new producer cannot silently bypass doctor.
type wakeTargetRoleProducer struct {
	File     string
	Function string
	Kind     string
}

var wakeTargetRoleProducers = []wakeTargetRoleProducer{
	{File: "internal/cli/reply_wake_outbox.go", Function: "replyWakeEvent", Kind: "reply"},
}

func eventObserverDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		return doctor.Check{}, false
	}
	return buildEventObserverDoctorCheck(rules)
}

// buildEventObserverDoctorCheck stays silent only when every event kind with a
// registered addressee producer has at least one enabled observer rule. An
// empty rule set therefore warns rather than vacuously reading as safe.
func buildEventObserverDoctorCheck(rules []db.EventRule) (doctor.Check, bool) {
	covered := make(map[string]bool)
	for _, rule := range rules {
		if rule.Enabled && rule.Scope == db.EventRuleScopeObserver {
			covered[strings.TrimSpace(rule.OnKind)] = true
		}
	}
	kinds := make(map[string]struct{})
	for _, producer := range wakeTargetRoleProducers {
		kind := strings.TrimSpace(producer.Kind)
		if kind != "" {
			kinds[kind] = struct{}{}
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
