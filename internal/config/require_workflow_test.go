package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRequireWorkflow(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[workflow]\nrequire_workflow = true\nrequire_workflow_mode = \"strict\"\n[repos.\"a/b\"]\nrequire_workflow = false\n[repos.\"c/d\"]\nrequire_workflow_mode = \"auto\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRequireWorkflow(paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.For("none/repo"); !got.Enabled || got.Mode != "strict" {
		t.Fatalf("global=%+v", got)
	}
	if got := cfg.For("a/b"); got.Enabled || got.Mode != "strict" {
		t.Fatalf("a/b=%+v", got)
	}
	if got := cfg.For("c/d"); !got.Enabled || got.Mode != "auto" {
		t.Fatalf("c/d=%+v", got)
	}
}

func TestLoadRequireWorkflowDefaultsAndRejectsBadMode(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRequireWorkflow(paths)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.For("a/b"); got.Enabled || got.Mode != "auto" {
		t.Fatalf("default=%+v", got)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[workflow]\nrequire_workflow_mode = \"bad\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRequireWorkflow(paths); err == nil || !strings.Contains(err.Error(), "require_workflow_mode") {
		t.Fatalf("error=%v", err)
	}
}
