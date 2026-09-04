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
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
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
	store, err := dbtest.Open(t, paths.Database)
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
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+"\n[transcripts]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if file, err := openRetainedTranscriptLog(home, "disabled"); err != nil || file != nil {
		t.Fatalf("disabled open = file %v err %v, want no file and no error", file, err)
	}
	if _, err := os.Stat(filepath.Join(paths.Logs, "jobs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled capture created jobs dir: %v", err)
	}
	content := config.DefaultConfig(paths) + "\n[transcripts]\nenabled = true\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := openRetainedTranscriptLog(home, "retry/id")
	path := retainedTranscriptLogPathForTest(t, home, "retry/id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteString("attempt-one\n"); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	second, err := openRetainedTranscriptLog(home, "retry/id")
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
	if file, err := openRetainedTranscriptLog(home, strings.Repeat("x", 5000)); err == nil || file != nil {
		t.Fatalf("oversized filename open = file %v err %v, want fail-open signal", file, err)
	}
}

func TestRetainedTranscriptLogDefaultOn(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := openRetainedTranscriptLog(home, "default-on")
	path := retainedTranscriptLogPathForTest(t, home, "default-on")
	if err != nil {
		t.Fatal(err)
	}
	if file == nil || path != transcript.JobLogPath(paths.Logs, "default-on") {
		t.Fatalf("default-on log = path %q file %v", path, file)
	}
	if _, err := file.WriteString("stdout and stderr stream\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() == 0 {
		t.Fatalf("default-on log mode=%v size=%d", info.Mode().Perm(), info.Size())
	}
}

// retainedTranscriptLogPathForTest derives the on-disk transcript location the
// way production does. openRetainedTranscriptLog no longer returns the path
// (#1787 review F5): no production caller read it, and a test can ask the same
// authority production asks.
func retainedTranscriptLogPathForTest(t *testing.T, home, jobID string) string {
	t.Helper()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatalf("pathsFromFlag: %v", err)
	}
	return transcript.JobLogPath(paths.Logs, jobID)
}

// Seat-mode cockpit logs held UNREDACTED runtime output at 0600, and the only
// code that ever removed them went with the TUI, so an upgraded installation
// keeps them forever with no writer, reader or reaper (#1787 review F2). The
// retention sweep now reaps that tree. Two properties matter beyond "the files
// are gone": the reap must NOT be gated on the transcripts config, or turning
// transcripts off becomes a way to preserve secrets, and it must leave the jobs
// tree alone.
func TestSweepReapsOrphanedSeatLogsEvenWhenTranscriptsAreDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "transcripts_enabled"
		if !enabled {
			name = "transcripts_disabled"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			paths, err := pathsFromFlag(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			body := config.DefaultConfig(paths) + "\n[transcripts]\nenabled = " + map[bool]string{true: "true", false: "false"}[enabled] + "\n"
			if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			seatDir := filepath.Join(paths.Logs, "seats", "root123")
			if err := os.MkdirAll(seatDir, 0o700); err != nil {
				t.Fatal(err)
			}
			seatLog := filepath.Join(seatDir, "seat-a.log")
			if err := os.WriteFile(seatLog, []byte("unredacted secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			jobsDir := filepath.Join(paths.Logs, "jobs")
			if err := os.MkdirAll(jobsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(jobsDir, "keep-me.log")
			if err := os.WriteFile(keep, []byte("recent\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			store := daemonWorkerStore(t)
			if _, err := sweepTranscriptRetention(ctx, paths, store, time.Now().UTC(), os.Remove); err != nil {
				t.Fatalf("sweepTranscriptRetention: %v", err)
			}
			if _, err := os.Stat(seatLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("orphaned seat log survived the sweep: %v", err)
			}
			if _, err := os.Stat(filepath.Join(paths.Logs, "seats")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("orphaned seats tree survived the sweep: %v", err)
			}
			if _, err := os.Stat(keep); err != nil {
				t.Fatalf("the sweep removed a live job transcript: %v", err)
			}
		})
	}
}

// A home with no seats tree is the normal case after the first sweep, and must
// not be an error or a repeated cost.
func TestSweepWithNoSeatLogsIsClean(t *testing.T) {
	home := t.TempDir()
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	removed, failed := reapOrphanedSeatLogs(paths, os.Remove)
	if removed != 0 || failed != 0 {
		t.Fatalf("reap on a home with no seats tree = removed %d failed %d, want 0/0", removed, failed)
	}
}
