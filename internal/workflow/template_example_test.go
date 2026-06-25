package workflow

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/skills"
)

// TestPackagedTemplateExamplesPassValidation guards every packaged coordinator
// agent-template's worked gitmoot_result example against the result contract,
// including the fail-closed capability<->policy check (#452): a coordinator that
// follows its own example verbatim must not have its result rejected. In
// particular, every ephemeral "implement" delegation in an example must carry an
// explicit write policy (workspace-write or danger-full-access); an empty/auto/
// read-only policy normalizes to no headless write permission and is refused.
func TestPackagedTemplateExamplesPassValidation(t *testing.T) {
	root := "gitmoot/agent-templates"
	entries, err := fs.ReadDir(skills.FS, root)
	if err != nil {
		t.Fatalf("read agent-templates dir: %v", err)
	}
	found := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := root + "/" + entry.Name()
		body, err := fs.ReadFile(skills.FS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(body)
		// Only templates that actually carry a gitmoot_result example exercise
		// the validation; metadata-only or prose-only templates are skipped.
		if !strings.Contains(text, "gitmoot_result") {
			continue
		}
		found++
		if _, err := ExtractAgentResult(text); err != nil {
			t.Errorf("packaged template %s example fails the result contract: %v", entry.Name(), err)
		}
	}
	if found == 0 {
		t.Fatal("no packaged templates with a gitmoot_result example were exercised")
	}
}
