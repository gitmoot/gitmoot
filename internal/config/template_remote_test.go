package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadTemplateRemoteDefaultsUnconfigured(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	// The DefaultConfig ships a [template_remote] section with an empty repo, which
	// must read back as NOT configured (off by default).
	remote, err := LoadTemplateRemote(paths)
	if err != nil {
		t.Fatalf("LoadTemplateRemote returned error: %v", err)
	}
	if remote.Configured() {
		t.Fatalf("default template remote must be unconfigured, got %+v", remote)
	}
	if remote.Repo != "" || remote.Ref != "" || remote.Path != "" {
		t.Fatalf("default template remote must be all-empty, got %+v", remote)
	}
	// Resolved* fall back to the documented defaults.
	if remote.ResolvedRef() != DefaultTemplateRemoteRef {
		t.Fatalf("ResolvedRef default = %q, want %q", remote.ResolvedRef(), DefaultTemplateRemoteRef)
	}
	if remote.ResolvedPath() != DefaultTemplateRemotePath {
		t.Fatalf("ResolvedPath default = %q, want %q", remote.ResolvedPath(), DefaultTemplateRemotePath)
	}
}

func TestLoadTemplateRemoteParsesSection(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[template_remote]
repo = "jerry/my-templates"
ref = "publish"
path = "agents"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	remote, err := LoadTemplateRemote(paths)
	if err != nil {
		t.Fatalf("LoadTemplateRemote returned error: %v", err)
	}
	if !remote.Configured() {
		t.Fatalf("template remote should be configured, got %+v", remote)
	}
	if remote.Repo != "jerry/my-templates" || remote.ResolvedRef() != "publish" || remote.ResolvedPath() != "agents" {
		t.Fatalf("parsed template remote = %+v", remote)
	}
}

func TestLoadTemplateRemoteRejectsMalformedRepo(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[template_remote]
repo = "not-an-owner-repo"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadTemplateRemote(paths); err == nil {
		t.Fatalf("LoadTemplateRemote accepted a malformed repo")
	}
}

func TestEnsureTemplateRemoteSectionAppendsThenSetRoundTrips(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	// Simulate an older config that predates the section by stripping it from the
	// default config (the other sections stay intact so SetConfigScalar's
	// post-edit validation of every parser still passes).
	full := DefaultConfig(paths)
	stripped := full[:strings.Index(full, "[template_remote]")] + full[strings.Index(full, "[skillopt] is"):]
	if strings.Contains(stripped, "[template_remote]") {
		t.Fatalf("failed to strip [template_remote] from default config")
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(stripped), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if err := EnsureTemplateRemoteSection(paths); err != nil {
		t.Fatalf("EnsureTemplateRemoteSection returned error: %v", err)
	}
	// Idempotent: a second call is a no-op (does not append a duplicate section).
	if err := EnsureTemplateRemoteSection(paths); err != nil {
		t.Fatalf("EnsureTemplateRemoteSection (second) returned error: %v", err)
	}
	body, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if got := strings.Count(string(body), "[template_remote]"); got != 1 {
		t.Fatalf("expected exactly one [template_remote] section, got %d:\n%s", got, string(body))
	}
	// SetConfigScalar can now edit the appended keys, and the result round-trips.
	if err := SetConfigScalar(paths, []string{"template_remote", "repo"}, StringScalar("jerry/templates")); err != nil {
		t.Fatalf("SetConfigScalar repo returned error: %v", err)
	}
	if err := SetConfigScalar(paths, []string{"template_remote", "path"}, StringScalar("agents")); err != nil {
		t.Fatalf("SetConfigScalar path returned error: %v", err)
	}
	remote, err := LoadTemplateRemote(paths)
	if err != nil {
		t.Fatalf("LoadTemplateRemote returned error: %v", err)
	}
	if remote.Repo != "jerry/templates" || remote.ResolvedPath() != "agents" {
		t.Fatalf("round-tripped template remote = %+v", remote)
	}
}
