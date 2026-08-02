package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

const orgRoleActivityDoctorCheckName = "org role activity"

// orgRoleActivityDoctorCheck reports absence of activity for roles whose
// recycle_after policy makes that absence meaningful. This is deliberately a
// deterministic, read-only check; it is unrelated to scheduled agent
// heartbeats, which enqueue LLM jobs.
func orgRoleActivityDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return unverifiedOrgRoleActivityCheck(fmt.Sprintf("load org registry: %v", err)), true
	}
	if len(monitoredOrgActivityRoles(cfg)) == 0 {
		return doctor.Check{}, false
	}
	if strings.TrimSpace(paths.Database) == "" {
		return unverifiedOrgRoleActivityCheck("database path is empty"), true
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return unverifiedOrgRoleActivityCheck(fmt.Sprintf("open presence store: %v", err)), true
	}
	defer store.Close()
	presence, err := store.ListOrgRolePresence(context.Background())
	if err != nil {
		return unverifiedOrgRoleActivityCheck(fmt.Sprintf("list org role presence: %v", err)), true
	}
	return buildOrgRoleActivityDoctorCheck(cfg, presence, time.Now().UTC()), true
}

func monitoredOrgActivityRoles(cfg config.OrgConfig) []config.OrgRole {
	roles := make([]config.OrgRole, 0)
	for _, role := range cfg.Roles() {
		if cfg.RecycleAfterFor(role.Name) > 0 {
			roles = append(roles, role)
		}
	}
	return roles
}

func buildOrgRoleActivityDoctorCheck(cfg config.OrgConfig, presence []db.OrgRolePresence, now time.Time) doctor.Check {
	byRole := make(map[string]db.OrgRolePresence, len(presence))
	for _, row := range presence {
		byRole[strings.ToLower(strings.TrimSpace(row.Role))] = row
	}

	var overdue, neverObserved, unverified []string
	for _, role := range monitoredOrgActivityRoles(cfg) {
		threshold := cfg.RecycleAfterFor(role.Name)
		row, found := byRole[role.Name]
		if !found {
			neverObserved = append(neverObserved, fmt.Sprintf("%s (recycle_after %s)", role.Name, formatOrgRecycleAfter(threshold)))
			continue
		}
		observed, ok := parseOrgPresenceTime(row.LastSeenAt)
		if !ok {
			unverified = append(unverified, fmt.Sprintf("%s (invalid last_seen_at %q)", role.Name, row.LastSeenAt))
			continue
		}
		observed = observed.UTC()
		if observed.After(now) {
			unverified = append(unverified, fmt.Sprintf("%s (future last_seen_at %s)", role.Name, observed.Format(time.RFC3339)))
			continue
		}
		age := now.Sub(observed)
		if age >= threshold {
			overdue = append(overdue, fmt.Sprintf("%s (idle %s, recycle_after %s)", role.Name, age.Round(time.Second), formatOrgRecycleAfter(threshold)))
		}
	}
	sort.Strings(overdue)
	sort.Strings(neverObserved)
	sort.Strings(unverified)

	parts := make([]string, 0, 3)
	if len(overdue) > 0 {
		parts = append(parts, "overdue: "+strings.Join(overdue, ", "))
	}
	if len(neverObserved) > 0 {
		parts = append(parts, "never observed: "+strings.Join(neverObserved, ", "))
	}
	if len(unverified) > 0 {
		parts = append(parts, "unverified: "+strings.Join(unverified, ", "))
	}
	if len(parts) > 0 {
		return doctor.Check{
			Name:     orgRoleActivityDoctorCheckName,
			Required: false,
			Detail:   strings.Join(parts, "; "),
		}
	}
	return doctor.Check{
		Name:     orgRoleActivityDoctorCheckName,
		OK:       true,
		Required: false,
		Detail:   fmt.Sprintf("all %d monitored org roles reported activity within recycle_after", len(monitoredOrgActivityRoles(cfg))),
	}
}

func unverifiedOrgRoleActivityCheck(detail string) doctor.Check {
	return doctor.Check{
		Name:     orgRoleActivityDoctorCheckName,
		Required: false,
		Detail:   "org role activity unverified: " + detail,
	}
}
