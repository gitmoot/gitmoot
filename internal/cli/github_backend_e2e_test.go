//go:build e2e

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

// fakeGitHubBackend is a SINGLE faithful in-memory "GitHub" sitting at the gh
// wire boundary (a subprocess.Runner). The pipeline remote round-trip E2E shares
// it across the write path (github.GhClient Upsert/Delete/Create) and the read
// path (agenttemplate.GHFetcher ResolveRef/FetchFile/ListDir), so bytes a
// publisher PUTs are EXACTLY the bytes a pull GETs back. Everything above the
// wire is the real CLI code. If publish and pull disagreed on the path or the
// encoding, the GET would 404 or decode to different bytes, and the byte-exact
// assertions would go red — that is the whole point.
//
// It moved here in #2204 from the deleted agent-template publish/pull
// round-trip E2E; the template half of the harness went with the authoring and
// distribution surface, the pipeline half did not.
type fakeGitHubBackend struct {
	mu    sync.Mutex
	repos map[string]bool              // "owner/repo" -> exists
	files map[string]map[string][]byte // "owner/repo" -> path -> raw stored bytes
	calls []string
}

func newFakeGitHubBackend() *fakeGitHubBackend {
	return &fakeGitHubBackend{
		repos: map[string]bool{},
		files: map[string]map[string][]byte{},
	}
}

func (b *fakeGitHubBackend) LookPath(string) (string, error) { return "gh", nil }

func (b *fakeGitHubBackend) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, command+" "+strings.Join(args, " "))
	if command != "gh" {
		return subprocess.Result{Stderr: "unexpected command " + command}, fmt.Errorf("unexpected command %q", command)
	}
	switch {
	case len(args) >= 3 && args[0] == "repo" && args[1] == "view":
		repo := args[2]
		if b.repos[repo] {
			return subprocess.Result{Stdout: `{"nameWithOwner":"` + repo + `"}`}, nil
		}
		return subprocess.Result{Stderr: "Could not resolve to a Repository with the name '" + repo + "'"}, errors.New("not found")
	case len(args) >= 3 && args[0] == "repo" && args[1] == "create":
		b.repos[args[2]] = true
		return subprocess.Result{Stdout: ""}, nil
	case len(args) >= 1 && args[0] == "api":
		return b.handleAPI(args)
	default:
		joined := strings.Join(args, " ")
		return subprocess.Result{Stderr: "unexpected gh call"}, errors.New("unexpected gh call: " + joined)
	}
}

func (b *fakeGitHubBackend) handleAPI(args []string) (subprocess.Result, error) {
	// ResolveRef: `api repos/<repo>/git/ref/heads/<ref> --jq .object.sha`. The
	// --jq makes real gh print just the sha, so the fake returns the bare sha as
	// stdout (GHFetcher.ResolveRef reads result.Stdout trimmed).
	if endpoint, ok := findArgContaining(args, "/git/ref/heads/"); ok {
		repo := strings.SplitN(strings.TrimPrefix(endpoint, "repos/"), "/git/ref/heads/", 2)[0]
		return subprocess.Result{Stdout: b.repoSHA(repo) + "\n"}, nil
	}
	endpoint, ok := findArgContaining(args, "/contents/")
	if !ok {
		return subprocess.Result{Stderr: "unexpected api call"}, errors.New("unexpected api call: " + strings.Join(args, " "))
	}
	rest := strings.TrimPrefix(endpoint, "repos/")
	parts := strings.SplitN(rest, "/contents/", 2)
	repo := parts[0]
	path := strings.Trim(parts[1], "/")
	switch apiMethod(args) {
	case "PUT":
		// publish: UpsertFile PUT stores the rebuilt .md bytes keyed by <path>.
		content, err := decodeContentArg(args)
		if err != nil {
			return subprocess.Result{Stderr: err.Error()}, err
		}
		if b.files[repo] == nil {
			b.files[repo] = map[string][]byte{}
		}
		b.files[repo][path] = content
		b.repos[repo] = true
		body := fmt.Sprintf(`{"content":{"path":%q,"html_url":%q,"sha":%q}}`,
			path, "https://github.com/"+repo+"/blob/main/"+path, blobSHA(content))
		return subprocess.Result{Stdout: body}, nil
	case "DELETE":
		if _, ok := b.files[repo][path]; !ok {
			return subprocess.Result{Stderr: "gh: Not Found (HTTP 404)"}, errors.New("not found")
		}
		delete(b.files[repo], path)
		return subprocess.Result{Stdout: `{}`}, nil
	default: // GET — serves UpsertFile's sha probe, FetchFile, and ListDir.
		if data, ok := b.files[repo][path]; ok {
			// A single-file contents response: `.sha` answers UpsertFile's probe,
			// `.encoding`/`.content` answer FetchFile. ONE shape serves both.
			body := fmt.Sprintf(`{"path":%q,"sha":%q,"encoding":"base64","content":%q}`,
				path, blobSHA(data), base64.StdEncoding.EncodeToString(data))
			return subprocess.Result{Stdout: body}, nil
		}
		if listing, ok := b.listDir(repo, path); ok {
			return subprocess.Result{Stdout: listing}, nil
		}
		// No such file and not a directory -> 404, so UpsertFile creates rather
		// than updates (sha stays empty) and FetchFile reports a clean miss.
		return subprocess.Result{Stderr: "gh: Not Found (HTTP 404)"}, errors.New("not found")
	}
}

// listDir returns a GitHub contents directory listing (a JSON array) for the
// immediate children under path, or ok=false when path holds no files. It is the
// faithful counterpart to ListDir's expectation.
func (b *fakeGitHubBackend) listDir(repo, path string) (string, bool) {
	prefix := strings.Trim(path, "/") + "/"
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	seenDir := map[string]bool{}
	entries := make([]entry, 0)
	for stored := range b.files[repo] {
		if !strings.HasPrefix(stored, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(stored, prefix)
		if idx := strings.Index(remainder, "/"); idx >= 0 {
			dir := remainder[:idx]
			if !seenDir[dir] {
				seenDir[dir] = true
				entries = append(entries, entry{Name: dir, Path: prefix + dir, Type: "dir"})
			}
			continue
		}
		entries = append(entries, entry{Name: remainder, Path: stored, Type: "file"})
	}
	if len(entries) == 0 {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// repoSHA is a deterministic, content-derived "commit sha" for the repo: it
// changes whenever any file changes, so a re-published template yields a new
// upstream sha (which `diff` renders and `update` records).
func (b *fakeGitHubBackend) repoSHA(repo string) string {
	files := b.files[repo]
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hasher := sha256.New()
	for _, path := range paths {
		hasher.Write([]byte(path))
		hasher.Write([]byte{0})
		hasher.Write(files[path])
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func blobSHA(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func findArgContaining(args []string, substr string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "repos/") && strings.Contains(a, substr) {
			return a, true
		}
	}
	return "", false
}

func apiMethod(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-X" {
			return strings.ToUpper(args[i+1])
		}
	}
	return "GET"
}

func decodeContentArg(args []string) ([]byte, error) {
	for _, a := range args {
		if strings.HasPrefix(a, "content=") {
			return base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "content="))
		}
	}
	return nil, errors.New("PUT contents call missing content= argument")
}

// contentsPath extracts the repo-relative file path from a `gh api
// repos/<owner>/<repo>/contents/<path>` argv. It moved here in #2204 with the
// rest of the shared gh harness; the pipeline remote round-trip uses it to
// assert exactly which paths were written.
func contentsPath(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "repos/") {
			if idx := strings.Index(a, "/contents/"); idx >= 0 {
				return a[idx+len("/contents/"):]
			}
		}
	}
	return ""
}
