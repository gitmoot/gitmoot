package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func diskGuardDispatchFixture(t *testing.T) (context.Context, *db.Store, config.Paths, jobWorker) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	job := db.Job{
		ID:      "disk-guard-job",
		Agent:   "worker",
		Type:    "ask",
		State:   string(workflow.JobQueued),
		Payload: `{"repo":"owner/repo"}`,
	}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	return ctx, store, paths, defaultJobWorker(store, io.Discard, home)
}

func replaceDiskGuardMeasurement(t *testing.T, fn func(string) (diskFilesystemUsage, error)) {
	t.Helper()
	original := measureDiskGuardFilesystem
	measureDiskGuardFilesystem = fn
	t.Cleanup(func() {
		measureDiskGuardFilesystem = original
		diskGuardLogMu.Lock()
		diskGuardLogEntries = map[string]diskGuardLogEntry{}
		diskGuardLogMu.Unlock()
	})
}

func TestDispatchDiskGuardFailsClosedOnStatfsError(t *testing.T) {
	ctx, store, paths, worker := diskGuardDispatchFixture(t)
	var output bytes.Buffer
	worker.Stdout = &output
	var measuredPath string
	replaceDiskGuardMeasurement(t, func(path string) (diskFilesystemUsage, error) {
		measuredPath = path
		return diskFilesystemUsage{}, errors.New("injected statfs failure")
	})

	pending, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("listPendingQueuedJobs: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending jobs = %d, want 0 while disk state is unknown", len(pending))
	}
	if measuredPath != paths.Home {
		t.Fatalf("measured path = %q, want Gitmoot home %q", measuredPath, paths.Home)
	}
	job, err := store.GetJob(ctx, "disk-guard-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	for _, want := range []string{
		"DISK GUARD REFUSED JOB DISPATCH",
		"path=" + paths.Home,
		"free_bytes=unknown",
		"min_free_bytes=2147483648",
		"min_free_percent=5.00",
		"injected statfs failure",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("dispatch log missing %q:\n%s", want, output.String())
		}
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Kind == diskGuardRefusalEventKind {
			found = strings.Contains(event.Message, "free_bytes=unknown") &&
				strings.Contains(event.Message, "injected statfs failure") &&
				strings.Contains(event.Message, "path="+paths.Home)
		}
	}
	if !found {
		t.Fatalf("missing detailed %s event: %+v", diskGuardRefusalEventKind, events)
	}
}

func TestDispatchDiskGuardLowSpaceThenAutomaticRecovery(t *testing.T) {
	ctx, store, paths, worker := diskGuardDispatchFixture(t)
	var output bytes.Buffer
	worker.Stdout = &output
	usage := diskFilesystemUsage{
		TotalBytes: 100 << 30,
		FreeBytes:  1 << 30,
	}
	replaceDiskGuardMeasurement(t, func(path string) (diskFilesystemUsage, error) {
		if path != filepath.Clean(paths.Home) {
			t.Fatalf("measured path = %q, want %q", path, paths.Home)
		}
		return usage, nil
	})

	pending, err := listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("low-space listPendingQueuedJobs: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("low-space pending jobs = %d, want 0", len(pending))
	}
	if !strings.Contains(output.String(), "free_bytes=1073741824") ||
		!strings.Contains(output.String(), "free_percent=1.00") {
		t.Fatalf("low-space log lacks measured values:\n%s", output.String())
	}

	usage.FreeBytes = 20 << 30
	pending, err = listPendingQueuedJobs(ctx, worker, "owner/repo", "", true)
	if err != nil {
		t.Fatalf("recovered listPendingQueuedJobs: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "disk-guard-job" {
		t.Fatalf("healthy pending jobs = %+v, want original queued job", pending)
	}
	job, err := store.GetJob(ctx, "disk-guard-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("job state after recovery = %q, want queued until normal dispatcher claims it", job.State)
	}
}

func TestDiskGuardUsesMoreConservativeConfiguredFloor(t *testing.T) {
	evaluation := diskGuardEvaluation{
		Policy: config.DiskGuardPolicy{
			Enabled:        true,
			MinFreeBytes:   100,
			MinFreePercent: 10,
		},
		Usage:       diskFilesystemUsage{TotalBytes: 2000, FreeBytes: 150},
		FreePercent: 7.5,
	}
	if evaluation.allowsDispatch() {
		t.Fatal("percent floor failed but dispatch was allowed")
	}
	evaluation.Usage = diskFilesystemUsage{TotalBytes: 1000, FreeBytes: 90}
	evaluation.FreePercent = 9
	if evaluation.allowsDispatch() {
		t.Fatal("byte and percent floors failed but dispatch was allowed")
	}
	evaluation.Usage = diskFilesystemUsage{TotalBytes: 1000, FreeBytes: 150}
	evaluation.FreePercent = 15
	if !evaluation.allowsDispatch() {
		t.Fatal("both floors passed but dispatch was refused")
	}
}

func TestDaemonDiskGuardLineReportsHealthyAndUnhealthy(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 100 << 30, FreeBytes: 20 << 30}, nil
	})
	if line := daemonDiskGuardLine(paths); !strings.Contains(line, "disk guard: healthy") ||
		!strings.Contains(line, "free_bytes=21474836480") {
		t.Fatalf("healthy status line = %q", line)
	}

	measureDiskGuardFilesystem = func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{}, errors.New("status probe denied")
	}
	if line := daemonDiskGuardLine(paths); !strings.Contains(line, "UNHEALTHY") ||
		!strings.Contains(line, "dispatch paused") ||
		!strings.Contains(line, "status probe denied") {
		t.Fatalf("unhealthy status line = %q", line)
	}
}
