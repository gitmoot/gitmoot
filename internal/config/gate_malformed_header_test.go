package config

import (
	"os"
	"strings"
	"testing"
)

// writeGateConfig puts content at the config path for a fresh home and returns the
// paths, so every case below enters through the PRODUCTION loader rather than
// through sectionHeader.
func writeGateConfig(t *testing.T, content string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths
}

// TestGateLoadersRefuseTheirOwnMalformedHeader is the round-N P2-1.
//
// The #1759 malformed-header reset stopped MISATTRIBUTING keys, which was the
// point, but for the guard loaders it also discarded the operator's settings
// and returned the DEFAULT with a nil error. For merge_gate that default is the
// permissive one, so a config typo silently re-enabled auto-merge and dropped
// the external-CI requirement. Silence plus a permissive default is the worst
// available combination, so these loaders now refuse the file.
//
// Fixtures are the reviewer's, verbatim in shape.
func TestGateLoadersRefuseTheirOwnMalformedHeader(t *testing.T) {
	t.Run("merge_gate", func(t *testing.T) {
		paths := writeGateConfig(t, "[merge_gate]\n[merge_gate\nauto_merge = false\nrequire_external_ci = true\n")
		_, err := LoadMergeGatePolicy(paths)
		if err == nil {
			t.Fatal("a malformed merge_gate header loaded cleanly; the permissive default would silently re-enable auto-merge")
		}
		if !strings.Contains(err.Error(), "merge_gate") || !strings.Contains(err.Error(), "missing closing") {
			t.Errorf("error = %v, want it to name the section and the defect", err)
		}
	})

	t.Run("admission", func(t *testing.T) {
		paths := writeGateConfig(t, "[admission]\n[admission\nmax_concurrent_sessions = 2\nmax_memory_gb = 8\n")
		_, err := LoadAdmissionPolicy(paths)
		if err == nil {
			t.Fatal("a malformed admission header loaded cleanly; the zero policy disables admission accounting entirely")
		}
		if !strings.Contains(err.Error(), "admission") {
			t.Errorf("error = %v, want it to name admission", err)
		}
	})

	t.Run("workflow", func(t *testing.T) {
		paths := writeGateConfig(t, "[workflow]\n[workflow\nresult_checks = block\n")
		if _, err := LoadRequireWorkflow(paths); err == nil {
			t.Fatal("a malformed workflow header loaded cleanly")
		}
	})

	t.Run("review", func(t *testing.T) {
		paths := writeGateConfig(t, "[review]\n[review\nrequire_external_ci = true\n")
		if _, err := LoadReviewConfig(paths); err == nil {
			t.Fatal("a malformed review header loaded cleanly")
		}
	})

	t.Run("repo-scoped malformed header is also refused", func(t *testing.T) {
		paths := writeGateConfig(t, "[repos.\"owner/repo\".merge_gate\nauto_merge = false\n")
		if _, err := LoadMergeGatePolicy(paths); err == nil {
			t.Fatal("a malformed repo-scoped merge_gate header loaded cleanly")
		}
	})
}

// TestAnUnrelatedMalformedHeaderStillLoadsTheGates is the direction that keeps
// the refusal from becoming a worse defect than the one it fixes.
//
// org.go set the precedent in this package: fail closed only for a header
// shaped like YOUR OWN section, because "a typo in an unrelated section must
// not brick dispatch". A daemon that will not start because of a typo in
// [disk_guard is a bigger outage than the one being prevented.
func TestAnUnrelatedMalformedHeaderStillLoadsTheGates(t *testing.T) {
	const unrelated = "[merge_gate]\nauto_merge = false\n\n[disk_guard\nmin_free_bytes = 111\n"

	gate, err := LoadMergeGatePolicy(writeGateConfig(t, unrelated))
	if err != nil {
		t.Fatalf("a typo in an unrelated section bricked the merge gate: %v", err)
	}
	if gate.For("owner/repo").AutoMerge {
		t.Error("the operator's auto_merge = false was discarded even though the typo was elsewhere")
	}

	if _, err := LoadAdmissionPolicy(writeGateConfig(t, unrelated)); err != nil {
		t.Errorf("a typo in an unrelated section bricked admission: %v", err)
	}
	if _, err := LoadRequireWorkflow(writeGateConfig(t, unrelated)); err != nil {
		t.Errorf("a typo in an unrelated section bricked require_workflow: %v", err)
	}
	if _, err := LoadReviewConfig(writeGateConfig(t, unrelated)); err != nil {
		t.Errorf("a typo in an unrelated section bricked review: %v", err)
	}

	// A name that merely STARTS with a gate name is unrelated too.
	if _, err := LoadMergeGatePolicy(writeGateConfig(t, "[merge_gateway\nx = 1\n")); err != nil {
		t.Errorf("[merge_gateway is not a merge_gate header and must not refuse: %v", err)
	}
}

// TestWellFormedGateConfigIsUnchanged pins that none of the above touched the
// ordinary path: a valid file must load exactly as before.
func TestWellFormedGateConfigIsUnchanged(t *testing.T) {
	paths := writeGateConfig(t, "[merge_gate]\nauto_merge = false\nrequire_external_ci = true\n")
	gate, err := LoadMergeGatePolicy(paths)
	if err != nil {
		t.Fatalf("a well-formed config was refused: %v", err)
	}
	policy := gate.For("owner/repo")
	if policy.AutoMerge {
		t.Error("auto_merge = false was not applied")
	}
	if !policy.RequireExternalCI {
		t.Error("require_external_ci = true was not applied")
	}
}

// TestMalformedHeaderRoutingPerCallSite is the round-N P2-3: the converted call
// sites were unproven, and reverting merge_gate.go's site to the inline
// two-bracket form survived the whole package.
//
// Each case drives a PRODUCTION loader and asserts the misattribution property
// directly: a value set inside a well-formed section, then a malformed header
// of that same section, then an OVERRIDE of the value. If the site is reverted,
// the malformed line stops being a boundary, the override is misattributed to
// the still-open section, and the loader returns the override - so a revert at
// any covered site fails here rather than passing quietly.
func TestMalformedHeaderRoutingPerCallSite(t *testing.T) {
	t.Run("parallel_sessions", func(t *testing.T) {
		paths := writeGateConfig(t, "[parallel_sessions]\nsame_session = \"queue\"\n[parallel_sessions\nsame_session = \"fork_temp_session\"\n")
		policy, err := LoadParallelSessionPolicy(paths)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if policy.SameSession != "queue" {
			t.Fatalf("same_session = %q, want queue: the override after a malformed header was misattributed", policy.SameSession)
		}
	})

	t.Run("transcripts", func(t *testing.T) {
		paths := writeGateConfig(t, "[transcripts]\nenabled = false\n[transcripts\nenabled = true\n")
		if got := LoadTranscriptsConfig(paths); got.Enabled {
			t.Fatal("enabled = true after a malformed header was misattributed to [transcripts]")
		}
	})

	t.Run("router", func(t *testing.T) {
		paths := writeGateConfig(t, "[router]\ncontext_enabled = false\n[router\ncontext_enabled = true\n")
		settings, err := LoadRouterSettings(paths)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if settings.ContextEnabled {
			t.Fatal("context_enabled = true after a malformed header was misattributed to [router]")
		}
	})

	// The valid-header path for the same loaders, so these assertions cannot be
	// satisfied by a loader that simply ignores everything.
	t.Run("valid headers still apply their values", func(t *testing.T) {
		policy, err := LoadParallelSessionPolicy(writeGateConfig(t, "[parallel_sessions]\nsame_session = \"fork_temp_session\"\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if policy.SameSession != "fork_temp_session" {
			t.Errorf("same_session = %q, want fork_temp_session", policy.SameSession)
		}
		if got := LoadTranscriptsConfig(writeGateConfig(t, "[transcripts]\nenabled = true\n")); !got.Enabled {
			t.Error("transcripts enabled = true was not applied")
		}
		settings, err := LoadRouterSettings(writeGateConfig(t, "[router]\ncontext_enabled = true\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !settings.ContextEnabled {
			t.Error("router context_enabled = true was not applied")
		}
	})
}
