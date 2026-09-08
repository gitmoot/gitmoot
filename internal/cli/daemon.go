package cli

import (
	"fmt"
	"io"
)

func runDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printDaemonUsage(stdout)
		return 0
	}
	switch args[0] {
	case "start":
		return runDaemonStart(args[1:], stdout, stderr)
	case "run":
		return runDaemonRun(args[1:], stdout, stderr)
	case "stop":
		return runDaemonStop(args[1:], stdout, stderr)
	case "restart":
		return runDaemonRestart(args[1:], stdout, stderr)
	case "reload":
		return runDaemonReload(args[1:], stdout, stderr)
	case "status":
		return runDaemonStatus(args[1:], stdout, stderr)
	case "logs":
		return runDaemonLogs(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown daemon command %q\n\n", args[0])
		printDaemonUsage(stderr)
		return 2
	}
}

func printDaemonUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot daemon start [--repo owner/repo] [--poll 30s] [--workers 1 | --parallel N] [--scheduler barrier|pool] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon run [--repo owner/repo] [--poll 30s] [--workers 1 | --parallel N] [--scheduler barrier|pool] [--watch-issues]")
	fmt.Fprintln(w, "  gitmoot daemon stop")
	fmt.Fprintln(w, "  gitmoot daemon restart")
	fmt.Fprintln(w, "  gitmoot daemon reload")
	fmt.Fprintln(w, "  gitmoot daemon status")
	fmt.Fprintln(w, "  gitmoot daemon logs")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  --repo owner/repo SCOPES the daemon to a SINGLE repo: it polls only that repo's PRs and")
	fmt.Fprintln(w, "  claims only that repo's queued jobs. Omit --repo to supervise ALL enabled registered repos.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  reload sends SIGHUP to the recorded daemon pid ONLY, so runtime config is re-read")
	fmt.Fprintln(w, "  without disturbing in-flight jobs. Do NOT use `systemctl kill -s HUP`: --kill-whom")
	fmt.Fprintln(w, "  defaults to `all`, which signals every process in the unit and kills dispatched")
	fmt.Fprintln(w, "  runtime binaries with exit 129 (#1820).")
}
