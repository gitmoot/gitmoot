package execbackend

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"syscall"
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
