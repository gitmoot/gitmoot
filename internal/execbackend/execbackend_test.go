package execbackend

import (
	"strings"
	"testing"
)

func TestParseResolvesAdvertisedBackends(t *testing.T) {
	for _, value := range []string{"local", " local ", "remote", " remote "} {
		backend, err := Parse(value)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", value, err)
		}
		if backend != Backend(strings.TrimSpace(value)) {
			t.Fatalf("Parse(%q) = %q", value, backend)
		}
	}
}

func TestParseUnknownAndBlankFailLoudNamingValueAndAllowedSet(t *testing.T) {
	for _, value := range []string{"", "   ", "e2b", "loca", "LOCAL"} {
		backend, err := Parse(value)
		if err == nil {
			t.Fatalf("Parse(%q) = %q, want a loud error", value, backend)
		}
		if !strings.Contains(err.Error(), `"`+strings.TrimSpace(value)+`"`) {
			t.Fatalf("Parse(%q) error = %q, want it to name the offending value", value, err)
		}
		if !strings.Contains(err.Error(), "allowed: local, remote") {
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
	if !strings.Contains(err.Error(), "exec_backend") || !strings.Contains(err.Error(), `"e2b"`) || !strings.Contains(err.Error(), "allowed: local, remote") {
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
		if _, err := ParseImplemented(name); err != nil {
			t.Fatalf("AllowedNames entry %q is not implemented: %v", name, err)
		}
	}
	_, err := Parse("definitely-not-a-backend")
	if err == nil || !strings.Contains(err.Error(), "allowed: "+Allowed()) {
		t.Fatalf("unknown-backend error = %v, want the rendered AllowedNames set", err)
	}
}

func TestAdvertisedBackendWithoutImplementationFailsLoud(t *testing.T) {
	original := append([]string(nil), AllowedNames...)
	defer func() { AllowedNames = original }()
	AllowedNames = append(AllowedNames, "future-remote")

	if backend, err := Parse("future-remote"); err != nil || backend != Backend("future-remote") {
		t.Fatalf("Parse(advertised future backend) = %q, %v; want advertised name accepted", backend, err)
	}
	if _, err := ParseImplemented("future-remote"); err == nil || !strings.Contains(err.Error(), `"future-remote"`) || !strings.Contains(err.Error(), "advertised but not implemented") {
		t.Fatalf("ParseImplemented(advertised future backend) error = %v, want loud missing-implementation error", err)
	}
	if _, err := Resolve("future-remote", nil); err == nil || !strings.Contains(err.Error(), "advertised but not implemented") {
		t.Fatalf("Resolve(advertised future backend) error = %v, want loud missing-implementation error", err)
	}
}

func TestConsumeRequiresPositiveImplementation(t *testing.T) {
	localCalls := 0
	remoteCalls := 0
	local := func() (string, error) {
		localCalls++
		return "local-result", nil
	}
	remote := func() (string, error) {
		remoteCalls++
		return "remote-result", nil
	}
	got, err := Consume(Local, func() (string, error) {
		return local()
	}, remote)
	if err != nil || got != "local-result" || localCalls != 1 || remoteCalls != 0 {
		t.Fatalf("Consume(local) = %q, %v, local calls=%d remote calls=%d", got, err, localCalls, remoteCalls)
	}

	got, err = Consume(Remote, local, remote)
	if err != nil || got != "remote-result" || localCalls != 1 || remoteCalls != 1 {
		t.Fatalf("Consume(remote) = %q, %v, local calls=%d remote calls=%d", got, err, localCalls, remoteCalls)
	}

	got, err = Consume(Backend("p2-probe"), local, remote)
	if err == nil || !strings.Contains(err.Error(), `"p2-probe"`) {
		t.Fatalf("Consume(p2-probe) = %q, %v; want a loud missing-implementation error", got, err)
	}
	if localCalls != 1 || remoteCalls != 1 {
		t.Fatalf("p2-probe invoked a builder: local calls=%d remote calls=%d", localCalls, remoteCalls)
	}
}
