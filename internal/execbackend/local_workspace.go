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

// StartWorkspaceProcess starts one process for a local workspace instance in
// a new session, so terminal signals aimed at the caller never reach it. Dir
// must be the instance home or below it. It runs as the configured local
// identity, like Exec. The local backend gives no isolation beyond that: it
// is for development and tests.
func (b *LocalBackend) StartWorkspaceProcess(instance *Instance, process WorkspaceProcess) (*exec.Cmd, error) {
	home, err := b.InstanceHome(instance)
	if err != nil {
		return nil, err
	}
	if process.Name == "" {
		return nil, errors.New("local workspace command is required")
	}
	dir, err := localCommandDir(home, process.Dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(process.Name, process.Args...)
	cmd.Dir = dir
	cmd.Env = append([]string{}, process.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = process.Stdin, process.Stdout, process.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: b.credential()}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start local workspace command: %w", err)
	}
	return cmd, nil
}

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

// StopWorkspaceProcesses ends every process working in home (its current
// directory is home or below it): the workspace supervisors, which kill their
// command's whole process group on SIGTERM, and any command still inside the
// tree. Survivors after wait are killed outright. It fails if any remain.
func StopWorkspaceProcesses(home string, wait time.Duration) error {
	home = filepath.Clean(home)
	signalAll := func(sig syscall.Signal) []int {
		var matched []int
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
			if err != nil {
				continue
			}
			cwd = strings.TrimSuffix(cwd, " (deleted)")
			if cwd != home && !strings.HasPrefix(cwd, home+string(filepath.Separator)) {
				continue
			}
			if syscall.Kill(pid, sig) == nil {
				matched = append(matched, pid)
			}
		}
		return matched
	}
	alive := func(pids []int) []int {
		var left []int
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil && !processZombie(pid) {
				left = append(left, pid)
			}
		}
		return left
	}
	pids := signalAll(syscall.SIGTERM)
	deadline := time.Now().Add(wait)
	for len(alive(pids)) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if left := alive(pids); len(left) > 0 {
		for _, pid := range left {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	signalAll(syscall.SIGKILL)
	deadline = time.Now().Add(5 * time.Second)
	for {
		left := alive(append(pids, signalAll(0)...))
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("local workspace processes %v did not exit", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
