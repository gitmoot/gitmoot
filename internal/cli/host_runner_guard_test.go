package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type hostRunnerAllowance struct {
	execRunners int
	rawCommands int
	reason      string
}

type hostRunnerSite struct {
	key      string
	kind     string
	position token.Position
}

var hostRunnerAllowlist = map[string]hostRunnerAllowance{
	"internal/cli/activepieces.go:ensureActivepiecesBridge": {
		rawCommands: 1,
		reason:      "operator command starts the detached local Activepieces bridge process",
	},
	"internal/cli/agent.go:<package>": {
		execRunners: 1,
		reason:      "operator-side agent doctor uses a replaceable local-runtime probe seam",
	},
	"internal/cli/agent_dispatch.go:localDispatchJobRunner": {
		execRunners: 1,
		reason:      "operator and test helper fallback; admitted jobs install the resolved backend runner before dispatch",
	},
	"internal/cli/agent_dispatch.go:resolveLocalImplementBase": {
		execRunners: 1,
		reason:      "operator-side base resolution inspects the managed host checkout before backend provisioning",
	},
	"internal/cli/ask_diff_precleanup.go:askReviewDiffPrecleanupHook": {
		execRunners: 1,
		reason:      "host-side post-delivery hook captures an already-collected read-only worktree before cleanup",
	},
	"internal/cli/ask_diff_precleanup.go:captureReadOnlyWorktreeDiff": {
		execRunners: 1,
		reason:      "host-side post-delivery diff capture delegates to the injectable runner implementation",
	},
	"internal/cli/daemon_checkout.go:defaultCheckout": {
		execRunners: 1,
		reason:      "legacy host-mode checkout wrapper; backend jobs call defaultCheckoutForRunner with the instance runner",
	},
	"internal/cli/daemon_checkout.go:resolveJobCheckout": {
		execRunners: 1,
		reason:      "legacy host-mode checkout resolver; backend jobs use resolveJobCheckoutForRunner",
	},
	"internal/cli/daemon_checkout.go:validateTargetCheckout": {
		execRunners: 1,
		reason:      "legacy host-mode checkout validator; backend jobs use validateTargetCheckoutForRunner",
	},
	"internal/cli/daemon_lifecycle.go:preflightDaemonRepoCheckout": {
		execRunners: 1,
		reason:      "daemon preflight inspects the registered host checkout before any job backend is provisioned",
	},
	"internal/cli/daemon_lifecycle.go:processArgv": {
		rawCommands: 1,
		reason:      "daemon operator lifecycle falls back to host ps when procfs argv is unavailable",
	},
	"internal/cli/daemon_lifecycle.go:runDaemonRun": {
		execRunners: 1,
		reason:      "host-mode daemon merge gate is the compatibility default; backend jobs inject their instance runner",
	},
	"internal/cli/daemon_lifecycle.go:startDaemonChild": {
		rawCommands: 1,
		reason:      "daemon operator lifecycle starts the detached gitmoot daemon process on the host",
	},
	"internal/cli/daemon_worker.go:defaultWorkflow": {
		execRunners: 1,
		reason:      "legacy host-mode workflow factory; backend jobs use defaultWorkflowForRunner with the instance runner",
	},
	"internal/cli/daemon_workflow.go:daemonWorkflowEngine": {
		execRunners: 1,
		reason:      "legacy host-mode engine wrapper; backend jobs call daemonWorkflowEngineForRunner",
	},
	"internal/cli/daemon_workflow.go:implementationFinalizationTargetFor": {
		execRunners: 1,
		reason:      "host-mode finalization wrapper delegates checkout inspection to implementationFinalizationTargetForRunner",
	},
	"internal/cli/daemon_workflow.go:newDaemonPolicyMergeGate": {
		execRunners: 1,
		reason:      "host-mode policy gate wrapper delegates to newDaemonPolicyMergeGateForRunner",
	},
	"internal/cli/daemon_workflow.go:newHostDaemonImplementationFinalizer": {
		execRunners: 1,
		reason:      "explicit host finalizer is used only after backend output has been collected into the managed checkout",
	},
	"internal/cli/daemon_workflow.go:newHostDaemonMergeGate": {
		execRunners: 1,
		reason:      "explicit host merge gate evaluates and merges the managed checkout after job execution",
	},
	"internal/cli/daemon_workflow.go:refreshDaemonJobPayload": {
		execRunners: 1,
		reason:      "host-mode payload refresh wrapper delegates to refreshDaemonJobPayloadForRunner",
	},
	"internal/cli/dashboard_config.go:editAgentPromptCmd": {
		rawCommands: 1,
		reason:      "interactive dashboard launches the operator's configured editor",
	},
	"internal/cli/dashboard_config.go:editConfigCmd": {
		rawCommands: 1,
		reason:      "interactive dashboard launches the operator's configured editor",
	},
	"internal/cli/dashboard_web.go:execBinaryBuild": {
		rawCommands: 2,
		reason:      "dashboard probes the deployed gitmoot binary version on the host",
	},
	"internal/cli/fix_worktree.go:allocateFixWorktree": {
		execRunners: 1,
		reason:      "host-side allocator creates the managed fix worktree before execution",
	},
	"internal/cli/job_resume_worktree.go:resumeSelfDirtyWorktree": {
		execRunners: 1,
		reason:      "host-side recovery inspects the managed checkout before a backend instance can resume",
	},
	"internal/cli/org.go:<package>": {
		execRunners: 2,
		reason:      "operator org doctor and seat-management commands use replaceable host runner seams",
	},
	"internal/cli/pipeline_enqueue.go:allocatePipelineServiceShellWorktree": {
		execRunners: 1,
		reason:      "host-side allocator prepares a managed service-shell worktree before execution",
	},
	"internal/cli/pipeline_enqueue.go:allocatePipelineShellStageReadOnlyWorktree": {
		execRunners: 1,
		reason:      "host-side allocator prepares a managed read-only shell-stage worktree before execution",
	},
	"internal/cli/pipeline_enqueue.go:allocatePipelineStageReadOnlyWorktree": {
		execRunners: 1,
		reason:      "host-side allocator prepares a managed read-only stage worktree before execution",
	},
	"internal/cli/pipeline_enqueue.go:allocatePipelineStageWritableWorktree": {
		execRunners: 1,
		reason:      "host-side allocator prepares a managed writable stage worktree before execution",
	},
	"internal/cli/plugin.go:<package>": {
		execRunners: 2,
		reason:      "operator plugin install and doctor commands use replaceable host runner seams",
	},
	"internal/cli/repo.go:resolveRegisteredRepoRecord": {
		execRunners: 1,
		reason:      "operator repo registration inspects and repairs the selected host checkout",
	},
	"internal/cli/skillopt_gate.go:<package>": {
		execRunners: 1,
		reason:      "operator skillopt gate replays a local candidate command through a replaceable seam",
	},
	"internal/cli/skillopt_optimize.go:<package>": {
		execRunners: 1,
		reason:      "operator skillopt optimization invokes the configured local optimizer through a replaceable seam",
	},
	"internal/cli/skillopt_publish.go:<package>": {
		execRunners: 1,
		reason:      "operator skillopt publish preview invokes local tooling through a replaceable seam",
	},
	"internal/cli/skillopt_trainrun_tui.go:<package>": {
		rawCommands: 1,
		reason:      "interactive skillopt TUI starts a detached local train process",
	},
	"internal/cli/trace_harvest_daemon.go:daemonDeterministicCheckerDispatcher": {
		execRunners: 1,
		reason:      "post-delivery deterministic checks run against the collected managed checkout on the host",
	},
	"internal/cli/update.go:runDaemonRestartFromExecutable": {
		rawCommands: 1,
		reason:      "operator update lifecycle restarts the local daemon executable",
	},
	"internal/cli/workflow.go:recoverTaskImplementation": {
		execRunners: 1,
		reason:      "operator recovery command inspects the managed host task worktree",
	},
	"internal/cli/workflow.go:taskRecoverBaseHead": {
		execRunners: 1,
		reason:      "operator recovery command resolves the managed checkout base before redispatch",
	},
	"internal/cli/workflow.go:taskWorktreeDirty": {
		execRunners: 1,
		reason:      "operator recovery command checks whether the managed host task worktree is dirty",
	},
	"internal/workflow/result_observation.go:changedWorktreeFiles": {
		execRunners: 1,
		reason:      "result observation inspects the host worktree only after backend output has been collected",
	},
}

func TestJobPathsDoNotHardcodeHostRunners(t *testing.T) {
	repoRoot := hostRunnerGuardRepoRoot()
	sites := collectHardcodedHostRunnerSites(t, repoRoot)
	var execRunnerSites, rawCommandSites int

	byKey := make(map[string][]hostRunnerSite)
	for _, site := range sites {
		byKey[site.key] = append(byKey[site.key], site)
		if site.kind == "subprocess.ExecRunner{}" {
			execRunnerSites++
		} else {
			rawCommandSites++
		}
	}

	for key, allowance := range hostRunnerAllowlist {
		if strings.TrimSpace(allowance.reason) == "" {
			t.Errorf("host runner allowlist entry %q must include a reason", key)
		}
		actual := byKey[key]
		if len(actual) == 0 {
			t.Errorf("stale host runner allowlist entry %q (%s)", key, allowance.reason)
		}
	}

	for key, actual := range byKey {
		allowance, ok := hostRunnerAllowlist[key]
		if !ok {
			for _, site := range actual {
				reportUnallowlistedHostRunner(t, site)
			}
			continue
		}
		var execRunners, rawCommands int
		for _, site := range actual {
			switch site.kind {
			case "subprocess.ExecRunner{}":
				execRunners++
			case "exec.Command":
				rawCommands++
			}
		}
		if execRunners != allowance.execRunners || rawCommands != allowance.rawCommands {
			for _, site := range actual {
				t.Errorf("%s: host runner count changed in %s: got ExecRunner=%d raw-command=%d, allowlist expects ExecRunner=%d raw-command=%d (%s); pass the job's resolved runner, or update hostRunnerAllowlist with a reason",
					positionString(repoRoot, site.position), key, execRunners, rawCommands, allowance.execRunners, allowance.rawCommands, allowance.reason)
			}
		}
	}

	t.Logf("classified %d hardcoded host-runner sites (ExecRunner=%d raw-command=%d) across %d allowlisted file:function entries",
		len(sites), execRunnerSites, rawCommandSites, len(hostRunnerAllowlist))
}

// collectHardcodedHostRunnerSites is intentionally a syntactic guard. It does
// not follow runners returned by helpers, fields assigned elsewhere, commands
// constructed in another package and passed in, reflection, generated files,
// or os/exec and subprocess imports reached through aliases or dot imports.
// Production files in all three directories are scanned by default, so a new
// internal/cli file is guarded unless each detected file:function site earns an
// explicit allowance with a reason.
func collectHardcodedHostRunnerSites(t *testing.T, repoRoot string) []hostRunnerSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []hostRunnerSite
	for _, dir := range []string{"internal/cli", "internal/workflow", "internal/daemon"} {
		root := filepath.Join(repoRoot, filepath.FromSlash(dir))
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			if ast.IsGenerated(parsed) {
				return nil
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			file := filepath.ToSlash(rel)
			for _, decl := range parsed.Decls {
				function := "<package>"
				node := ast.Node(decl)
				if fn, ok := decl.(*ast.FuncDecl); ok {
					function = fn.Name.Name
					node = fn.Body
				}
				if node == nil {
					continue
				}
				key := file + ":" + function
				ast.Inspect(node, func(node ast.Node) bool {
					switch node := node.(type) {
					case *ast.CompositeLit:
						if isSelector(node.Type, "subprocess", "ExecRunner") {
							sites = append(sites, hostRunnerSite{key: key, kind: "subprocess.ExecRunner{}", position: fset.Position(node.Pos())})
						}
					case *ast.CallExpr:
						if isSelector(node.Fun, "exec", "Command") || isSelector(node.Fun, "exec", "CommandContext") {
							sites = append(sites, hostRunnerSite{key: key, kind: "exec.Command", position: fset.Position(node.Pos())})
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s for hardcoded host runners: %v", dir, err)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].position.Filename == sites[j].position.Filename {
			return sites[i].position.Offset < sites[j].position.Offset
		}
		return sites[i].position.Filename < sites[j].position.Filename
	})
	return sites
}

func isSelector(node ast.Expr, pkg, name string) bool {
	selector, ok := node.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func reportUnallowlistedHostRunner(t *testing.T, site hostRunnerSite) {
	t.Helper()
	t.Errorf("%s: hardcoded %s in %s; pass the job's resolved runner, or add a hostRunnerAllowlist entry with a reason",
		positionString(hostRunnerGuardRepoRoot(), site.position), site.kind, site.key)
}

func hostRunnerGuardRepoRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate host runner guard source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func positionString(repoRoot string, position token.Position) string {
	rel, err := filepath.Rel(repoRoot, position.Filename)
	if err != nil {
		return fmt.Sprintf("%s:%d", filepath.ToSlash(position.Filename), position.Line)
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), position.Line)
}
