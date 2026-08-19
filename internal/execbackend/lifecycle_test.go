package execbackend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalBackendSyncInUsesIndependentGitMetadataAndExistingChangeTransport(t *testing.T) {
	host, _, base := changeSetRepoPair(t)
	writeChangeSetFile(t, host, "tracked.txt", "dirty source bytes\x00\n", 0o755)
	writeChangeSetFile(t, host, "nested/untracked.bin", "untracked\x00bytes\n", 0o644)

	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "gitdir-proof", LifecycleGeneration: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatalf("SyncIn: %v", err)
	}
	if instance.BaseHEAD != base {
		t.Fatalf("BaseHEAD = %q, want %q", instance.BaseHEAD, base)
	}

	gitInfo, err := os.Stat(filepath.Join(instance.Workspace, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if !gitInfo.IsDir() {
		t.Fatalf("local workspace .git mode = %s, want independently owned directory", gitInfo.Mode())
	}
	hostCommon := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	workspaceCommon := strings.TrimSpace(changeSetGit(t, instance.Workspace, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	if filepath.Clean(hostCommon) == filepath.Clean(workspaceCommon) {
		t.Fatalf("local workspace shares host Git common directory %q", hostCommon)
	}
	if _, err := os.Stat(filepath.Join(instance.Workspace, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local workspace has a host object alternates link: %v", err)
	}
	if got := strings.TrimSpace(changeSetGit(t, instance.Workspace, "remote")); got != "" {
		t.Fatalf("local workspace retained host remote: %q", got)
	}
	if got := strings.TrimSpace(changeSetGit(t, instance.Workspace, "rev-parse", "HEAD")); got != base {
		t.Fatalf("workspace HEAD = %q, want %q", got, base)
	}
	if want, got := changeSetSnapshot(t, host), changeSetSnapshot(t, instance.Workspace); got != want {
		t.Fatalf("SyncIn tree is not byte-identical\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestLocalBackendCollectRoundTripsByteIdentically(t *testing.T) {
	host, _, _ := changeSetRepoPair(t)
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "roundtrip"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	writeChangeSetFile(t, instance.Workspace, "tracked.txt", "backend produced\x00bytes\n", 0o755)
	writeChangeSetFile(t, instance.Workspace, "new.bin", "\x00\x01\x02\xff", 0o644)

	changes, err := backend.Collect(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatal(err)
	}
	if want, got := changeSetSnapshot(t, instance.Workspace), changeSetSnapshot(t, host); got != want {
		t.Fatalf("collected host tree is not byte-identical\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestLocalBackendGitRefsAreIndependentFromHost(t *testing.T) {
	host, _, victimHead := changeSetRepoPair(t)
	changeSetGit(t, host, "branch", "ref-victim", victimHead)
	writeChangeSetFile(t, host, "host-next.txt", "next host commit\n", 0o644)
	changeSetGit(t, host, "add", "host-next.txt")
	changeSetGit(t, host, "commit", "-m", "host next")
	sourceHead := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "HEAD"))

	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "ref-isolation"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	stream, err := backend.Exec(context.Background(), instance, Command{
		Dir: instance.Workspace, Name: "git", Args: []string{"update-ref", "refs/heads/ref-victim", "HEAD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := stream.Wait(); err != nil {
		t.Fatalf("backend git update-ref: %v (stderr: %s)", err, result.Stderr)
	}

	if got := strings.TrimSpace(changeSetGit(t, host, "rev-parse", "refs/heads/ref-victim")); got != victimHead {
		t.Fatalf("host ref changed through backend: got %s, want %s", got, victimHead)
	}
	if got := strings.TrimSpace(changeSetGit(t, instance.Workspace, "rev-parse", "refs/heads/ref-victim")); got != sourceHead {
		t.Fatalf("backend ref = %s, want independently updated %s", got, sourceHead)
	}
	if got := strings.TrimSpace(changeSetGit(t, instance.Workspace, "rev-parse", "HEAD")); got != sourceHead {
		t.Fatalf("backend HEAD moved: got %s, want %s", got, sourceHead)
	}
	if got := changeSetGit(t, instance.Workspace, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("backend worktree changed while updating ref: %q", got)
	}
}

func TestLocalBackendCancelDestroysInstance(t *testing.T) {
	host, _, _ := changeSetRepoPair(t)
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(instance.Workspace, "ready")
	stream, err := backend.Exec(context.Background(), instance, Command{
		Dir:  instance.Workspace,
		Name: "sh",
		Args: []string{"-c", "touch ready; exec sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForPath(t, ready)
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := backend.Cancel(cancelCtx, instance); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := stream.Wait(); err == nil {
		t.Fatal("cancelled execution returned nil error")
	}
	if _, err := os.Stat(instance.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists after Cancel: %v", err)
	}
	if _, err := os.Stat(instance.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance root still exists after Cancel: %v", err)
	}
}

func TestLocalBackendReapsProcessKilledMidProvision(t *testing.T) {
	if os.Getenv("GITMOOT_LOCAL_BACKEND_REAP_HELPER") == "1" {
		runLocalBackendReapHelper(t)
		return
	}
	host, _, _ := changeSetRepoPair(t)
	root := filepath.Join(t.TempDir(), "instances")
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalBackendReapsProcessKilledMidProvision$")
	cmd.Env = append(os.Environ(),
		"GITMOOT_LOCAL_BACKEND_REAP_HELPER=1",
		"GITMOOT_LOCAL_BACKEND_ROOT="+root,
		"GITMOOT_LOCAL_BACKEND_SOURCE="+host,
		"GITMOOT_LOCAL_BACKEND_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, ready)
	idBytes, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(idBytes))
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	backend := newTestLocalBackendAt(t, root)
	reaped, err := backend.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != id {
		t.Fatalf("reaped = %v, want [%s]", reaped, id)
	}
	instanceRoot := filepath.Join(root, id)
	if _, err := os.Stat(instanceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("killed mid-provision instance still exists: %v", err)
	}
	listed := changeSetGit(t, host, "worktree", "list", "--porcelain")
	if strings.Contains(listed, instanceRoot) {
		t.Fatalf("reaped workspace remains registered:\n%s", listed)
	}
}

func TestLocalBackendReapLeavesLiveInstance(t *testing.T) {
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "live-owner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	reaped, err := backend.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("Reap removed live-owner instances: %v", reaped)
	}
	if _, err := os.Stat(instance.root); err != nil {
		t.Fatalf("live-owner instance disappeared: %v", err)
	}
}

func TestLocalBackendExecRejectsDirectoryOutsideWorkspace(t *testing.T) {
	host, _, _ := changeSetRepoPair(t)
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "dir-guard"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Exec(context.Background(), instance, Command{Dir: filepath.Dir(instance.Workspace), Name: "true"}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("outside-workspace Exec error = %v", err)
	}
}

func runLocalBackendReapHelper(t *testing.T) {
	backend := newTestLocalBackendAt(t, os.Getenv("GITMOOT_LOCAL_BACKEND_ROOT"))
	backend.afterWorkspaceCreated = func(instance *Instance) {
		if err := os.WriteFile(os.Getenv("GITMOOT_LOCAL_BACKEND_READY"), []byte(instance.ID), 0o600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "killed-mid-provision"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: os.Getenv("GITMOOT_LOCAL_BACKEND_SOURCE")}); err != nil {
		t.Fatal(err)
	}
}

func newTestLocalBackend(t *testing.T) *LocalBackend {
	t.Helper()
	return newTestLocalBackendAt(t, filepath.Join(t.TempDir(), "instances"))
}

func newTestLocalBackendAt(t *testing.T, root string) *LocalBackend {
	t.Helper()
	backend, err := NewLocalBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
