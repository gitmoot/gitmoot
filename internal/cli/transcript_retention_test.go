package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func transcriptRetentionTestStore(t *testing.T, maxBytes int64) (string, config.Paths, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	content := config.DefaultConfig(paths) + "\n[transcripts]\nenabled = true\nretain = \"168h\"\nmax_total_bytes = " + fmtInt64(maxBytes) + "\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return home, paths, store
}

func fmtInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func createTranscriptRetentionJob(t *testing.T, store *db.Store, id, state string) {
	t.Helper()
	if err := store.CreateJob(context.Background(), db.Job{ID: id, Agent: "agent", Type: "ask", State: state, Payload: `{}`}); err != nil {
		t.Fatal(err)
	}
}

func writeRetentionLog(t *testing.T, paths config.Paths, id, body string) string {
	t.Helper()
	path := transcript.JobLogPath(paths.Logs, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTranscriptRetentionTTLProtectsLiveAndGraceJobs(t *testing.T) {
	_, paths, store := transcriptRetentionTestStore(t, 1<<30)
	for _, job := range []struct{ id, state string }{
		{"done", string(workflow.JobSucceeded)}, {"blocked", string(workflow.JobBlocked)},
		{"queued", string(workflow.JobQueued)}, {"running", string(workflow.JobRunning)},
	} {
		createTranscriptRetentionJob(t, store, job.id, job.state)
		writeRetentionLog(t, paths, job.id, "data")
	}
	now := time.Now().UTC()
	stats, err := sweepTranscriptRetention(context.Background(), paths, store, now.Add(5*time.Minute), os.Remove)
	if err != nil || stats.Removed != 0 {
		t.Fatalf("grace sweep = %+v, err=%v", stats, err)
	}
	stats, err = sweepTranscriptRetention(context.Background(), paths, store, now.Add(8*24*time.Hour), os.Remove)
	if err != nil || stats.Removed != 2 {
		t.Fatalf("TTL sweep = %+v, err=%v", stats, err)
	}
	for _, id := range []string{"done", "blocked"} {
		if _, err := os.Stat(transcript.JobLogPath(paths.Logs, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s log still exists: %v", id, err)
		}
	}
	for _, id := range []string{"queued", "running"} {
		if _, err := os.Stat(transcript.JobLogPath(paths.Logs, id)); err != nil {
			t.Fatalf("protected %s log: %v", id, err)
		}
	}
}

func TestTranscriptRetentionCapOldestSettledAndENOENT(t *testing.T) {
	_, paths, store := transcriptRetentionTestStore(t, 5)
	createTranscriptRetentionJob(t, store, "old", string(workflow.JobSucceeded))
	createTranscriptRetentionJob(t, store, "new", string(workflow.JobFailed))
	oldPath := writeRetentionLog(t, paths, "old", "1234")
	newPath := writeRetentionLog(t, paths, "new", "5678")
	conn, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`UPDATE jobs SET updated_at = '2020-01-01 00:00:00' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE jobs SET updated_at = '2020-01-02 00:00:00' WHERE id = 'new'`); err != nil {
		t.Fatal(err)
	}
	stats, err := sweepTranscriptRetention(context.Background(), paths, store, time.Date(2020, 1, 2, 1, 0, 0, 0, time.UTC), os.Remove)
	if err != nil || stats.Removed != 1 {
		t.Fatalf("cap sweep = %+v, err=%v", stats, err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest log not evicted: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("newer log evicted: %v", err)
	}

	// ENOENT after selection is success, matching a concurrent finalizer/GC race.
	if err := os.WriteFile(oldPath, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	stats, err = sweepTranscriptRetention(context.Background(), paths, store, time.Date(2020, 1, 2, 1, 0, 0, 0, time.UTC), func(string) error { return os.ErrNotExist })
	if err != nil || stats.Removed != 1 || stats.Errors != 0 {
		t.Fatalf("ENOENT sweep = %+v, err=%v", stats, err)
	}
}

func TestTranscriptRetentionOrphansAndSweepLimit(t *testing.T) {
	_, paths, store := transcriptRetentionTestStore(t, 1<<30)
	dir := filepath.Join(paths.Logs, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	for i := 0; i < transcriptSweepDeleteLimit+4; i++ {
		path := filepath.Join(dir, fmtInt64(int64(i))+".log")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := sweepTranscriptRetention(context.Background(), paths, store, time.Now().UTC(), os.Remove)
	if err != nil || stats.Removed != transcriptSweepDeleteLimit {
		t.Fatalf("bounded sweep = %+v, err=%v", stats, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("remaining orphans = %d, err=%v", len(entries), err)
	}
}

func TestTranscriptRunnerCompositionPreservesWrappersAndExistingTee(t *testing.T) {
	var progress, retained strings.Builder
	runner := subprocess.WrappingRunner{Inner: subprocess.EnvInjectingRunner{Env: []string{"RELAY=yes"}, Inner: subprocess.TeeRunner{Inner: subprocess.GroupRunner{}, Out: &progress}}}
	got := appendRuntimeOutputRunner(runner, &retained)
	wrap, ok := got.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("runner = %T, want WrappingRunner", got)
	}
	env, ok := wrap.Inner.(subprocess.EnvInjectingRunner)
	if !ok || len(env.Env) != 1 || env.Env[0] != "RELAY=yes" {
		t.Fatalf("env wrapper lost: %#v", wrap.Inner)
	}
	tee, ok := env.Inner.(subprocess.TeeRunner)
	if !ok {
		t.Fatalf("tee lost: %T", env.Inner)
	}
	if _, err := tee.Out.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	if progress.String() != "line\n" || retained.String() != "line\n" {
		t.Fatalf("progress=%q retained=%q", progress.String(), retained.String())
	}
	adapter, err := appendDeliveryAdapterOutput(runtime.ShellAdapter{Runner: runner}, &retained)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter.(runtime.ShellAdapter); !ok {
		t.Fatalf("adapter = %T", adapter)
	}
}

func TestRetainedTranscriptLogAppendPermissionsDisabledAndOpenFailure(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if path, file, err := openRetainedTranscriptLog(home, "disabled"); err != nil || path != "" || file != nil {
		t.Fatalf("disabled open = path %q file %v err %v", path, file, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Logs, "jobs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled capture created jobs dir: %v", err)
	}
	content := config.DefaultConfig(paths) + "\n[transcripts]\nenabled = true\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	path, first, err := openRetainedTranscriptLog(home, "retry/id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteString("attempt-one\n"); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	_, second, err := openRetainedTranscriptLog(home, "retry/id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.WriteString("attempt-two\n"); err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "attempt-one\nattempt-two\n" {
		t.Fatalf("append body = %q, err=%v", body, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, file, err := openRetainedTranscriptLog(home, strings.Repeat("x", 5000)); err == nil || file != nil {
		t.Fatalf("oversized filename open = file %v err %v, want fail-open signal", file, err)
	}
}
