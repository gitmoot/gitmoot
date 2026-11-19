package cli

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Slice 2 of remote workspaces (#2678): file operations, changes, mirror and
// commits out, on the offline E2B fake and on provider = "local".

func forEachWorkspaceProvider(t *testing.T, test func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON)) {
	for _, local := range []bool{false, true} {
		name := "e2b-fake"
		if local {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			env := newWorkspaceTestEnv(t, workspaceTestConfig{local: local})
			test(t, env, env.open("--issue", "12"))
		})
	}
}

func (e *workspaceTestEnv) mustRun(stdin io.Reader, args ...string) string {
	e.t.Helper()
	result := e.runWithStdin(stdin, args...)
	if result.code != 0 {
		e.t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, result.code, result.stdout, result.stderr)
	}
	return result.stdout
}

func (e *workspaceTestEnv) expectFailure(code int, errCode string, stdin io.Reader, args ...string) {
	e.t.Helper()
	result := e.runWithStdin(stdin, args...)
	if result.code != code || workspaceErrorCode(e.t, result.stderr) != errCode {
		e.t.Fatalf("%v = exit %d %q, want exit %d code %s", args, result.code, result.stderr, code, errCode)
	}
}

func fileMode(t *testing.T, file string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestWorkspaceFilesReadWriteStat(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		// write creates parents; a new file gets 0644.
		env.mustRun(strings.NewReader("hello\n"), "workspace", "write", id, "dir/sub/a.txt")
		if data, err := os.ReadFile(filepath.Join(root, "dir/sub/a.txt")); err != nil || string(data) != "hello\n" {
			t.Fatalf("written file = %q, %v", data, err)
		}
		if mode := fileMode(t, filepath.Join(root, "dir/sub/a.txt")); mode != 0o644 {
			t.Fatalf("new file mode = %o, want 644", mode)
		}
		if got := env.mustRun(nil, "workspace", "read", id, "dir/sub/a.txt"); got != "hello\n" {
			t.Fatalf("read = %q", got)
		}
		// An existing file keeps its mode; --mode sets it.
		if err := os.Chmod(filepath.Join(root, "dir/sub/a.txt"), 0o750); err != nil {
			t.Fatal(err)
		}
		env.mustRun(strings.NewReader("again\n"), "workspace", "write", id, "dir/sub/a.txt")
		if mode := fileMode(t, filepath.Join(root, "dir/sub/a.txt")); mode != 0o750 {
			t.Fatalf("rewritten file mode = %o, want 750 kept", mode)
		}
		env.mustRun(strings.NewReader("mode\n"), "workspace", "write", id, "dir/sub/a.txt", "--mode", "600")
		if mode := fileMode(t, filepath.Join(root, "dir/sub/a.txt")); mode != 0o600 {
			t.Fatalf("--mode 600 file mode = %o", mode)
		}
		env.mustRun(strings.NewReader("x"), "workspace", "write", id, "new.sh", "--mode", "755")
		if mode := fileMode(t, filepath.Join(root, "new.sh")); mode != 0o755 {
			t.Fatalf("new --mode 755 file mode = %o", mode)
		}
		// No temp file is left behind.
		if leftovers, _ := filepath.Glob(filepath.Join(root, "dir/sub/.gitmoot-*")); len(leftovers) != 0 {
			t.Fatalf("temp files left: %v", leftovers)
		}

		// A large write goes through the upload path, byte for byte.
		large := bytes.Repeat([]byte("0123456789abcdef\x00\xff"), 20000)
		env.mustRun(bytes.NewReader(large), "workspace", "write", id, "big.bin")
		if got := env.mustRun(nil, "workspace", "read", id, "big.bin"); got != string(large) {
			t.Fatalf("large round trip: %d bytes back, want %d", len(got), len(large))
		}
		if uploads, _ := filepath.Glob(filepath.Join(filepath.Dir(root), ".gitmoot-ws-*")); len(uploads) != 0 {
			t.Fatalf("upload files left in the guest home: %v", uploads)
		}

		var info workspaceStat
		if err := json.Unmarshal([]byte(env.mustRun(nil, "workspace", "stat", id, "big.bin", "--json")), &info); err != nil || info.Type != "file" || info.Size != int64(len(large)) || info.Mtime <= 0 {
			t.Fatalf("stat file = %+v, %v", info, err)
		}
		if err := json.Unmarshal([]byte(env.mustRun(nil, "workspace", "stat", id, "dir", "--json")), &info); err != nil || info.Type != "dir" {
			t.Fatalf("stat dir = %+v, %v", info, err)
		}
		if err := os.Symlink("README.md", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(env.mustRun(nil, "workspace", "stat", id, "link", "--json")), &info); err != nil || info.Type != "symlink" {
			t.Fatalf("stat symlink = %+v, %v", info, err)
		}

		// Exit 3 for a missing path, 4 for a directory.
		env.expectFailure(workspaceExitMissing, wsErrNotFound, nil, "workspace", "read", id, "nope.txt")
		env.expectFailure(workspaceExitMissing, wsErrNotFound, nil, "workspace", "read", id, "nodir/nope.txt")
		env.expectFailure(workspaceExitMissing, wsErrNotFound, nil, "workspace", "stat", id, "nope.txt")
		env.expectFailure(workspaceExitIsDir, wsErrIsDirectory, nil, "workspace", "read", id, "dir")
		env.expectFailure(workspaceExitIsDir, wsErrIsDirectory, strings.NewReader("x"), "workspace", "write", id, "dir")

		// Nothing outside the root: .., absolute, or through a symlink.
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "secret-link")); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"workspace", "read", id, "../x"},
			{"workspace", "read", id, filepath.Join(outside, "secret")},
			{"workspace", "read", id, "out/secret"},
			{"workspace", "read", id, "secret-link"},
			{"workspace", "stat", id, "out/secret"},
		} {
			env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, nil, args...)
		}
		for _, rel := range []string{"../x", "/tmp/x", "out/new", "secret-link"} {
			env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, strings.NewReader("pwned"), "workspace", "write", id, rel)
		}
		if data, _ := os.ReadFile(filepath.Join(outside, "secret")); string(data) != "secret" {
			t.Fatalf("the outside file changed: %q", data)
		}
		if _, err := os.Lstat(filepath.Join(outside, "new")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a write through a symlink created %s", filepath.Join(outside, "new"))
		}
	})
}

// Path arguments are taken exactly as given: leading and trailing spaces are
// part of the name, never trimmed (omp-sandbox #8 review).
func TestWorkspacePathsKeepSurroundingSpaces(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		spaced := " spaced dir "
		for _, dir := range []string{spaced, "spaced dir"} {
			if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		env.mustRun(strings.NewReader("with spaces\n"), "workspace", "write", id, spaced+"/ f.txt ")
		if data, err := os.ReadFile(filepath.Join(root, spaced, " f.txt ")); err != nil || string(data) != "with spaces\n" {
			t.Fatalf("write into %q = %q, %v", spaced, data, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "spaced dir", "f.txt")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the write landed in the trimmed directory")
		}
		if got := env.mustRun(nil, "workspace", "read", id, spaced+"/ f.txt "); got != "with spaces\n" {
			t.Fatalf("read = %q", got)
		}
		env.expectFailure(workspaceExitMissing, wsErrNotFound, nil, "workspace", "read", id, "spaced dir/f.txt")
		var info workspaceStat
		if err := json.Unmarshal([]byte(env.mustRun(nil, "workspace", "stat", id, spaced, "--json")), &info); err != nil || info.Type != "dir" {
			t.Fatalf("stat %q = %+v, %v", spaced, info, err)
		}
		got := env.mustRun(nil, "workspace", "exec", id, "--cwd", spaced, "--", "pwd")
		if strings.TrimSuffix(got, "\n") != filepath.Join(root, spaced) {
			t.Fatalf("exec --cwd %q ran in %q", spaced, got)
		}
		t.Setenv("GITMOOT_WORKSPACE", id)
		got = env.mustRun(nil, "ws", "sh", "--cwd", spaced, "pwd")
		if strings.TrimSuffix(got, "\n") != filepath.Join(root, spaced) {
			t.Fatalf("ws sh --cwd %q ran in %q", spaced, got)
		}
		// Only the empty string means the root.
		got = env.mustRun(nil, "workspace", "exec", id, "--cwd", "", "--", "pwd")
		if strings.TrimSuffix(got, "\n") != root {
			t.Fatalf("exec --cwd '' ran in %q", got)
		}
		env.expectFailure(workspaceCLIFailure, wsErrInvalidArgs, nil, "workspace", "exec", id, "--cwd", " ", "--", "pwd")
	})
}

func decodeWorkspaceChanges(t *testing.T, out string) workspaceChanges {
	t.Helper()
	var changes workspaceChanges
	if err := json.Unmarshal([]byte(out), &changes); err != nil {
		t.Fatalf("changes printed %q: %v", out, err)
	}
	return changes
}

func changedPaths(changes workspaceChanges) map[string]workspaceChangeEntry {
	out := map[string]workspaceChangeEntry{}
	for _, entry := range changes.Changed {
		out[entry.Path] = entry
	}
	return out
}

func TestWorkspaceChanges(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		changes := func(since string) workspaceChanges {
			return decodeWorkspaceChanges(t, env.mustRun(nil, "workspace", "changes", id, "--since", since, "--json"))
		}
		first := changes("start")
		if first.Reset || !strings.HasPrefix(first.Token, "ws1.") || len(first.Deleted) != 0 {
			t.Fatalf("changes --since start = %+v", first)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(first.Token, "ws1."))
		var token struct {
			W string `json:"w"`
			G int64  `json:"g"`
			T int64  `json:"t"`
		}
		if err != nil || json.Unmarshal(raw, &token) != nil || token.W != id || token.G == 0 || token.T == 0 {
			t.Fatalf("token %s = %s (%v)", first.Token, raw, err)
		}
		listed := changedPaths(first)
		if entry, ok := listed["README.md"]; !ok || entry.Type != "file" || entry.Size != int64(len("widget\n")) || entry.Mode != 0o644 || entry.Mtime <= 0 {
			t.Fatalf("README.md entry = %+v (listed %v)", entry, workspaceKeys(listed))
		}
		for path := range listed {
			if path == ".git" || strings.HasPrefix(path, ".git/") {
				t.Fatalf("changes lists %s", path)
			}
		}
		// The manifest lives in the guest home, outside the root.
		if manifests, _ := filepath.Glob(filepath.Join(filepath.Dir(root), ".gitmoot", "workspace", "changes", "manifest-*.json")); len(manifests) != 1 {
			t.Fatalf("manifests = %v", manifests)
		}

		// Past the racy window, an unchanged tree reports nothing.
		time.Sleep(1100 * time.Millisecond)
		settled := changes(first.Token)
		quiet := changes(settled.Token)
		if quiet.Reset || len(quiet.Changed) != 0 || len(quiet.Deleted) != 0 {
			t.Fatalf("changes on an unchanged tree = %+v", quiet)
		}

		// add, modify, delete, untracked; .git activity is not reported.
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("widget v2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pkg", "added.go"), []byte("package pkg\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "add", "pkg/added.go")
		next := changes(quiet.Token)
		got := changedPaths(next)
		for _, want := range []string{"README.md", "untracked.txt", "pkg", "pkg/added.go"} {
			if _, ok := got[want]; !ok {
				t.Fatalf("changes lacks %s: %+v", want, next)
			}
		}
		if got["untracked.txt"].Mode != 0o600 || got["pkg"].Type != "dir" {
			t.Fatalf("entries = %+v", next.Changed)
		}
		for path := range got {
			if path == ".git" || strings.HasPrefix(path, ".git/") {
				t.Fatalf("changes lists %s after git add", path)
			}
		}
		if err := os.Remove(filepath.Join(root, "untracked.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "pkg")); err != nil {
			t.Fatal(err)
		}
		gone := changes(next.Token)
		sort.Strings(gone.Deleted)
		if strings.Join(gone.Deleted, ",") != "pkg,pkg/added.go,untracked.txt" {
			t.Fatalf("deleted = %v", gone.Deleted)
		}
		// Deletions are exact: each is reported once.
		if again := changes(gone.Token); len(again.Deleted) != 0 {
			t.Fatalf("deleted again = %v", again.Deleted)
		}

		// An unknown, malformed, stale or foreign token is a reset with the
		// whole tree.
		for _, bad := range []string{"bogus", "ws1.!!!", first.Token, workspaceTestToken(t, "ws-other", token.G, token.T), workspaceTestToken(t, id, token.G+1, token.T)} {
			reset := changes(bad)
			if !reset.Reset || reset.Token == "" || len(reset.Deleted) != 0 {
				t.Fatalf("changes --since %s = %+v, want reset", bad, reset)
			}
			if _, ok := changedPaths(reset)["README.md"]; !ok {
				t.Fatalf("a reset lacks README.md: %+v", reset)
			}
		}
		env.expectFailure(workspaceCLIFailure, wsErrInvalidArgs, nil, "workspace", "changes", id, "--json")
	})
}

func workspaceTestToken(t *testing.T, id string, generation, mono int64) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"w": id, "g": generation, "t": mono})
	if err != nil {
		t.Fatal(err)
	}
	return "ws1." + base64.RawURLEncoding.EncodeToString(raw)
}

func workspaceKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// runWorkspaceFilesScript runs the VM-side file operation program directly on
// root, as the CLI does in the VM.
func runWorkspaceFilesScript(t *testing.T, root string, fields map[string]any, stdout io.Writer) (int, string) {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	settings := map[string]any{"root": root, "root_dev": stat.Dev, "root_ino": stat.Ino}
	for name, value := range fields {
		settings[name] = value
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", workspaceFilesScript, string(encoded))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stderr.String()
}

// workspaceRacyDriver loads the VM-side file program's functions (without
// running its main) and runs 200 iterations of: a same-size edit that
// restores the mtime, immediately followed by `changes` since the last token.
// Each edit lands well under a millisecond after the previous walk stat the
// file, so usually in the same coarse ctime tick: size, mtime, ctime and inode
// all match the manifest. It prints the misses: edits not reported. argv: the
// program, the root, the state dir, and "1" to switch the racy rule off.
const workspaceRacyDriver = `import json, os, sys
g = {"__name__": "gitmoot_racy_test"}
exec(compile(open(sys.argv[1]).read(), "workspace_files", "exec"), g)
root, state = sys.argv[2], sys.argv[3]
if sys.argv[4] == "1":
    g["RACY_NS"] = -(1 << 62)
info = os.lstat(root)
out = []
g["write_all"] = lambda fd, data: out.append(bytes(data))
hot = os.path.join(root, "hot.txt")
def edit(i):
    with open(hot, "wb") as handle:
        handle.write(bytes([97 + i % 26, 97 + (i // 26) % 26, 10]))
    os.utime(hot, (1767323045, 1767323045))
def changes(since):
    g["cfg"] = {"op": "changes", "since": since, "workspace": "ws-racy", "state_dir": state,
                "root": root, "root_dev": info.st_dev, "root_ino": info.st_ino}
    del out[:]
    g["op_changes"]()
    return json.loads(b"".join(out))
edit(0)
token = changes("start")["token"]
misses = 0
for i in range(1, 201):
    edit(i)
    result = changes(token)
    if result["reset"]:
        raise SystemExit("iteration %d: reset" % i)
    token = result["token"]
    if not any(entry["path"] == "hot.txt" for entry in result["changed"]):
        misses += 1
print(misses)
`

// The slice 2 requirement: same-size edits that restore the mtime, in a
// tight loop, are never missed. ctime advances on a coarse kernel clock, so
// most of these leave size, mtime, ctime and inode unchanged; only the racy
// clean rule reports them.
func TestWorkspaceChangesRacyCleanNeverMissesAnEdit(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	program := filepath.Join(t.TempDir(), "workspace_files.py")
	if err := os.WriteFile(program, []byte(workspaceFilesScript), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(disableRule bool) string {
		home := t.TempDir()
		root := filepath.Join(home, "work")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		flag := "0"
		if disableRule {
			flag = "1"
		}
		output, err := exec.Command("python3", "-c", workspaceRacyDriver, program, root, filepath.Join(home, "state"), flag).CombinedOutput()
		if err != nil {
			t.Fatalf("racy driver: %v\n%s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	if misses := run(false); misses != "0" {
		t.Fatalf("%s of 200 same-size, mtime-restoring edits were not reported", misses)
	}
	// Evidence the loop exercises the race: without the rule, the
	// fingerprint alone misses edits (how many depends on the clock tick).
	t.Logf("without the racy-clean rule: %s of 200 edits missed", run(true))
}

type workspaceTarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

func workspaceTar(t *testing.T, entries ...workspaceTarEntry) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode, Linkname: entry.linkname, ModTime: time.Unix(1700000000, 0)}
		if entry.typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.typeflag == tar.TypeReg {
			if _, err := io.WriteString(writer, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &out
}

func readWorkspaceTar(t *testing.T, data string) map[string]workspaceTarEntry {
	t.Helper()
	out := map[string]workspaceTarEntry{}
	reader := tar.NewReader(strings.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("pull tar: %v", err)
		}
		body, _ := io.ReadAll(reader)
		name := strings.TrimSuffix(header.Name, "/")
		out[name] = workspaceTarEntry{name: name, typeflag: header.Typeflag, mode: header.Mode, body: string(body), linkname: header.Linkname}
	}
}

func TestWorkspacePullPushFilesRoundTrip(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		applied := env.mustRun(workspaceTar(t,
			workspaceTarEntry{name: "bin", typeflag: tar.TypeDir, mode: 0o750},
			workspaceTarEntry{name: "bin/run.sh", typeflag: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho run\n"},
			workspaceTarEntry{name: "./docs/notes.md", typeflag: tar.TypeReg, mode: 0o600, body: "notes\n"},
			workspaceTarEntry{name: "README.md", typeflag: tar.TypeReg, mode: 0o644, body: "replaced\n"},
			workspaceTarEntry{name: "docs/link", typeflag: tar.TypeSymlink, linkname: "../README.md"},
			workspaceTarEntry{name: "abs-link", typeflag: tar.TypeSymlink, linkname: root + "/bin/run.sh"},
		), "workspace", "push", id, "--files")
		if strings.TrimSpace(applied) != "applied 6 entries" {
			t.Fatalf("push --files = %q", applied)
		}
		if mode := fileMode(t, filepath.Join(root, "bin")); mode != 0o750 {
			t.Fatalf("bin mode = %o", mode)
		}
		if mode := fileMode(t, filepath.Join(root, "bin/run.sh")); mode != 0o755 {
			t.Fatalf("run.sh mode = %o", mode)
		}
		if mode := fileMode(t, filepath.Join(root, "docs/notes.md")); mode != 0o600 {
			t.Fatalf("notes.md mode = %o", mode)
		}
		if data, _ := os.ReadFile(filepath.Join(root, "README.md")); string(data) != "replaced\n" {
			t.Fatalf("README.md = %q", data)
		}
		if target, err := os.Readlink(filepath.Join(root, "docs/link")); err != nil || target != "../README.md" {
			t.Fatalf("docs/link = %q, %v", target, err)
		}

		pulled := readWorkspaceTar(t, env.mustRun(nil, "workspace", "pull", id, "--", "bin/run.sh", "docs/notes.md", "docs/link", "bin", "missing.txt"))
		if entry := pulled["bin/run.sh"]; entry.typeflag != tar.TypeReg || entry.mode != 0o755 || entry.body != "#!/bin/sh\necho run\n" {
			t.Fatalf("pulled run.sh = %+v", entry)
		}
		if entry := pulled["docs/notes.md"]; entry.mode != 0o600 || entry.body != "notes\n" {
			t.Fatalf("pulled notes.md = %+v", entry)
		}
		if entry := pulled["docs/link"]; entry.typeflag != tar.TypeSymlink || entry.linkname != "../README.md" {
			t.Fatalf("pulled link = %+v", entry)
		}
		if entry := pulled["bin"]; entry.typeflag != tar.TypeDir || entry.mode != 0o750 {
			t.Fatalf("pulled dir = %+v", entry)
		}
		if _, ok := pulled["missing.txt"]; ok || len(pulled) != 4 {
			t.Fatalf("pulled = %v", workspaceKeys(pulled))
		}

		// Round trip: what pull gives, push --files puts back.
		if err := os.WriteFile(filepath.Join(root, "bin/run.sh"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		var again bytes.Buffer
		writer := tar.NewWriter(&again)
		header := &tar.Header{Name: "bin/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(pulled["bin/run.sh"].body))}
		_ = writer.WriteHeader(header)
		_, _ = io.WriteString(writer, pulled["bin/run.sh"].body)
		_ = writer.Close()
		env.mustRun(&again, "workspace", "push", id, "--files")
		if data, _ := os.ReadFile(filepath.Join(root, "bin/run.sh")); string(data) != "#!/bin/sh\necho run\n" || fileMode(t, filepath.Join(root, "bin/run.sh")) != 0o755 {
			t.Fatalf("round trip run.sh = %q %o", data, fileMode(t, filepath.Join(root, "bin/run.sh")))
		}

		// Refused: .., absolute, a symlink out of the root. A refused stream
		// applies nothing, not even its earlier entries.
		outside := t.TempDir()
		for _, bad := range []workspaceTarEntry{
			{name: "../escape.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
			{name: "a/../../escape.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
			{name: filepath.Join(outside, "escape.txt"), typeflag: tar.TypeReg, mode: 0o644, body: "x"},
			{name: "up", typeflag: tar.TypeSymlink, linkname: "../outside"},
			{name: "deep/up", typeflag: tar.TypeSymlink, linkname: "../../x"},
			{name: "abs", typeflag: tar.TypeSymlink, linkname: outside},
		} {
			stream := workspaceTar(t, workspaceTarEntry{name: "first.txt", typeflag: tar.TypeReg, mode: 0o644, body: "first"}, bad)
			env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, stream, "workspace", "push", id, "--files")
			if _, err := os.Lstat(filepath.Join(root, "first.txt")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a refused stream (%s) applied first.txt", bad.name)
			}
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("push wrote outside the root: %v", entries)
		}
		// A file entry below an existing symlink is refused in the VM.
		if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
			t.Fatal(err)
		}
		env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, workspaceTar(t, workspaceTarEntry{name: "out/x.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"}), "workspace", "push", id, "--files")
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("push wrote through a symlink: %v", entries)
		}
		env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, nil, "workspace", "pull", id, "--", "../x")
	})
}

// The VM side refuses the same entries on its own, whatever the host sent.
func TestWorkspaceFilesScriptPushRefusesEscapes(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	root := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []workspaceTarEntry{
		{name: "../escape.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "/tmp/escape.txt", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
		{name: "up", typeflag: tar.TypeSymlink, linkname: "../x"},
		{name: "abs", typeflag: tar.TypeSymlink, linkname: "/etc"},
	} {
		stream := workspaceTar(t, bad)
		code, stderr := runWorkspaceFilesScript(t, root, map[string]any{"op": "push", "input_b64": base64.StdEncoding.EncodeToString(stream.Bytes())}, io.Discard)
		if code != 2 || !strings.Contains(stderr, `"cwd_outside_root"`) {
			t.Fatalf("VM push of %s = %d %s", bad.name, code, stderr)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("refused pushes left %v", entries)
	}
}

// workspaceTreeSnapshot describes every entry under dir except the top-level .git:
// type, permission bits, and content or link target.
func workspaceTreeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, file)
		if rel == "." {
			return nil
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		info, err := os.Lstat(file)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(file)
			out[rel] = "symlink " + target
		case info.IsDir():
			out[rel] = "dir " + info.Mode().Perm().String()
		default:
			data, _ := os.ReadFile(file)
			out[rel] = "file " + info.Mode().Perm().String() + " " + string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkspaceMirrorEqualsWorkTree(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		// A base with more to it: committed on origin before the workspace
		// opened is simplest via the workspace itself, so shape the VM tree.
		writes := map[string]string{"README.md": "edited same\n", "src/main.go": "package main\n", "untracked/deep/file.txt": "deep\n", " spaced .txt": "space\n"}
		for rel, body := range writes {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(filepath.Join(root, "src/main.go"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("src/main.go", filepath.Join(root, "main-link")); err != nil {
			t.Fatal(err)
		}
		mirror := filepath.Join(t.TempDir(), "mirror")
		token := strings.TrimSpace(env.mustRun(nil, "workspace", "mirror", id, mirror))
		if !strings.HasPrefix(token, "ws1.") {
			t.Fatalf("mirror printed %q", token)
		}
		want, got := workspaceTreeSnapshot(t, root), workspaceTreeSnapshot(t, mirror)
		if len(want) != len(got) {
			t.Fatalf("mirror entries %v, work tree %v", workspaceKeys(got), workspaceKeys(want))
		}
		for rel, entry := range want {
			if got[rel] != entry {
				t.Fatalf("mirror %s = %q, work tree %q", rel, got[rel], entry)
			}
		}
		// The token continues: a later change is reported against it.
		if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
			t.Fatal(err)
		}
		next := decodeWorkspaceChanges(t, env.mustRun(nil, "workspace", "changes", id, "--since", token, "--json"))
		if next.Reset || strings.Join(next.Deleted, ",") != "README.md" {
			t.Fatalf("changes after mirror = %+v", next)
		}
		// A non-empty target is refused.
		env.expectFailure(workspaceCLIFailure, wsErrInvalidArgs, nil, "workspace", "mirror", id, mirror)
	})
}

// TestWorkspaceMirrorDeletedTrackedFile: a file of the base that the VM
// deleted is not in the mirror.
func TestWorkspaceMirrorDropsDeletedTrackedFile(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	opened := env.open("--issue", "12")
	if err := os.Remove(filepath.Join(opened.Root, "README.md")); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(t.TempDir(), "mirror")
	env.mustRun(nil, "workspace", "mirror", opened.ID, mirror)
	if _, err := os.Lstat(filepath.Join(mirror, "README.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the mirror kept README.md, deleted in the workspace")
	}
}

func workspaceCommit(t *testing.T, env *workspaceTestEnv, id, file, body string) {
	t.Helper()
	env.mustRun(strings.NewReader(body), "workspace", "write", id, file)
	env.mustRun(nil, "workspace", "exec", id, "--", "git", "add", file)
	env.mustRun(nil, "workspace", "exec", id, "--", "git", "commit", "--quiet", "-m", "change "+file)
}

func TestWorkspacePushCommitsOut(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id := opened.ID
		// Nothing to push yet.
		env.expectFailure(workspaceCLIFailure, "nothing_to_push", nil, "workspace", "push", id)

		workspaceCommit(t, env, id, "feature.txt", "one\n")
		pushed := strings.TrimSpace(env.mustRun(nil, "workspace", "push", id))
		vmHead := strings.TrimSpace(env.mustRun(nil, "workspace", "exec", id, "--", "git", "rev-parse", "HEAD"))
		if pushed != vmHead || !workspaceCommitSHA.MatchString(pushed) {
			t.Fatalf("push printed %q, VM HEAD %q", pushed, vmHead)
		}
		if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/ws/intern/12"); got != pushed {
			t.Fatalf("origin ws/intern/12 = %s, want %s", got, pushed)
		}
		if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/main"); got != env.baseSHA {
			t.Fatalf("origin main moved to %s", got)
		}
		// A second commit fast-forwards.
		workspaceCommit(t, env, id, "feature.txt", "two\n")
		second := strings.TrimSpace(env.mustRun(nil, "workspace", "push", id))
		if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/ws/intern/12"); got != second || second == pushed {
			t.Fatalf("origin ws/intern/12 = %s after the second push %s", got, second)
		}

		// Rewritten history that still descends from the base needs a force
		// push, which is never done.
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "reset", "--quiet", "--hard", env.baseSHA)
		workspaceCommit(t, env, id, "other.txt", "rewritten\n")
		env.expectFailure(workspaceCLIFailure, wsErrPushRefused, nil, "workspace", "push", id)
		if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/ws/intern/12"); got != second {
			t.Fatalf("a refused push moved origin to %s", got)
		}

		// A branch that does not descend from the base is refused.
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "checkout", "--quiet", "--orphan", "unrelated")
		workspaceCommit(t, env, id, "orphan.txt", "orphan\n")
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "branch", "--quiet", "-f", "ws/intern/12", "unrelated")
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "checkout", "--quiet", "ws/intern/12")
		result := env.run("workspace", "push", id)
		if result.code != workspaceCLIFailure || workspaceErrorCode(t, result.stderr) != wsErrPushRefused || !strings.Contains(result.stderr, "does not descend") {
			t.Fatalf("non-descendant push = %+v", result)
		}

		// Any other branch is refused.
		env.mustRun(nil, "workspace", "exec", id, "--", "git", "checkout", "--quiet", "-b", "elsewhere", second)
		workspaceCommit(t, env, id, "elsewhere.txt", "x\n")
		result = env.run("workspace", "push", id)
		if result.code != workspaceCLIFailure || workspaceErrorCode(t, result.stderr) != wsErrPushRefused || !strings.Contains(result.stderr, "not the workspace branch") {
			t.Fatalf("other-branch push = %+v", result)
		}
		if out := runWorkspaceTestGit(t, env.origin, "for-each-ref", "--format=%(refname)", "refs/heads/"); strings.Join(strings.Fields(out), " ") != "refs/heads/main refs/heads/ws/intern/12" {
			t.Fatalf("origin branches = %q", out)
		}
		if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/ws/intern/12"); got != second {
			t.Fatalf("origin ws/intern/12 = %s, want %s", got, second)
		}
	})
}

func TestWorkspacePushPullRequestGoesToHeadBranch(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{local: true})
	head := env.addPullRequest(7, "feature-x")
	opened := env.open("--pr", "7")
	workspaceCommit(t, env, opened.ID, "pr.txt", "pr change\n")
	pushed := strings.TrimSpace(env.mustRun(nil, "workspace", "push", opened.ID))
	if got := runWorkspaceTestGit(t, env.origin, "rev-parse", "refs/heads/feature-x"); got != pushed {
		t.Fatalf("origin feature-x = %s, want %s", got, pushed)
	}
	if parent := runWorkspaceTestGit(t, env.origin, "rev-parse", pushed+"^"); parent != head {
		t.Fatalf("pushed commit's parent = %s, want the PR head %s", parent, head)
	}
}

func TestWorkspaceTemplateFallsBackToRemoteExecTemplate(t *testing.T) {
	env := newWorkspaceTestEnv(t, workspaceTestConfig{})
	first := env.open("--issue", "1")
	if got := env.cloud.sandbox(env.record(first.ID).ProviderInstanceID).template; got != "tmpl" {
		t.Fatalf("without [workspaces] template the sandbox template = %q, want [remote_exec] e2b_template", got)
	}
	appendWorkspaceConfig(t, env.paths, "template = \"gitmoot-omp-go126-2c4g-v1\"\n")
	second := env.open("--issue", "2")
	if got := env.cloud.sandbox(env.record(second.ID).ProviderInstanceID).template; got != "gitmoot-omp-go126-2c4g-v1" {
		t.Fatalf("with [workspaces] template the sandbox template = %q", got)
	}
}

func TestWorkspaceRemove(t *testing.T) {
	forEachWorkspaceProvider(t, func(t *testing.T, env *workspaceTestEnv, opened workspaceJSON) {
		id, root := opened.ID, opened.Root
		outside := filepath.Join(t.TempDir(), "target.txt")
		if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{"empty", "full", " spaced dir "} {
			if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, file := range []string{"a.txt", "full/inside.txt", " spaced dir / f.txt ", "spaced dir.txt"} {
			if err := os.WriteFile(filepath.Join(root, file), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(outside, filepath.Join(root, "out-link")); err != nil {
			t.Fatal(err)
		}
		token := decodeWorkspaceChanges(t, env.mustRun(nil, "workspace", "changes", id, "--since", "start", "--json")).Token

		out := env.mustRun(nil, "workspace", "remove", id, "--", "a.txt", "out-link", "empty", " spaced dir / f.txt ", "missing.txt", "nodir/missing.txt")
		if strings.TrimSpace(out) != `{"removed":4}` {
			t.Fatalf("remove printed %q", out)
		}
		for _, gone := range []string{"a.txt", "out-link", "empty", " spaced dir / f.txt "} {
			if _, err := os.Lstat(filepath.Join(root, gone)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%q survived remove", gone)
			}
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
			t.Fatalf("the symlink's target = %q, %v; it must survive", data, err)
		}
		if _, err := os.Lstat(filepath.Join(root, "spaced dir.txt")); err != nil {
			t.Fatalf("the untrimmed name removed the trimmed one: %v", err)
		}
		next := decodeWorkspaceChanges(t, env.mustRun(nil, "workspace", "changes", id, "--since", token, "--json"))
		sort.Strings(next.Deleted)
		if next.Reset || strings.Join(next.Deleted, "|") != " spaced dir / f.txt |a.txt|empty|out-link" {
			t.Fatalf("changes after remove = %+v", next)
		}

		env.expectFailure(workspaceCLIFailure, wsErrNotEmpty, nil, "workspace", "remove", id, "--", "full")
		// All or nothing: a batch with a non-empty directory removes nothing,
		// not even the file listed before it.
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		result := env.run("workspace", "remove", id, "--", "b.txt", "full")
		if result.code != workspaceCLIFailure || workspaceErrorCode(t, result.stderr) != wsErrNotEmpty || !strings.Contains(result.stderr, "path full") {
			t.Fatalf("remove [file, non-empty dir] = %+v", result)
		}
		if _, err := os.Lstat(filepath.Join(root, "b.txt")); err != nil {
			t.Fatalf("a refused batch removed b.txt: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "full", "inside.txt")); err != nil {
			t.Fatalf("a refused remove touched full/: %v", err)
		}
		// A directory whose entries are listed before it in the batch goes too.
		if out := env.mustRun(nil, "workspace", "remove", id, "--", "full/inside.txt", "full", "b.txt"); strings.TrimSpace(out) != `{"removed":3}` {
			t.Fatalf("remove [entries, dir] printed %q", out)
		}
		if _, err := os.Lstat(filepath.Join(root, "full")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("full survived: %v", err)
		}
		if out := env.mustRun(nil, "workspace", "remove", id, "--", "missing.txt"); strings.TrimSpace(out) != `{"removed":0}` {
			t.Fatalf("remove of a missing path printed %q", out)
		}
		for _, bad := range []string{"../x", filepath.Join(root, "full"), "full/../../x"} {
			env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, nil, "workspace", "remove", id, "--", bad)
		}
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "out-dir")); err != nil {
			t.Fatal(err)
		}
		env.expectFailure(workspaceCLIFailure, wsErrCwdOutsideRoot, nil, "workspace", "remove", id, "--", "out-dir/target.txt")
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("remove through a symlink reached %s: %v", outside, err)
		}
		for _, bad := range []string{".git", ".git/HEAD", "", "."} {
			env.expectFailure(workspaceCLIFailure, wsErrInvalidArgs, nil, "workspace", "remove", id, "--", bad)
		}
		if _, err := os.Stat(filepath.Join(root, ".git", "HEAD")); err != nil {
			t.Fatalf(".git/HEAD: %v", err)
		}
	})
}
