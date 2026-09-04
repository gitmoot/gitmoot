package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sectionHeaderCoverage is the SINGLE authority for which sectionHeader call
// sites exist and which of them are pinned. Everything else in this package
// points here instead of restating a number: section.go's doc, and the COVERAGE
// block above TestMalformedHeaderRoutingPerCallSite.
//
// WHY THIS IS A TEST RATHER THAN A COMMENT (#1795 review P3-2). Consolidating to
// one prose derivation made the count auditable in one read but not
// self-checking, so it could go stale silently three ways: a 27th call site left
// the prose wrong with the suite green; the hand-maintained file:line citations
// misaimed on any edit above a cited line, while the behavioural subtests kept
// passing because they pin BEHAVIOUR and never line numbers; and the pointer
// named a test function with nothing tying the two together. This test turns all
// three into a failing test.
//
// CITATIONS ARE BY SYMBOL, NEVER BY LINE, deliberately: enforcing line numbers
// would make an ordinary edit above a call site fail, which is a guard that
// punishes unrelated work. file::function survives reordering and reformatting
// and still names one site unambiguously, because no function in this package
// calls sectionHeader twice.
var (
	// pinnedSectionHeaderSites are MUTATION-PROVEN: reverting the site to the
	// pre-#1759 two-bracket form fails a named subtest. 4 guard loaders by
	// refusal, 8 by routing, 4 in TestMalformedHeaderRoutingRemainingLoaders.
	pinnedSectionHeaderSites = []string{
		"admission.go::LoadAdmissionPolicy",
		"credentials.go::LoadCredentialsConfig",
		"daemon_runtime.go::LoadDaemonRuntimeConfig",
		"github_limiter.go::LoadGitHubLimiterPolicy",
		"heartbeats.go::LoadHeartbeats",
		"memory.go::LoadMemorySettings",
		"merge_gate.go::LoadMergeGatePolicy",
		"orchestrate.go::LoadReviewConfig",
		"parallel_sessions.go::LoadParallelSessionPolicy",
		"remote_exec.go::LoadRemoteExecConfig",
		"repo_concurrency.go::LoadRepoConcurrency",
		"require_workflow.go::LoadRequireWorkflow",
		"result_checks.go::LoadResultChecksMode",
		"router.go::LoadRouterSettings",
		"runtime_registry.go::LoadRuntimeOverrides",
		"transcripts.go::LoadTranscriptsConfig",
	}

	// unpinnedSectionHeaderSites key off prefixes, repo-scoped names or no fixed
	// section string, so each needs its own observable field. A revert at one of
	// these would NOT fail this package's suite today. Named so the next reader
	// does not re-derive them, and enforced so the list cannot rot.
	unpinnedSectionHeaderSites = []string{
		"edit_compat.go::configSectionAtParseError",
		"github_remote.go::loadGitHubRemote",
		"implement_base.go::LoadImplementBase",
		"orchestrate.go::LoadEventsPolicy",
		"orchestrate.go::LoadOrchestratePolicy",
		"org.go::parseOrgContent",
		"stale_tasks.go::LoadDelegationWorktreeTTL",
		"stale_tasks.go::LoadPlannedTaskTTL",
		"stale_tasks.go::LoadStaleTaskTTL",
		"workflow_lifecycle.go::LoadWorkflowLifecycle",
	}
)

// discoverSectionHeaderCallSites parses this package's PRODUCTION files and
// returns every sectionHeader call site as "file::enclosingFunc", plus how many
// files it parsed. Test files are excluded: the count documents production
// reach, and a fixture calling sectionHeader is not a call site to pin.
func discoverSectionHeaderCallSites(t *testing.T) (sites []string, filesParsed int, distinctFiles int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			// Fail closed: an unparseable file must not silently shrink the count.
			t.Fatalf("parse %s: %v", name, err)
		}
		filesParsed++
		var enclosing string
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				enclosing = typed.Name.Name
			case *ast.CallExpr:
				ident, ok := typed.Fun.(*ast.Ident)
				if ok && ident.Name == "sectionHeader" {
					sites = append(sites, name+"::"+enclosing)
					seen[name] = true
				}
			}
			return true
		})
	}
	sort.Strings(sites)
	return sites, filesParsed, len(seen)
}

// TestSectionHeaderCoverageIsDerivedNotAsserted derives the call-site set from
// the AST and requires the pinned and unpinned lists to PARTITION it exactly.
// Add a 27th call site and this fails naming it, whichever file it lands in.
func TestSectionHeaderCoverageIsDerivedNotAsserted(t *testing.T) {
	sites, filesParsed, distinctFiles := discoverSectionHeaderCallSites(t)

	// Positive control, so a broken or empty derivation cannot pass quietly: the
	// walk must have parsed real files and found a site we know exists.
	if filesParsed == 0 {
		t.Fatal("parsed 0 production files: the derivation is broken, not the package")
	}
	if len(sites) == 0 {
		t.Fatal("found 0 sectionHeader call sites: the derivation is broken, not the package")
	}
	if !slicesContain(sites, "heartbeats.go::LoadHeartbeats") {
		t.Fatalf("control site heartbeats.go::LoadHeartbeats missing from %d discovered sites; the walk is not seeing real calls", len(sites))
	}

	classified := map[string]string{}
	for _, site := range pinnedSectionHeaderSites {
		classified[site] = "pinned"
	}
	for _, site := range unpinnedSectionHeaderSites {
		if existing, dup := classified[site]; dup {
			t.Fatalf("%s is listed as both %s and unpinned", site, existing)
		}
		classified[site] = "unpinned"
	}

	discovered := map[string]bool{}
	for _, site := range sites {
		discovered[site] = true
		if _, ok := classified[site]; !ok {
			t.Errorf("sectionHeader call site %s is not classified: add it to pinnedSectionHeaderSites (with a subtest that fails when the site is reverted) or to unpinnedSectionHeaderSites", site)
		}
	}
	for site := range classified {
		if !discovered[site] {
			t.Errorf("classified site %s no longer exists: remove it from its list", site)
		}
	}

	if want := len(pinnedSectionHeaderSites) + len(unpinnedSectionHeaderSites); len(sites) != want {
		t.Errorf("discovered %d sectionHeader call sites, classified %d: the lists and the package disagree", len(sites), want)
	}
	if distinctFiles != 22 {
		t.Errorf("sectionHeader call sites span %d files, want 22: update this expectation together with the lists above", distinctFiles)
	}
}

func slicesContain(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}
