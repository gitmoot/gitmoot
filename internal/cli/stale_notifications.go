package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
)

// staleNotificationReport is the one read-only view of notifications that
// have waited longer than [org].notification_stale_after (#2303). The
// dashboard ("Needs a human" and Comms), `gitmoot doctor` and
// `gitmoot daemon status` all render it, so they can never disagree about
// what is stale. Building it never changes a delivery state or retries one.
//
// Rows the daemon leaves pending ON PURPOSE (#2309) are not stale: a route the
// operator muted, or a row no delivery rule routes. They are reported apart in
// PendingOnPurpose, never in Total or Roles, so a deliberate mute does not
// raise a permanent warning.
type staleNotificationReport struct {
	Threshold        time.Duration
	Total            int
	Roles            []staleNotificationRole
	PendingOnPurpose []staleNotificationRole
}

type staleNotificationRole struct {
	Role            string
	Count           int
	OldestCreatedAt time.Time
	OldestAge       time.Duration
	// Reason is the plain-language last reason the role was not notified. For
	// a PendingOnPurpose entry it is why the daemon holds the rows back.
	Reason string
	// lastReasonAt orders recorded reasons while the report is built.
	lastReasonAt time.Time
}

const (
	staleNotificationNoReason = "no delivery attempt recorded yet"
	// The labels for rows left pending on purpose, matching what the daemon's
	// drain health reports as suppressed and inert/route_removed.
	staleNotificationMuted          = "muted"
	staleNotificationNoDeliveryRule = "no delivery rule"
)

// staleNotificationThreshold resolves [org].notification_stale_after. A
// malformed org config fails open to the documented default so the flag
// keeps working; the config error itself is reported by the org commands.
func staleNotificationThreshold(paths config.Paths) time.Duration {
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return config.DefaultNotificationStaleAfter
	}
	return cfg.NotificationStaleAfter()
}

func loadStaleNotificationReport(ctx context.Context, store *db.Store, threshold time.Duration, now time.Time) (staleNotificationReport, error) {
	stale, err := store.ListStaleNotifications(ctx, now.Add(-threshold))
	if err != nil {
		return staleNotificationReport{}, fmt.Errorf("list stale notifications: %w", err)
	}
	report := staleNotificationReport{Threshold: threshold, Roles: []staleNotificationRole{}, PendingOnPurpose: []staleNotificationRole{}}
	if len(stale.Pending) == 0 {
		return report, nil
	}
	// The same rules snapshot and per-row classification the daemon's drain
	// uses (jobWorker.replyWakeDelivery, classifyWakeOutboxObligations).
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return staleNotificationReport{}, fmt.Errorf("list event rules: %w", err)
	}
	type groupKey struct{ role, label string }
	groups := map[groupKey]*staleNotificationRole{}
	for _, row := range stale.Pending {
		route, _, err := classifyPendingWakeRoute(rules, row, now)
		if err != nil {
			// A row the drain cannot even decode is stuck, not held back on
			// purpose: keep it in the warning rather than hide the whole report.
			route = pendingWakeDeliverable
		}
		created, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err != nil {
			return staleNotificationReport{}, fmt.Errorf("parse stale notification created_at for row %d: %w", row.ID, err)
		}
		key := groupKey{role: row.TargetRole}
		switch route {
		case pendingWakeMuted:
			key.label = staleNotificationMuted
		case pendingWakeUnroutable:
			key.label = staleNotificationNoDeliveryRule
		}
		entry, ok := groups[key]
		if !ok {
			entry = &staleNotificationRole{Role: row.TargetRole, OldestCreatedAt: created.UTC(), Reason: key.label}
			groups[key] = entry
		}
		entry.Count++
		if created.Before(entry.OldestCreatedAt) {
			entry.OldestCreatedAt = created.UTC()
		}
		if key.label != "" {
			continue
		}
		report.Total++
		// The reason shown is the most recently recorded one among the role's
		// stale rows.
		if reason := strings.TrimSpace(row.LastError); reason != "" {
			updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
			if err != nil {
				return staleNotificationReport{}, fmt.Errorf("parse stale notification updated_at for row %d: %w", row.ID, err)
			}
			if entry.Reason == "" || !updated.Before(entry.lastReasonAt) {
				entry.Reason, entry.lastReasonAt = reason, updated
			}
		}
	}
	for key, entry := range groups {
		entry.OldestAge = max(now.Sub(entry.OldestCreatedAt), 0)
		if entry.Reason == "" {
			entry.Reason = staleNotificationNoReason
		}
		if key.label == "" {
			report.Roles = append(report.Roles, *entry)
		} else {
			report.PendingOnPurpose = append(report.PendingOnPurpose, *entry)
		}
	}
	// Oldest role first.
	for _, roles := range [][]staleNotificationRole{report.Roles, report.PendingOnPurpose} {
		sort.Slice(roles, func(i, j int) bool {
			if !roles[i].OldestCreatedAt.Equal(roles[j].OldestCreatedAt) {
				return roles[i].OldestCreatedAt.Before(roles[j].OldestCreatedAt)
			}
			if roles[i].Role != roles[j].Role {
				return roles[i].Role < roles[j].Role
			}
			return roles[i].Reason < roles[j].Reason
		})
	}
	return report, nil
}

// Summary is the one-line plain-language form used by doctor and daemon status.
// Rows left pending on purpose are named after the stale count, never in it.
func (r staleNotificationReport) Summary() string {
	threshold := formatOrgRecycleAfter(r.Threshold)
	summary := fmt.Sprintf("none waiting longer than %s", threshold)
	if r.Total > 0 {
		parts := make([]string, 0, len(r.Roles))
		for _, role := range r.Roles {
			parts = append(parts, fmt.Sprintf("%s: %d waiting, oldest %s, last reason: %s",
				role.Role, role.Count, formatStaleNotificationAge(role.OldestAge), role.Reason))
		}
		noun := "notifications"
		if r.Total == 1 {
			noun = "notification"
		}
		summary = fmt.Sprintf("%d %s waiting longer than %s (%s)", r.Total, noun, threshold, strings.Join(parts, "; "))
	}
	if len(r.PendingOnPurpose) == 0 {
		return summary
	}
	held := 0
	parts := make([]string, 0, len(r.PendingOnPurpose))
	for _, role := range r.PendingOnPurpose {
		held += role.Count
		parts = append(parts, fmt.Sprintf("%s: %d %s", role.Role, role.Count, role.Reason))
	}
	return fmt.Sprintf("%s; %d left pending on purpose, not counted (%s)", summary, held, strings.Join(parts, "; "))
}

// formatStaleNotificationAge renders an age in the largest two units an
// operator reads at a glance ("2d3h", "1h5m", "42m", "<1m").
func formatStaleNotificationAge(age time.Duration) string {
	minutes := int64(age / time.Minute)
	switch {
	case minutes < 1:
		return "<1m"
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	case minutes < 48*60:
		if minutes%60 == 0 {
			return fmt.Sprintf("%dh", minutes/60)
		}
		return fmt.Sprintf("%dh%dm", minutes/60, minutes%60)
	default:
		hours := minutes / 60
		if hours%24 == 0 {
			return fmt.Sprintf("%dd", hours/24)
		}
		return fmt.Sprintf("%dd%dh", hours/24, hours%24)
	}
}

const staleNotificationsCheckName = "stale notifications"

// readStaleNotificationReport opens the home's store read-only. ok is false
// for a home with no database yet: there is no inbox to inspect, and status
// and doctor must not create one as a side effect.
func readStaleNotificationReport(paths config.Paths, now time.Time) (report staleNotificationReport, ok bool, err error) {
	if strings.TrimSpace(paths.Database) == "" {
		return staleNotificationReport{}, false, nil
	}
	if _, statErr := os.Stat(paths.Database); statErr != nil {
		return staleNotificationReport{}, false, nil
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return staleNotificationReport{}, true, err
	}
	defer store.Close()
	report, err = loadStaleNotificationReport(context.Background(), store, staleNotificationThreshold(paths), now)
	return report, true, err
}

// staleNotificationsDoctorCheck warns (never fails) when any notification has
// waited past the threshold.
func staleNotificationsDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	report, ok, err := readStaleNotificationReport(paths, time.Now().UTC())
	if !ok {
		return doctor.Check{}, false
	}
	if err != nil {
		return doctor.Check{Name: staleNotificationsCheckName, Detail: "unverified: " + err.Error()}, true
	}
	return doctor.Check{Name: staleNotificationsCheckName, OK: report.Total == 0, Detail: report.Summary()}, true
}

// daemonStaleNotificationsLine is the `gitmoot daemon status` health line; ""
// when the home has no database yet.
func daemonStaleNotificationsLine(paths config.Paths) string {
	report, ok, err := readStaleNotificationReport(paths, time.Now().UTC())
	switch {
	case !ok:
		return ""
	case err != nil:
		return "stale notifications: unavailable (" + err.Error() + ")"
	case report.Total == 0:
		return "stale notifications: " + report.Summary()
	default:
		return "WARNING: stale notifications: " + report.Summary()
	}
}
