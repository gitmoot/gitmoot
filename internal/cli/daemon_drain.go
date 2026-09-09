package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// #1207, bounded half: DRAIN LETS A BUSY FLEET REACH AN IDLE WINDOW.
//
// The deploy recipe requires zero engine-dispatched jobs before a restart, and
// on a continuously busy fleet that window never arrives - so either deploys
// stop or the rule gets broken quietly. Drain makes the precondition satisfiable
// by CONSTRUCTION: stop claiming, let in-flight work finish, then restart into a
// real idle window.
//
// This is NOT the re-exec handoff the issue also describes. That needs FD
// preservation across exec and an owner-scale decision about process identity;
// this needs neither, and it removes the reason a verified binary sat undeployed
// while seven jobs ran.
//
// THE SENTINEL IS A FILE, NOT A ROW, for one reason: drain must hold when the
// store is the thing being contended. A daemon whose queue query is blocked
// behind a 160 MB WAL still stats a file in microseconds, and the night this was
// written the store was exactly that contended. A drain flag that needs a
// healthy database to be read cannot be trusted when it is most needed.
const daemonDrainSentinelName = "drain"

func daemonDrainSentinelPath(configHome string) (string, error) {
	paths, err := pathsFromFlag(configHome)
	if err != nil {
		return "", err
	}
	// Paths.Home is the RESOLVED <home>/.gitmoot root, which is where the daemon
	// already keeps its own state. Never re-resolve it (#446/#459).
	return filepath.Join(paths.Home, daemonDrainSentinelName), nil
}

// daemonDrainActive reports whether the daemon should stop claiming new work.
//
// It takes the store for signature symmetry with the guards beside it and does
// not read it; see the sentinel comment above. A missing file is the normal
// case and is not an error. Any OTHER stat error returns the error rather than
// false: failing open would resume dispatch during a deploy, which is the one
// outcome drain exists to prevent.
// daemonDrainActive reports whether the operator has drained this daemon.
//
// IT TAKES A RESOLVED PATH AND DOES ONE STAT. No home resolution, no syscall
// beyond the stat, nothing that can fail in a way that stalls dispatch.
func daemonDrainActive(sentinelPath string) (bool, error) {
	if sentinelPath == "" {
		// Resolution failed at construction and was announced there. Claiming is
		// the lesser evil versus refusing every job; see defaultJobWorker.
		return false, nil
	}
	return drainSentinelPresent(sentinelPath)
}

func daemonDrainActiveForHome(configHome string) (bool, error) {
	// NO EMPTY-HOME SHORTCUT. "" IS THE DEFAULT HOME, NOT THE ABSENCE OF ONE.
	//
	// P1 found in review (#2096). worker.ConfigHome and cfg.Home are EMPTY for
	// the documented no-flag invocation - daemonChildArgs drops --home from the
	// spawned child when home == "" (daemon_lifecycle.go:1598). Meanwhile
	// setDaemonDrain("") writes the sentinel through pathsFromFlag("") ->
	// config.DefaultPaths(). So the WRITER resolved the default and the READER
	// returned early, and `gitmoot daemon drain` against a default-started
	// daemon printed "stopped claiming new work" while the daemon kept claiming.
	// Drain was a NO-OP in exactly the configuration the docs describe.
	//
	// The reader now takes the same path as the writer, so the two cannot
	// disagree. daemonDrainSentinelPath already routes "" to DefaultPaths.
	path, err := daemonDrainSentinelPath(configHome)
	if err != nil {
		return false, err
	}
	return drainSentinelPresent(path)
}

// drainSentinelPresent is the ONE stat, shared by the hot path and the CLI so
// they cannot drift on what "present" or "unreadable" means.
func drainSentinelPresent(path string) (bool, error) {
	switch _, statErr := os.Stat(path); {
	case statErr == nil:
		return true, nil
	case errors.Is(statErr, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("read drain sentinel %q: %w", path, statErr)
	}
}

func setDaemonDrain(configHome string, on bool) error {
	path, err := daemonDrainSentinelPath(configHome)
	if err != nil {
		return err
	}
	if !on {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("clear drain sentinel %q: %w", path, removeErr)
		}
		return nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return mkErr
	}
	// The body is provenance, not state: the file's EXISTENCE is the flag, so a
	// truncated or partially written body still drains. Anything that depends on
	// parsing it would fail open, which is the direction that loses work.
	body := fmt.Sprintf("draining since %s\n", time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(path, []byte(body), 0o600)
}

// engineDispatchedInFlight counts the jobs a drain is waiting for.
//
// SESSION JOBS ARE EXCLUDED, matching the deploy recipe's own rule: a
// session-recorded job runs entirely outside the daemon process, holds no
// subprocess and no lease, and may legitimately stay running for hours. Counting
// them would make the drain never finish and teach operators to pass a deadline
// that defeats the purpose.
func engineDispatchedInFlight(ctx context.Context, store *db.Store) ([]db.Job, error) {
	var inFlight []db.Job
	// ONLY "running". An earlier version also scanned "claimed", which is NOT a
	// member of workflow.JobState and which no row has ever carried (measured:
	// zero of 14,917 job rows). It could only ever return empty - via a FULL
	// UNINDEXED SCAN of jobs, because the partial indexes cover running/queued/
	// blocked and nothing else. Every 5s, against exactly the contended store
	// the file sentinel exists to avoid depending on. Found in review (#2096).
	for _, state := range []string{"running"} {
		jobs, err := store.ListJobsByState(ctx, state)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if strings.HasPrefix(job.ID, "session-") {
				continue
			}
			inFlight = append(inFlight, job)
		}
	}
	return inFlight, nil
}

func runDaemonDrain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon drain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to wait for in-flight jobs before reporting what remains")
	clear := fs.Bool("clear", false, "stop draining and resume claiming")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon drain does not accept positional arguments")
		return 2
	}

	if *clear {
		if err := setDaemonDrain(*home, false); err != nil {
			fmt.Fprintf(stderr, "daemon drain: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "drain cleared: the daemon will resume claiming queued work")
		return 0
	}

	if err := setDaemonDrain(*home, true); err != nil {
		fmt.Fprintf(stderr, "daemon drain: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "draining: the daemon has stopped claiming new work; queued jobs stay queued")

	deadline := time.Now().Add(*timeout)
	var remaining []db.Job
	err := withStoreAndPaths(*home, func(_ config.Paths, store *db.Store) error {
		for {
			jobs, err := engineDispatchedInFlight(context.Background(), store)
			if err != nil {
				return err
			}
			remaining = jobs
			if len(jobs) == 0 || !time.Now().Before(deadline) {
				return nil
			}
			time.Sleep(5 * time.Second)
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "daemon drain: %v\n", err)
		return 1
	}

	if len(remaining) == 0 {
		fmt.Fprintln(stdout, "drained: no engine-dispatched job is in flight, it is safe to restart")
		fmt.Fprintln(stdout, "run `gitmoot daemon drain --clear` after the restart, or the new daemon will not claim.")
		return 0
	}

	// RECORDED, NOT KILLED, AND DELIBERATELY NOT PARKED.
	//
	// #1207's acceptance says an overdue job must be "parked with a recorded
	// reason, never killed silently". This delivers the RECORD and declines the
	// PARK, and the split is a deliberate scope decision rather than an omission.
	//
	// The record is the half that matters and is now durable: each straggler gets
	// a daemon_drain_deadline_exceeded event naming the deadline, so the reason
	// survives the terminal this command printed to.
	//
	// The park is declined because parking is a STATE TRANSITION on a running job
	// this command cannot see inside. It holds a lease, a subprocess and possibly
	// a worktree; moving it to parked from a CLI would strand or terminate work
	// whose progress is invisible here, which is precisely the loss drain exists
	// to prevent. An operator who decides a job should stop has `gitmoot job
	// cancel`, which is explicit and attributable. Doing it implicitly on a
	// timeout would make a deadline into a killer.
	if err := recordDrainDeadlineExceeded(*home, remaining, *timeout); err != nil {
		fmt.Fprintf(stderr, "daemon drain: record deadline: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nstill in flight after %s - the restart is NOT safe yet:\n", *timeout)
	for _, job := range remaining {
		fmt.Fprintf(stdout, "  %s  %s  %s\n", job.ID, job.Type, job.Agent)
	}
	fmt.Fprintln(stdout, "\nEach is recorded with a daemon_drain_deadline_exceeded event; none was parked or killed.")
	fmt.Fprintln(stdout, "\nDrain remains ACTIVE, so nothing new is being claimed and the set can only shrink.")
	fmt.Fprintln(stdout, "Wait and re-run, or decide about these jobs explicitly before restarting.")
	return 1
}

// recordDrainDeadlineExceeded writes the reason an operator will need later.
//
// Best-effort per job is NOT acceptable here - a partially recorded deadline is
// the shape this campaign keeps finding, where a report reads complete and the
// store holds less. Any failure is returned so the command exits non-zero and
// the operator knows the record is incomplete.
func recordDrainDeadlineExceeded(configHome string, jobs []db.Job, timeout time.Duration) error {
	if len(jobs) == 0 {
		return nil
	}
	return withStoreAndPaths(configHome, func(_ config.Paths, store *db.Store) error {
		for _, job := range jobs {
			if err := store.AddJobEvent(context.Background(), db.JobEvent{
				JobID: job.ID,
				Kind:  "daemon_drain_deadline_exceeded",
				Message: fmt.Sprintf(
					"still in flight when a %s drain deadline elapsed; NOT parked and NOT killed - "+
						"drain remains active so nothing new is claimed, and this job was left to finish. "+
						"Stopping it is an explicit operator decision (gitmoot job cancel), never a timeout's.",
					timeout),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
