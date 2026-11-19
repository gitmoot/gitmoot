package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
	"github.com/gitmoot/gitmoot/internal/github"
)

// Workspace lifecycle (#2678): close (logs out first, then teardown, then the
// ledger settles), the time-based cost ledger, and the daemon sweep. The
// daemon keeps no workspace state in memory: every pass re-reads the
// workspaces table and re-attaches to each sandbox by its recorded ID, so a
// restarted daemon carries on exactly where the previous one stopped.

const (
	// workspaceSweepInterval spaces the daemon's workspace passes.
	workspaceSweepInterval = 30 * time.Second
	// workspaceRenewLead is how long before paid_until the daemon settles the
	// accrued cost and reserves the next period.
	workspaceRenewLead = 10 * time.Minute
	// workspaceOpenAbandonedAfter fails an `open` that never finished: its
	// process died between the reservation and ready.
	workspaceOpenAbandonedAfter = 15 * time.Minute
	// workspaceLogCopyLimit bounds what close copies to the host.
	workspaceLogCopyLimit = 1 << 30
)

// Close reasons the daemon and CLI record.
const (
	workspaceReasonCostCap       = "cost_cap"
	workspaceReasonIdle          = "idle_timeout"
	workspaceReasonPRMerged      = "pr_merged"
	workspaceReasonPRClosed      = "pr_closed"
	workspaceReasonProviderGone  = "provider_gone"
	workspaceReasonExpired       = "expired"
	workspaceReasonOpenFailed    = "open_failed"
	workspaceReasonOpenAbandoned = "open_abandoned"
)

// workspaceAccruedUSD is a workspace's spend at t: its sandbox time up to t,
// never past paid_until (the provider retires the sandbox there), at the
// rate recorded when it opened.
func workspaceAccruedUSD(w db.Workspace, at time.Time) float64 {
	end := at
	if end.After(w.PaidUntil) {
		end = w.PaidUntil
	}
	return sandboxHoursCostUSD(end.Sub(w.CreatedAt), w.RateUSDPerHour)
}

// workspaceCloseRecord is <home>/workspaces/<id>/close.json.
type workspaceCloseRecord struct {
	ID              string  `json:"id"`
	Reason          string  `json:"reason"`
	State           string  `json:"state"`
	ClosedAt        string  `json:"closed_at"`
	LogsCopied      bool    `json:"logs_copied"`
	LogsError       string  `json:"logs_error,omitempty"`
	CostAccruedUSD  float64 `json:"cost_accrued_usd"`
	CostReservedUSD float64 `json:"cost_reserved_usd"`
}

// closeWorkspace closes one workspace: it records the reason (the first one
// wins), copies the VM's logs and a summary of the work tree to
// <home>/workspaces/<id>/, destroys the sandbox, and settles the ledger. A
// teardown the provider cannot confirm leaves the workspace closing, and the
// daemon retries it; the provider deadline (paid_until) bounds the spend
// either way.
func closeWorkspace(ctx context.Context, store *db.Store, paths config.Paths, id, reason string, log io.Writer) error {
	workspace, err := store.GetWorkspace(ctx, id)
	if err != nil {
		return err
	}
	if !workspace.Active() {
		return nil
	}
	if workspace.State != db.WorkspaceStateClosing {
		if _, err := store.BeginWorkspaceClose(ctx, id, reason); err != nil {
			return err
		}
		if workspace, err = store.GetWorkspace(ctx, id); err != nil {
			return err
		}
		if workspace.State != db.WorkspaceStateClosing {
			return nil
		}
	}
	hostDir, err := workspaceHostDir(paths, id)
	if err != nil {
		return err
	}
	record := workspaceCloseRecord{ID: id, Reason: workspace.CloseReason, CostReservedUSD: workspace.CostReservedUSD}
	outcome := db.WorkspaceStateClosed
	if workspace.CloseReason == workspaceReasonOpenFailed || workspace.CloseReason == workspaceReasonOpenAbandoned {
		outcome = db.WorkspaceStateFailed
	}
	if workspace.ProviderInstanceID == "" {
		// The provider never returned a sandbox (or open died before recording
		// it). Nothing can be copied or destroyed; a sandbox created in that gap
		// is retired by its provider deadline, which its reservation covers.
		outcome = db.WorkspaceStateFailed
		record.LogsError = "no sandbox was recorded"
	} else {
		provider, err := newWorkspaceProvider(paths, workspace.Provider)
		if err != nil {
			return fmt.Errorf("close workspace %s: %w", id, err)
		}
		sandbox, state, attachErr := provider.backend.attach(ctx, workspace.ProviderInstanceID)
		switch {
		case attachErr == nil:
			if err := copyWorkspaceLogs(ctx, sandbox, workspace, hostDir); err != nil {
				record.LogsError = err.Error()
				writeLine(log, "workspace %s: copying logs failed before teardown: %v", id, err)
			} else {
				record.LogsCopied = true
			}
		case state == e2b.Gone:
			outcome = db.WorkspaceStateFailed
			record.LogsError = "the sandbox was already gone"
		default:
			// Unreachable is not gone: tear it down anyway so it stops costing.
			record.LogsError = attachErr.Error()
			writeLine(log, "workspace %s: cannot reach the sandbox to copy logs: %v", id, attachErr)
		}
		if state != e2b.Gone {
			destroyed, err := provider.backend.destroy(ctx, workspace.ProviderInstanceID)
			if err != nil || destroyed != e2b.Gone {
				return fmt.Errorf("close workspace %s: the provider did not confirm teardown (%s): %v", id, destroyed, err)
			}
		}
	}
	now := workspaceNow().UTC()
	accrued := workspaceAccruedUSD(workspace, now)
	record.State, record.ClosedAt, record.CostAccruedUSD = outcome, now.Format(time.RFC3339), accrued
	if encoded, err := json.MarshalIndent(record, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(hostDir, "close.json"), append(encoded, '\n'), 0o600)
	}
	return store.FinishWorkspaceClose(ctx, id, outcome, accrued, now)
}

// workspaceLogScript writes a summary of the work tree beside the exec logs
// and streams both as a gzip tar on stdout. $1 is the guest's workspace state
// directory, $2 the work tree.
const workspaceLogScript = `set -u
mkdir -p "$1/logs"
cd "$1"
{
  echo "# git status"
  git -C "$2" status --short --branch
  echo
  echo "# commits since gitmoot-base"
  git -C "$2" log --oneline gitmoot-base..HEAD
  echo
  echo "# work tree diff stat against gitmoot-base"
  git -C "$2" diff --stat gitmoot-base
} > close-summary.txt 2>&1
tar -czf - logs close-summary.txt`

// copyWorkspaceLogs extracts the VM's exec logs and work-tree summary into
// hostDir. Only regular files and directories under hostDir are written.
func copyWorkspaceLogs(ctx context.Context, sandbox workspaceSandbox, workspace db.Workspace, hostDir string) error {
	reader, writer := io.Pipe()
	extracted := make(chan error, 1)
	go func() {
		err := extractWorkspaceLogs(reader, hostDir)
		_ = reader.CloseWithError(err)
		extracted <- err
	}()
	stateDir := path.Dir(remoteexec.WorkspaceLogDir(sandbox.Home()))
	result, runErr := sandbox.Run(ctx, e2b.StartRequest{
		Name:           "sh",
		Args:           []string{"-c", workspaceLogScript, "gitmoot-ws-logs", stateDir, workspace.Root},
		Dir:            sandbox.Home(),
		Output:         writer,
		ErrOutput:      io.Discard,
		MaxOutputBytes: 4 << 10,
	})
	_ = writer.CloseWithError(runErr)
	extractErr := <-extracted
	if runErr != nil {
		return fmt.Errorf("%w: %s", runErr, strings.TrimSpace(result.Stderr))
	}
	return extractErr
}

func extractWorkspaceLogs(reader io.Reader, hostDir string) error {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("read workspace logs: %w", err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	var total int64
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read workspace logs: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." || name == ".." || path.IsAbs(name) || strings.HasPrefix(name, "../") {
			continue
		}
		target := filepath.Join(hostDir, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if total+header.Size > workspaceLogCopyLimit {
				return fmt.Errorf("workspace logs exceed %d bytes; copied the first files only", workspaceLogCopyLimit)
			}
			total += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, io.LimitReader(archive, header.Size))
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		}
	}
}

// workspaceSweepGate runs one workspace pass at a time and at most once per
// workspaceSweepInterval, however many repos the daemon ticks.
type workspaceSweepGate struct {
	mu        sync.Mutex
	next      time.Time
	configErr string
}

var workspaceSweepState = &workspaceSweepGate{}

// workspaceSweepTimeout bounds one daemon pass, which runs on the tick.
const workspaceSweepTimeout = 2 * time.Minute

// sweepWorkspacesTick is the daemon's entry: gated, logged, never fatal.
func sweepWorkspacesTick(ctx context.Context, worker jobWorker, stdout io.Writer, now time.Time) {
	if worker.Store == nil || !workspaceSweepState.mu.TryLock() {
		return
	}
	defer workspaceSweepState.mu.Unlock()
	if now.Before(workspaceSweepState.next) {
		return
	}
	workspaceSweepState.next = now.Add(workspaceSweepInterval)
	paths, err := worker.configPaths()
	if err != nil {
		writeLine(stdout, "workspace sweep skipped: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, workspaceSweepTimeout)
	defer cancel()
	if err := sweepWorkspaces(ctx, worker.Store, paths, stdout); err != nil {
		writeLine(stdout, "workspace sweep: %v", err)
	}
}

// sweepWorkspaces runs one pass over every open workspace: the global cap,
// abandoned opens, unfinished closes, idle timeouts, and the hourly
// settlement that reserves each workspace's next period and moves its
// provider deadline to the new paid_until.
func sweepWorkspaces(ctx context.Context, store *db.Store, paths config.Paths, stdout io.Writer) error {
	active, err := store.ListWorkspaces(ctx, false)
	if err != nil || len(active) == 0 {
		return err
	}
	policy, err := config.LoadWorkspacesPolicy(paths)
	if err != nil {
		// The caps still apply: an invalid section falls back to the defaults
		// rather than to no cap at all.
		if message := err.Error(); workspaceSweepState.configErr != message {
			workspaceSweepState.configErr = message
			writeLine(stdout, "workspace sweep: invalid [workspaces] config, using the defaults: %v", err)
		}
		policy = config.DefaultWorkspacesPolicy()
	} else {
		workspaceSweepState.configErr = ""
	}
	var errs []error
	closeFor := func(w db.Workspace, reason string) {
		if err := closeWorkspace(ctx, store, paths, w.ID, reason, stdout); err != nil {
			errs = append(errs, err)
			return
		}
		writeLine(stdout, "workspace %s closed: %s", w.ID, reason)
	}
	// A lowered max_total_usd stops spending now: close the newest workspaces
	// until what is committed fits again.
	for _, w := range active {
		committed, err := store.WorkspaceCommittedUSD(ctx)
		if err != nil {
			return err
		}
		if committed <= policy.MaxTotalUSD+1e-9 {
			break
		}
		if w.State == db.WorkspaceStateReady || w.State == db.WorkspaceStateOpening {
			closeFor(w, workspaceReasonCostCap)
		}
	}
	providers := map[string]*workspaceProvider{}
	// Oldest first: when the cap cannot renew them all, the workspace that
	// has been working longest keeps its VM.
	for i := len(active) - 1; i >= 0; i-- {
		w, err := store.GetWorkspace(ctx, active[i].ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		now := workspaceNow().UTC()
		switch w.State {
		case db.WorkspaceStateClosing:
			closeFor(w, w.CloseReason)
		case db.WorkspaceStateOpening:
			if now.Sub(w.CreatedAt) >= workspaceOpenAbandonedAfter {
				closeFor(w, workspaceReasonOpenAbandoned)
			}
		case db.WorkspaceStateReady:
			switch {
			case !now.Before(w.PaidUntil):
				closeFor(w, workspaceReasonExpired)
			case now.Sub(w.LastActiveAt) >= policy.IdleTimeout:
				closeFor(w, workspaceReasonIdle)
			case w.PaidUntil.Sub(now) <= workspaceRenewLead:
				provider := providers[w.Provider]
				if provider == nil {
					if provider, err = newWorkspaceProvider(paths, w.Provider); err != nil {
						errs = append(errs, fmt.Errorf("renew workspace %s: %w", w.ID, err))
						continue
					}
					providers[w.Provider] = provider
				}
				if reason, err := renewWorkspace(ctx, store, provider, w, policy, now); err != nil {
					errs = append(errs, err)
				} else if reason != "" {
					closeFor(w, reason)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// renewWorkspace settles w's accrued cost and reserves up to one period ahead
// of now, then moves the provider deadline to the new paid_until. A refused
// reservation returns the cost_cap close reason; a sandbox the provider no
// longer has returns provider_gone.
func renewWorkspace(ctx context.Context, store *db.Store, provider *workspaceProvider, w db.Workspace, policy config.WorkspacesPolicy, now time.Time) (string, error) {
	paidUntil := now.Add(workspaceReservePeriod)
	add := sandboxHoursCostUSD(paidUntil.Sub(w.PaidUntil), w.RateUSDPerHour)
	err := store.ExtendWorkspaceReservation(ctx, w.ID, add, paidUntil, workspaceAccruedUSD(w, now), policy.MaxTotalUSD, policy.MaxWorkspaceUSD)
	var capErr *db.WorkspaceCostCapError
	if errors.As(err, &capErr) {
		return workspaceReasonCostCap, nil
	}
	if err != nil {
		return "", fmt.Errorf("renew workspace %s: %w", w.ID, err)
	}
	state, err := provider.backend.extend(ctx, w.ProviderInstanceID, paidUntil.Sub(now))
	if state == e2b.Gone {
		return workspaceReasonProviderGone, nil
	}
	if err != nil {
		return "", fmt.Errorf("renew workspace %s: extend the provider deadline: %w", w.ID, err)
	}
	return "", nil
}

// workspacePullRequestReconciler closes a repo's workspaces whose pull
// request merged or closed. The daemon calls it from each repo poll with
// that poll's open pull requests. An issue workspace follows the pull
// request later opened from its branch. A repo with no open workspace costs
// one local query and no GitHub call.
func workspacePullRequestReconciler(store *db.Store, rawHome string, gh github.Client, repo github.Repository, stdout io.Writer) func(context.Context, []github.PullRequest) error {
	return func(ctx context.Context, open []github.PullRequest) error {
		active, err := store.ListWorkspaces(ctx, false)
		if err != nil {
			return err
		}
		var mine []db.Workspace
		for _, w := range active {
			if strings.EqualFold(w.Repo, repo.FullName()) && (w.State == db.WorkspaceStateReady || w.State == db.WorkspaceStateOpening) {
				mine = append(mine, w)
			}
		}
		if len(mine) == 0 {
			return nil
		}
		paths, err := pathsFromFlag(rawHome)
		if err != nil {
			return err
		}
		openByNumber := map[int64]bool{}
		openByBranch := map[string]int64{}
		for _, pull := range open {
			openByNumber[pull.Number] = true
			if head := strings.TrimSpace(pull.HeadRepoFullName); head == "" || strings.EqualFold(head, repo.FullName()) {
				openByBranch[pull.HeadRef] = pull.Number
			}
		}
		var errs []error
		for _, w := range mine {
			number := w.PR
			if w.Issue > 0 {
				if w.LinkedPR == 0 {
					if linked, ok := openByBranch[w.Branch]; ok && w.Branch != "" {
						if err := store.SetWorkspaceLinkedPR(ctx, w.ID, linked); err != nil {
							errs = append(errs, err)
						}
					}
					continue
				}
				number = w.LinkedPR
			}
			if number <= 0 || openByNumber[number] {
				continue
			}
			// The open listing is a prefilter; the pull request itself is the
			// evidence, read before anything is torn down.
			pull, err := gh.GetPullRequest(ctx, repo, number)
			if err != nil {
				errs = append(errs, fmt.Errorf("workspace %s: read pull request #%d: %w", w.ID, number, err))
				continue
			}
			if strings.EqualFold(strings.TrimSpace(pull.State), "open") {
				continue
			}
			reason := workspaceReasonPRClosed
			if pull.Merged || strings.TrimSpace(pull.MergedAt) != "" {
				reason = workspaceReasonPRMerged
			}
			if err := closeWorkspace(ctx, store, paths, w.ID, reason, stdout); err != nil {
				errs = append(errs, err)
				continue
			}
			writeLine(stdout, "workspace %s closed: %s #%d", w.ID, reason, number)
		}
		return errors.Join(errs...)
	}
}
