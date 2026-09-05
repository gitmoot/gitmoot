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

// TestReadOnlySeatShellRuntimeResolvesRuntimeBinariesE2E is #1918 AT THE TRUE
// RUNTIME BOUNDARY.
//
// The component test in toolchain_seat_test.go asserts the env this package
// BUILDS. This one runs the real worker (`job run`) with a shell read-only seat
// on an isolated home and asks the seat itself, from inside the sandbox,
// whether it can resolve the runtime binaries. That distinction is the #446 /
// #459 lesson: a home-scoped seam is only proved at the boundary, and #1879
// shipped through a full component suite.
//
// WHY `shell`: no LLM, no runtime auth, no network, so the only thing under
// test is the seat's environment. The probe uses `command -v`, which RESOLVES
// without executing — the fixtures below stand in for /root/.local/bin/claude
// and /root/.kimi-code/bin/kimi, whose real launch failure was
// `sandbox-exec: resolve sandbox target "claude": executable file not found in
// $PATH`.
//
// Before the fix the seat's PATH was the fixed list
// `<staged>/bin:/usr/local/bin:/usr/bin:/bin`, so both probes report MISSING
// and this fails.
func TestReadOnlySeatShellRuntimeResolvesRuntimeBinariesE2E(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERDR_ENV", "")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "absent-herdr.sock"))

	// Two separate directories, matching the host layout: a fix that rescued one
	// hardcoded directory would still strand the other.
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

	// The seat reports what IT can resolve. MISSING is a value, not an error, so
	// a failure names which binary was unreachable instead of only failing the
	// job — an empty result would not say which arm broke.
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
			t.Errorf("the seat could not resolve %q from inside the sandbox (wanted %q in %q).\nThis is the #1918 launch failure: sandbox-exec resolves argv[0] with exec.LookPath before any Landlock rule is applied.", binary, want, summary)
		}
	}
}

func writeSeatProbeBinary(t *testing.T, dir string, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
