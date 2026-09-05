package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestReadOnlySeatJobRunShipsAPathThatResolvesRuntimeBinariesE2E covers the
// WORKER WIRING half of #1918: a seat dispatched through `job run` must actually
// RECEIVE an environment in which the runtime binaries resolve.
//
// SCOPE, STATED NARROWLY BECAUSE AN EARLIER VERSION OF THIS TEST OVERCLAIMED.
// The #1921 review found that its predecessor called itself a runtime-boundary
// test while asking a SHELL seat to `command -v`: a shell seat's own argv[0] is
// /bin/sh, which resolves whatever PATH says, so it never exercised the
// execLookPath(argv[0]) resolution the launch failure came from. That mechanism
// is now covered where it lives, by
// TestSandboxExecResolvesInheritedPathBinaryWithoutGrantingProfileKernelE2E in
// internal/sandbox, which launches a fixture runtime BY BARE NAME through the
// real sandbox-exec shim.
//
// What THIS test uniquely covers is the layer between those two:
// readOnlyRuntimeSandboxGrants BUILDS the env, and the daemon worker must SHIP
// it. toolchain_seat_test.go inspects what this package builds; only a real
// `job run` proves the seat receives it. That distinction is the #446/#459
// lesson — a home-scoped seam is proved at the boundary, and #1879 shipped
// through a full component suite.
//
// WHY `shell`: no LLM, no runtime auth, no network, so the only variable is the
// environment. The fixtures stand in for /root/.local/bin/claude and
// /root/.kimi-code/bin/kimi, in two SEPARATE directories because that is the
// host layout — a fix rescuing one hardcoded directory would strand the other.
//
// Before the fix the seat's PATH was the fixed list
// `<staged>/bin:/usr/local/bin:/usr/bin:/bin`, so both probes report MISSING and
// this fails.
func TestReadOnlySeatJobRunShipsAPathThatResolvesRuntimeBinariesE2E(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))

	claudeDir := t.TempDir()
	kimiDir := t.TempDir()
	writeSeatProbeBinary(t, claudeDir, "claude")
	writeSeatProbeBinary(t, kimiDir, "kimi")
	t.Setenv("PATH", strings.Join([]string{claudeDir, kimiDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	store := openCLIJobStore(t, home)
	defer store.Close()

	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// The seat reports what IT resolved. MISSING is a value rather than an
	// error, so a failure names which binary was unreachable instead of only
	// failing the job.
	probe := `claude_path=$(command -v claude || echo MISSING); ` +
		`kimi_path=$(command -v kimi || echo MISSING); ` +
		`printf '{"gitmoot_result":{"decision":"approved","summary":"claude=%s kimi=%s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}\n' "$claude_path" "$kimi_path"`
	seedDaemonWorkerAgent(t, store, "seat", "shell", probe, []string{"ask"}, "owner/repo")

	seedCLIJob(t, store, db.Job{
		ID:    "job-seat-path",
		Agent: "seat",
		Type:  "ask",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo:         "owner/repo",
			Branch:       "main",
			WorktreePath: checkout,
			ReadOnlySeat: true,
		}),
	}, "queued")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "run", "job-seat-path", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job run exit code = %d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}

	job, err := store.GetJob(context.Background(), "job-seat-path")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	if !payload.ReadOnlySeat {
		t.Fatalf("the job did not run as a read-only seat, so this test measured the wrong environment: payload=%+v", payload)
	}
	summary := payload.Result.Summary
	for binary, dir := range map[string]string{"claude": claudeDir, "kimi": kimiDir} {
		want := "=" + filepath.Join(dir, binary)
		if !strings.Contains(summary, want) {
			t.Errorf("the seat the worker launched could not resolve %q (wanted %q in %q).\nThe env readOnlyRuntimeSandboxGrants builds is not reaching the seat, which is the wiring half of #1918.", binary, want, summary)
		}
	}
}

func writeSeatProbeBinary(t *testing.T, dir string, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
