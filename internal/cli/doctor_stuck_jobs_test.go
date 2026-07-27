package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestStuckJobsStatusReadsRecordedRuntimeProcesses(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	livePID := os.Getpid()
	liveIdentity := workflow.RuntimeProcessIdentity(livePID)
	if liveIdentity == "" {
		t.Fatal("current process identity is unavailable")
	}
	const deadPID = 999999999
	for _, item := range []struct {
		id       string
		pid      int
		identity string
	}{
		{id: "live", pid: livePID, identity: liveIdentity},
		{id: "dead", pid: deadPID, identity: "old-start"},
		{id: "legacy"},
	} {
		seedCLIJob(t, store, db.Job{
			ID:    item.id,
			Agent: "worker",
			Type:  "ask",
			State: string(workflow.JobRunning),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo:                "owner/repo",
				RuntimePID:          item.pid,
				RuntimePIDStartTime: item.identity,
			}),
		}, string(workflow.JobRunning))
	}
	store.Close()

	status := stuckJobsStatus(config.PathsForHome(home))
	if status == nil || len(status.Jobs) != 3 {
		t.Fatalf("status = %+v, want three running jobs", status)
	}
	check := doctor.CheckStuckJobs(*status)
	if check.OK || !check.Required || !strings.Contains(check.Detail, "dead (pid 999999999)") {
		t.Fatalf("check = %+v, want hard finding for only the dead job", check)
	}
	for _, absent := range []string{"live", "legacy"} {
		if strings.Contains(check.Detail, absent) {
			t.Fatalf("detail = %q, must not flag %s", check.Detail, absent)
		}
	}
}
