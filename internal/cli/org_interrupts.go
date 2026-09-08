package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// orgInterruptShortGap is the threshold the campaign measured against: a seat
// whose wakes arrive closer together than this cannot hold a long piece of
// work, and the two worst-affected seats sat at a 4.2 and 4.8 minute MEDIAN
// gap (#1977).
const orgInterruptShortGap = 5 * time.Minute

const orgInterruptDefaultWindow = 7 * 24 * time.Hour

// orgInterruptSeat is one seat's interrupt profile over the window.
type orgInterruptSeat struct {
	Role             string  `json:"role"`
	Wakes            int     `json:"wakes"`
	PerDay           float64 `json:"per_day"`
	MedianGapSeconds float64 `json:"median_gap_seconds"`
	Gaps             int     `json:"gaps"`
	GapsUnderShort   int     `json:"gaps_under_short_threshold"`
	// Collapsed counts wakes this seat did NOT receive because coalescing
	// carried them on another row. It is the #1978 saving, per seat.
	Collapsed  int            `json:"collapsed"`
	Sources    map[string]int `json:"sources"`
	States     map[string]int `json:"states"`
	Delivered  int            `json:"delivered"`
	Unproven   int            `json:"unproven"`
	Pending    int            `json:"pending"`
	Routeless  int            `json:"routeless_pending"`
	OldestRLAt string         `json:"oldest_routeless_at,omitempty"`
	// CompletionNags is how many completion-phase nudges this seat was sent
	// for directives created in the window (#1979's population).
	CompletionNags int `json:"completion_nags"`
	AckNudges      int `json:"acknowledgment_nudges"`
}

type orgInterruptReport struct {
	WindowStart      string             `json:"window_start"`
	WindowEnd        string             `json:"window_end"`
	WindowHours      float64            `json:"window_hours"`
	ShortGapSeconds  float64            `json:"short_gap_threshold_seconds"`
	Wakes            int                `json:"wakes"`
	Collapsed        int                `json:"collapsed"`
	Unproven         int                `json:"unproven"`
	RoutelessPending int                `json:"routeless_pending"`
	Seats            []orgInterruptSeat `json:"seats"`
	// RouteHistoryStart is the oldest recorded route deletion. Anything before
	// it has no route history, so an unroutable row from that era reads as
	// never-configured even if its seat was retired. Empty means NO route
	// history is recorded at all.
	RouteHistoryStart  string         `json:"route_history_start,omitempty"`
	RoutelessByKindLbl map[string]int `json:"routeless_pending_by_kind,omitempty"`
}

func runOrgInterrupts(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org interrupts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	window := fs.String("window", "7d", "window to report over, for example 24h or 7d; 0 reports every recorded wake")
	jsonOutput := fs.Bool("json", false, "emit the report as JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "org interrupts: %v\n", err)
		return 2
	}
	span, err := parseOrgInterruptWindow(*window)
	if err != nil {
		fmt.Fprintf(stderr, "org interrupts: %v\n", err)
		return 2
	}
	now := time.Now().UTC()
	since := time.Time{}
	if span > 0 {
		since = now.Add(-span)
	}

	var report orgInterruptReport
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		activity, err := store.ListWakeOutboxActivity(ctx, since)
		if err != nil {
			return err
		}
		nudges, err := store.ListOrgDirectiveNudges(ctx, since)
		if err != nil {
			return err
		}
		rules, err := store.ListEventRules(ctx)
		if err != nil {
			return err
		}
		historyStart, err := store.EarliestEventRuleDeletion(ctx)
		if err != nil {
			return err
		}
		report = buildOrgInterruptReport(activity, nudges, rules, since, now, span)
		report.RouteHistoryStart = historyStart
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "org interrupts: %v\n", err)
		return 1
	}

	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "org interrupts: %v\n", err)
			return 1
		}
		return 0
	}
	writeOrgInterruptText(stdout, report)
	return 0
}

func parseOrgInterruptWindow(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return orgInterruptDefaultWindow, nil
	}
	if value == "0" || value == "all" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		parsed, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, fmt.Errorf("invalid window %q", value)
		}
		parsed *= 24
		if parsed < 0 {
			return 0, fmt.Errorf("window must not be negative")
		}
		return parsed, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q", value)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("window must not be negative")
	}
	return parsed, nil
}

// buildOrgInterruptReport aggregates the interrupt profile. It is separated
// from the command so the arithmetic is testable without a store or a home.
func buildOrgInterruptReport(
	activity []db.WakeOutboxActivity,
	nudges []db.OrgDirectiveNudgeRow,
	rules []db.EventRule,
	since, now time.Time,
	span time.Duration,
) orgInterruptReport {
	seats := map[string]*orgInterruptSeat{}
	arrivals := map[string][]time.Time{}
	routelessByKind := map[string]int{}
	seat := func(role string) *orgInterruptSeat {
		role = strings.ToLower(strings.TrimSpace(role))
		existing, ok := seats[role]
		if !ok {
			existing = &orgInterruptSeat{
				Role:    role,
				Sources: map[string]int{},
				States:  map[string]int{},
			}
			seats[role] = existing
		}
		return existing
	}

	report := orgInterruptReport{
		WindowEnd:       now.Format(time.RFC3339),
		ShortGapSeconds: orgInterruptShortGap.Seconds(),
	}
	if !since.IsZero() {
		report.WindowStart = since.Format(time.RFC3339)
	}
	windowHours := span.Hours()
	for _, row := range activity {
		entry := seat(row.TargetRole)
		entry.States[row.State]++
		if row.Collapsed {
			// A collapsed row is an interrupt that did NOT happen, so it is
			// counted separately and never as a wake.
			entry.Collapsed++
			report.Collapsed++
			continue
		}
		entry.Wakes++
		report.Wakes++
		entry.Sources[row.SourceKind]++
		switch row.State {
		case db.WakeOutboxStateDelivered:
			entry.Delivered++
		case db.WakeOutboxStateStalled, db.WakeOutboxStateFailed, db.WakeOutboxStateDeliveryUnknown:
			entry.Unproven++
			report.Unproven++
		case db.WakeOutboxStatePending:
			entry.Pending++
			if !wakeRowHasRoute(row, rules) {
				entry.Routeless++
				report.RoutelessPending++
				routelessByKind[wakeRowKind(row)]++
				if entry.OldestRLAt == "" {
					entry.OldestRLAt = row.CreatedAt
				}
			}
		}
		if createdAt, err := time.Parse(time.RFC3339Nano, row.CreatedAt); err == nil {
			arrivals[entry.Role] = append(arrivals[entry.Role], createdAt)
		}
	}

	for _, row := range nudges {
		_, to, _, _, ok := workflow.ParseOrgDirectiveNote(row.Body)
		if !ok {
			continue
		}
		entry := seat(to)
		entry.CompletionNags += row.CompletionNudges
		entry.AckNudges += row.AckNudges
	}

	for role, entry := range seats {
		series := arrivals[role]
		sort.Slice(series, func(i, j int) bool { return series[i].Before(series[j]) })
		gaps := make([]float64, 0, len(series))
		for index := 1; index < len(series); index++ {
			gaps = append(gaps, series[index].Sub(series[index-1]).Seconds())
		}
		entry.Gaps = len(gaps)
		for _, gap := range gaps {
			if gap < orgInterruptShortGap.Seconds() {
				entry.GapsUnderShort++
			}
		}
		entry.MedianGapSeconds = medianFloat(gaps)
		if windowHours > 0 {
			entry.PerDay = float64(entry.Wakes) / (windowHours / 24)
		}
	}

	report.WindowHours = windowHours
	report.Seats = make([]orgInterruptSeat, 0, len(seats))
	for _, entry := range seats {
		report.Seats = append(report.Seats, *entry)
	}
	sort.Slice(report.Seats, func(i, j int) bool {
		if report.Seats[i].Wakes != report.Seats[j].Wakes {
			return report.Seats[i].Wakes > report.Seats[j].Wakes
		}
		return report.Seats[i].Role < report.Seats[j].Role
	})
	if len(routelessByKind) > 0 {
		report.RoutelessByKindLbl = routelessByKind
	}
	return report
}

// wakeRowKind maps a stored row back to the wake kind its delivery rule must
// name, reusing the drain's own mapping so the report cannot disagree with the
// component that actually delivers.
func wakeRowKind(row db.WakeOutboxActivity) string {
	kind, ok := wakeOutboxKindForSource(row.SourceKind, row.CoalesceKey)
	if !ok {
		return row.SourceKind
	}
	return kind
}

// wakeRowHasRoute reports whether an enabled rule exists for this row's kind
// and target role. A pending row with no route is not waiting for a tick; it is
// waiting for configuration, and on the live fleet 49 `awaited_fact` rows had
// been waiting that way since 2026-08-02 (#1982).
func wakeRowHasRoute(row db.WakeOutboxActivity, rules []db.EventRule) bool {
	kind := wakeRowKind(row)
	role := strings.TrimSpace(row.TargetRole)
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rule.OnKind), kind) &&
			strings.EqualFold(strings.TrimSpace(rule.WakeRole), role) {
			return true
		}
	}
	return false
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func writeOrgInterruptText(stdout io.Writer, report orgInterruptReport) {
	window := "every recorded wake"
	if report.WindowStart != "" {
		window = fmt.Sprintf("%s to %s (%.0fh)", report.WindowStart, report.WindowEnd, report.WindowHours)
	}
	fmt.Fprintf(stdout, "Interrupt rate per seat — %s\n", window)
	fmt.Fprintf(stdout, "%d wake(s) delivered or queued, %d collapsed by coalescing, %d unproven, %d pending with no route\n\n",
		report.Wakes, report.Collapsed, report.Unproven, report.RoutelessPending)
	if len(report.Seats) == 0 {
		fmt.Fprintln(stdout, "no wakes recorded in this window")
		return
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SEAT\tWAKES\tPER DAY\tMEDIAN GAP\t<5MIN\tNOTE\tESC\tBLK\tFACT\tDELIVERED\tUNPROVEN\tCOLLAPSED\tNO ROUTE\tNAGS")
	for _, seat := range report.Seats {
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			seat.Role,
			seat.Wakes,
			seat.PerDay,
			formatInterruptGap(seat.MedianGapSeconds),
			formatShortGapShare(seat),
			seat.Sources[db.WakeOutboxSourceWorkflowNote],
			seat.Sources[db.WakeOutboxSourceEscalation],
			seat.Sources[db.WakeOutboxSourceBlocked],
			seat.Sources[db.WakeOutboxSourceAwaitedFact],
			seat.Delivered,
			seat.Unproven,
			seat.Collapsed,
			seat.Routeless,
			seat.CompletionNags,
		)
	}
	_ = tw.Flush()
	routeless := false
	for _, seat := range report.Seats {
		if seat.Routeless > 0 {
			routeless = true
			fmt.Fprintf(stdout, "\n%s has %d pending wake(s) with no enabled route, oldest %s\n",
				seat.Role, seat.Routeless, seat.OldestRLAt)
		}
	}
	if routeless {
		// PRINTED WITH THE ROWS, not left in a PR body. Each unroutable row
		// records a `wake_unroutable` job event whose `condition` says whether
		// the route was removed (a retired seat) or never configured (a gap).
		// That categorisation reads the deletion tombstones, and they do not go
		// back far enough, so history is miscategorised in one direction. A
		// reader deciding whether these wakes were load-bearing needs to know
		// that before trusting the split.
		boundary := report.RouteHistoryStart
		if boundary == "" {
			fmt.Fprintf(stdout,
				"\nNo route deletions are recorded at all, so every unroutable row above reads as %q in its wake_unroutable event whether or not its seat was retired.\n",
				db.WakeOutboxUnroutableNeverConfigured)
		} else {
			fmt.Fprintf(stdout,
				"\nRoute history begins %s; a role retired before then reads as %q rather than %q in its wake_unroutable event.\n",
				boundary, db.WakeOutboxUnroutableNeverConfigured, db.WakeOutboxUnroutableRouteRemoved)
		}
	}
}

func formatInterruptGap(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	return time.Duration(seconds * float64(time.Second)).Round(time.Second).String()
}

func formatShortGapShare(seat orgInterruptSeat) string {
	if seat.Gaps == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", seat.GapsUnderShort, seat.Gaps)
}
