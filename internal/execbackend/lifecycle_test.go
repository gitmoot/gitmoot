package execbackend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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

func TestLocalBackendExecDropsPrivilegesAndImportNormalizesOwnership(t *testing.T) {
	identity := testUnprivilegedIdentities(t, 1)[0]
	host, _, _ := changeSetRepoPair(t)
	backend := newPrivilegedTestLocalBackend(t, identity)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "privilege-roundtrip"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	if uid, gid := pathOwnership(t, instance.Workspace); uid != identity.UID || gid != identity.GID {
		t.Fatalf("workspace ownership = %d:%d, want configured %d:%d", uid, gid, identity.UID, identity.GID)
	}

	var streamed strings.Builder
	stream, err := backend.Exec(context.Background(), instance, Command{
		Dir:    instance.Workspace,
		Name:   "/bin/sh",
		Args:   []string{"-c", `id -u; printf 'agent-owned\000bytes\n' > new.bin`},
		Output: &streamed,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("non-root Exec: %v (stderr: %s)", err, result.Stderr)
	}
	if got, want := strings.TrimSpace(result.Stdout), strconv.FormatUint(uint64(identity.UID), 10); got != want {
		t.Fatalf("command-reported euid = %q, want configured non-root uid %s", got, want)
	}
	if streamed.String() != result.Stdout {
		t.Fatalf("streamed output = %q, buffered output = %q", streamed.String(), result.Stdout)
	}
	if uid, gid := pathOwnership(t, filepath.Join(instance.Workspace, "new.bin")); uid != identity.UID || gid != identity.GID {
		t.Fatalf("agent-created ownership = %d:%d, want %d:%d", uid, gid, identity.UID, identity.GID)
	}

	changes, err := backend.Collect(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	repairStream, err := backend.Exec(context.Background(), instance, Command{
		Dir:  instance.Workspace,
		Name: "/bin/sh",
		Args: []string{"-c", `set -e; id -u; printf 'repair' > .repair-write; rm .repair-write`},
	})
	if err != nil {
		t.Fatal(err)
	}
	repairResult, err := repairStream.Wait()
	if err != nil {
		t.Fatalf("post-collection non-root Exec: %v", err)
	}
	if got, want := strings.TrimSpace(repairResult.Stdout), strconv.FormatUint(uint64(identity.UID), 10); got != want {
		t.Fatalf("post-collection command-reported euid = %q, want %s", got, want)
	}
	if err := ImportChangeSet(context.Background(), host, changes); err != nil {
		t.Fatal(err)
	}
	// changeSetSnapshot invokes Git as the daemon uid; reclaim the already-
	// collected test workspace so the byte-for-byte P2a assertion itself is not
	// blocked by Git's cross-uid safe.directory policy.
	chownTree(t, instance.Workspace, LocalIdentity{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())})
	if want, got := changeSetSnapshot(t, instance.Workspace), changeSetSnapshot(t, host); got != want {
		t.Fatalf("non-root collected host tree is not byte-identical\nwant:\n%s\ngot:\n%s", want, got)
	}
	if uid, gid := pathOwnership(t, filepath.Join(host, "new.bin")); uid != uint32(os.Geteuid()) || gid != uint32(os.Getegid()) {
		t.Fatalf("imported ownership = %d:%d, want daemon %d:%d", uid, gid, os.Geteuid(), os.Getegid())
	}
}

func TestLocalBackendWorkspaceTraverseLimitedToConfiguredGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("workspace traverse identity proof requires a root test process")
	}
	identities := testUnprivilegedIdentitiesWithDistinctGIDs(t, 3)
	configured := LocalIdentity{UID: identities[0].UID, GID: identities[1].GID}
	third := identities[2]

	host, _, _ := changeSetRepoPair(t)
	backend := newPrivilegedTestLocalBackend(t, configured)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "traverse-boundary"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}

	if err := runAsIdentity(configured, instance.Workspace, "/bin/pwd"); err != nil {
		t.Fatalf("configured identity uid %d gid %d cannot traverse workspace: %v", configured.UID, configured.GID, err)
	}
	if err := runAsIdentity(third, instance.Workspace, "/bin/pwd"); err == nil {
		t.Fatalf("identity outside configured group, uid %d gid %d, traversed workspace; want permission denied", third.UID, third.GID)
	}
	sameGroupThird := LocalIdentity{UID: third.UID, GID: configured.GID}
	if err := runAsIdentity(sameGroupThird, instance.Workspace, "/bin/pwd"); err != nil {
		t.Fatalf("different uid %d carrying configured gid %d cannot traverse group-scoped workspace: %v", sameGroupThird.UID, sameGroupThird.GID, err)
	}

	for _, path := range []string{backend.root, instance.root} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o710 {
			t.Fatalf("traverse parent %q mode = %#o, want 0710", path, got)
		}
		if _, gid := pathOwnership(t, path); gid != configured.GID {
			t.Fatalf("traverse parent %q gid = %d, want configured gid %d", path, gid, configured.GID)
		}
	}
}

func TestLocalBackendWorkspaceTraverseParentPermissions(t *testing.T) {
	identity := LocalIdentity{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	if identity.UID == 0 || identity.GID == 0 {
		identity = testUnprivilegedIdentities(t, 1)[0]
	}
	backend, err := NewLocalBackend(filepath.Join(t.TempDir(), "instances"), &identity)
	if err != nil {
		t.Fatal(err)
	}
	type chownCall struct {
		path     string
		uid, gid int
	}
	var chownCalls []chownCall
	backend.chown = func(path string, uid, gid int) error {
		chownCalls = append(chownCalls, chownCall{path: path, uid: uid, gid: gid})
		return nil
	}
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "traverse-permissions"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(instance.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := backend.handoffWorkspace(instance); err != nil {
		t.Fatal(err)
	}
	wantChownCalls := []chownCall{
		{path: backend.root, uid: -1, gid: int(identity.GID)},
		{path: instance.root, uid: -1, gid: int(identity.GID)},
	}
	if !reflect.DeepEqual(chownCalls, wantChownCalls) {
		t.Fatalf("traverse-parent chown calls = %+v, want %+v", chownCalls, wantChownCalls)
	}

	for _, path := range []string{backend.root, instance.root} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o710 {
			t.Fatalf("traverse parent %q mode = %#o, want 0710", path, got)
		}
	}
}

func TestLocalBackendCuratedScratchUsesConfiguredIdentity(t *testing.T) {
	identity := testUnprivilegedIdentities(t, 1)[0]
	host, _, _ := changeSetRepoPair(t)
	backend := newPrivilegedTestLocalBackend(t, identity)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "privilege-curated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(filepath.Dir(backend.root), "credential-scratch")
	stream, err := backend.Exec(context.Background(), instance, Command{
		Dir:         instance.Workspace,
		Name:        "/bin/sh",
		Args:        []string{"-c", `set -e; printf 'scratch-ok' > "$GH_CONFIG_DIR/proof"; id -u`},
		BaseEnv:     []string{"PATH=/usr/bin:/bin", "GH_CONFIG_DIR=" + scratch},
		ScratchDirs: []string{scratch},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("curated non-root Exec: %v (stderr: %s)", err, result.Stderr)
	}
	if got, want := strings.TrimSpace(result.Stdout), strconv.FormatUint(uint64(identity.UID), 10); got != want {
		t.Fatalf("curated command-reported euid = %q, want %s", got, want)
	}
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("curated scratch survived cleanup: %v", err)
	}
}

func TestLocalBackendExecConfiguredIdentityFailureIsLoud(t *testing.T) {
	identities := testUnprivilegedIdentities(t, 2)
	if os.Getenv("GITMOOT_LOCAL_BACKEND_IDENTITY_FAILURE_HELPER") == "1" {
		runLocalBackendIdentityFailureHelper(t, identities[1])
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("configured identity failure proof requires a root test process")
	}
	host, _, _ := changeSetRepoPair(t)
	backend := newPrivilegedTestLocalBackend(t, identities[1])
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "privilege-failure"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	chownTree(t, instance.Workspace, identities[0])

	helper := filepath.Join(filepath.Dir(backend.root), "identity-failure-helper")
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(helper, int(identities[0].UID), int(identities[0].GID)); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helper, "-test.run=^TestLocalBackendExecConfiguredIdentityFailureIsLoud$")
	cmd.Env = append(os.Environ(),
		"GITMOOT_LOCAL_BACKEND_IDENTITY_FAILURE_HELPER=1",
		"GITMOOT_LOCAL_BACKEND_ROOT="+backend.root,
		"GITMOOT_LOCAL_BACKEND_INSTANCE_ROOT="+instance.root,
		"GITMOOT_LOCAL_BACKEND_INSTANCE_ID="+instance.ID,
		"GITMOOT_LOCAL_BACKEND_WORKSPACE="+instance.Workspace,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: identities[0].UID, Gid: identities[0].GID, Groups: []uint32{},
	}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("identity failure helper: %v\n%s", err, output)
	}
}

func TestLocalBackendExecNonzeroExitDoesNotBlameConfiguredIdentity(t *testing.T) {
	identity := testUnprivilegedIdentities(t, 1)[0]
	host, _, _ := changeSetRepoPair(t)
	backend := newPrivilegedTestLocalBackend(t, identity)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "ordinary-exit"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		output *strings.Builder
	}{
		{name: "buffered"},
		{name: "streaming", output: &strings.Builder{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := backend.Exec(context.Background(), instance, Command{
				Dir: instance.Workspace, Name: "/bin/sh", Args: []string{"-c", "exit 3"}, Output: tc.output,
			})
			if err != nil {
				t.Fatalf("Exec setup: %v", err)
			}
			_, err = stream.Wait()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
				t.Fatalf("ordinary exit error = %v, want exit status 3", err)
			}
			if strings.Contains(err.Error(), "execute local backend command as uid") {
				t.Fatalf("ordinary exit error = %v, want no configured-identity framing", err)
			}
		})
	}
}

func runLocalBackendIdentityFailureHelper(t *testing.T, target LocalIdentity) {
	backend, err := NewLocalBackend(os.Getenv("GITMOOT_LOCAL_BACKEND_ROOT"), &target)
	if err != nil {
		t.Fatal(err)
	}
	instance := &Instance{
		ID:        os.Getenv("GITMOOT_LOCAL_BACKEND_INSTANCE_ID"),
		Workspace: os.Getenv("GITMOOT_LOCAL_BACKEND_WORKSPACE"),
		root:      os.Getenv("GITMOOT_LOCAL_BACKEND_INSTANCE_ROOT"),
	}
	stream, err := backend.Exec(context.Background(), instance, Command{Dir: instance.Workspace, Name: "/bin/true"})
	if err != nil {
		t.Fatalf("Exec setup: %v", err)
	}
	_, err = stream.Wait()
	if err == nil {
		t.Fatal("configured identity failure silently ran the command")
	}
	if !strings.Contains(err.Error(), "execute local backend command as uid") || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("configured identity failure = %v, want attributed loud error", err)
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

func TestLocalBackendDestroyIgnoresTamperedMetadataPaths(t *testing.T) {
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "tampered-destroy"})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	writeChangeSetFile(t, outside, "sentinel", "keep\n", 0o600)
	tamperLocalMetadata(t, instance.root, func(meta map[string]any) {
		meta["workspace"] = outside
	})
	attached, err := backend.Attach(context.Background(), instance.ID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attached.Workspace != filepath.Join(instance.root, localWorkspaceName) {
		t.Fatalf("attached workspace = %q, want canonical instance workspace", attached.Workspace)
	}

	if err := backend.Destroy(context.Background(), attached); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(got) != "keep\n" {
		t.Fatalf("outside sentinel after Destroy = %q, %v", got, err)
	}
	if _, err := os.Stat(instance.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance root still exists after Destroy: %v", err)
	}
}

func TestLocalBackendReapIgnoresTamperedMetadataPathsAndID(t *testing.T) {
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "tampered-reap"})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := backend.Provision(context.Background(), JobScope{JobID: "reap-sibling"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), sibling) })
	outside := filepath.Join(t.TempDir(), "outside")
	writeChangeSetFile(t, outside, "sentinel", "keep\n", 0o600)
	tamperLocalMetadata(t, instance.root, func(meta map[string]any) {
		meta["id"] = sibling.ID
		meta["workspace"] = outside
		meta["owner_pid"] = 0
	})

	reaped, err := backend.Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != instance.ID {
		t.Fatalf("reaped = %v, want [%s]", reaped, instance.ID)
	}
	if got, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(got) != "keep\n" {
		t.Fatalf("outside sentinel after Reap = %q, %v", got, err)
	}
	if _, err := os.Stat(sibling.root); err != nil {
		t.Fatalf("sibling instance removed through tampered metadata id: %v", err)
	}
	if _, err := os.Stat(instance.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped instance root still exists: %v", err)
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
	backend, err := NewLocalBackend(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func newPrivilegedTestLocalBackend(t *testing.T, identity LocalIdentity) *LocalBackend {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("no privilege to apply a configured non-root identity")
	}
	parent := t.TempDir()
	relative, err := filepath.Rel(os.TempDir(), parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Skipf("temporary test root %q is not beneath traversable temp root %q", parent, os.TempDir())
	}
	for path := parent; filepath.Clean(path) != filepath.Clean(os.TempDir()); path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := NewLocalBackend(filepath.Join(parent, "instances"), &identity)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func testUnprivilegedIdentities(t *testing.T, count int) []LocalIdentity {
	t.Helper()
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("no unprivileged identity configured: read /etc/passwd: %v", err)
	}
	identities := make([]LocalIdentity, 0, count)
	seen := make(map[uint32]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 || uid == uint64(^uint32(0)) || gid == uint64(^uint32(0)) {
			continue
		}
		convertedUID := uint32(uid)
		if _, duplicate := seen[convertedUID]; duplicate {
			continue
		}
		seen[convertedUID] = struct{}{}
		identities = append(identities, LocalIdentity{UID: convertedUID, GID: uint32(gid)})
		if len(identities) == count {
			return identities
		}
	}
	t.Skipf("no unprivileged identity configured: need %d distinct uid/gid pairs, found %d", count, len(identities))
	return nil
}

func testUnprivilegedIdentitiesWithDistinctGIDs(t *testing.T, count int) []LocalIdentity {
	t.Helper()
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("no unprivileged identity configured: read /etc/passwd: %v", err)
	}
	identities := make([]LocalIdentity, 0, count)
	seenUIDs := make(map[uint32]struct{})
	seenGIDs := make(map[uint32]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 || uid == uint64(^uint32(0)) || gid == uint64(^uint32(0)) {
			continue
		}
		convertedUID, convertedGID := uint32(uid), uint32(gid)
		if _, duplicate := seenUIDs[convertedUID]; duplicate {
			continue
		}
		if _, duplicate := seenGIDs[convertedGID]; duplicate {
			continue
		}
		seenUIDs[convertedUID] = struct{}{}
		seenGIDs[convertedGID] = struct{}{}
		identities = append(identities, LocalIdentity{UID: convertedUID, GID: convertedGID})
		if len(identities) == count {
			return identities
		}
	}
	t.Skipf("no unprivileged identities configured: need %d distinct uid/gid pairs, found %d", count, len(identities))
	return nil
}

func runAsIdentity(identity LocalIdentity, dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: identity.UID, Gid: identity.GID, Groups: []uint32{},
	}}
	return cmd.Run()
}

func chownTree(t *testing.T, root string, identity LocalIdentity) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(path, int(identity.UID), int(identity.GID))
	}); err != nil {
		t.Fatal(err)
	}
}

func pathOwnership(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("filesystem ownership is unavailable on this platform")
	}
	return stat.Uid, stat.Gid
}

func tamperLocalMetadata(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(root, localMetadataName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	mutate(meta)
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
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
