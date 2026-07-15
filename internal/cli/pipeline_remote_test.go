package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/github"
	yaml "gopkg.in/yaml.v3"
)

type unitPipelineRemoteSource struct {
	files map[string]string
}

func (s unitPipelineRemoteSource) ResolveRef(context.Context, string, string) (string, error) {
	return "commit-1", nil
}

func (s unitPipelineRemoteSource) FetchFile(_ context.Context, _, _, path string) (agenttemplate.File, error) {
	content, ok := s.files[path]
	if !ok {
		return agenttemplate.File{}, errors.New("not found")
	}
	return agenttemplate.File{Content: content}, nil
}

func (s unitPipelineRemoteSource) ListDir(_ context.Context, _, _, path string) ([]agenttemplate.DirEntry, error) {
	prefix := strings.Trim(path, "/") + "/"
	dirs := map[string]bool{}
	entries := []agenttemplate.DirEntry{}
	for stored := range s.files {
		if !strings.HasPrefix(stored, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(stored, prefix)
		if index := strings.Index(remainder, "/"); index >= 0 {
			name := remainder[:index]
			if !dirs[name] {
				dirs[name] = true
				entries = append(entries, agenttemplate.DirEntry{Name: name, Path: prefix + name, Type: "dir"})
			}
			continue
		}
		entries = append(entries, agenttemplate.DirEntry{Name: remainder, Path: stored, Type: "file"})
	}
	if len(entries) == 0 {
		return nil, errors.New("not found (404)")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

type recordingPipelineRemoteClient struct {
	upserts []string
	deletes []string
}

func (c *recordingPipelineRemoteClient) UpsertFile(_ context.Context, input github.UpsertFileInput) (github.RepositoryFile, error) {
	c.upserts = append(c.upserts, input.Path)
	return github.RepositoryFile{Path: input.Path}, nil
}
func (c *recordingPipelineRemoteClient) DeleteFile(_ context.Context, input github.DeleteFileInput) (github.RepositoryFile, error) {
	c.deletes = append(c.deletes, input.Path)
	return github.RepositoryFile{Path: input.Path}, nil
}
func (*recordingPipelineRemoteClient) RepositoryExists(context.Context, github.Repository) (bool, error) {
	return true, nil
}
func (*recordingPipelineRemoteClient) CreateRepository(context.Context, github.Repository, bool) error {
	return nil
}

func TestPipelineRemoteSetShowAndFlagPrecedence(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pipeline", "remote", "set", "jerry/catalog", "--home", home, "--ref", "shared", "--path", "automation"}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote set exit=%d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"pipeline", "remote", "show", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("remote show exit=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"repo: jerry/catalog", "ref: shared", "path: automation"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("remote show missing %q:\n%s", want, stdout.String())
		}
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		t.Fatal(err)
	}
	repo, ref, path, err := resolvePipelineRemote(paths, "other/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "other/catalog" || ref != "shared" || path != "automation" {
		t.Fatalf("flag precedence resolved repo/ref/path = %q/%q/%q", repo, ref, path)
	}
}

func TestPipelineRemotePathLayout(t *testing.T) {
	if got := pipelineRemoteBundlePath("/pipelines/", "nightly-sync"); got != "pipelines/nightly-sync" {
		t.Fatalf("bundle path = %q", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bundle.yaml"), []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte("spec"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "reviewer.md"), []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := readPipelinePublishFiles(dir, pipelineRemoteBundlePath("pipelines", "nightly-sync"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pipelines/nightly-sync/bundle.yaml", "pipelines/nightly-sync/spec.yaml", "pipelines/nightly-sync/templates/reviewer.md"}
	got := make([]string, 0, len(files))
	for path := range files {
		got = append(got, path)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("remote files = %v, want %v", got, want)
	}
}

func TestSyncPipelineRemoteFilesNoopAndDeletesVanishedFiles(t *testing.T) {
	repo, err := daemon.ParseRepository("jerry/catalog")
	if err != nil {
		t.Fatal(err)
	}
	source := unitPipelineRemoteSource{files: map[string]string{
		"pipelines/nightly/bundle.yaml":            "bundle",
		"pipelines/nightly/spec.yaml":              "spec",
		"pipelines/nightly/templates/vanished.md":  "old",
		"pipelines/nightly/templates/unchanged.md": "prompt",
	}}
	desired := map[string][]byte{
		"pipelines/nightly/bundle.yaml":            []byte("bundle"),
		"pipelines/nightly/spec.yaml":              []byte("spec"),
		"pipelines/nightly/templates/unchanged.md": []byte("prompt"),
	}
	client := &recordingPipelineRemoteClient{}
	changed, deleted, err := syncPipelineRemoteFiles(context.Background(), client, source, repo, "main", "pipelines/nightly", desired, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || deleted != 1 || len(client.upserts) != 0 || !reflect.DeepEqual(client.deletes, []string{"pipelines/nightly/templates/vanished.md"}) {
		t.Fatalf("changed=%d deleted=%d upserts=%v deletes=%v", changed, deleted, client.upserts, client.deletes)
	}
}

func TestParsePipelineRemoteListingAndRequirementsLine(t *testing.T) {
	manifest := pipelineBundleManifest{
		BundleVersion: pipelineBundleVersion,
		Pipeline:      "nightly",
		Description:   "Nightly deployment sync.",
		Requirements: pipelineBundleRequirements{
			Runtimes:          []string{"shell", "codex"},
			Connections:       []pipelineBundleConnection{{Kind: "email", Name: "deploy-mail"}},
			UpstreamPipelines: []string{"ingest"},
		},
	}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := parsePipelineRemoteListing("nightly", raw)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Name != "nightly" || listing.Description != "Nightly deployment sync." {
		t.Fatalf("listing = %+v", listing)
	}
	line := pipelineRemoteRequirementsLine(listing.Requirements)
	for _, want := range []string{"runtimes=codex,shell", "connections=email/deploy-mail", "upstreams=ingest"} {
		if !strings.Contains(line, want) {
			t.Fatalf("requirements line %q missing %q", line, want)
		}
	}
}

func TestPipelinePublishWithoutRemoteNamesSetupCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"pipeline", "publish", "nightly", "--home", t.TempDir()}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "gitmoot pipeline remote set") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestConfiguredPipelineRemoteFreshHomeIsOff(t *testing.T) {
	if remote, ok := configuredPipelineRemote(t.TempDir()); ok || remote.Configured() {
		t.Fatalf("fresh home remote = %+v configured=%v", remote, ok)
	}
}
