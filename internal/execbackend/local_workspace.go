package execbackend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WorkspaceProcess describes one process of a local remote-workspace instance
// (#2678, `[workspaces] provider = "local"`). Env is the process's whole
// environment: nothing is inherited from the calling process.
type WorkspaceProcess struct {
	Dir    string
	Env    []string
	Name   string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// InstanceHome is the instance's own directory, which holds its workspace.
// Local workspaces use it as the guest home.
func (b *LocalBackend) InstanceHome(instance *Instance) (string, error) {
	if err := b.validateInstance(instance); err != nil {
		return "", err
	}
	return filepath.Clean(instance.root), nil
}

// WriteWorkspaceHomeFile writes reader to a new file (0600) named name
// directly in the instance home, owned by the configured local identity: the
// local counterpart of an envd upload, for a workspace file operation to
// read and unlink. name is one plain file name; an existing file is refused.
func (b *LocalBackend) WriteWorkspaceHomeFile(instance *Instance, name string, reader io.Reader) error {
	home, err := b.InstanceHome(instance)
	if err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("local workspace file name %q is not one plain file name", name)
	}
	target := filepath.Join(home, name)
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create local workspace file: %w", err)
	}
	_, copyErr := io.Copy(file, reader)
	var chownErr error
	if b.identity != nil {
		chownErr = file.Chown(int(b.identity.UID), int(b.identity.GID))
	}
	if err := errors.Join(copyErr, chownErr, file.Close()); err != nil {
		_ = os.Remove(target)
		return fmt.Errorf("write local workspace file: %w", err)
	}
	return nil
}

// StartWorkspaceProcess starts one process for a local workspace instance in
// a new session, so terminal signals aimed at the caller never reach it. Dir
// must be the instance home or below it. It runs as the configured local
// identity, like Exec. The local backend gives no isolation beyond that: it
// is for development and tests.
//
// The process is recorded (PID and start time) under the instance's procs
// directory before this returns, so StopWorkspaceProcesses finds it from any
// later CLI process. The returned forget removes the record once the caller
// has reaped the process.
func (b *LocalBackend) StartWorkspaceProcess(instance *Instance, process WorkspaceProcess) (*exec.Cmd, func(), error) {
	home, err := b.InstanceHome(instance)
	if err != nil {
		return nil, nil, err
	}
	if process.Name == "" {
		return nil, nil, errors.New("local workspace command is required")
	}
	dir, err := localCommandDir(home, process.Dir)
	if err != nil {
		return nil, nil, err
	}
	procs := filepath.Join(home, localWorkspaceProcsDir)
	if err := os.MkdirAll(procs, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create local workspace process records: %w", err)
	}
	cmd := exec.Command(process.Name, process.Args...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, process.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = process.Stdin, process.Stdout, process.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: b.credential()}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start local workspace command: %w", err)
	}
	pid := cmd.Process.Pid
	record := filepath.Join(procs, strconv.Itoa(pid))
	if err := os.WriteFile(record, []byte(localProcessStartTime(pid)), 0o600); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("record local workspace process: %w", err)
	}
	return cmd, func() { _ = os.Remove(record) }, nil
}

// localWorkspaceProcsDir holds one file per running workspace process, named
// by its PID and holding its start time, so a PID reused by another process
// is never signalled.
const localWorkspaceProcsDir = ".gitmoot-procs"

// FindInstanceByJobID returns the instance whose metadata carries jobID, or
// "" when none does. The instance directory is the whole local inventory, so
// an empty result proves no such instance exists.
func (b *LocalBackend) FindInstanceByJobID(jobID string) (string, error) {
	entries, err := os.ReadDir(b.root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("scan local execution-backend root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := readLocalMetadata(filepath.Join(b.root, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read local execution instance %q: %w", entry.Name(), err)
		}
		if meta.JobID == jobID {
			return meta.ID, nil
		}
	}
	return "", nil
}

// StopWorkspaceProcesses ends every process of a local workspace instance
// and verifies they are gone. It signals the recorded workspace processes
// (each supervisor kills its command's whole process group on SIGTERM) and
// every process whose working directory is inside the instance, compared on
// the canonical path because /proc reports physical paths. Survivors after
// wait get SIGKILL. It returns an error unless every one of them is verified
// gone, so a caller never reports a teardown it could not confirm.
func StopWorkspaceProcesses(home string, wait time.Duration) error {
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		return fmt.Errorf("resolve local workspace instance %s: %w", home, err)
	}
	canonical = filepath.Clean(canonical)
	type target struct {
		pid   int
		start string
	}
	collect := func() ([]target, error) {
		var targets []target
		entries, err := os.ReadDir(filepath.Join(canonical, localWorkspaceProcsDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read local workspace process records: %w", err)
		}
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 0 {
				continue
			}
			start, err := os.ReadFile(filepath.Join(canonical, localWorkspaceProcsDir, entry.Name()))
			if err != nil {
				return nil, fmt.Errorf("read local workspace process record %d: %w", pid, err)
			}
			targets = append(targets, target{pid: pid, start: strings.TrimSpace(string(start))})
		}
		procs, err := os.ReadDir("/proc")
		if err != nil {
			return nil, fmt.Errorf("scan processes: %w", err)
		}
		for _, entry := range procs {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
			if err != nil {
				continue
			}
			cwd = strings.TrimSuffix(cwd, " (deleted)")
			if cwd == canonical || strings.HasPrefix(cwd, canonical+string(filepath.Separator)) {
				// A process that exited between the two reads has no start
				// time; it is gone, and its PID must not be signalled later.
				if start := localProcessStartTime(pid); start != "" {
					targets = append(targets, target{pid: pid, start: start})
				}
			}
		}
		return targets, nil
	}
	// alive is the subset still running as the same process: same start
	// time (a reused PID has another), a process leader rather than a thread
	// (/proc/<tid> answers for threads, and signalling a thread ID signals its
	// whole process), never this process, and not merely awaiting a reaper.
	alive := func(targets []target) []target {
		var left []target
		for _, t := range targets {
			if t.start == "" || t.pid == os.Getpid() || localProcessStartTime(t.pid) != t.start || !processLeader(t.pid) || processZombie(t.pid) {
				continue
			}
			left = append(left, t)
		}
		return left
	}
	signal := func(targets []target, sig syscall.Signal) {
		for _, t := range alive(targets) {
			_ = syscall.Kill(t.pid, sig)
		}
	}
	targets, err := collect()
	if err != nil {
		return err
	}
	signal(targets, syscall.SIGTERM)
	deadline := time.Now().Add(wait)
	for len(alive(targets)) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		more, err := collect()
		if err != nil {
			return err
		}
		targets = append(targets, more...)
		left := alive(targets)
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			pids := make([]int, len(left))
			for i, t := range left {
				pids[i] = t.pid
			}
			return fmt.Errorf("local workspace processes %v did not exit", pids)
		}
		signal(left, syscall.SIGKILL)
		time.Sleep(20 * time.Millisecond)
	}
}

// processLeader reports whether pid names a process (its thread group
// leader), not one thread of another process.
func processLeader(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "Tgid:"); ok {
			return strings.TrimSpace(value) == strconv.Itoa(pid)
		}
	}
	return false
}

// processZombie reports whether pid has exited and only awaits its reaper.
func processZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}
