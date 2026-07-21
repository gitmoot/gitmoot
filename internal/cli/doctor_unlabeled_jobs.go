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
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const unlabeledJobsDoctorThreshold = 10

func unlabeledJobsDoctorCheck(paths config.Paths) (doctor.Check, bool) {
	if strings.TrimSpace(paths.Database) == "" {
		return doctor.Check{}, false
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return doctor.Check{}, false
	}
	defer store.Close()
	jobs, err := store.ListDashboardUnlabeledJobs(context.Background(), nowSQLite(time.Now().UTC().Add(-24*time.Hour)))
	if err != nil {
		return doctor.Check{}, false
	}
	return buildUnlabeledJobsCheck(jobs, time.Now().UTC(), unlabeledJobsDoctorThreshold), true
}

// buildUnlabeledJobsCheck is shared by doctor and dashboard. It uses the stored
// row approximation documented by workflow.IsUnlabeledAgentDispatch.
func buildUnlabeledJobsCheck(jobs []db.Job, now time.Time, threshold int) doctor.Check {
	if threshold <= 0 {
		threshold = unlabeledJobsDoctorThreshold
	}
	counts := map[string]int{}
	scanned := 0
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil {
			continue
		}
		if !workflow.IsUnlabeledAgentDispatch(payload.WorkflowID, payload.Sender, payload.DelegationReason) {
			continue
		}
		if created := parseJobTimeMillis(job.CreatedAt); created <= cutoff {
			continue
		}
		scanned++
		counts[payload.Repo]++
	}
	var worst []string
	for repo, n := range counts {
		if n >= threshold {
			worst = append(worst, fmt.Sprintf("%s (%d)", repo, n))
		}
	}
	sort.Strings(worst)
	if len(worst) == 0 {
		return doctor.Check{Name: "unlabeled jobs", OK: true, Required: false, Detail: fmt.Sprintf("%d unlabeled agent jobs scanned in the last 24h", scanned)}
	}
	return doctor.Check{Name: "unlabeled jobs", Required: false, Detail: fmt.Sprintf("unlabeled agent-job drift in last 24h: %s — configure [workflow] require_workflow", strings.Join(worst, ", "))}
}

func unlabeledJobCounts(jobs []db.Job, now time.Time, threshold int) map[string]int {
	check := buildUnlabeledJobsCheck(jobs, now, threshold)
	if check.OK {
		return nil
	}
	counts := map[string]int{}
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	for _, job := range jobs {
		p, err := workflow.ParseJobPayload(job.Payload)
		if err == nil && workflow.IsUnlabeledAgentDispatch(p.WorkflowID, p.Sender, p.DelegationReason) && parseJobTimeMillis(job.CreatedAt) > cutoff {
			counts[p.Repo]++
		}
	}
	for repo, n := range counts {
		if n < threshold {
			delete(counts, repo)
		}
	}
	return counts
}

func nowSQLite(value time.Time) string { return value.UTC().Format("2006-01-02 15:04:05") }
