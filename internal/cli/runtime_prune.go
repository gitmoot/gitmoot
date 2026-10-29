package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/toolchain"
)

// runtimeCollectionInterval is how often the daemon collects idle staged
// runtime copies (#2382). The first pass waits one interval after start, so a
// restart never deletes before the new daemon has staged anything.
const runtimeCollectionInterval = 24 * time.Hour

// startRuntimeCollectionLoop removes staged runtime copies that no job staged
// for toolchain.DefaultRuntimeMinIdle and no live process references. Errors
// are logged and retried at the next interval, like transcript retention.
func startRuntimeCollectionLoop(ctx context.Context, paths config.Paths, stdout io.Writer) {
	go func() {
		ticker := time.NewTicker(runtimeCollectionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runRuntimeCollectionPass(paths, now, stdout)
			}
		}
	}()
}

// runtimeCollectionProcRoot is the procfs root both collection paths scan; empty
// means /proc. Tests point it at a synthetic process table.
var runtimeCollectionProcRoot = ""

// runRuntimeCollectionPass is one daemon collection pass: errors and an
// incomplete process scan are logged, never fatal.
func runRuntimeCollectionPass(paths config.Paths, now time.Time, stdout io.Writer) {
	report, err := toolchain.CollectRuntimes(paths.Home, toolchain.RuntimeCollectOptions{MinIdle: toolchain.DefaultRuntimeMinIdle, Now: now, ProcRoot: runtimeCollectionProcRoot})
	state := runtimeCollectionState{
		LastPassAt: now.UTC(), Kept: len(report.Kept), Removed: len(report.Removed),
		RemovedBytes: report.RemovedBytes, Incomplete: report.Incomplete,
	}
	if err != nil {
		state.Error = err.Error()
	}
	writeLine(stdout, "runtime collection: kept %d, removed %d (%d bytes); error=%q incomplete=%q",
		state.Kept, state.Removed, state.RemovedBytes, state.Error, state.Incomplete)
	if err := saveRuntimeCollectionState(paths, state); err != nil {
		writeLine(stdout, "runtime collection: cannot record pass: %v", err)
	}
}

// Keep the last daemon pass outside runtimes/: collection must not remove its
// own record. Manual prune runs do not stand in for a daemon pass.
type runtimeCollectionState struct {
	LastPassAt   time.Time `json:"last_pass_at"`
	Kept         int       `json:"kept"`
	Removed      int       `json:"removed"`
	RemovedBytes int64     `json:"removed_bytes"`
	Incomplete   string    `json:"incomplete,omitempty"`
	Error        string    `json:"error,omitempty"`
}

func saveRuntimeCollectionState(paths config.Paths, state runtimeCollectionState) error {
	file, err := os.CreateTemp(paths.Home, ".runtime-collection-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(state)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(paths.Home, "runtime-collection.json"))
}

func runtimeCollectionDoctorCheck(paths config.Paths, now time.Time) doctor.Check {
	check := doctor.Check{Name: "runtime collector", Required: false}
	data, err := os.ReadFile(filepath.Join(paths.Home, "runtime-collection.json"))
	if errors.Is(err, os.ErrNotExist) {
		check.Detail = "no daemon pass recorded; first pass runs 24h after daemon start"
		return check
	}
	var state runtimeCollectionState
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err != nil {
		check.Detail = fmt.Sprintf("cannot read last daemon pass: %v", err)
		return check
	}
	if state.LastPassAt.IsZero() || state.LastPassAt.After(now) {
		check.Detail = "invalid last daemon pass time"
		return check
	}
	age := now.Sub(state.LastPassAt)
	check.OK = age <= runtimeCollectionInterval+time.Hour && state.Error == "" && state.Incomplete == ""
	check.Detail = fmt.Sprintf("last pass %s (%s ago); kept %d, removed %d (%d bytes)",
		state.LastPassAt.Format(time.RFC3339), age.Round(time.Second), state.Kept, state.Removed, state.RemovedBytes)
	if age > runtimeCollectionInterval+time.Hour {
		check.Detail += "; overdue (24h interval, 1h grace)"
	}
	if state.Error != "" {
		check.Detail += "; failed: " + state.Error
	}
	if state.Incomplete != "" {
		check.Detail += "; incomplete: " + state.Incomplete
	}
	return check
}

func runRuntimePrune(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runtime prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	dryRun := fs.Bool("dry-run", false, "report what would be removed without removing anything")
	minIdle := fs.Duration("min-idle", toolchain.DefaultRuntimeMinIdle, "remove only copies no job staged for at least this long")
	jsonOutput := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "runtime prune does not accept positional arguments")
		return 2
	}
	if *minIdle <= 0 {
		fmt.Fprintln(stderr, "runtime prune: --min-idle must be positive")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "runtime prune: %v\n", err)
		return 1
	}
	report, err := toolchain.CollectRuntimes(paths.Home, toolchain.RuntimeCollectOptions{MinIdle: *minIdle, DryRun: *dryRun, ProcRoot: runtimeCollectionProcRoot})
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(report); encErr != nil {
			fmt.Fprintf(stderr, "runtime prune: %v\n", encErr)
			return 1
		}
	} else {
		verb := "removed"
		if *dryRun {
			verb = "would remove"
		}
		for _, copy := range report.Removed {
			fmt.Fprintf(stdout, "%s %s (last staged %s, %d bytes)\n", verb, copy.Name, copy.LastUsed.UTC().Format(time.RFC3339), copy.Bytes)
		}
		for _, copy := range report.Kept {
			fmt.Fprintf(stdout, "kept %s: %s\n", copy.Name, copy.Reason)
		}
		if report.Incomplete != "" {
			fmt.Fprintf(stdout, "nothing removed: %s\n", report.Incomplete)
		}
		fmt.Fprintf(stdout, "%s %d copies, %d bytes\n", verb, len(report.Removed), report.RemovedBytes)
	}
	if err != nil {
		fmt.Fprintf(stderr, "runtime prune: %v\n", err)
		return 1
	}
	return 0
}
