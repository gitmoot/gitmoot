package config

import (
	"testing"
	"time"
)

func TestLoadWorkspacesPolicyDefaultsAreDisabledWithPilotCap(t *testing.T) {
	policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[remote_exec]\nbackend = \"local\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspacesPolicy{Provider: "e2b", MaxTotalUSD: 50, MaxWorkspaceUSD: 10, IdleTimeout: 4 * time.Hour}
	if policy != want {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
}

func TestLoadWorkspacesPolicyRefusesInvalidValues(t *testing.T) {
	for _, body := range []string{
		"[workspaces]\nmax_total_usd = 0\n",
		"[workspaces]\nmax_total_usd = -5\n",
		"[workspaces]\nmax_workspace_usd = lots\n",
		"[workspaces]\nmax_total_usd = 5\nmax_workspace_usd = 6\n",
		"[workspaces]\nidle_timeout = \"30s\"\n",
		"[workspaces]\nenabled = yes\n",
		"[workspaces]\nprovider = \"aws\"\n",
		"[workspaces]\nmax_total = 5\n",
		"[workspaces\nenabled = true\n",
		"[workspaces]\ntemplate = \"\"\n",
		"[workspaces]\ntemplate = \"has space\"\n",
		"[workspaces]\ntemplate = \"-leading-dash\"\n",
		"[workspaces]\ntemplate = \"quote\\\"d\"\n",
		"[workspaces]\ntemplate = base\n",
	} {
		if _, err := LoadWorkspacesPolicy(writeGateConfig(t, body)); err == nil {
			t.Fatalf("LoadWorkspacesPolicy accepted %q", body)
		}
	}
	policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nenabled = true\nprovider = \"sandboxd\"\nmax_total_usd = 20\nmax_workspace_usd = 4.5\nidle_timeout = \"90m\"\n[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspacesPolicy{Enabled: true, Provider: "sandboxd", MaxTotalUSD: 20, MaxWorkspaceUSD: 4.5, IdleTimeout: 90 * time.Minute}
	if policy != want {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
	if policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nprovider = \"local\"\n")); err != nil || policy.Provider != WorkspacesLocalProvider {
		t.Fatalf("local provider = %+v %v", policy, err)
	}
}

func TestLoadWorkspacesPolicyTemplate(t *testing.T) {
	for _, name := range []string{"gitmoot-omp-go126-2c4g-v1", "rki5dems9wqfm4r03t7g", "team/name:v1.2_3"} {
		policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\ntemplate = \""+name+"\"\n"))
		if err != nil || policy.Template != name {
			t.Fatalf("template %q = %+v, %v", name, policy, err)
		}
	}
	// Unset means the provider's [remote_exec] template (resolved by the CLI).
	if policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nenabled = true\n")); err != nil || policy.Template != "" {
		t.Fatalf("no template = %+v, %v", policy, err)
	}
}
