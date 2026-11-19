package cli

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	remoteexec "github.com/gitmoot/gitmoot/internal/execbackend/remote"
)

// workspaceConfineScript is the root confinement every VM-side workspace
// script starts with: identity-checked root, O_NOFOLLOW walk.
//
//go:embed workspace_confine.py
var workspaceConfineScript string

//go:embed workspace_supervisor.py
var workspaceSupervisorBody string

// workspaceSupervisorScript runs every workspace command inside the VM. It is
// the VM-side watchdog the exec contract requires: see its header.
var workspaceSupervisorScript = workspaceConfineScript + "\n" + workspaceSupervisorBody

// workspaceStatusMark starts the supervisor's status trailer, the last bytes
// it writes to stderr.
var workspaceStatusMark = []byte("\x1e\x1fgitmoot-ws-status ")

// Exec session timing. The host sends a heartbeat frame every
// workspaceHeartbeat; the supervisor kills the command's process group after
// workspaceWatchdogGrace without one. Variables only so the offline tests can
// shorten them.
var (
	workspaceHeartbeat     = 2 * time.Second
	workspaceWatchdogGrace = 15 * time.Second
	// workspaceSignalDrain bounds how long exec waits for the remote command
	// to end after the host process is told to stop.
	workspaceSignalDrain = 10 * time.Second
	// workspaceTouchInterval is how often a running exec moves last_active_at.
	workspaceTouchInterval = time.Minute
)

// workspaceKillGrace is how long after a forwarded signal or timeout the
// command's group gets SIGKILL. It is the only copy: the supervisor receives
// it as kill_grace, and the host backstop is built from the same value.
const workspaceKillGrace = 3 * time.Second

// workspaceInputChunk bounds one stdin frame.
const workspaceInputChunk = 32 << 10

var workspaceEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// workspaceExecStatus is the --status-file shape, written last. Error is
// non-null exactly when gitmoot itself failed (exit 125), which tells such a
// failure apart from a command that exits 125 or 124 on its own.
type workspaceExecStatus struct {
	ExitCode int             `json:"exit_code"`
	Signal   *int            `json:"signal"`
	TimedOut bool            `json:"timed_out"`
	Error    *workspaceError `json:"error"`
}

type workspaceExecRequest struct {
	ID         string
	Cwd        string
	Env        []string
	Timeout    time.Duration
	Stdin      bool
	StatusFile string
	Argv       []string
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runWorkspaceExec(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, home := newWorkspaceFlagSet("workspace exec", stderr)
	cwd := fs.String("cwd", ".", "working directory relative to the workspace root; must stay under it")
	var env multiFlag
	fs.Var(&env, "env", "K=V added to the command's environment (repeatable); the host environment is never passed")
	timeout := fs.Float64("timeout", 0, "seconds (fractions allowed) before the command's process group is killed (exit 124); 0 means none")
	withStdin := fs.Bool("stdin", false, "connect this process's stdin to the command (default: /dev/null)")
	statusFile := fs.String("status-file", "", `host path written last with {"exit_code","signal","timed_out","error"}`)
	positionals, argv, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "%v", err))
	}
	request := workspaceExecRequest{Cwd: *cwd, Env: env, Timeout: time.Duration(*timeout * float64(time.Second)), Stdin: *withStdin, StatusFile: *statusFile, Argv: argv}
	if len(positionals) != 1 || len(argv) == 0 {
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace exec <id> [flags] -- <argv...>"))
	}
	if *timeout < 0 {
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "--timeout must not be negative"))
	}
	request.ID = positionals[0]
	return executeInWorkspace(*home, request, stdin, stdout, stderr)
}

// runWS is the short form for agents: `gitmoot ws sh [--cwd REL] '<cmd>'`
// runs exactly one command string as `bash -lc` in $GITMOOT_WORKSPACE.
func runWS(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printWSUsage(stdout)
		return 0
	}
	if args[0] != "sh" {
		fmt.Fprintf(stderr, "unknown ws command %q\n\n", args[0])
		printWSUsage(stderr)
		return 2
	}
	fs, home := newWorkspaceFlagSet("ws sh", stderr)
	cwd := fs.String("cwd", ".", "working directory relative to the workspace root; must stay under it")
	statusFile := fs.String("status-file", "", `host path written last with {"exit_code","signal","timed_out","error"}`)
	positionals, rest, err := parseInterspersed(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "%v", err))
	}
	positionals = append(positionals, rest...)
	if len(positionals) != 1 {
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "ws sh takes exactly one command string, got %d arguments", len(positionals)))
	}
	id := strings.TrimSpace(os.Getenv("GITMOOT_WORKSPACE"))
	if id == "" {
		return finishWorkspaceExec(stdout, stderr, *statusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "GITMOOT_WORKSPACE is not set"))
	}
	return executeInWorkspace(*home, workspaceExecRequest{
		ID: id, Cwd: *cwd, StatusFile: *statusFile, Argv: []string{"bash", "-lc", positionals[0]},
	}, nil, stdout, stderr)
}

func printWSUsage(w io.Writer) {
	fmt.Fprintln(w, "Short forms for the workspace named by $GITMOOT_WORKSPACE (#2678).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot ws sh [--cwd REL] [--status-file HOSTPATH] '<command>'")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Runs exactly one command string as `bash -lc '<command>'` in the workspace, like")
	fmt.Fprintln(w, "`gitmoot workspace exec $GITMOOT_WORKSPACE --cwd REL -- bash -lc '<command>'`.")
}

// finishWorkspaceExec reports a result: on a gitmoot failure the error shape
// on stderr and exit 125; the status file, when asked for, is written last.
func finishWorkspaceExec(stdout, stderr io.Writer, statusFile string, status workspaceExecStatus, failure *workspaceError) int {
	if failure != nil {
		status = workspaceExecStatus{ExitCode: workspaceCLIFailure, Error: failure}
		reportWorkspaceError(stdout, stderr, false, failure)
	}
	if statusFile != "" {
		if err := writeWorkspaceStatusFile(statusFile, status); err != nil {
			fmt.Fprintf(stderr, `{"error":{"code":"internal","message":%q}}`+"\n", "write status file: "+err.Error())
			return workspaceCLIFailure
		}
	}
	return status.ExitCode
}

func writeWorkspaceStatusFile(file string, status workspaceExecStatus) error {
	encoded, err := json.Marshal(status)
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	temp, err := os.CreateTemp(dir, ".gitmoot-status-*")
	if err != nil {
		return err
	}
	_, writeErr := temp.Write(append(encoded, '\n'))
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(temp.Name())
		return err
	}
	if err := os.Rename(temp.Name(), file); err != nil {
		_ = os.Remove(temp.Name())
		return err
	}
	return nil
}

// workspaceRelativeCwd validates --cwd lexically: relative, and still under
// the root once cleaned. The supervisor re-checks the resolved path in the VM,
// so a symlink cannot lead out either. The argument is taken exactly as
// given: only the empty string is special (the root); whitespace is part of
// a name.
func workspaceRelativeCwd(cwd string) (string, *workspaceError) {
	return workspaceRelativePath("--cwd", cwd)
}

// workspaceRelativePath validates a path argument relative to the workspace
// root, exactly as given (never trimmed): "" is the root, an absolute path or
// one that climbs out with .. is cwd_outside_root, a NUL byte invalid_args.
func workspaceRelativePath(what, rel string) (string, *workspaceError) {
	if rel == "" {
		return ".", nil
	}
	if strings.ContainsRune(rel, 0) {
		return "", wsErrorf(wsErrInvalidArgs, "%s contains a NUL byte", what)
	}
	if path.IsAbs(rel) {
		return "", wsErrorf(wsErrCwdOutsideRoot, "%s %q must be relative to the workspace root", what, rel)
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", wsErrorf(wsErrCwdOutsideRoot, "%s %q leaves the workspace root", what, rel)
	}
	return cleaned, nil
}

func executeInWorkspace(home string, request workspaceExecRequest, stdin io.Reader, stdout, stderr io.Writer) int {
	cwd, cwdErr := workspaceRelativeCwd(request.Cwd)
	if cwdErr != nil {
		return finishWorkspaceExec(stdout, stderr, request.StatusFile, workspaceExecStatus{}, cwdErr)
	}
	request.Cwd = cwd
	for _, entry := range request.Env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !workspaceEnvName.MatchString(name) {
			return finishWorkspaceExec(stdout, stderr, request.StatusFile, workspaceExecStatus{}, wsErrorf(wsErrInvalidArgs, "--env %q must be NAME=VALUE", entry))
		}
	}
	var status workspaceExecStatus
	var failure *workspaceError
	err := withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		status, failure = runWorkspaceSession(store, paths, request, stdin, stdout, stderr)
		return nil
	})
	if err != nil {
		failure = asWorkspaceError(err, wsErrInternal)
	}
	return finishWorkspaceExec(stdout, stderr, request.StatusFile, status, failure)
}

// workspaceSessionFrame is one control-channel frame for the supervisor.
func workspaceSessionFrame(kind byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// attachReadyWorkspace loads a ready workspace with a recorded root
// identity, moves its last_active_at and attaches its sandbox. A sandbox the
// provider retired closes the record (provider_gone).
func attachReadyWorkspace(ctx context.Context, store *db.Store, paths config.Paths, id string) (db.Workspace, workspaceSandbox, *workspaceError) {
	workspace, err := store.GetWorkspace(ctx, id)
	if err != nil {
		if errors.Is(err, db.ErrWorkspaceNotFound) {
			return db.Workspace{}, nil, wsErrorf(wsErrNotFound, "no workspace %s", id)
		}
		return db.Workspace{}, nil, asWorkspaceError(err, wsErrInternal)
	}
	if workspace.State == db.WorkspaceStateReady && workspace.RootDev == 0 && workspace.RootIno == 0 {
		return db.Workspace{}, nil, wsErrorf(wsErrNotReady, "workspace %s has no recorded root identity; close it and open a new one", workspace.ID)
	}
	if workspace.State != db.WorkspaceStateReady {
		return db.Workspace{}, nil, wsErrorf(wsErrNotReady, "workspace %s is %s", workspace.ID, workspace.State)
	}
	provider, err := newWorkspaceProvider(paths, workspace.Provider, "")
	if err != nil {
		return db.Workspace{}, nil, asWorkspaceError(err, wsErrProviderUnavailable)
	}
	if err := store.TouchWorkspace(ctx, workspace.ID, workspaceNow()); err != nil {
		return db.Workspace{}, nil, asWorkspaceError(err, wsErrInternal)
	}
	sandbox, state, err := provider.backend.attach(ctx, workspace.ProviderInstanceID)
	if err != nil {
		if state == e2b.Gone {
			// The provider retired the sandbox (its deadline passed, or it was
			// deleted elsewhere). Settle the record now rather than leave a
			// ready workspace nothing can run in.
			_ = closeWorkspace(ctx, store, paths, workspace.ID, "provider_gone", io.Discard)
			return db.Workspace{}, nil, wsErrorf(wsErrNotReady, "workspace %s: its sandbox is gone", workspace.ID)
		}
		return db.Workspace{}, nil, wsErrorf(wsErrProviderUnavailable, "%v", err)
	}
	return workspace, sandbox, nil
}

func runWorkspaceSession(store *db.Store, paths config.Paths, request workspaceExecRequest, stdin io.Reader, stdout, stderr io.Writer) (workspaceExecStatus, *workspaceError) {
	ctx := context.Background()
	workspace, sandbox, failure := attachReadyWorkspace(ctx, store, paths, request.ID)
	if failure != nil {
		return workspaceExecStatus{}, failure
	}
	return runWorkspaceCommand(ctx, store, workspace, sandbox, request, stdin, stdout, stderr)
}

// runWorkspaceCommand runs request.Argv in an attached workspace under the
// supervisor: the one exec path, shared by `workspace exec`, `ws sh` and
// open's require checks.
func runWorkspaceCommand(ctx context.Context, store *db.Store, workspace db.Workspace, sandbox workspaceSandbox, request workspaceExecRequest, stdin io.Reader, stdout, stderr io.Writer) (workspaceExecStatus, *workspaceError) {
	env := workspaceBaseEnv(sandbox.Home(), workspace.Root)
	for _, entry := range request.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	logName := fmt.Sprintf("%s-%d.log", workspaceNow().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	config := map[string]any{
		"root":       workspace.Root,
		"cwd":        request.Cwd,
		"env":        env,
		"argv":       request.Argv,
		"timeout":    request.Timeout.Seconds(),
		"grace":      workspaceWatchdogGrace.Seconds(),
		"kill_grace": workspaceKillGrace.Seconds(),
		"root_dev":   uint64(workspace.RootDev),
		"root_ino":   uint64(workspace.RootIno),
		"stdin":      request.Stdin,
		"log":        path.Join(remoteexec.WorkspaceLogDir(sandbox.Home()), logName),
	}
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return workspaceExecStatus{}, asWorkspaceError(err, wsErrInternal)
	}

	// Signals are caught before the command starts, so a SIGINT or SIGTERM
	// at any point is forwarded rather than killing this process outright.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	frames := make(chan []byte, 16)
	started := make(chan int, 1)
	trailer := &workspaceStatusTrailer{out: stderr}
	stream, err := sandbox.Start(sessionCtx, e2b.StartRequest{
		Name:           "python3",
		Args:           []string{"-c", workspaceSupervisorScript, string(encodedConfig)},
		Dir:            sandbox.Home(),
		Output:         stdout,
		ErrOutput:      trailer,
		MaxOutputBytes: 64 << 10,
		Stdin:          true,
		OnStart:        func(pid int) { started <- pid },
	})
	if err != nil {
		return workspaceExecStatus{}, wsErrorf(wsErrProviderUnavailable, "start the workspace command: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := stream.Wait()
		done <- waitErr
	}()

	// One sender keeps frames whole and in order: two concurrent SendInput
	// calls could interleave their bytes in the supervisor's stdin pipe.
	var senderWG sync.WaitGroup
	sendCtx, stopSending := context.WithCancel(ctx)
	defer stopSending()
	pidKnown := make(chan int, 1)
	// Kill requests have their own queue, which the sender always drains
	// first: a stdin backlog can delay a kill by at most the one SendInput in
	// flight, and the supervisor (the watchdog) is never signalled itself.
	kills := make(chan byte, 1)
	senderWG.Add(1)
	go func() {
		defer senderWG.Done()
		var pid int
		select {
		case pid = <-started:
			pidKnown <- pid
		case <-sendCtx.Done():
			return
		}
		ticker := time.NewTicker(workspaceHeartbeat)
		defer ticker.Stop()
		touched := time.Now()
		heartbeat := workspaceSessionFrame('H', nil)
		_ = sandbox.SendInput(sendCtx, pid, heartbeat)
		for {
			select {
			case sig := <-kills:
				_ = sandbox.SendInput(sendCtx, pid, workspaceSessionFrame('K', []byte{sig}))
				continue
			default:
			}
			select {
			case <-sendCtx.Done():
				return
			case sig := <-kills:
				_ = sandbox.SendInput(sendCtx, pid, workspaceSessionFrame('K', []byte{sig}))
			case frame := <-frames:
				_ = sandbox.SendInput(sendCtx, pid, frame)
			case <-ticker.C:
				_ = sandbox.SendInput(sendCtx, pid, heartbeat)
				if time.Since(touched) >= workspaceTouchInterval {
					touched = time.Now()
					_ = store.TouchWorkspace(sendCtx, workspace.ID, workspaceNow())
				}
			}
		}
	}()
	if request.Stdin && stdin != nil {
		go func() {
			buffer := make([]byte, workspaceInputChunk)
			for {
				count, readErr := stdin.Read(buffer)
				if count > 0 {
					select {
					case frames <- workspaceSessionFrame('D', append([]byte(nil), buffer[:count]...)):
					case <-sendCtx.Done():
						return
					}
				}
				if readErr != nil {
					select {
					case frames <- workspaceSessionFrame('E', nil):
					case <-sendCtx.Done():
					}
					return
				}
			}
		}()
	}

	var hostDeadline <-chan time.Time
	if request.Timeout > 0 {
		// The supervisor enforces --timeout; this only bounds a supervisor that
		// stopped answering.
		backstop := time.NewTimer(request.Timeout + workspaceKillGrace + workspaceSignalDrain)
		defer backstop.Stop()
		hostDeadline = backstop.C
	}
	var waitErr error
	var hostSignal syscall.Signal
	var drain <-chan time.Time
wait:
	for {
		select {
		case waitErr = <-done:
			break wait
		case received := <-signals:
			if hostSignal == 0 {
				hostSignal = received.(syscall.Signal)
				drainTimer := time.NewTimer(workspaceSignalDrain)
				defer drainTimer.Stop()
				drain = drainTimer.C
			}
			select {
			case pid := <-pidKnown:
				pidKnown <- pid
				select {
				case kills <- byte(received.(syscall.Signal)):
				default:
					// A kill is already queued; it covers this one.
				}
			default:
				// Not started yet: drop the request. A process that starts anyway
				// never receives a heartbeat, so its watchdog ends it.
				cancelSession()
			}
		case <-hostDeadline:
			waitErr = errors.New("the workspace command did not end after its timeout")
			break wait
		case <-drain:
			waitErr = fmt.Errorf("the workspace command did not end within %s of %s", workspaceSignalDrain, hostSignal)
			break wait
		}
	}
	stopSending()
	senderWG.Wait()
	reported, ok := trailer.finish()
	if ok {
		if reported.Error != nil && (reported.Error.Code == wsErrCwdOutsideRoot || reported.Error.Code == wsErrInvalidArgs) {
			return workspaceExecStatus{}, reported.Error
		}
		if reported.Error != nil && hostSignal == 0 {
			// The supervisor lost its control channel while this process was
			// alive: the command was killed under it.
			return workspaceExecStatus{}, wsErrorf(wsErrProviderUnavailable, "the exec session was cut off: %s", reported.Error.Message)
		}
		reported.Error = nil
		return reported, nil
	}
	if hostSignal != 0 {
		return workspaceExecStatus{ExitCode: 128 + int(hostSignal), Signal: nil}, nil
	}
	if waitErr == nil {
		waitErr = errors.New("the workspace supervisor ended without reporting a status")
	}
	return workspaceExecStatus{}, wsErrorf(wsErrProviderUnavailable, "%v", waitErr)
}

// workspaceStatusTrailer passes stderr through and holds back the
// supervisor's trailing status. Output that only looks like the mark is
// written out once the stream proves it was not the trailer.
type workspaceStatusTrailer struct {
	mu      sync.Mutex
	out     io.Writer
	pending []byte
	trailer []byte
	found   bool
}

const workspaceTrailerMax = 64 << 10

func (w *workspaceStatusTrailer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := len(data)
	if w.found {
		w.trailer = append(w.trailer, data...)
		if len(w.trailer) <= workspaceTrailerMax {
			return count, nil
		}
		// Too long to be the trailer: it was output.
		held := append(append([]byte(nil), workspaceStatusMark...), w.trailer...)
		w.found, w.trailer = false, nil
		w.pending = append(w.pending[:0], held...)
		data = nil
	}
	w.pending = append(w.pending, data...)
	if index := bytes.Index(w.pending, workspaceStatusMark); index >= 0 {
		if err := w.flush(w.pending[:index]); err != nil {
			return count, err
		}
		w.trailer = append([]byte(nil), w.pending[index+len(workspaceStatusMark):]...)
		w.found = true
		w.pending = w.pending[:0]
		return count, nil
	}
	keep := 0
	for n := min(len(w.pending), len(workspaceStatusMark)-1); n > 0; n-- {
		if bytes.HasSuffix(w.pending, workspaceStatusMark[:n]) {
			keep = n
			break
		}
	}
	if err := w.flush(w.pending[:len(w.pending)-keep]); err != nil {
		return count, err
	}
	w.pending = append(w.pending[:0], w.pending[len(w.pending)-keep:]...)
	return count, nil
}

func (w *workspaceStatusTrailer) flush(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	_, err := w.out.Write(data)
	return err
}

// finish returns the parsed trailer, or writes back whatever was held.
func (w *workspaceStatusTrailer) finish() (workspaceExecStatus, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.found {
		var status workspaceExecStatus
		decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(w.trailer)))
		if err := decoder.Decode(&status); err == nil && !decoder.More() && status.ExitCode >= 0 && status.ExitCode <= 255 {
			_ = w.flush(w.pending)
			return status, true
		}
		_ = w.flush(append(append([]byte(nil), workspaceStatusMark...), w.trailer...))
	}
	_ = w.flush(w.pending)
	return workspaceExecStatus{}, false
}
