package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadImplementBaseDefaultsToUnset(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	got, err := LoadImplementBase(paths)
	if err != nil {
		t.Fatalf("LoadImplementBase: %v", err)
	}
	if got != "" {
		t.Fatalf("implement base = %q, want unset", got)
	}
}

func TestLoadImplementBaseParsesWorkflowString(t *testing.T) {
	for _, value := range []string{"origin/main", "HEAD", strings.Repeat("a", 40)} {
		t.Run(value, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			content := DefaultConfig(paths) + "\n[workflow]\nimplement_base = \"" + value + "\"\n"
			if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			got, err := LoadImplementBase(paths)
			if err != nil {
				t.Fatalf("LoadImplementBase: %v", err)
			}
			if got != value {
				t.Fatalf("implement base = %q, want %q", got, value)
			}
		})
	}
}

func TestLoadImplementBaseRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{`origin/main`, `"-bad"`, `"bad ref"`} {
		t.Run(value, func(t *testing.T) {
			paths := PathsForHome(t.TempDir())
			if err := Initialize(paths); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte("[workflow]\nimplement_base = "+value+"\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if _, err := LoadImplementBase(paths); err == nil {
				t.Fatalf("LoadImplementBase accepted %s", value)
			}
		})
	}
}

func TestLoadImplementBaseIgnoresOtherSections(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[memory]\nimplement_base = \"origin/main\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	got, err := LoadImplementBase(paths)
	if err != nil {
		t.Fatalf("LoadImplementBase: %v", err)
	}
	if got != "" {
		t.Fatalf("implement base = %q, want unset", got)
	}
}
