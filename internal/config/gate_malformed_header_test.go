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
// COVERAGE, STATED AS A COUNT RATHER THAN IMPLIED. sectionHeader has 26 call
// sites across 22 files in this package. Pinned through a production loader
// here: the 4 guard loaders by refusal (merge_gate, admission, workflow,
// review) and 8 by routing (parallel_sessions, transcripts, router, daemon,
// github, remote_exec, credentials, plus admission's original regression) - 12
// of 26 (8 routing + 4 guards + admission original, then 4 more in
// TestMalformedHeaderRoutingRemainingLoaders = 16).
//
// NOT PINNED, and named so the next reader does not have to re-derive it:
// github_remote, heartbeats, implement_base, memory, repo_concurrency,
// result_checks, runtime_registry, stale_tasks (3 sites), workflow_lifecycle,
// orchestrate's LoadOrchestratePolicy and LoadEventsPolicy, org.go's
// parseOrgContent (which has its own stricter org-shaped refusal), and
// edit_compat's configSectionAtParseError (a diagnostic, not a loader). Those
// loaders key off prefixes, repo-scoped names or no fixed section string, so
// each needs its own observable field; a revert at one of them would NOT fail
// this test.
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

	t.Run("daemon", func(t *testing.T) {
		cfg, err := LoadDaemonRuntimeConfig(writeGateConfig(t, "[daemon]\nworkers = 2\n[daemon\nworkers = 99\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Workers != 2 {
			t.Fatalf("workers = %d, want 2: the override after a malformed header was misattributed", cfg.Workers)
		}
	})

	t.Run("github", func(t *testing.T) {
		policy, err := LoadGitHubLimiterPolicy(writeGateConfig(t, "[github]\nmax_concurrent = 2\n[github\nmax_concurrent = 99\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if policy.MaxConcurrent != 2 {
			t.Fatalf("max_concurrent = %d, want 2", policy.MaxConcurrent)
		}
	})

	t.Run("remote_exec", func(t *testing.T) {
		cfg, err := LoadRemoteExecConfig(writeGateConfig(t, "[remote_exec]\nbackend = \"local\"\n[remote_exec\nbackend = \"remote\"\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Backend != "local" {
			t.Fatalf("backend = %q, want local", cfg.Backend)
		}
	})

	t.Run("credentials", func(t *testing.T) {
		cfg, err := LoadCredentialsConfig(writeGateConfig(t, "[credentials]\nenv_curation = false\n[credentials\nenv_curation = true\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.EnvCuration {
			t.Fatal("env_curation = true after a malformed header was misattributed to [credentials]")
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

// TestMalformedHeaderRoutingRemainingLoaders extends per-call-site coverage to
// loaders whose section name is compared after assignment rather than at the
// header. Each case uses the loader's REAL entrypoint and a field that
// distinguishes pre-header state from a post-malformed-header override, and
// each was MUTATION-PROVEN: reverting that loader's call site to the inline
// two-bracket form fails the matching subtest.
//
// NOT INCLUDED, and stated rather than quietly omitted: [agents.*] heartbeats.
// A test of the same shape PASSED under the reverted call site, because an
// enabled flag alone does not make LoadHeartbeats emit a distinguishable
// entry - so it would have been coverage in name only. That site remains
// unpinned and is counted as such.
func TestMalformedHeaderRoutingRemainingLoaders(t *testing.T) {
	t.Run("result_checks", func(t *testing.T) {
		mode, err := LoadResultChecksMode(writeGateConfig(t, "[workflow]\nresult_checks = warn\n[workflow\nresult_checks = block\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if string(mode) != "warn" {
			t.Fatalf("result_checks = %q, want warn: the override after a malformed header was misattributed", mode)
		}
	})

	t.Run("memory", func(t *testing.T) {
		settings, err := LoadMemorySettings(writeGateConfig(t, "[memory]\ntoken_budget = 111\n[memory\ntoken_budget = 999\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if settings.TokenBudget == 999 {
			t.Fatal("token_budget = 999 after a malformed header was misattributed to [memory]")
		}
		if settings.TokenBudget != 111 {
			t.Fatalf("token_budget = %d, want 111 (keys BEFORE the malformed header must still apply)", settings.TokenBudget)
		}
	})

	t.Run("runtime_registry", func(t *testing.T) {
		overrides, err := LoadRuntimeOverrides(writeGateConfig(t, "[runtimes.codex]\ndefault_model = \"keep\"\n[runtimes.codex\ndefault_model = \"leaked\"\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		for _, override := range overrides {
			if override.DefaultModel == "leaked" {
				t.Fatalf("a key after a malformed header was misattributed: %+v", override)
			}
		}
	})

	t.Run("repo_concurrency", func(t *testing.T) {
		limits, err := LoadRepoConcurrency(writeGateConfig(t, "[repos.\"owner/repo\"]\nmax_parallel = 2\n[repos.\"owner/repo\"\nmax_parallel = 99\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		for _, limit := range limits {
			if limit.MaxParallel == 99 {
				t.Fatalf("max_parallel = 99 after a malformed header was misattributed: %+v", limit)
			}
		}
	})
}
