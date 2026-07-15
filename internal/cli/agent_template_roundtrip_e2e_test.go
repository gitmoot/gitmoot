package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// fakeGitHubBackend is a SINGLE faithful in-memory "GitHub" sitting at the gh
// wire boundary (a subprocess.Runner). Template and pipeline round-trip E2Es
// share it across the write path (github.GhClient Upsert/Delete/Create) and the
// read path (agenttemplate.GHFetcher ResolveRef/FetchFile/ListDir), so bytes a
// publisher PUTs are EXACTLY the bytes pull/add GET back. Everything above the
// wire (Export -> FormatTemplateContent -> publish ->
// UpsertFile; pull -> ListDir -> AddRemote -> ParseTemplateContent -> store) is
// the real CLI/agenttemplate code. If publish and pull disagreed on the path or
// the encoding, the GET would 404 (PullFailed) or decode to different bytes, and
// the byte-exact assertions below would go red — that is the whole point.
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

// useFakeGitHubBackend points ALL three template gh seams (publish client, bulk
// pull source, and the single-file diff/update fetcher) at one shared backend,
// restoring them on cleanup.
func useFakeGitHubBackend(t *testing.T, backend *fakeGitHubBackend) {
	t.Helper()
	restorePublish := replaceTemplateRemoteClient(&github.GhClient{Runner: backend, MaxRetries: 1})
	restoreSource := replaceAgentTemplateRemoteSource(agenttemplate.GHFetcher{Runner: backend})
	restoreFetcher := replaceAgentTemplateFetcher(agenttemplate.GHFetcher{Runner: backend})
	t.Cleanup(func() {
		restoreFetcher()
		restoreSource()
		restorePublish()
	})
}

// roundTripTemplateContent builds a deliberately rich .md (multiple
// capabilities/runtimes/tags and an evaluation map) so the frontmatter round-trip
// through FormatTemplateContent -> ParseTemplateContent is meaningfully exercised.
func roundTripTemplateContent(id, body string) string {
	return agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 testTemplateName(id),
		Description:          "Round-trip fidelity probe for " + id + ".",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask", "review"},
		RuntimeCompatibility: []string{"codex", "claude", "kimi"},
		Tags:                 []string{"review", "round-trip", "custom"},
		Inputs:               []string{"repo", "task", "visible_context"},
		Outputs:              []string{"response", "review_findings"},
		Evaluation:           map[string]string{"driver": "code-review", "preferred_gate": "pairwise"},
	}, body)
}

func installLocalTemplate(t *testing.T, home, id, content string) {
	t.Helper()
	promptPath := filepath.Join(t.TempDir(), id+".md")
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, id}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add %s exit code = %d, stderr=%s", id, code, stderr.String())
	}
}

func storedTemplate(t *testing.T, home, id string) db.AgentTemplate {
	t.Helper()
	var row db.AgentTemplate
	if err := withStore(home, func(store *db.Store) error {
		got, err := store.GetAgentTemplate(context.Background(), id)
		if err != nil {
			return err
		}
		row = got
		return nil
	}); err != nil {
		t.Fatalf("read stored template %s from %s: %v", id, home, err)
	}
	return row
}

func mustParse(t *testing.T, content string) agenttemplate.ParsedTemplate {
	t.Helper()
	parsed, err := agenttemplate.ParseTemplateContent(content)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	return parsed
}

// TestAgentTemplatePublishPullRoundTripThroughSharedBackend is the full-chain
// E2E: home A installs two custom templates and `publish`es them (with --create)
// into one in-memory GitHub; a FRESH home B `pull`s them back through the SAME
// backend. It asserts the restored rows are byte-exact with what publish wrote,
// that their metadata/frontmatter parse identically, that SourceRepo/Ref/Path
// point at the fake repo, and then that `update`/`diff` re-fetch a re-published
// v2 (a new version; the diff renders) — proving export -> publish -> pull ->
// add/update is a genuine inverse pair, not two stubs that merely agree by luck.
func TestAgentTemplatePublishPullRoundTripThroughSharedBackend(t *testing.T) {
	backend := newFakeGitHubBackend()
	useFakeGitHubBackend(t, backend)

	homeA := t.TempDir()
	homeB := t.TempDir()
	const repo = "jerry/templates"

	installLocalTemplate(t, homeA, "frontend-reviewer", roundTripTemplateContent("frontend-reviewer", "Review UI changes carefully.\n"))
	installLocalTemplate(t, homeA, "api-reviewer", roundTripTemplateContent("api-reviewer", "Review API changes carefully.\n"))

	// The exact bytes publish reconstructs for each row (DB -> canonical .md).
	wantExport := map[string]string{}
	for _, id := range []string{"frontend-reviewer", "api-reviewer"} {
		export, err := agenttemplate.Export(storedTemplate(t, homeA, id))
		if err != nil {
			t.Fatalf("Export(%s) returned error: %v", id, err)
		}
		wantExport[id] = export
	}

	// --- publish from home A (creates the repo, pushes both .md files) ---
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "publish", "--home", homeA, "--all", "--repo", repo, "--create"}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created "+repo+" (private)") {
		t.Fatalf("expected --create to create the repo:\n%s", stdout.String())
	}
	for _, id := range []string{"frontend-reviewer", "api-reviewer"} {
		if !strings.Contains(stdout.String(), "published "+id+" -> templates/"+id+".md") {
			t.Fatalf("expected publish line for %s:\n%s", id, stdout.String())
		}
	}

	// The backend now physically holds exactly what publish wrote, keyed by the
	// path pull will read from. This is the one shared surface.
	for _, id := range []string{"frontend-reviewer", "api-reviewer"} {
		stored, ok := backend.files[repo]["templates/"+id+".md"]
		if !ok {
			t.Fatalf("backend missing published file templates/%s.md", id)
		}
		if string(stored) != wantExport[id] {
			t.Fatalf("backend bytes for %s != Export bytes\nwant:\n%q\ngot:\n%q", id, wantExport[id], string(stored))
		}
	}

	// --- pull into a FRESH home B through the SAME backend ---
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "pull", "--home", homeB, "--all", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("pull exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, id := range []string{"frontend-reviewer", "api-reviewer"} {
		if !strings.Contains(stdout.String(), "installed "+id+" at ") {
			t.Fatalf("expected pull to install %s:\n%s", id, stdout.String())
		}
	}

	// --- the heart of the round-trip: home B == home A, byte- and semantic-exact ---
	for _, id := range []string{"frontend-reviewer", "api-reviewer"} {
		original := storedTemplate(t, homeA, id)
		restored := storedTemplate(t, homeB, id)

		if restored.Content != wantExport[id] {
			t.Fatalf("restored %s content is not byte-exact with the published .md\nwant:\n%q\ngot:\n%q", id, wantExport[id], restored.Content)
		}
		// Re-deriving Export from the restored row must reproduce the same bytes:
		// proves export/parse are inverse across the whole hop.
		reExport, err := agenttemplate.Export(restored)
		if err != nil {
			t.Fatalf("Export(restored %s) returned error: %v", id, err)
		}
		if reExport != wantExport[id] {
			t.Fatalf("restored %s does not re-export to the published bytes", id)
		}
		if restored.MetadataJSON != original.MetadataJSON {
			t.Fatalf("restored %s metadata JSON diverged\nwant: %s\ngot:  %s", id, original.MetadataJSON, restored.MetadataJSON)
		}
		if got, want := mustParse(t, restored.Content).Metadata, mustParse(t, original.Content).Metadata; !reflect.DeepEqual(got, want) {
			t.Fatalf("restored %s frontmatter metadata diverged\nwant: %+v\ngot:  %+v", id, want, got)
		}
		// The restored row points back at the fake repo, so it is re-fetchable.
		if restored.SourceRepo != repo || restored.SourceRef != "main" || restored.SourcePath != "templates/"+id+".md" {
			t.Fatalf("restored %s source pointers wrong: repo=%q ref=%q path=%q", id, restored.SourceRepo, restored.SourceRef, restored.SourcePath)
		}
		if !agenttemplate.IsRemote(restored) {
			t.Fatalf("restored %s should be a remote-sourced (re-fetchable) row", id)
		}
		if restored.VersionNumber != 1 {
			t.Fatalf("restored %s should be v1, got v%d", id, restored.VersionNumber)
		}
	}

	// A second identical pull is a no-op (identical-content), proving the bytes
	// truly round-trip (any drift would re-trigger a version bump).
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "pull", "--home", homeB, "--all", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("second pull exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unchanged frontend-reviewer") || !strings.Contains(stdout.String(), "unchanged api-reviewer") {
		t.Fatalf("identical re-pull should be a no-op:\n%s", stdout.String())
	}

	// --- re-publish a CHANGED frontend-reviewer (v2) from home A ---
	installLocalTemplate(t, homeA, "frontend-reviewer", roundTripTemplateContent("frontend-reviewer", "Review UI changes EXHAUSTIVELY.\n"))
	wantV2, err := agenttemplate.Export(storedTemplate(t, homeA, "frontend-reviewer"))
	if err != nil {
		t.Fatalf("Export v2 returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "publish", "frontend-reviewer", "--home", homeA, "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish v2 exit code = %d, stderr=%s", code, stderr.String())
	}

	// --- diff on home B's pulled row now sees the upstream change ---
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "diff", "--home", homeB, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("diff exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"-Review UI changes carefully.", "+Review UI changes EXHAUSTIVELY."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("diff against published source missing %q:\n%s", want, stdout.String())
		}
	}

	// --- update on home B re-fetches v2 as a new version ---
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "update", "--home", homeB, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("update exit code = %d, stderr=%s", code, stderr.String())
	}
	updated := storedTemplate(t, homeB, "frontend-reviewer")
	if updated.VersionNumber != 2 {
		t.Fatalf("expected frontend-reviewer at v2 after update, got v%d", updated.VersionNumber)
	}
	if updated.Content != wantV2 {
		t.Fatalf("updated frontend-reviewer is not byte-exact with the re-published v2\nwant:\n%q\ngot:\n%q", wantV2, updated.Content)
	}
}

// TestAgentTemplateAddFromRepoRoundTripThroughSharedBackend covers the
// single-file leg: `add --from-repo` (FetchFile, no directory listing) reads back
// EXACTLY what `publish` (UpsertFile) wrote through the same backend.
func TestAgentTemplateAddFromRepoRoundTripThroughSharedBackend(t *testing.T) {
	backend := newFakeGitHubBackend()
	useFakeGitHubBackend(t, backend)

	homeA := t.TempDir()
	homeB := t.TempDir()
	const repo = "jerry/templates"
	const id = "security-reviewer"

	installLocalTemplate(t, homeA, id, roundTripTemplateContent(id, "Audit changes for security regressions.\n"))
	wantExport, err := agenttemplate.Export(storedTemplate(t, homeA, id))
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"agent", "template", "publish", id, "--home", homeA, "--repo", repo, "--create"}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "add", id, "--home", homeB, "--from-repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("add --from-repo exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added "+id+" at ") {
		t.Fatalf("expected add success line:\n%s", stdout.String())
	}

	restored := storedTemplate(t, homeB, id)
	if restored.Content != wantExport {
		t.Fatalf("add --from-repo restored %s is not byte-exact with the published .md\nwant:\n%q\ngot:\n%q", id, wantExport, restored.Content)
	}
	if restored.SourceRepo != repo || restored.SourceRef != "main" || restored.SourcePath != "templates/"+id+".md" {
		t.Fatalf("restored %s source pointers wrong: repo=%q ref=%q path=%q", id, restored.SourceRepo, restored.SourceRef, restored.SourcePath)
	}
	original := storedTemplate(t, homeA, id)
	if got, want := mustParse(t, restored.Content).Metadata, mustParse(t, original.Content).Metadata; !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %s frontmatter metadata diverged\nwant: %+v\ngot:  %+v", id, want, got)
	}
}
