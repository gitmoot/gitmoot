package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Remote workspaces (#2678): a provider sandbox bound to one seat and one
// issue or pull request. The seat keeps its pane and session on the host and
// runs builds and tests in the sandbox through `workspace exec` / `ws sh`.
// The CLI contract is pinned in skills/gitmoot/references/WORKSPACES.md.

// Error codes of the workspace commands. Every failure prints
// {"error":{"code","message"}} as one line on stderr (and on stdout too for
// --json) and exits workspaceCLIFailure, except the file commands' missing
// path (workspaceExitMissing) and directory (workspaceExitIsDir).
const (
	wsErrNotFound            = "not_found"
	wsErrNotReady            = "not_ready"
	wsErrCostCap             = "cost_cap"
	wsErrProviderUnavailable = "provider_unavailable"
	wsErrCwdOutsideRoot      = "cwd_outside_root"
	wsErrAlreadyOpen         = "already_open"
	wsErrInvalidArgs         = "invalid_args"
	wsErrDisabled            = "disabled"
	wsErrInternal            = "internal"
	wsErrIsDirectory         = "is_directory"
	wsErrPushRefused         = "push_refused"
	wsErrNotEmpty            = "not_empty"
	// wsErrRequirementsUnmet: a [repos."o/r".workspace] require check failed
	// at open, which then closed the workspace.
	wsErrRequirementsUnmet = "requirements_unmet"
)

// workspaceCLIFailure is the exit status of a workspace command that failed
// itself, before or around the command it was asked to run (docker run's
// convention). exec and ws sh otherwise exit with the command's own status.
const workspaceCLIFailure = 125

// Exit statuses of read, write and stat for a path that does not exist and
// for a directory where a file was wanted (code not_found / is_directory).
const (
	workspaceExitMissing = 3
	workspaceExitIsDir   = 4
)

// workspaceReservePeriod is one reservation: the hour reserved up front and
// each hour the daemon settles. It equals the provider's per-request TTL
// ceiling, so a sandbox's provider deadline never passes its paid_until.
const workspaceReservePeriod = remoteexec.ProviderMaxTTL

// workspaceBasePATH is the PATH every workspace command starts with: the E2B
// templates' image PATH, which envd itself does not apply.
const workspaceBasePATH = "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Offline test seams. Production leaves them at these values.
var (
	// workspaceGuestHome is the guest home workspaces live under.
	workspaceGuestHome = remoteexec.DefaultWorkspaceGuestHome
	// workspaceEnvdResolver overrides the provider's envd endpoint.
	workspaceEnvdResolver func(sandboxID string, port int) string
	// workspaceGitHubClient reads pull requests for open and the daemon.
	workspaceGitHubClient = func(checkout string) github.Client { return github.NewClient(checkout) }
	// workspaceNow is the ledger's clock.
	workspaceNow = time.Now
)

type workspaceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// FailedCheck, ExitCode and OutputTail are set for requirements_unmet:
	// the require command that failed, its exit status (124 on timeout) and
	// the redacted last workspaceRequireTailBytes of its combined output.
	FailedCheck string  `json:"failed_check,omitempty"`
	ExitCode    *int    `json:"exit_code,omitempty"`
	OutputTail  *string `json:"output_tail,omitempty"`
	// exit overrides workspaceCLIFailure: a file command's missing path (3)
	// or directory (4).
	exit int
}

func (e *workspaceError) Error() string { return e.Code + ": " + e.Message }

func wsErrorf(code, format string, args ...any) *workspaceError {
	return &workspaceError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// asWorkspaceError classifies any error into the pinned error shape.
func asWorkspaceError(err error, fallback string) *workspaceError {
	var wsErr *workspaceError
	if errors.As(err, &wsErr) {
		return wsErr
	}
	var already *db.WorkspaceAlreadyOpenError
	if errors.As(err, &already) {
		return &workspaceError{Code: wsErrAlreadyOpen, Message: err.Error()}
	}
	var capErr *db.WorkspaceCostCapError
	if errors.As(err, &capErr) {
		return &workspaceError{Code: wsErrCostCap, Message: err.Error()}
	}
	if errors.Is(err, db.ErrWorkspaceNotFound) {
		return &workspaceError{Code: wsErrNotFound, Message: err.Error()}
	}
	return &workspaceError{Code: fallback, Message: err.Error()}
}

// reportWorkspaceError prints the error shape and returns the failure exit.
func reportWorkspaceError(stdout, stderr io.Writer, jsonOutput bool, err error) int {
	wsErr := asWorkspaceError(err, wsErrInternal)
	line, _ := json.Marshal(struct {
		Error *workspaceError `json:"error"`
	}{wsErr})
	fmt.Fprintln(stderr, string(line))
	if jsonOutput {
		fmt.Fprintln(stdout, string(line))
	}
	if wsErr.exit != 0 {
		return wsErr.exit
	}
	return workspaceCLIFailure
}

// workspaceJSON is the pinned `open` / `show --json` / `close --json` shape.
// Issue and PR are null when unset; ClosedAt and CloseReason are null until
// the workspace closes. Env is the base environment, in `show` only.
type workspaceJSON struct {
	ID           string            `json:"id"`
	Seat         string            `json:"seat"`
	Repo         string            `json:"repo"`
	Issue        *int64            `json:"issue"`
	PR           *int64            `json:"pr"`
	Branch       string            `json:"branch"`
	Base         string            `json:"base"`
	Root         string            `json:"root"`
	Provider     string            `json:"provider"`
	State        string            `json:"state"`
	CreatedAt    string            `json:"created_at"`
	LastActiveAt string            `json:"last_active_at"`
	ClosedAt     *string           `json:"closed_at"`
	CloseReason  *string           `json:"close_reason"`
	Env          map[string]string `json:"env,omitempty"`
}

// workspaceListJSON is one `ls --json` entry: the pinned fields plus the
// ledger.
type workspaceListJSON struct {
	workspaceJSON
	CostReservedUSD float64 `json:"cost_reserved_usd"`
	CostAccruedUSD  float64 `json:"cost_accrued_usd"`
}

// workspaceDetailJSON is the `open --json` / `show --json` shape: the pinned
// fields plus how open created the workspace (#2704). Template is the
// provider template the sandbox was created from, null for the local
// provider and for a workspace opened before it was recorded. Checks are the
// [repos."o/r".workspace] require commands open ran; a workspace open
// returned passed every one.
type workspaceDetailJSON struct {
	workspaceJSON
	Template *string  `json:"template"`
	Checks   []string `json:"checks"`
}

func newWorkspaceDetailJSON(w db.Workspace, record workspaceOpenRecord) workspaceDetailJSON {
	out := workspaceDetailJSON{workspaceJSON: newWorkspaceJSON(w), Checks: append([]string{}, record.Checks...)}
	if record.Template != "" {
		template := record.Template
		out.Template = &template
	}
	return out
}

func newWorkspaceJSON(w db.Workspace) workspaceJSON {
	out := workspaceJSON{
		ID: w.ID, Seat: w.Seat, Repo: w.Repo, Branch: w.Branch, Base: w.Base, Root: w.Root,
		Provider: w.Provider, State: w.State,
		CreatedAt: formatWorkspaceJSONTime(w.CreatedAt), LastActiveAt: formatWorkspaceJSONTime(w.LastActiveAt),
	}
	if w.Issue > 0 {
		issue := w.Issue
		out.Issue = &issue
	}
	if w.PR > 0 {
		pr := w.PR
		out.PR = &pr
	}
	if !w.ClosedAt.IsZero() {
		closedAt := formatWorkspaceJSONTime(w.ClosedAt)
		out.ClosedAt = &closedAt
	}
	if w.CloseReason != "" {
		reason := w.CloseReason
		out.CloseReason = &reason
	}
	return out
}

func formatWorkspaceJSONTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// workspaceBaseEnv is the fixed environment every workspace command starts
// with. The host environment never reaches the VM; --env adds to this.
func workspaceBaseEnv(guestHome, root string) map[string]string {
	return map[string]string{
		"PATH":                   workspaceBasePATH,
		"HOME":                   guestHome,
		"LANG":                   "C.UTF-8",
		"GITMOOT_WORKSPACE_ROOT": root,
	}
}

func runWorkspace(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printWorkspaceUsage(stdout)
		return 0
	}
	switch args[0] {
	case "open":
		return runWorkspaceOpen(args[1:], stdout, stderr)
	case "show":
		return runWorkspaceShow(args[1:], stdout, stderr)
	case "ls":
		return runWorkspaceList(args[1:], stdout, stderr)
	case "exec":
		return runWorkspaceExec(args[1:], os.Stdin, stdout, stderr)
	case "close":
		return runWorkspaceClose(args[1:], stdout, stderr)
	case "read":
		return runWorkspaceRead(args[1:], stdout, stderr)
	case "write":
		return runWorkspaceWrite(args[1:], os.Stdin, stdout, stderr)
	case "stat":
		return runWorkspaceStat(args[1:], stdout, stderr)
	case "changes":
		return runWorkspaceChanges(args[1:], stdout, stderr)
	case "pull":
		return runWorkspacePull(args[1:], stdout, stderr)
	case "push":
		return runWorkspacePush(args[1:], os.Stdin, stdout, stderr)
	case "remove":
		return runWorkspaceRemove(args[1:], stdout, stderr)
	case "mirror":
		return runWorkspaceMirror(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown workspace command %q\n\n", args[0])
		printWorkspaceUsage(stderr)
		return 2
	}
}

func printWorkspaceUsage(w io.Writer) {
	fmt.Fprintln(w, "Remote workspaces: a VM bound to one seat and one issue or pull request (#2678).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot workspace open --repo owner/repo (--issue N | --pr N) [--json]")
	fmt.Fprintln(w, "  gitmoot workspace show <id> [--json]")
	fmt.Fprintln(w, "  gitmoot workspace ls [--all] [--json]")
	fmt.Fprintln(w, "  gitmoot workspace exec <id> [--cwd REL] [--env K=V]... [--timeout S] [--stdin] [--status-file HOSTPATH] -- <argv...>")
	fmt.Fprintln(w, "  gitmoot workspace close <id> [--reason TEXT] [--json]")
	fmt.Fprintln(w, "  gitmoot workspace read <id> <rel>")
	fmt.Fprintln(w, "  gitmoot workspace write <id> <rel> [--mode OCTAL]")
	fmt.Fprintln(w, "  gitmoot workspace stat <id> <rel> [--json]")
	fmt.Fprintln(w, "  gitmoot workspace changes <id> --since <token|start> [--json]")
	fmt.Fprintln(w, "  gitmoot workspace pull <id> -- <paths...>")
	fmt.Fprintln(w, "  gitmoot workspace push <id> --files")
	fmt.Fprintln(w, "  gitmoot workspace push <id>")
	fmt.Fprintln(w, "  gitmoot workspace remove <id> -- <paths...>")
	fmt.Fprintln(w, "  gitmoot workspace mirror <id> <dir>")
	fmt.Fprintln(w, "  gitmoot ws sh [--cwd REL] '<command>'")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The seat is the acting org role (GITMOOT_ORG_ROLE or the calling pane's seat).")
	fmt.Fprintln(w, "Requires [workspaces] enabled = true. A failure prints")
	fmt.Fprintln(w, `{"error":{"code","message"}} on stderr (and stdout with --json) and exits 125;`)
	fmt.Fprintln(w, "read, write and stat exit 3 for a missing path and 4 for a directory.")
	fmt.Fprintln(w, "See skills/gitmoot/references/WORKSPACES.md.")
}

// parseInterspersed parses flags anywhere among positionals, stopping at "--".
// It returns the positionals and whatever followed "--".
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, []string, error) {
	var positionals, rest []string
	for i, arg := range args {
		if arg == "--" {
			rest = append([]string{}, args[i+1:]...)
			args = args[:i]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positionals, rest, nil
		}
		positionals = append(positionals, args[0])
		args = args[1:]
	}
}

func newWorkspaceFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	return fs, home
}

func workspaceFlagError(err error, stdout, stderr io.Writer, jsonOutput bool) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "%v", err))
}

func runWorkspaceOpen(args []string, stdout, stderr io.Writer) int {
	fs, home := newWorkspaceFlagSet("workspace open", stderr)
	repoFlag := fs.String("repo", "", "repository as owner/repo (registered with gitmoot)")
	issue := fs.Int64("issue", 0, "issue number: opens a new branch ws/<seat>/<issue> from the default branch")
	pr := fs.Int64("pr", 0, "pull request number: checks out the PR's head branch")
	jsonOutput := fs.Bool("json", false, "print the workspace as JSON")
	positionals, _, err := parseInterspersed(fs, args)
	if err != nil {
		return workspaceFlagError(err, stdout, stderr, *jsonOutput)
	}
	if len(positionals) != 0 {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "workspace open takes no positional arguments"))
	}
	if (*issue > 0) == (*pr > 0) || *issue < 0 || *pr < 0 {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "workspace open needs exactly one of --issue N or --pr N"))
	}
	repo, err := github.ParseRepository(*repoFlag)
	if err != nil {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "--repo: %v", err))
	}
	ctx := context.Background()
	seat, err := resolveDispatchOrgRole(ctx, *home, "")
	if err != nil {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "resolve the acting seat: %v", err))
	}
	if seat == "" {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "no acting seat: set GITMOOT_ORG_ROLE or run from a registered seat's pane"))
	}
	var opened db.Workspace
	var record workspaceOpenRecord
	err = withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var openErr error
		opened, record, openErr = openWorkspace(ctx, store, paths, workspaceOpenRequest{Seat: seat, Repo: repo, Issue: *issue, PR: *pr})
		return openErr
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, newWorkspaceDetailJSON(opened, record))
	}
	fmt.Fprintf(stdout, "workspace %s ready: %s %s at %s (%s)\n", opened.ID, opened.Repo, opened.Branch, shortSHA(opened.Base), opened.Root)
	if record.Template != "" {
		fmt.Fprintf(stdout, "template: %s\n", record.Template)
	}
	for _, check := range record.Checks {
		fmt.Fprintf(stdout, "check passed: %s\n", check)
	}
	return 0
}

func writeWorkspaceJSON(stdout, stderr io.Writer, value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return reportWorkspaceError(stdout, stderr, true, err)
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

type workspaceOpenRequest struct {
	Seat  string
	Repo  github.Repository
	Issue int64
	PR    int64
}

// workspaceSourcePlan is what open checks out, resolved on the host before
// any provider spend.
type workspaceSourcePlan struct {
	Checkout string
	Branch   string
	Head     string
	Base     string
	BaseRef  string
}

// openWorkspace provisions, checks out and then checks the workspace: each
// [repos."o/r".workspace] require command runs in order in the ready
// workspace through the exec path. The first that fails closes the workspace
// (reason "requirements_unmet: <command>", through closeWorkspace, so its
// spend settles like any close) and open fails with requirements_unmet.
func openWorkspace(ctx context.Context, store *db.Store, paths config.Paths, request workspaceOpenRequest) (db.Workspace, workspaceOpenRecord, error) {
	policy, err := config.LoadWorkspacesPolicy(paths)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrInvalidArgs, "invalid [workspaces] config: %v", err)
	}
	if !policy.Enabled {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrDisabled, "remote workspaces are off: set [workspaces] enabled = true")
	}
	repoName := request.Repo.FullName()
	repoTables, err := config.LoadRepoWorkspaces(paths)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrInvalidArgs, "invalid [repos.\"owner/repo\".workspace] config: %v", err)
	}
	repoWorkspace, err := repoTables.For(repoName)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrInvalidArgs, "invalid config: %v", err)
	}
	if existing, err := store.FindActiveWorkspace(ctx, request.Seat, repoName, request.Issue, request.PR); err == nil {
		return db.Workspace{}, workspaceOpenRecord{}, &db.WorkspaceAlreadyOpenError{ID: existing.ID}
	} else if !errors.Is(err, db.ErrWorkspaceNotFound) {
		return db.Workspace{}, workspaceOpenRecord{}, err
	}
	record, err := store.GetRepo(ctx, repoName)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrNotFound, "repository %s is not registered with gitmoot: %v", repoName, err)
	}
	// The repository's template, else [workspaces] template, else (inside
	// newWorkspaceProvider) the provider's [remote_exec] e2b_template.
	template := repoWorkspace.Template
	if template == "" {
		template = policy.Template
	}
	provider, err := newWorkspaceProvider(paths, policy.Provider, template)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, err
	}
	opened := workspaceOpenRecord{Template: provider.template, Checks: repoWorkspace.Require}
	reserve := sandboxHoursCostUSD(workspaceReservePeriod, provider.rate)
	if reserve > policy.MaxWorkspaceUSD {
		return db.Workspace{}, workspaceOpenRecord{}, wsErrorf(wsErrCostCap, "one reserved hour ($%.2f at $%.2f/h) passes [workspaces].max_workspace_usd $%.2f", reserve, provider.rate, policy.MaxWorkspaceUSD)
	}
	plan, err := resolveWorkspaceSource(ctx, record, request)
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, err
	}
	id, err := newWorkspaceID()
	if err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, err
	}
	now := workspaceNow().UTC()
	workspace := db.Workspace{
		ID: id, Seat: request.Seat, Repo: repoName, Issue: request.Issue, PR: request.PR,
		Branch: plan.Branch, Base: plan.Head, BaseRef: plan.BaseRef,
		Provider:  provider.name,
		CreatedAt: now, LastActiveAt: now,
		RateUSDPerHour: provider.rate, CostReservedUSD: reserve, PaidUntil: now.Add(workspaceReservePeriod),
	}
	// The reservation is written before the provider is called: a refused
	// cap or a second open for the same seat and subject spends nothing.
	if err := store.CreateWorkspace(ctx, workspace, policy.MaxTotalUSD); err != nil {
		return db.Workspace{}, workspaceOpenRecord{}, err
	}
	fail := func(cause error) (db.Workspace, workspaceOpenRecord, error) {
		closeErr := closeWorkspace(context.WithoutCancel(ctx), store, paths, workspace.ID, "open_failed", io.Discard)
		return db.Workspace{}, workspaceOpenRecord{}, errors.Join(asWorkspaceError(cause, wsErrProviderUnavailable), closeErr)
	}
	// The create's deadline derives from paid_until, recorded above before the
	// provider is called, so even a create whose answer is lost is bounded.
	ttl, err := workspaceTTLUntil(workspace.PaidUntil)
	if err != nil {
		return fail(err)
	}
	instanceID, sandbox, err := provider.backend.provision(ctx, workspace.ID, ttl)
	if err != nil {
		var refused *e2b.RequestRefusedError
		if errors.As(err, &refused) && refused.Operation == e2b.OperationCreate {
			// Only a refused create proves nothing was allocated, so only it
			// releases the reservation at once. Every other failure is
			// ambiguous and stays reserved (closeWorkspace).
			if _, beginErr := store.BeginWorkspaceClose(context.WithoutCancel(ctx), workspace.ID, workspaceReasonOpenFailed); beginErr != nil {
				return db.Workspace{}, workspaceOpenRecord{}, errors.Join(asWorkspaceError(err, wsErrProviderUnavailable), beginErr)
			}
			finishErr := store.FinishWorkspaceClose(context.WithoutCancel(ctx), workspace.ID, db.WorkspaceStateFailed, 0, workspaceNow(), "")
			return db.Workspace{}, workspaceOpenRecord{}, errors.Join(asWorkspaceError(err, wsErrProviderUnavailable), finishErr)
		}
		return fail(err)
	}
	if err := store.SetWorkspaceInstance(ctx, workspace.ID, instanceID, sandbox.Root()); err != nil {
		_, _ = provider.backend.destroy(context.WithoutCancel(ctx), instanceID)
		return fail(err)
	}
	hostDir, err := workspaceHostDir(paths, workspace.ID)
	if err != nil {
		return fail(err)
	}
	if err := writeWorkspaceOpenRecord(hostDir, opened); err != nil {
		return fail(err)
	}
	if _, err := sandbox.Sync(ctx, hostDir, remoteexec.WorkspaceSource{
		Checkout: plan.Checkout, Head: plan.Head, Base: plan.Base, Branch: plan.Branch,
	}, workspaceBasePATH); err != nil {
		return fail(err)
	}
	rootDev, rootIno, err := workspaceRootIdentity(ctx, sandbox)
	if err != nil {
		return fail(err)
	}
	if err := store.MarkWorkspaceReady(ctx, workspace.ID, plan.Branch, plan.Head, plan.BaseRef, int64(rootDev), int64(rootIno), workspaceNow()); err != nil {
		return fail(err)
	}
	ready, err := store.GetWorkspace(ctx, workspace.ID)
	if err != nil {
		return fail(err)
	}
	// The checks run in the ready workspace, so the daemon renews and settles
	// it like any other while they run; open returns it only once all pass.
	for _, check := range opened.Checks {
		unmet, failure := runWorkspaceRequire(ctx, store, ready, sandbox, check)
		if failure != nil {
			return fail(failure)
		}
		if unmet != nil {
			closeErr := closeWorkspace(context.WithoutCancel(ctx), store, paths, workspace.ID, workspaceReasonRequirementsUnmet+": "+check, io.Discard)
			return db.Workspace{}, workspaceOpenRecord{}, errors.Join(unmet, closeErr)
		}
	}
	return ready, opened, nil
}

// workspaceOpenRecord is <home>/workspaces/<id>/open.json (#2704): what open
// created the workspace from, beside close.json. Template is the provider
// template the sandbox was created from ("" for the local provider); Checks
// are the require commands open ran.
type workspaceOpenRecord struct {
	Template string   `json:"template"`
	Checks   []string `json:"checks"`
}

func writeWorkspaceOpenRecord(hostDir string, record workspaceOpenRecord) error {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(hostDir, "open.json"), append(encoded, '\n'), 0o600)
}

// readWorkspaceOpenRecord reads a workspace's open.json. A workspace opened
// before #2704 (or whose open failed before provisioning) has none: that is
// the zero record.
func readWorkspaceOpenRecord(paths config.Paths, id string) (workspaceOpenRecord, error) {
	data, err := os.ReadFile(filepath.Join(paths.Workspaces, id, "open.json"))
	if errors.Is(err, os.ErrNotExist) {
		return workspaceOpenRecord{}, nil
	}
	if err != nil {
		return workspaceOpenRecord{}, err
	}
	var record workspaceOpenRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return workspaceOpenRecord{}, fmt.Errorf("read %s: %w", filepath.Join(paths.Workspaces, id, "open.json"), err)
	}
	return record, nil
}

// Require checks (#2704). Each command gets workspaceRequireTimeout (a
// variable only so the offline tests can shorten it); a failure reports the
// redacted last workspaceRequireTailBytes of the command's combined output,
// cut from at most workspaceRequireCaptureBytes kept while it runs.
var workspaceRequireTimeout = 60 * time.Second

const (
	workspaceRequireTailBytes    = 2 << 10
	workspaceRequireCaptureBytes = 16 << 10
)

// runWorkspaceRequire runs one require command as `bash -lc '<command>'` in
// the workspace root through runWorkspaceCommand, the exec path. It returns
// the requirements_unmet error for a command that exited non-zero or timed
// out (exit 124), or the session's own failure.
func runWorkspaceRequire(ctx context.Context, store *db.Store, workspace db.Workspace, sandbox workspaceSandbox, command string) (unmet, failure *workspaceError) {
	output := &workspaceTail{max: workspaceRequireCaptureBytes}
	status, failure := runWorkspaceCommand(ctx, store, workspace, sandbox, workspaceExecRequest{
		ID: workspace.ID, Cwd: ".", Timeout: workspaceRequireTimeout, Argv: []string{"bash", "-lc", command},
	}, nil, output, output)
	if failure != nil {
		return nil, failure
	}
	if status.ExitCode == 0 && !status.TimedOut {
		return nil, nil
	}
	exitCode := status.ExitCode
	message := fmt.Sprintf("require check %q exited %d", command, exitCode)
	if status.TimedOut {
		exitCode = 124
		message = fmt.Sprintf("require check %q timed out after %s", command, workspaceRequireTimeout)
	}
	tail := workspaceRequireTail(output.String())
	return &workspaceError{Code: wsErrRequirementsUnmet, Message: message, FailedCheck: command, ExitCode: &exitCode, OutputTail: &tail}, nil
}

// workspaceRequireTail redacts output with the job-output redactor before it
// cuts the tail, so a secret split by the cut cannot leak, and advances the
// cut to a rune boundary.
func workspaceRequireTail(output string) string {
	tail := workflow.RedactCommentText(output)
	if len(tail) <= workspaceRequireTailBytes {
		return tail
	}
	tail = tail[len(tail)-workspaceRequireTailBytes:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}

func newWorkspaceID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ws-" + hex.EncodeToString(raw[:]), nil
}

// workspaceHostDir is <home>/workspaces/<id>: the host side of a workspace,
// where its sync scratch lives and where close copies its logs.
func workspaceHostDir(paths config.Paths, id string) (string, error) {
	dir := filepath.Join(paths.Workspaces, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveWorkspaceSource fetches what the workspace checks out into the
// registered host clone, with the host's own credentials. Only the object
// store derived from it reaches the VM.
func resolveWorkspaceSource(ctx context.Context, record db.Repo, request workspaceOpenRequest) (workspaceSourcePlan, error) {
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return workspaceSourcePlan{}, wsErrorf(wsErrNotFound, "repository %s has no registered checkout", record.FullName())
	}
	plan := workspaceSourcePlan{Checkout: checkout}
	if request.PR > 0 {
		pull, err := workspaceGitHubClient(checkout).GetPullRequest(ctx, request.Repo, request.PR)
		if err != nil {
			return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "read pull request #%d: %v", request.PR, err)
		}
		if !strings.EqualFold(strings.TrimSpace(pull.State), "open") {
			return workspaceSourcePlan{}, wsErrorf(wsErrInvalidArgs, "pull request #%d is %s, not open", request.PR, pull.State)
		}
		if head := strings.TrimSpace(pull.HeadRepoFullName); head != "" && !strings.EqualFold(head, request.Repo.FullName()) {
			return workspaceSourcePlan{}, wsErrorf(wsErrInvalidArgs, "pull request #%d comes from fork %s; a workspace needs a branch of %s", request.PR, head, request.Repo.FullName())
		}
		plan.Branch, plan.Head, plan.BaseRef = strings.TrimSpace(pull.HeadRef), strings.ToLower(strings.TrimSpace(pull.HeadSHA)), strings.TrimSpace(pull.BaseRef)
		if plan.Branch == "" || plan.Head == "" || plan.BaseRef == "" {
			return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "pull request #%d has no head branch, head commit or base branch", request.PR)
		}
		if err := gitFetchForWorkspace(ctx, checkout, fmt.Sprintf("refs/pull/%d/head", request.PR), plan.BaseRef); err != nil {
			return workspaceSourcePlan{}, err
		}
		if _, err := workspaceGit(ctx, checkout, "cat-file", "-e", plan.Head+"^{commit}"); err != nil {
			return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "pull request #%d head %s is not in the host clone after fetch: %v", request.PR, plan.Head, err)
		}
		baseTip, err := workspaceGit(ctx, checkout, "rev-parse", "--verify", "refs/remotes/origin/"+plan.BaseRef+"^{commit}")
		if err != nil {
			return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "resolve base branch %s: %v", plan.BaseRef, err)
		}
		mergeBase, err := workspaceGit(ctx, checkout, "merge-base", plan.Head, baseTip)
		if err != nil {
			return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "find the merge base of #%d and %s: %v", request.PR, plan.BaseRef, err)
		}
		plan.Base = mergeBase
		return plan, nil
	}
	plan.BaseRef = strings.TrimSpace(record.DefaultBranch)
	if plan.BaseRef == "" {
		plan.BaseRef = "main"
	}
	plan.Branch = fmt.Sprintf("ws/%s/%d", request.Seat, request.Issue)
	if _, err := workspaceGit(ctx, checkout, "check-ref-format", "refs/heads/"+plan.Branch); err != nil {
		return workspaceSourcePlan{}, wsErrorf(wsErrInvalidArgs, "seat %q does not make a valid branch name %q", request.Seat, plan.Branch)
	}
	if err := gitFetchForWorkspace(ctx, checkout, "", plan.BaseRef); err != nil {
		return workspaceSourcePlan{}, err
	}
	tip, err := workspaceGit(ctx, checkout, "rev-parse", "--verify", "refs/remotes/origin/"+plan.BaseRef+"^{commit}")
	if err != nil {
		return workspaceSourcePlan{}, wsErrorf(wsErrProviderUnavailable, "resolve base branch %s: %v", plan.BaseRef, err)
	}
	plan.Head, plan.Base = tip, tip
	return plan, nil
}

// gitFetchForWorkspace fetches extra (objects only, no ref written) and the
// base branch into its remote-tracking ref, exactly what `git fetch origin
// <base>` would update.
func gitFetchForWorkspace(ctx context.Context, checkout, extra, baseRef string) error {
	args := []string{"-c", "gc.auto=0", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin"}
	if extra != "" {
		args = append(args, extra)
	}
	args = append(args, "+refs/heads/"+baseRef+":refs/remotes/origin/"+baseRef)
	if _, err := workspaceGit(ctx, checkout, args...); err != nil {
		return wsErrorf(wsErrProviderUnavailable, "fetch the workspace source into %s: %v", checkout, err)
	}
	return nil
}

func workspaceGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// workspaceProvider is the execution stack for one provider: the remote
// backend reviews use (E2B, sandboxd), without the job attempt ledger
// (workspaces are ledgered in the workspaces table), or the local backend.
type workspaceProvider struct {
	backend workspaceBackend
	name    string
	rate    float64
	// template is the template a create on this provider uses; "" for the
	// local provider, which has none.
	template string
}

// newWorkspaceProvider builds the provider's stack. template, when set,
// replaces the provider's [remote_exec] e2b_template; only open passes one
// (the repository's or [workspaces] template), since only create uses it,
// and attach, renew and close keep working whatever the sections say.
func newWorkspaceProvider(paths config.Paths, providerName, template string) (*workspaceProvider, error) {
	if providerName == workspaceLocalProvider {
		root := filepath.Join(paths.Home, "execbackends", string(execbackend.Local))
		var identity *execbackend.LocalIdentity
		if cfg, err := config.LoadRemoteExecConfig(paths); err == nil {
			if cfg.LocalRoot != "" {
				root = filepath.Clean(cfg.LocalRoot)
			}
			identity = cfg.LocalIdentity()
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, wsErrorf(wsErrProviderUnavailable, "read [remote_exec]: %v", err)
		}
		local, err := execbackend.NewLocalBackend(root, identity)
		if err != nil {
			return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
		}
		return &workspaceProvider{backend: localWorkspaceBackend{backend: local}, name: workspaceLocalProvider}, nil
	}
	remoteCfg, err := config.LoadRemoteExecConfig(paths)
	if err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "read [remote_exec]: %v", err)
	}
	cfg, err := remoteCfg.ForProvider(providerName)
	if err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	if template != "" {
		cfg.E2BTemplate = template
	}
	if err := cfg.ValidateE2BProvider(); err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	rate := cfg.ExecBackendCost.PerHourUSD
	if !config.IsSandboxdProvider(cfg.Provider) && rate <= 0 {
		return nil, wsErrorf(wsErrCostCap, "the workspace cost cap cannot be enforced without a price: set [remote_exec].cost_per_hour_usd")
	}
	apiKey, err := cfg.LoadE2BAPIKey()
	if err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	client, err := e2b.NewClient(apiKey, e2b.Options{BaseURL: cfg.E2BBaseURL})
	if err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	backend, err := remoteexec.NewBackend(client, remoteexec.Options{
		TemplateID:         cfg.E2BTemplate,
		Envd:               remoteEnvdOptions(cfg, workspaceEnvdResolver),
		WorkspaceGuestHome: workspaceGuestHome,
	})
	if err != nil {
		return nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	return &workspaceProvider{backend: remoteWorkspaceBackend{backend: backend}, name: cfg.Provider, rate: rate, template: cfg.E2BTemplate}, nil
}

func runWorkspaceShow(args []string, stdout, stderr io.Writer) int {
	fs, home := newWorkspaceFlagSet("workspace show", stderr)
	jsonOutput := fs.Bool("json", false, "print the workspace as JSON")
	positionals, _, err := parseInterspersed(fs, args)
	if err != nil {
		return workspaceFlagError(err, stdout, stderr, *jsonOutput)
	}
	if len(positionals) != 1 {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "workspace show takes exactly one workspace id"))
	}
	var workspace db.Workspace
	var opened workspaceOpenRecord
	err = withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var getErr error
		if workspace, getErr = store.GetWorkspace(context.Background(), positionals[0]); getErr != nil {
			return getErr
		}
		opened, getErr = readWorkspaceOpenRecord(paths, workspace.ID)
		return getErr
	})
	if err != nil {
		if errors.Is(err, db.ErrWorkspaceNotFound) {
			err = wsErrorf(wsErrNotFound, "no workspace %s", positionals[0])
		}
		return reportWorkspaceError(stdout, stderr, *jsonOutput, err)
	}
	out := newWorkspaceDetailJSON(workspace, opened)
	out.Env = workspaceBaseEnv(workspaceGuestHomeFor(workspace), workspace.Root)
	if *jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, out)
	}
	template := opened.Template
	if template == "" {
		template = "-"
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(writer, "id\t%s\nseat\t%s\nrepo\t%s\nsubject\t%s\nbranch\t%s\nbase\t%s\nroot\t%s\nprovider\t%s\ntemplate\t%s\nstate\t%s\ncreated\t%s\nlast active\t%s\ncost\t$%.2f accrued, $%.2f reserved\n",
		workspace.ID, workspace.Seat, workspace.Repo, workspaceSubject(workspace), workspace.Branch, workspace.Base, workspace.Root,
		workspace.Provider, template, workspace.State, out.CreatedAt, out.LastActiveAt, workspace.CostAccruedUSD, workspace.CostReservedUSD)
	for _, check := range opened.Checks {
		fmt.Fprintf(writer, "check\t%s\n", check)
	}
	if workspace.CloseReason != "" {
		fmt.Fprintf(writer, "close reason\t%s\n", workspace.CloseReason)
	}
	_ = writer.Flush()
	return 0
}

// workspaceGuestHomeFor derives the guest home from a recorded root.
func workspaceGuestHomeFor(w db.Workspace) string {
	return filepath.ToSlash(filepath.Dir(w.Root))
}

func workspaceSubject(w db.Workspace) string {
	if w.PR > 0 {
		return "pr #" + strconv.FormatInt(w.PR, 10)
	}
	return "issue #" + strconv.FormatInt(w.Issue, 10)
}

func runWorkspaceList(args []string, stdout, stderr io.Writer) int {
	fs, home := newWorkspaceFlagSet("workspace ls", stderr)
	all := fs.Bool("all", false, "include closed and failed workspaces")
	jsonOutput := fs.Bool("json", false, "print the workspaces as a JSON array")
	positionals, _, err := parseInterspersed(fs, args)
	if err != nil {
		return workspaceFlagError(err, stdout, stderr, *jsonOutput)
	}
	if len(positionals) != 0 {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "workspace ls takes no positional arguments"))
	}
	var workspaces []db.Workspace
	if err := withStore(*home, func(store *db.Store) error {
		var listErr error
		workspaces, listErr = store.ListWorkspaces(context.Background(), *all)
		return listErr
	}); err != nil {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		out := make([]workspaceListJSON, 0, len(workspaces))
		for _, w := range workspaces {
			out = append(out, workspaceListJSON{
				workspaceJSON:   newWorkspaceJSON(w),
				CostReservedUSD: w.CostReservedUSD, CostAccruedUSD: w.CostAccruedUSD,
			})
		}
		return writeWorkspaceJSON(stdout, stderr, out)
	}
	if len(workspaces) == 0 {
		fmt.Fprintln(stdout, "no workspaces")
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tSEAT\tREPO\tSUBJECT\tBRANCH\tSTATE\tLAST ACTIVE\tCOST")
	for _, w := range workspaces {
		state := w.State
		if w.CloseReason != "" {
			state += " (" + w.CloseReason + ")"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t$%.2f/$%.2f\n", w.ID, w.Seat, w.Repo, workspaceSubject(w), w.Branch, state,
			formatWorkspaceJSONTime(w.LastActiveAt), w.CostAccruedUSD, w.CostReservedUSD)
	}
	_ = writer.Flush()
	return 0
}

func runWorkspaceClose(args []string, stdout, stderr io.Writer) int {
	fs, home := newWorkspaceFlagSet("workspace close", stderr)
	reason := fs.String("reason", "manual", "why the workspace is closed (recorded as close_reason)")
	jsonOutput := fs.Bool("json", false, "print the closed workspace as JSON")
	positionals, _, err := parseInterspersed(fs, args)
	if err != nil {
		return workspaceFlagError(err, stdout, stderr, *jsonOutput)
	}
	if len(positionals) != 1 {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "workspace close takes exactly one workspace id"))
	}
	if strings.TrimSpace(*reason) == "" {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, wsErrorf(wsErrInvalidArgs, "--reason must not be empty"))
	}
	var closed db.Workspace
	err = withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		if _, err := store.GetWorkspace(ctx, positionals[0]); err != nil {
			if errors.Is(err, db.ErrWorkspaceNotFound) {
				return wsErrorf(wsErrNotFound, "no workspace %s", positionals[0])
			}
			return err
		}
		if err := closeWorkspace(ctx, store, paths, positionals[0], strings.TrimSpace(*reason), stderr); err != nil {
			return wsErrorf(wsErrProviderUnavailable, "%v", err)
		}
		var getErr error
		closed, getErr = store.GetWorkspace(ctx, positionals[0])
		return getErr
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, newWorkspaceJSON(closed))
	}
	fmt.Fprintf(stdout, "workspace %s %s (%s); logs in %s\n", closed.ID, closed.State, closed.CloseReason, filepath.Join(config.WorkDir, closed.ID))
	return 0
}
