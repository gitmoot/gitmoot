package execbackend

import (
	"strings"
	"testing"
)

func TestParseResolvesAdvertisedLocal(t *testing.T) {
	for _, value := range []string{"local", " local "} {
		backend, err := Parse(value)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v, want local", value, err)
		}
		if backend != Local {
			t.Fatalf("Parse(%q) = %q, want %q", value, backend, Local)
		}
	}
}

func TestParseUnknownAndBlankFailLoudNamingValueAndAllowedSet(t *testing.T) {
	for _, value := range []string{"", "   ", "e2b", "loca", "LOCAL", "remote"} {
		backend, err := Parse(value)
		if err == nil {
			t.Fatalf("Parse(%q) = %q, want a loud error", value, backend)
		}
		if !strings.Contains(err.Error(), `"`+strings.TrimSpace(value)+`"`) {
			t.Fatalf("Parse(%q) error = %q, want it to name the offending value", value, err)
		}
		if !strings.Contains(err.Error(), "allowed: local") {
			t.Fatalf("Parse(%q) error = %q, want it to name the allowed set", value, err)
		}
	}
}

func TestResolveOverrideWinsOverConfig(t *testing.T) {
	local := "local"
	backend, err := Resolve("local", &local)
	if err != nil || backend != Local {
		t.Fatalf("Resolve(local, local) = %q, %v", backend, err)
	}
	// An unknown override fails loud even when the config value is valid, and
	// the error identifies the override as the source.
	e2b := "e2b"
	_, err = Resolve("local", &e2b)
	if err == nil {
		t.Fatal("Resolve(local, e2b) succeeded, want a loud override error")
	}
	if !strings.Contains(err.Error(), "exec_backend") || !strings.Contains(err.Error(), `"e2b"`) || !strings.Contains(err.Error(), "allowed: local") {
		t.Fatalf("Resolve(local, e2b) error = %q, want override source + value + allowed set", err)
	}
	// An absent override falls through to the config value; an unknown config
	// value fails loud.
	if _, err := Resolve("loca", nil); err == nil || !strings.Contains(err.Error(), `"loca"`) {
		t.Fatalf("Resolve(loca, \"\") error = %v, want a loud config error", err)
	}
	if backend, err := Resolve("", nil); err != nil || backend != Local {
		t.Fatalf("Resolve(\"\", \"\") = %q, %v, want local default", backend, err)
	}
	blank := ""
	if _, err := Resolve("local", &blank); err == nil || !strings.Contains(err.Error(), "exec_backend") || !strings.Contains(err.Error(), `unknown execution backend ""`) {
		t.Fatalf("Resolve(local, explicit blank) error = %v, want a loud override error", err)
	}
}

// TestAllowedNamesMatchesParse pins the single-source contract: every name in
// AllowedNames parses, and Parse's error renders exactly that set. Parse uses
// this list as its acceptance source, so accepted-but-unadvertised backends are
// structurally impossible.
func TestAllowedNamesMatchesParse(t *testing.T) {
	if len(AllowedNames) == 0 {
		t.Fatal("AllowedNames is empty; the fail-loud error would name no allowed set")
	}
	for _, name := range AllowedNames {
		if _, err := Parse(name); err != nil {
			t.Fatalf("AllowedNames entry %q does not parse: %v", name, err)
		}
	}
	_, err := Parse("definitely-not-a-backend")
	if err == nil || !strings.Contains(err.Error(), "allowed: "+Allowed()) {
		t.Fatalf("unknown-backend error = %v, want the rendered AllowedNames set", err)
	}
}
