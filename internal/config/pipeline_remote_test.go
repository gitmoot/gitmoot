package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadPipelineRemoteDefaultsAndConfiguredValues(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	remote, err := LoadPipelineRemote(paths)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Configured() || remote.ResolvedRef() != DefaultPipelineRemoteRef || remote.ResolvedPath() != DefaultPipelineRemotePath {
		t.Fatalf("default pipeline remote = %+v ref=%q path=%q", remote, remote.ResolvedRef(), remote.ResolvedPath())
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[pipeline_remote]
repo = "jerry/pipelines"
ref = "shared"
path = "/catalog/pipelines/"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	remote, err = LoadPipelineRemote(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !remote.Configured() || remote.Repo != "jerry/pipelines" || remote.ResolvedRef() != "shared" || remote.ResolvedPath() != "/catalog/pipelines/" {
		t.Fatalf("configured pipeline remote = %+v ref=%q path=%q", remote, remote.ResolvedRef(), remote.ResolvedPath())
	}
}

func TestLoadPipelineRemoteRejectsMalformedRepo(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[pipeline_remote]\nrepo = \"missing-owner\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPipelineRemote(paths); err == nil || !strings.Contains(err.Error(), "pipeline_remote.repo") {
		t.Fatalf("LoadPipelineRemote malformed repo error = %v", err)
	}
}

func TestEnsurePipelineRemoteSectionAppendsOnce(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[database]\npath = \"state.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePipelineRemoteSection(paths); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePipelineRemoteSection(paths); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "[pipeline_remote]"); got != 1 {
		t.Fatalf("pipeline remote section count = %d\n%s", got, body)
	}
}
