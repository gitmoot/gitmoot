package execbackend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// provisionLaunchDirInstance is a synced local instance for the #2563 launch
// dir tests.
func provisionLaunchDirInstance(t *testing.T) (*LocalBackend, *Instance) {
	t.Helper()
	host, _, _ := changeSetRepoPair(t)
	backend := newTestLocalBackend(t)
	instance, err := backend.Provision(context.Background(), JobScope{JobID: "launch-dir"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, Materials{SourceWorktree: host}); err != nil {
		t.Fatal(err)
	}
	return backend, instance
}

func privateLaunchDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "launch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestInstanceRunnerStartsInValidatedLaunchDirOutsideWorkspace: the no-execution
// seat's launch dir (#2364) lies outside the instance workspace, and the local
// backend refused it as an escape (#2563). Named on the runner, it is the
// command's start directory, while every other dir keeps the workspace check.
func TestInstanceRunnerStartsInValidatedLaunchDirOutsideWorkspace(t *testing.T) {
	backend, instance := provisionLaunchDirInstance(t)
	launchDir := privateLaunchDir(t, t.TempDir())
	runner := InstanceRunner{Backend: backend, Instance: instance, LaunchDir: launchDir}
	result, err := runner.Run(context.Background(), launchDir, "sh", "-c", "pwd; ls -A | wc -l")
	if err != nil {
		t.Fatalf("Run in launch dir: %v", err)
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 2 || fields[0] != launchDir || fields[1] != "0" {
		t.Fatalf("launch-dir command saw %q, want start dir %s and no entries", result.Stdout, launchDir)
	}
	if _, err := runner.Run(context.Background(), instance.Workspace, "true"); err != nil {
		t.Fatalf("workspace command with a launch dir named: %v", err)
	}
	outside := t.TempDir()
	if _, err := runner.Run(context.Background(), outside, "true"); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("ordinary out-of-workspace dir with a launch dir named: err = %v, want escapes workspace", err)
	}
	unnamed := InstanceRunner{Backend: backend, Instance: instance}
	if _, err := unnamed.Run(context.Background(), launchDir, "true"); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("launch dir not named on the runner: err = %v, want escapes workspace", err)
	}
}

func TestLocalBackendRefusesUnsafeLaunchDir(t *testing.T) {
	backend, instance := provisionLaunchDirInstance(t)
	cases := []struct {
		name    string
		command func(t *testing.T) Command
		want    string
	}{
		{name: "inside workspace", want: "must lie outside workspace", command: func(t *testing.T) Command {
			return Command{LaunchDir: privateLaunchDir(t, instance.Workspace)}
		}},
		{name: "workspace itself", want: "must lie outside workspace", command: func(t *testing.T) Command {
			if err := os.Chmod(instance.Workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: instance.Workspace}
		}},
		{name: "non-empty", want: "is not empty", command: func(t *testing.T) Command {
			dir := privateLaunchDir(t, t.TempDir())
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: dir}
		}},
		{name: "symlink", want: "is a symlink", command: func(t *testing.T) Command {
			target := privateLaunchDir(t, t.TempDir())
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: link}
		}},
		{name: "symlink into workspace", want: "is a symlink", command: func(t *testing.T) Command {
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(instance.Workspace, link); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: link}
		}},
		{name: "group readable", want: "mode 0700", command: func(t *testing.T) Command {
			dir := privateLaunchDir(t, t.TempDir())
			if err := os.Chmod(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: dir}
		}},
		{name: "not a directory", want: "mode 0700", command: func(t *testing.T) Command {
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0o700); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: file}
		}},
		{name: "missing", want: "inspect local execution launch dir", command: func(t *testing.T) Command {
			return Command{LaunchDir: filepath.Join(t.TempDir(), "absent")}
		}},
		{name: "relative", want: "must be absolute", command: func(t *testing.T) Command {
			return Command{LaunchDir: "launch"}
		}},
		{name: "dir also named", want: "names both directory", command: func(t *testing.T) Command {
			return Command{Dir: instance.Workspace, LaunchDir: privateLaunchDir(t, t.TempDir())}
		}},
		{name: "not owned by the daemon user", want: "not owned by the daemon user", command: func(t *testing.T) Command {
			if os.Geteuid() != 0 {
				t.Skip("no privilege to hand a directory to another uid")
			}
			dir := privateLaunchDir(t, t.TempDir())
			if err := os.Chown(dir, 65534, 65534); err != nil {
				t.Fatal(err)
			}
			return Command{LaunchDir: dir}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command(t)
			command.Name = "true"
			if _, err := backend.Exec(context.Background(), instance, command); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Exec(%+v) error = %v, want %q", command, err, tc.want)
			}
		})
	}
}

// TestLocalBackendRefusesLaunchDirUnderConfiguredIdentity: a configured
// execution uid cannot enter a 0700 directory the daemon user owns, so the
// combination is refused before anything starts rather than failing in chdir.
func TestLocalBackendRefusesLaunchDirUnderConfiguredIdentity(t *testing.T) {
	backend, instance := provisionLaunchDirInstance(t)
	backend.identity = &LocalIdentity{UID: 65534, GID: 65534}
	launchDir := privateLaunchDir(t, t.TempDir())
	if _, err := backend.Exec(context.Background(), instance, Command{LaunchDir: launchDir, Name: "true"}); err == nil || !strings.Contains(err.Error(), "configured execution identity") {
		t.Fatalf("Exec with identity error = %v", err)
	}
}
