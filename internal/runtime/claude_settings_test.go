package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverClaudeHookResources(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := `{
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{"type":"command","command":"if [ -x '/opt/hooks/prompt.sh' ]; then /bin/sh '/opt/hooks/prompt.sh'; fi"}]}],
    "PreToolUse": [{"hooks": [{"type":"command","command":"python3 \"/srv/hooks/tool hook.py\""}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/libexec/stop-hook"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	resources, warnings := DiscoverClaudeHookResources(home, configDir)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v", warnings)
	}
	var got []string
	for _, resource := range resources {
		got = append(got, resource.Event+":"+resource.Path)
		if strings.Contains(resource.Path, "if [") {
			t.Fatalf("resource retained command text: %+v", resource)
		}
	}
	want := []string{
		"PreToolUse:/srv/hooks/tool hook.py",
		"UserPromptSubmit:/opt/hooks/prompt.sh",
		"UserPromptSubmit:/bin/sh",
		"Stop:/usr/local/libexec/stop-hook",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %q, want %q", got, want)
	}
}

func TestDiscoverClaudeHookResourcesWarningsAndMissing(t *testing.T) {
	tests := []struct {
		name       string
		settings   string
		wantReason string
	}{
		{name: "relative direct", settings: `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"./hook.sh"}]}]}}`, wantReason: "relative path"},
		{name: "relative interpreter", settings: `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"python3 hook.py"}]}]}}`, wantReason: "relative path"},
		{name: "malformed command", settings: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'/opt/hook.sh"}]}]}}`, wantReason: "unterminated quote"},
		{name: "malformed json", settings: `{`, wantReason: "invalid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			configDir := filepath.Join(home, ".claude")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(tc.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			_, warnings := DiscoverClaudeHookResources(home, configDir)
			if len(warnings) != 1 || !strings.Contains(warnings[0].Reason, tc.wantReason) {
				t.Fatalf("warnings = %+v, want one containing %q", warnings, tc.wantReason)
			}
		})
	}

	home := t.TempDir()
	resources, warnings := DiscoverClaudeHookResources(home, filepath.Join(home, ".claude"))
	if len(resources) != 0 || len(warnings) != 0 {
		t.Fatalf("missing settings = resources %+v warnings %+v, want no-op", resources, warnings)
	}
}
