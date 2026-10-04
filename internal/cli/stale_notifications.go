package cli

import (
	"context"
	"fmt"
	"os"
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
type staleNotificationReport struct {
	Threshold time.Duration
	Total     int
	Roles     []staleNotificationRole
}

type staleNotificationRole struct {
	Role            string
	Count           int
	OldestCreatedAt time.Time
	OldestAge       time.Duration
	// Reason is the plain-language last reason the role was not notified.
	Reason string
}

const staleNotificationNoReason = "no delivery attempt recorded yet"

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
	rows, err := store.ListStaleNotifications(ctx, now.Add(-threshold))
	if err != nil {
		return staleNotificationReport{}, fmt.Errorf("list stale notifications: %w", err)
	}
	report := staleNotificationReport{Threshold: threshold, Roles: make([]staleNotificationRole, 0, len(rows))}
	for _, row := range rows {
		age := now.Sub(row.OldestCreatedAt)
		if age < 0 {
			age = 0
		}
		reason := strings.TrimSpace(row.LastError)
		if reason == "" {
			reason = staleNotificationNoReason
		}
		report.Total += row.Count
		report.Roles = append(report.Roles, staleNotificationRole{
			Role:            row.Role,
			Count:           row.Count,
			OldestCreatedAt: row.OldestCreatedAt,
			OldestAge:       age,
			Reason:          reason,
		})
	}
	return report, nil
}

// Summary is the one-line plain-language form used by doctor and daemon status.
func (r staleNotificationReport) Summary() string {
	threshold := formatOrgRecycleAfter(r.Threshold)
	if r.Total == 0 {
		return fmt.Sprintf("none waiting longer than %s", threshold)
	}
	parts := make([]string, 0, len(r.Roles))
	for _, role := range r.Roles {
		parts = append(parts, fmt.Sprintf("%s: %d waiting, oldest %s, last reason: %s",
			role.Role, role.Count, formatStaleNotificationAge(role.OldestAge), role.Reason))
	}
	noun := "notifications"
	if r.Total == 1 {
		noun = "notification"
	}
	return fmt.Sprintf("%d %s waiting longer than %s (%s)", r.Total, noun, threshold, strings.Join(parts, "; "))
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
