package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
)

// #1820. A warm reload had no command, so the only documented way to trigger
// one was `systemctl --user kill -s HUP gitmoot-daemon` - and that is the
// footgun, not the fix.
//
// Measured on this host (systemd 255): `--kill-whom=` is documented as "Must be
// one of main, control or all... If omitted, defaults to all", so the bare
// `kill` verb signals EVERY process in the unit. The daemon survives, because
// installDaemonReloadHandler deliberately does not cancel on SIGHUP. Its
// dispatched job subprocesses do not: they are external runtime binaries with
// SIGHUP at the default disposition, so they die with exit 128+1 = 129.
//
// Reproduced end to end with a throwaway unit carrying the live daemon's
// KillMode=mixed: parent trapped SIGHUP and survived, child was reaped rc=129,
// unit stayed active. KillMode is not protection here - it governs the STOP
// path and has no bearing on an explicit `kill` verb.
//
// Nor can the #1726 shutdown drain help: it runs from `defer tracker.drain(...)`
// when the SUPERVISOR context is cancelled, and SIGHUP deliberately never
// cancels it. So no drain budget is ever spent on a warm reload and tuning that
// budget cannot change this.
//
// The fix is to give the safe path a name. This signals the ONE recorded daemon
// pid and never a process group, so no job subprocess can receive it.

// daemonReloadSignal is the hook tests replace. Production sends SIGHUP to a
// single positive pid; a NEGATIVE pid would signal the whole process group,
// which is the defect this command exists to avoid.
var daemonReloadSignal = func(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGHUP)
}

func runDaemonReload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon reload does not accept positional arguments")
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "daemon reload: %v\n", err)
		return 1
	}
	state := daemonProcessState(paths)
	pid, stale, err := currentDaemonPID(state)
	if err != nil {
		fmt.Fprintf(stderr, "daemon reload: %v\n", err)
		return 1
	}
	// A reload that reloads nothing must NOT read as success: unlike `stop`,
	// whose goal is satisfied by an absent daemon, an operator running `reload`
	// wants a live process to re-read its config. Reporting 0 here would let a
	// deploy script believe a config change took effect.
	if stale || pid <= 0 {
		if stale {
			writeLine(stdout, "removed stale daemon pid file")
		}
		fmt.Fprintln(stderr, "daemon reload: daemon not running")
		return 1
	}
	if err := daemonReloadSignal(pid); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintln(stderr, "daemon reload: daemon not running")
			return 1
		}
		fmt.Fprintf(stderr, "daemon reload: %v\n", err)
		return 1
	}
	writeLine(stdout, "daemon reload signalled pid %d (SIGHUP to the daemon only; in-flight jobs untouched)", pid)
	return 0
}
