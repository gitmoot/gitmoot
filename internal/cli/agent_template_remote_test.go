package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// stubGitHubRunner answers the gh calls UpsertFile / RepositoryExists /
// CreateRepository make, so publish can be exercised against a real
// github.GhClient with no network (the seam the spec asks for).
type stubGitHubRunner struct {
	mu        sync.Mutex
	calls     []string
	failPaths map[string]bool // contents path -> force the PUT to fail
	repoVisib bool            // repo view reports the repo exists
}

func (s *stubGitHubRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, command+" "+strings.Join(args, " "))
	s.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case len(args) >= 2 && args[0] == "repo" && args[1] == "view":
		if s.repoVisib {
			return subprocess.Result{Stdout: `{"nameWithOwner":"jerry/templates"}`}, nil
		}
		return subprocess.Result{Stderr: "Could not resolve to a Repository with the name"}, errors.New("not found")
	case len(args) >= 2 && args[0] == "repo" && args[1] == "create":
		return subprocess.Result{Stdout: ""}, nil
	case strings.Contains(joined, "-X GET") && strings.Contains(joined, "/contents/"):
		// No existing file -> 404 so UpsertFile creates it (sha stays empty).
		return subprocess.Result{Stderr: "gh: Not Found (HTTP 404)"}, errors.New("not found")
	case strings.Contains(joined, "-X PUT") && strings.Contains(joined, "/contents/"):
		path := contentsPath(args)
		if s.failPaths[path] {
			return subprocess.Result{Stderr: "HTTP 422 validation failed"}, errors.New("upsert failed")
		}
		return subprocess.Result{Stdout: `{"content":{"path":"` + path + `","html_url":"https://github.com/jerry/templates/blob/main/` + path + `","sha":"deadbeefcafe1234"}}`}, nil
	default:
		return subprocess.Result{Stderr: "unexpected gh call"}, errors.New("unexpected gh call: " + joined)
	}
}

func (s *stubGitHubRunner) LookPath(file string) (string, error) { return file, nil }

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

func replaceTemplateRemoteClient(client templateRemoteClient) func() {
	previous := newTemplateRemoteClient
	newTemplateRemoteClient = func() templateRemoteClient { return client }
	return func() { newTemplateRemoteClient = previous }
}

func replaceAgentTemplateRemoteSource(source agenttemplate.RemoteSource) func() {
	previous := newAgentTemplateRemoteSource
	newAgentTemplateRemoteSource = func() agenttemplate.RemoteSource { return source }
	return func() { newAgentTemplateRemoteSource = previous }
}

func addCustomTemplate(t *testing.T, home, id, body string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	promptPath := filepath.Join(t.TempDir(), id+".md")
	if err := os.WriteFile(promptPath, []byte(testLocalTemplateContent(id, body)), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if code := Run([]string{"agent", "template", "add", "--home", home, "--file", promptPath, id}, &stdout, &stderr); code != 0 {
		t.Fatalf("template add %s exit code = %d, stderr=%s", id, code, stderr.String())
	}
}

func TestAgentTemplatePublishReportsPerFileResults(t *testing.T) {
	stub := &stubGitHubRunner{failPaths: map[string]bool{"templates/api-reviewer.md": true}}
	restore := replaceTemplateRemoteClient(&github.GhClient{Runner: stub, MaxRetries: 1})
	defer restore()

	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	addCustomTemplate(t, home, "frontend-reviewer", "Review UI.\n")
	addCustomTemplate(t, home, "api-reviewer", "Review API.\n")

	code := Run([]string{"agent", "template", "publish", "--home", home, "--all", "--repo", "jerry/templates"}, &stdout, &stderr)
	// One file failed, so the command reports failure overall (partial batch).
	if code == 0 {
		t.Fatalf("publish with one failing file should exit non-zero, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "published frontend-reviewer -> templates/frontend-reviewer.md") {
		t.Fatalf("expected the healthy file to publish:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "publish api-reviewer:") {
		t.Fatalf("expected the failing file reported on stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "1 of 2 template(s) failed to publish") {
		t.Fatalf("expected a partial-batch summary:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "verbatim") {
		t.Fatalf("expected a secrets/visibility caution before publishing:\n%s", stderr.String())
	}
}

func TestAgentTemplatePublishAllSucceeds(t *testing.T) {
	stub := &stubGitHubRunner{}
	restore := replaceTemplateRemoteClient(&github.GhClient{Runner: stub, MaxRetries: 1})
	defer restore()

	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	addCustomTemplate(t, home, "frontend-reviewer", "Review UI.\n")

	code := Run([]string{"agent", "template", "publish", "frontend-reviewer", "--home", home, "--repo", "jerry/templates", "--create"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("publish exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created jerry/templates (private)") {
		t.Fatalf("expected --create to create the missing repo:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "published frontend-reviewer ->") {
		t.Fatalf("expected publish success line:\n%s", stdout.String())
	}
}

// panickingRemoteClient fails the test if any of its methods are called, proving
// --dry-run never touches the network.
type panickingRemoteClient struct{ t *testing.T }

func (c panickingRemoteClient) UpsertFile(context.Context, github.UpsertFileInput) (github.RepositoryFile, error) {
	c.t.Fatal("UpsertFile called during --dry-run")
	return github.RepositoryFile{}, nil
}
func (c panickingRemoteClient) RepositoryExists(context.Context, github.Repository) (bool, error) {
	c.t.Fatal("RepositoryExists called during --dry-run")
	return false, nil
}
func (c panickingRemoteClient) CreateRepository(context.Context, github.Repository, bool) error {
	c.t.Fatal("CreateRepository called during --dry-run")
	return nil
}

func TestAgentTemplatePublishDryRunIsNetworkFree(t *testing.T) {
	restore := replaceTemplateRemoteClient(panickingRemoteClient{t: t})
	defer restore()

	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	addCustomTemplate(t, home, "frontend-reviewer", "Review UI.\n")

	code := Run([]string{"agent", "template", "publish", "--home", home, "--all", "--repo", "jerry/templates", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("publish --dry-run exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would publish frontend-reviewer -> jerry/templates/templates/frontend-reviewer.md") {
		t.Fatalf("dry-run stdout = %s", stdout.String())
	}
}

func TestAgentTemplatePublishRequiresRepoWhenNoRemoteConfigured(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	addCustomTemplate(t, home, "frontend-reviewer", "Review UI.\n")
	code := Run([]string{"agent", "template", "publish", "--home", home, "--all"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("publish without --repo or a configured remote should fail, stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no template repo") {
		t.Fatalf("stderr missing remote guidance:\n%s", stderr.String())
	}
}

func TestAgentTemplateRemoteSetShowAndPublishDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	home := t.TempDir()

	// show before set reports nothing configured.
	if code := Run([]string{"agent", "template", "remote", "show", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no default template remote configured") {
		t.Fatalf("remote show before set = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "remote", "set", "jerry/templates", "--home", home, "--path", "agents"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote set exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "set default template remote to jerry/templates") {
		t.Fatalf("remote set stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "remote", "show", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"repo: jerry/templates", "ref: main", "path: agents"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("remote show after set missing %q:\n%s", want, stdout.String())
		}
	}

	// publish with no --repo now defaults to the configured remote (path agents).
	stub := &stubGitHubRunner{}
	restore := replaceTemplateRemoteClient(&github.GhClient{Runner: stub, MaxRetries: 1})
	defer restore()
	addCustomTemplate(t, home, "frontend-reviewer", "Review UI.\n")
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "publish", "--home", home, "--all"}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish via configured remote exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "published frontend-reviewer -> agents/frontend-reviewer.md") {
		t.Fatalf("publish did not use the configured remote subdir:\n%s", stdout.String())
	}
}

// fakeRemoteSource serves a directory listing + per-path content for pull.
type cliFakeRemoteSource struct {
	commit  string
	entries []agenttemplate.DirEntry
	files   map[string]string
}

func (f cliFakeRemoteSource) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}
func (f cliFakeRemoteSource) FetchFile(_ context.Context, _ string, _ string, path string) (agenttemplate.File, error) {
	content, ok := f.files[path]
	if !ok {
		return agenttemplate.File{}, errors.New("no file at " + path)
	}
	return agenttemplate.File{Content: content}, nil
}
func (f cliFakeRemoteSource) ListDir(context.Context, string, string, string) ([]agenttemplate.DirEntry, error) {
	return f.entries, nil
}

func TestAgentTemplatePullInstallsThenDiffsRemoteRow(t *testing.T) {
	source := cliFakeRemoteSource{
		commit: "remote-sha-1",
		entries: []agenttemplate.DirEntry{
			{Name: "frontend-reviewer.md", Path: "templates/frontend-reviewer.md", Type: "file"},
		},
		files: map[string]string{
			"templates/frontend-reviewer.md": testLocalTemplateContent("frontend-reviewer", "Review UI changes.\n"),
		},
	}
	restore := replaceAgentTemplateRemoteSource(source)
	defer restore()

	var stdout, stderr bytes.Buffer
	home := t.TempDir()
	code := Run([]string{"agent", "template", "pull", "--home", home, "--all", "--repo", "jerry/templates"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pull exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed frontend-reviewer at remote-sha-1") {
		t.Fatalf("pull stdout = %s", stdout.String())
	}

	// A second identical pull is a no-op.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "pull", "--home", home, "--all", "--repo", "jerry/templates"}, &stdout, &stderr); code != 0 {
		t.Fatalf("second pull exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unchanged frontend-reviewer") {
		t.Fatalf("identical pull should be a no-op:\n%s", stdout.String())
	}

	// diff now works for the pulled (remote-sourced) row: point the diff fetcher at
	// changed upstream content and confirm the diff renders.
	diffRestore := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "remote-sha-2",
		content: testLocalTemplateContent("frontend-reviewer", "Review UI changes deeply.\n"),
	})
	defer diffRestore()
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"agent", "template", "diff", "--home", home, "frontend-reviewer"}, &stdout, &stderr); code != 0 {
		t.Fatalf("diff on remote row exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"cached:   remote-sha-1", "upstream: remote-sha-2", "-Review UI changes.", "+Review UI changes deeply."} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("diff on remote row missing %q:\n%s", want, stdout.String())
		}
	}
}
