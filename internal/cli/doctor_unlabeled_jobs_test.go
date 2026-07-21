package cli

import (
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func TestBuildUnlabeledJobsCheck(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	jobs := []db.Job{
		{Payload: `{"repo":"a/b","sender":"local"}`, CreatedAt: "2026-07-21 11:00:00"},
		{Payload: `{"repo":"a/b","sender":"local"}`, CreatedAt: "2026-07-21 10:00:00"},
		{Payload: `{"repo":"a/b","sender":"pipeline"}`, CreatedAt: "2026-07-21 11:00:00"},
		{Payload: `{"repo":"a/b","sender":"local","workflow_id":"team/x"}`, CreatedAt: "2026-07-21 11:00:00"},
		{Payload: `{"repo":"old/repo","sender":"local"}`, CreatedAt: "2026-07-19 11:00:00"},
	}
	if got := buildUnlabeledJobsCheck(jobs, now, 3); !got.OK {
		t.Fatalf("under threshold=%+v", got)
	}
	jobs = append(jobs, db.Job{Payload: `{"repo":"a/b","sender":"local"}`, CreatedAt: "2026-07-21 11:00:00"})
	if got := buildUnlabeledJobsCheck(jobs, now, 3); got.OK || got.Required {
		t.Fatalf("threshold=%+v", got)
	}
}
