package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

// Workspace file operations (#2678 slice 2): read, write, stat, changes,
// pull and push --files, and the bundle that `push` (commits out) fetches.
//
// Transport: each operation is one python3 process in the workspace, started
// through the same process API as exec (envd on E2B, a host process for
// local), running workspace_confine.py + workspace_files.py. envd's own file
// API is not used for the work tree: it resolves the path it is given with
// the envd user's rights, following symlinks, and has no notion of the
// workspace root, so it cannot give the confinement exec has. The python
// operation opens the root by its recorded identity and every path below it
// one component at a time with O_NOFOLLOW, exactly like the exec supervisor
// (the same code). envd's upload is used only to hand a large input (or
// config) to that process, as a file in the guest home whose name gitmoot
// chooses; the operation unlinks it at once.

//go:embed workspace_files.py
var workspaceFilesBody string

// workspaceFilesScript is the VM-side file operation program.
var workspaceFilesScript = workspaceConfineScript + "\n" + workspaceFilesBody

const (
	// workspaceFileInlineMax is the largest input sent inline (base64) in the
	// operation's config argument; a larger one is uploaded first.
	workspaceFileInlineMax = 48 << 10
	// workspaceFileArgMax bounds the config argument (Linux allows 128 KiB
	// for one argument); a longer config is uploaded as a file.
	workspaceFileArgMax = 96 << 10
	// workspaceMirrorBatch is how many paths one mirror pull asks for.
	workspaceMirrorBatch = 256
)

// workspaceChanges is the `changes --json` shape.
type workspaceChanges struct {
	Reset   bool                   `json:"reset"`
	Token   string                 `json:"token"`
	Changed []workspaceChangeEntry `json:"changed"`
	Deleted []string               `json:"deleted"`
}

type workspaceChangeEntry struct {
	Path  string  `json:"path"`
	Type  string  `json:"type"`
	Size  int64   `json:"size"`
	Mtime float64 `json:"mtime"`
	Mode  uint32  `json:"mode"`
}

// workspaceStat is the `stat --json` shape.
type workspaceStat struct {
	Type  string  `json:"type"`
	Size  int64   `json:"size"`
	Mtime float64 `json:"mtime"`
}

// asError keeps a nil *workspaceError a nil error.
func (e *workspaceError) asError() error {
	if e == nil {
		return nil
	}
	return e
}

func workspaceUploadName(kind string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return ".gitmoot-ws-" + kind + "-" + hex.EncodeToString(raw[:]), nil
}

func workspaceEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

// runWorkspaceFileOp runs one file operation in the workspace. fields name
// the operation (op) and its arguments; input, when non-nil, is the
// operation's input bytes; its stdout goes to stdout.
func runWorkspaceFileOp(ctx context.Context, sandbox workspaceSandbox, workspace db.Workspace, fields map[string]any, input io.Reader, stdout io.Writer) *workspaceError {
	settings := map[string]any{
		"root":     workspace.Root,
		"root_dev": uint64(workspace.RootDev),
		"root_ino": uint64(workspace.RootIno),
	}
	for name, value := range fields {
		settings[name] = value
	}
	upload := func(kind string, reader io.Reader) (string, *workspaceError) {
		name, err := workspaceUploadName(kind)
		if err != nil {
			return "", asWorkspaceError(err, wsErrInternal)
		}
		if err := sandbox.UploadHomeFile(ctx, name, reader); err != nil {
			return "", wsErrorf(wsErrProviderUnavailable, "upload to the workspace: %v", err)
		}
		return path.Join(sandbox.Home(), name), nil
	}
	if input != nil {
		head := make([]byte, workspaceFileInlineMax+1)
		count, err := io.ReadFull(input, head)
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			settings["input_b64"] = base64.StdEncoding.EncodeToString(head[:count])
		case err != nil:
			return wsErrorf(wsErrInvalidArgs, "read the input: %v", err)
		default:
			uploaded, failure := upload("in", io.MultiReader(bytes.NewReader(head), input))
			if failure != nil {
				return failure
			}
			settings["input"] = uploaded
		}
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return asWorkspaceError(err, wsErrInternal)
	}
	argument := string(encoded)
	if len(encoded) > workspaceFileArgMax {
		uploaded, failure := upload("cfg", bytes.NewReader(encoded))
		if failure != nil {
			return failure
		}
		argument = "@" + uploaded
	}
	errOut := &workspaceTail{max: 64 << 10}
	if stdout == nil {
		stdout = io.Discard
	}
	_, err = sandbox.Run(ctx, e2b.StartRequest{
		Name:           "python3",
		Args:           []string{"-c", workspaceFilesScript, argument},
		Dir:            sandbox.Home(),
		Env:            workspaceEnvList(workspaceBaseEnv(sandbox.Home(), workspace.Root)),
		Output:         stdout,
		ErrOutput:      errOut,
		MaxOutputBytes: 4 << 10,
	})
	if err == nil {
		return nil
	}
	var exitErr *e2b.ExitError
	if !errors.As(err, &exitErr) {
		return wsErrorf(wsErrProviderUnavailable, "workspace %s operation: %v", fields["op"], err)
	}
	reported := parseWorkspaceFilesError(errOut.String())
	switch {
	case exitErr.Code == workspaceExitMissing:
		reported.Code = wsErrNotFound
	case exitErr.Code == workspaceExitIsDir:
		reported.Code = wsErrIsDirectory
	case reported.Code == "":
		reported.Code = wsErrInternal
	}
	// Exits 3 and 4 are read, write and stat's contract; every other
	// operation fails with 125 and the code alone.
	switch fields["op"] {
	case "read", "write", "stat":
		if exitErr.Code == workspaceExitMissing || exitErr.Code == workspaceExitIsDir {
			reported.exit = exitErr.Code
		}
	}
	if reported.Message == "" {
		reported.Message = fmt.Sprintf("workspace %s operation exited %d: %s", fields["op"], exitErr.Code, strings.TrimSpace(errOut.String()))
	}
	return reported
}

// parseWorkspaceFilesError reads the operation's last stderr line,
// {"error":{"code","message"}}.
func parseWorkspaceFilesError(stderr string) *workspaceError {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	var line struct {
		Error *workspaceError `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err == nil && line.Error != nil {
		return &workspaceError{Code: line.Error.Code, Message: line.Error.Message}
	}
	return &workspaceError{}
}

// withReadyWorkspace attaches workspace id and runs fn with it.
func withReadyWorkspace(home, id string, fn func(ctx context.Context, store *db.Store, paths config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError) error {
	return withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		workspace, sandbox, failure := attachReadyWorkspace(ctx, store, paths, id)
		if failure != nil {
			return failure
		}
		return fn(ctx, store, paths, workspace, sandbox).asError()
	})
}

// workspaceFileArgs parses a file command's flags and returns its
// positionals, with whatever followed "--" appended (so a path that starts
// with "-" can be given after "--").
func workspaceFileArgs(name string, args []string, stdout, stderr io.Writer, define func(*flag.FlagSet)) ([]string, string, bool, int) {
	fs, home := newWorkspaceFlagSet(name, stderr)
	jsonOutput := fs.Bool("json", false, "print JSON")
	if define != nil {
		define(fs)
	}
	positionals, rest, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, "", *jsonOutput, workspaceFlagError(err, stdout, stderr, *jsonOutput)
	}
	return append(positionals, rest...), *home, *jsonOutput, -1
}

func runWorkspaceRead(args []string, stdout, stderr io.Writer) int {
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace read", args, stdout, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(positionals) != 2 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace read <id> <rel>"))
	}
	rel, failure := workspaceRelativePath("path", positionals[1])
	if failure != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, failure)
	}
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "read", "path": rel}, nil, stdout)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	return 0
}

func runWorkspaceWrite(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var modeFlag *string
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace write", args, stdout, stderr, func(fs *flag.FlagSet) {
		modeFlag = fs.String("mode", "", "octal permission bits for the file (default: keep an existing file's mode, 0644 for a new one)")
	})
	if code >= 0 {
		return code
	}
	if len(positionals) != 2 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace write <id> <rel> [--mode OCTAL] < data"))
	}
	rel, failure := workspaceRelativePath("path", positionals[1])
	if failure != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, failure)
	}
	fields := map[string]any{"op": "write", "path": rel, "mode": nil}
	if *modeFlag != "" {
		mode, err := strconv.ParseUint(*modeFlag, 8, 32)
		if err != nil || mode > 0o7777 {
			return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "--mode %q must be octal permission bits (at most 7777)", *modeFlag))
		}
		fields["mode"] = mode
	}
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return runWorkspaceFileOp(ctx, sandbox, workspace, fields, stdin, nil)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	return 0
}

func runWorkspaceStat(args []string, stdout, stderr io.Writer) int {
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace stat", args, stdout, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(positionals) != 2 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace stat <id> <rel> [--json]"))
	}
	rel, failure := workspaceRelativePath("path", positionals[1])
	if failure != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, failure)
	}
	var output bytes.Buffer
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "stat", "path": rel}, nil, &output)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	var info workspaceStat
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInternal, "workspace stat printed %q: %v", output.String(), err))
	}
	if jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, info)
	}
	fmt.Fprintf(stdout, "%s\t%d bytes\tmtime %.9f\n", info.Type, info.Size, info.Mtime)
	return 0
}

func runWorkspaceChanges(args []string, stdout, stderr io.Writer) int {
	var since *string
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace changes", args, stdout, stderr, func(fs *flag.FlagSet) {
		since = fs.String("since", "", `a token from an earlier changes, or "start" for the whole tree`)
	})
	if code >= 0 {
		return code
	}
	if len(positionals) != 1 || *since == "" {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace changes <id> --since <token|start> [--json]"))
	}
	var output bytes.Buffer
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return workspaceChangesOp(ctx, sandbox, workspace, *since, &output)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	if jsonOutput {
		output.WriteByte('\n')
		_, _ = stdout.Write(output.Bytes())
		return 0
	}
	var changes workspaceChanges
	if err := json.Unmarshal(output.Bytes(), &changes); err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInternal, "workspace changes printed invalid JSON: %v", err))
	}
	if changes.Reset {
		fmt.Fprintln(stdout, "reset: the token is unknown or expired; every entry follows")
	}
	for _, entry := range changes.Changed {
		fmt.Fprintf(stdout, "changed\t%s\t%s\n", entry.Type, entry.Path)
	}
	for _, rel := range changes.Deleted {
		fmt.Fprintf(stdout, "deleted\t%s\n", rel)
	}
	fmt.Fprintf(stdout, "token\t%s\n", changes.Token)
	return 0
}

// workspaceChangesStateDir is where the VM keeps the changes manifest:
// under the guest home, outside the work tree.
func workspaceChangesStateDir(guestHome string) string {
	return path.Join(guestHome, ".gitmoot", "workspace", "changes")
}

func workspaceChangesOp(ctx context.Context, sandbox workspaceSandbox, workspace db.Workspace, since string, output io.Writer) *workspaceError {
	return runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{
		"op": "changes", "since": since, "workspace": workspace.ID, "state_dir": workspaceChangesStateDir(sandbox.Home()),
	}, nil, output)
}

func runWorkspacePull(args []string, stdout, stderr io.Writer) int {
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace pull", args, stdout, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(positionals) < 1 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace pull <id> -- <paths...>"))
	}
	paths := make([]string, 0, len(positionals)-1)
	for _, raw := range positionals[1:] {
		rel, failure := workspaceRelativePath("path", raw)
		if failure != nil {
			return reportWorkspaceError(stdout, stderr, jsonOutput, failure)
		}
		paths = append(paths, rel)
	}
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "pull", "paths": paths}, nil, stdout)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	return 0
}

// runWorkspaceRemove unlinks files and symlinks (a symlink itself, never its
// target) and removes empty directories, under the same confinement as the
// other file commands. A missing path is not an error; .git is refused.
func runWorkspaceRemove(args []string, stdout, stderr io.Writer) int {
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace remove", args, stdout, stderr, nil)
	if code >= 0 {
		return code
	}
	if len(positionals) < 1 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace remove <id> -- <paths...>"))
	}
	paths := make([]string, 0, len(positionals)-1)
	for _, raw := range positionals[1:] {
		rel, failure := workspaceRelativePath("path", raw)
		if failure != nil {
			return reportWorkspaceError(stdout, stderr, jsonOutput, failure)
		}
		if rel == "." {
			return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "path %q names the workspace root", raw))
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "path %q is in .git, which workspace remove never touches", raw))
		}
		paths = append(paths, rel)
	}
	var output bytes.Buffer
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, _ *db.Store, _ config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		return runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "remove", "paths": paths}, nil, &output)
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	var removed struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(output.Bytes(), &removed); err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInternal, "workspace remove printed %q: %v", output.String(), err))
	}
	return writeWorkspaceJSON(stdout, stderr, removed)
}

func runWorkspacePush(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var files *bool
	positionals, home, jsonOutput, code := workspaceFileArgs("workspace push", args, stdout, stderr, func(fs *flag.FlagSet) {
		files = fs.Bool("files", false, "apply a tar stream from stdin to the work tree instead of pushing commits")
	})
	if code >= 0 {
		return code
	}
	if len(positionals) != 1 {
		return reportWorkspaceError(stdout, stderr, jsonOutput, wsErrorf(wsErrInvalidArgs, "usage: gitmoot workspace push <id> [--files]"))
	}
	if *files {
		return runWorkspacePushFiles(home, positionals[0], stdin, stdout, stderr, jsonOutput)
	}
	var pushed string
	err := withReadyWorkspace(home, positionals[0], func(ctx context.Context, store *db.Store, paths config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		var failure *workspaceError
		pushed, failure = pushWorkspaceCommits(ctx, store, paths, workspace, sandbox)
		return failure
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	if jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, map[string]string{"sha": pushed})
	}
	fmt.Fprintln(stdout, pushed)
	return 0
}

func runWorkspacePushFiles(home, id string, stdin io.Reader, stdout, stderr io.Writer, jsonOutput bool) int {
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	var applied struct {
		Applied int `json:"applied"`
	}
	err := withReadyWorkspace(home, id, func(ctx context.Context, _ *db.Store, paths config.Paths, workspace db.Workspace, sandbox workspaceSandbox) *workspaceError {
		hostDir, err := workspaceHostDir(paths, workspace.ID)
		if err != nil {
			return asWorkspaceError(err, wsErrInternal)
		}
		spool, err := os.CreateTemp(hostDir, "push-files-*.tar")
		if err != nil {
			return asWorkspaceError(err, wsErrInternal)
		}
		defer os.Remove(spool.Name())
		defer spool.Close()
		// The whole stream is checked before anything is sent, so a refused
		// entry applies nothing.
		if failure := spoolWorkspacePushTar(stdin, spool, workspace.Root); failure != nil {
			return failure
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return asWorkspaceError(err, wsErrInternal)
		}
		var output bytes.Buffer
		if failure := runWorkspaceFileOp(ctx, sandbox, workspace, map[string]any{"op": "push"}, spool, &output); failure != nil {
			return failure
		}
		if err := json.Unmarshal(output.Bytes(), &applied); err != nil {
			return wsErrorf(wsErrInternal, "workspace push printed %q: %v", output.String(), err)
		}
		return nil
	})
	if err != nil {
		return reportWorkspaceError(stdout, stderr, jsonOutput, err)
	}
	if jsonOutput {
		return writeWorkspaceJSON(stdout, stderr, applied)
	}
	fmt.Fprintf(stdout, "applied %d entries\n", applied.Applied)
	return 0
}

// workspaceTarParts validates a tar entry name: relative, no NUL, no ".."
// component, not the root itself. It returns the name's components.
func workspaceTarParts(name string) ([]string, *workspaceError) {
	if strings.ContainsRune(name, 0) {
		return nil, wsErrorf(wsErrInvalidArgs, "tar entry %q contains a NUL byte", name)
	}
	if strings.HasPrefix(name, "/") {
		return nil, wsErrorf(wsErrCwdOutsideRoot, "tar entry %q is an absolute path", name)
	}
	var parts []string
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return nil, wsErrorf(wsErrCwdOutsideRoot, "tar entry %q contains ..", name)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, wsErrorf(wsErrInvalidArgs, "tar entry %q names the workspace root", name)
	}
	return parts, nil
}

// workspaceSymlinkInside reports whether a symlink at parts pointing to
// target stays under root: a relative target never climbs above the root
// from the link's directory, an absolute one names root or a path below it.
func workspaceSymlinkInside(parts []string, target, root string) bool {
	if target == "" || strings.ContainsRune(target, 0) {
		return false
	}
	if path.IsAbs(target) {
		root = path.Clean(root)
		cleaned := path.Clean(target)
		return cleaned == root || strings.HasPrefix(cleaned, root+"/")
	}
	depth := len(parts) - 1
	for _, part := range strings.Split(target, "/") {
		switch part {
		case "", ".":
		case "..":
			depth--
			if depth < 0 {
				return false
			}
		default:
			depth++
		}
	}
	return true
}

// spoolWorkspacePushTar checks every entry of a push --files stream and
// writes a normalized copy (files, directories and symlinks only; names
// cleaned; permission bits only) to spool.
func spoolWorkspacePushTar(input io.Reader, spool io.Writer, root string) *workspaceError {
	reader := tar.NewReader(input)
	writer := tar.NewWriter(spool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return wsErrorf(wsErrInvalidArgs, "read the tar stream: %v", err)
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			return wsErrorf(wsErrInvalidArgs, "tar entry %q is not a file, directory or symlink", header.Name)
		}
		parts, failure := workspaceTarParts(header.Name)
		if failure != nil {
			return failure
		}
		if header.Typeflag == tar.TypeSymlink && !workspaceSymlinkInside(parts, header.Linkname, root) {
			return wsErrorf(wsErrCwdOutsideRoot, "tar entry %q is a symlink to %q, outside the workspace root", header.Name, header.Linkname)
		}
		normalized := &tar.Header{
			Typeflag: header.Typeflag, Name: strings.Join(parts, "/"), Mode: header.Mode & 0o7777,
			Linkname: header.Linkname, ModTime: header.ModTime, Format: tar.FormatPAX,
		}
		if header.Typeflag == tar.TypeReg {
			normalized.Size = header.Size
		}
		if header.Typeflag != tar.TypeSymlink {
			normalized.Linkname = ""
		}
		if err := writer.WriteHeader(normalized); err != nil {
			return asWorkspaceError(err, wsErrInternal)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(writer, reader); err != nil {
				return wsErrorf(wsErrInvalidArgs, "read tar entry %q: %v", header.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		return asWorkspaceError(err, wsErrInternal)
	}
	return nil
}
