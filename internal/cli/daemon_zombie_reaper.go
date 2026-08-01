package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// startDaemonZombieReaper reaps only detached session leaders spawned by this
// daemon. A blanket wait4(-1) would steal exit statuses from exec.Cmd.Wait for
// ordinary runtime children; Setsid children are the deliberate fire-and-forget
// class whose Process.Release otherwise leaves zombies behind.
func startDaemonZombieReaper(ctx context.Context, stdout io.Writer) {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go func() {
		defer signal.Stop(sigchld)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigchld:
				if reaped, err := reapDetachedDaemonChildren(); err != nil {
					writeLine(stdout, "daemon zombie reaper: %v", err)
				} else if reaped > 0 {
					writeLine(stdout, "daemon zombie reaper: reaped %d detached child process(es)", reaped)
				}
			}
		}
	}()
}

func reapDetachedDaemonChildren() (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	parentPID := os.Getpid()
	reaped := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		state, ppid, session, err := procProcessIdentity(pid)
		if err != nil || state != "Z" || ppid != parentPID || session != pid {
			continue
		}
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if err != nil && err != syscall.ECHILD {
			return reaped, fmt.Errorf("wait4 pid %d: %w", pid, err)
		}
		if waited == pid {
			reaped++
		}
	}
	return reaped, nil
}

func procProcessIdentity(pid int) (state string, parentPID, sessionID int, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, 0, err
	}
	// comm is parenthesized and may contain spaces or parentheses; fields after
	// the final ')' begin with state, ppid, pgrp, and session.
	close := strings.LastIndexByte(string(raw), ')')
	if close < 0 {
		return "", 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(raw[close+1:]))
	if len(fields) < 4 {
		return "", 0, 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, err
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return "", 0, 0, err
	}
	return fields[0], ppid, session, nil
}
