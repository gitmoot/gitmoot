package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/org"
)

type orgSeatFixtureProvider struct {
	roles []config.OrgRole
	panes *[]org.LivePane
}

func (p orgSeatFixtureProvider) Snapshot(context.Context) (org.Snapshot, error) {
	panes := append([]org.LivePane(nil), (*p.panes)...)
	bindings := make(map[string]org.PaneBinding, len(p.roles))
	for _, role := range p.roles {
		binding := strings.TrimSpace(role.Pane)
		if binding == "" {
			bindings[role.Name] = org.PaneBinding{Detail: "Herdr pane binding is unset"}
			continue
		}
		var matches []string
		for _, pane := range panes {
			if pane.PaneID == binding && pane.PaneID != "" {
				matches = []string{pane.PaneID}
				break
			}
			if pane.PaneID != "" && pane.Label == binding {
				matches = append(matches, pane.PaneID)
			}
		}
		if len(matches) == 1 {
			bindings[role.Name] = org.PaneBinding{PaneID: matches[0]}
		} else if len(matches) > 1 {
			bindings[role.Name] = org.PaneBinding{Detail: `multiple Herdr panes labeled "` + binding + `"`}
		} else {
			bindings[role.Name] = org.PaneBinding{Detail: `no Herdr pane bound as "` + binding + `"`}
		}
	}
	return org.Snapshot{
		PaneBindings: bindings,
		Panes:        panes,
	}, nil
}

func (orgSeatFixtureProvider) Recycle(context.Context, org.RecycleRequest) error { return nil }

func TestOrgHelpListsSeatPolicyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("org help code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	const want = "org seat add NAME [--pane ID_OR_LABEL] [--parent ROLE] [--scope REPO,...] [--merge-rule owner|self|none] [--home DIR]"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("org help missing %q:\n%s", want, stdout.String())
	}
}

func TestOrgSeatAddOwnerBootstrapRemainsRootWithMergeAuthority(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	panes := []org.LivePane{{PaneID: "w1:p1", Label: "Owner"}}
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "owner", "--pane", "Owner", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("owner seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := cfg.Role("owner")
	if !ok || owner.Parent != "" || len(owner.Scope) != 1 || owner.Scope[0] != "*" || owner.MergeRule != "owner" || owner.Pane != "w1:p1" {
		t.Fatalf("owner role = %+v, present=%t", owner, ok)
	}
}

func TestOrgSeatAddEndsGreenOnRealityValidation(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--pane", "Worker", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		`pane w1:p2 claimed label="Worker"`,
		`role worker created pane="w1:p2"`,
		"route org-seat-worker-blocked created",
		"route org-seat-worker-directive created",
		"route org-seat-worker-escalation created",
		"route org-seat-worker-fact created",
		"route org-seat-worker-reply created",
		"ok role worker pane=w1:p2 enabled_routes=5",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("seat add output missing %q:\n%s", want, stdout.String())
		}
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.Role("worker")
	if !ok || worker.Pane != "w1:p2" || worker.Parent != "owner" || len(worker.Scope) != 1 || worker.Scope[0] != "*" || worker.MergeRule != "" {
		t.Fatalf("worker role = %+v, present=%t", worker, ok)
	}
	rules := listOrgSeatTestRules(t, paths)
	if len(rules) != 6 {
		t.Fatalf("event rules = %+v, want owner route plus five worker routes", rules)
	}

	// Re-running add is the repair path and must not duplicate owned routes.
	stdout.Reset()
	stderr.Reset()
	code = runOrg([]string{"seat", "add", "worker", "--pane", "Worker", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "route org-seat-worker-reply existed") ||
		!strings.Contains(stdout.String(), "ok role worker pane=w1:p2 enabled_routes=5") {
		t.Fatalf("second seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 6 {
		t.Fatalf("event rules after repair = %d, want 6", got)
	}
}

func TestOrgSeatAddAcceptsLiteralPaneID(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--pane", "w1:p2", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.Role("worker")
	if !ok || worker.Pane != "w1:p2" {
		t.Fatalf("worker role = %+v, present=%t", worker, ok)
	}
	if !strings.Contains(stdout.String(), `pane w1:p2 claimed label="Worker"`) {
		t.Fatalf("seat add output did not retain the live label as context:\n%s", stdout.String())
	}
}

func TestOrgSeatBindingSurvivesLabelRename(t *testing.T) {
	home, _, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"seat", "add", "worker", "--pane", "Worker", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	panes[1].Label = "Renamed Worker"
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"validate", "--home", home}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "ok 2 roles, 2 live panes, 6 enabled routes") {
		t.Fatalf("validate after rename code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgSeatAddCreatesUnboundThenBinds(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `role worker created pane=""`) ||
		!strings.Contains(stdout.String(), "role worker is unbound") {
		t.Fatalf("unbound seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.Role("worker")
	if !ok || worker.Pane != "" {
		t.Fatalf("unbound worker role = %+v, present=%t", worker, ok)
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 6 {
		t.Fatalf("routes after unbound creation = %d, want 6", got)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"validate", "--home", home}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "role worker pane unresolved: Herdr pane binding is unset") {
		t.Fatalf("validate unbound seat code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runOrg([]string{"seat", "add", "worker", "--pane", "w1:p2", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `role worker repaired pane="w1:p2"`) {
		t.Fatalf("bind existing seat code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err = config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok = cfg.Role("worker")
	if !ok || worker.Pane != "w1:p2" {
		t.Fatalf("bound worker role = %+v, present=%t", worker, ok)
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 6 {
		t.Fatalf("routes after bind = %d, want 6", got)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"validate", "--home", home}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "ok 2 roles, 2 live panes, 6 enabled routes") {
		t.Fatalf("validate bound seat code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgSeatAddRejectsExplicitEmptyPane(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--pane=", "--home", home}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--pane must not be empty") {
		t.Fatalf("empty pane add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); ok {
		t.Fatal("explicit empty pane created worker")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 1 {
		t.Fatalf("explicit empty pane changed routes: %d", got)
	}
}

func TestOrgSeatAddSucceedsWithAnotherUnboundRole(t *testing.T) {
	home, _, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"seat", "add", "worker", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("unbound worker add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code := runOrg([]string{"seat", "add", "other", "--pane", "w1:p2", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "ok role other pane=w1:p2 enabled_routes=5") {
		t.Fatalf("bound other add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"validate", "--home", home}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "role worker pane unresolved: Herdr pane binding is unset") {
		t.Fatalf("global validate code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgSeatAddRejectsUnknownPaneReference(t *testing.T) {
	for _, reference := range []string{"missing-label", "w9:p9"} {
		t.Run(reference, func(t *testing.T) {
			home, paths, panes := setupOrgSeatTestHome(t)
			withOrgSeatFixtureProvider(t, &panes)

			var stdout, stderr bytes.Buffer
			code := runOrg([]string{"seat", "add", "worker", "--pane", reference, "--home", home}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), `no live Herdr pane has id or exact label "`+reference+`"`) {
				t.Fatalf("unknown pane add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
			}
			cfg, err := config.LoadOrg(paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Role("worker"); ok {
				t.Fatal("unknown pane reference created worker")
			}
		})
	}
}

func TestOrgSeatAddInheritsActingRoleWithoutMergeAuthority(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	addOrgSeatCoordinator(t, paths, "none")
	panes = append(panes, org.LivePane{PaneID: "w1:p3", Label: "Coordinator"})
	withOrgSeatFixtureProvider(t, &panes)
	t.Setenv("GITMOOT_ORG_ROLE", "coordinator")

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--pane", "Worker", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.Role("worker")
	if !ok || worker.Pane != "w1:p2" || worker.Parent != "coordinator" || len(worker.Scope) != 1 || worker.Scope[0] != "gitmoot/*" || worker.MergeRule != "" {
		t.Fatalf("worker role = %+v, present=%t", worker, ok)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runOrg([]string{"show", "--home", home}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "worker\tparent=coordinator\tscope=gitmoot/*\tmerge_rule=none") {
		t.Fatalf("org show code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestOrgSeatAddAcceptsBoundedOverrides(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	addOrgSeatCoordinator(t, paths, "self")
	panes = append(panes, org.LivePane{PaneID: "w1:p3", Label: "Coordinator"})
	withOrgSeatFixtureProvider(t, &panes)
	t.Setenv("GITMOOT_ORG_ROLE", "coordinator")

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{
		"seat", "add", "worker", "--pane", "Worker", "--home", home,
		"--parent", "owner", "--scope", "gitmoot/repo", "--merge-rule", "self",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("seat add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.Role("worker")
	if !ok || worker.Pane != "w1:p2" || worker.Parent != "owner" || len(worker.Scope) != 1 || worker.Scope[0] != "gitmoot/repo" || worker.MergeRule != "self" {
		t.Fatalf("worker role = %+v, present=%t", worker, ok)
	}
}

func TestOrgSeatAddRejectsUnsafeHierarchyAndAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		mergeRule string
		args      []string
		want      string
	}{
		{name: "unknown parent", args: []string{"--parent", "missing"}, want: `parent role "missing" is not declared`},
		{name: "parent cycle", args: []string{"--parent", "worker"}, want: `would create a cycle for "worker"`},
		{name: "scope widening", args: []string{"--parent", "owner", "--scope", "*"}, want: `exceeds acting role "coordinator" scope`},
		{name: "merge escalation", args: []string{"--parent", "owner", "--merge-rule", "self"}, want: `exceeds acting role "coordinator" authority`},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, paths, panes := setupOrgSeatTestHome(t)
			addOrgSeatCoordinator(t, paths, test.mergeRule)
			panes = append(panes, org.LivePane{PaneID: "w1:p3", Label: "Coordinator"})
			withOrgSeatFixtureProvider(t, &panes)
			t.Setenv("GITMOOT_ORG_ROLE", "coordinator")

			args := []string{"seat", "add", "worker", "--pane", "Worker", "--home", home}
			args = append(args, test.args...)
			var stdout, stderr bytes.Buffer
			code := runOrg(args, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("seat add code=%d out=%q err=%q, want error containing %q", code, stdout.String(), stderr.String(), test.want)
			}
			cfg, err := config.LoadOrg(paths)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := cfg.Role("worker"); ok {
				t.Fatal("rejected seat add created worker")
			}
		})
	}
}

func TestOrgSeatAddRejectsDuplicatePaneLabels(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	panes = append(panes, org.LivePane{PaneID: "w1:p3", Label: "Worker"})
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "add", "worker", "--pane", "Worker", "--home", home}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "2 live Herdr panes have label") ||
		!strings.Contains(stderr.String(), "refusing ambiguous adoption") {
		t.Fatalf("duplicate-label add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); ok {
		t.Fatal("duplicate-label mutant selected one pane and created worker")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 1 {
		t.Fatalf("duplicate-label add wrote routes: %d", got)
	}
}

func TestOrgSeatRemoveRefusesUnshippedBranchWork(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	appendOrgSeatWorker(t, paths)
	addOrgSeatWorkerRoutes(t, paths)

	repo := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.name", "Seat Test")
	runGit(t, repo, "config", "user.email", "seat@example.test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, t.TempDir(), "init", "--bare", remote)
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "checkout", "-b", "worker-unshipped")
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("not merged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "work.txt")
	runGit(t, repo, "commit", "-m", "unshipped")
	panes[1].CWD = repo
	panes[1].ForegroundCWD = repo
	withOrgSeatFixtureProvider(t, &panes)
	closeCalls := 0
	originalClose := orgSeatClosePane
	orgSeatClosePane = func(context.Context, string) error {
		closeCalls++
		return nil
	}
	t.Cleanup(func() { orgSeatClosePane = originalClose })

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "rm", "worker", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "branch worker-unshipped has commits not merged into origin/main") {
		t.Fatalf("seat rm did not refuse unshipped branch: code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	if closeCalls != 0 {
		t.Fatalf("pane close calls = %d, want 0 after branch refusal", closeCalls)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); !ok {
		t.Fatal("branch-check mutant removed worker role")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 6 {
		t.Fatalf("branch refusal changed routes: %d", got)
	}
}

func TestOrgSeatBranchCheckFailsClosedWithoutGitCheckout(t *testing.T) {
	err := checkOrgSeatBranches(context.Background(), org.LivePane{
		PaneID:        "w1:p2",
		CWD:           t.TempDir(),
		ForegroundCWD: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "reports no Git checkout; branch safety is unverified") {
		t.Fatalf("branch check error = %v, want unverified refusal", err)
	}
}

func TestOrgSeatRemoveUnboundRoleWithoutClosingPane(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	withOrgSeatFixtureProvider(t, &panes)

	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"seat", "add", "worker", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("unbound worker add code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	originalClose := orgSeatClosePane
	orgSeatClosePane = func(context.Context, string) error {
		t.Fatal("unbound removal attempted to close a pane")
		return nil
	}
	t.Cleanup(func() { orgSeatClosePane = originalClose })

	stdout.Reset()
	stderr.Reset()
	code := runOrg([]string{"seat", "rm", "worker", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "no pane closed") ||
		!strings.Contains(stdout.String(), "ok role worker absent owned_routes=0") {
		t.Fatalf("unbound seat rm code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); ok {
		t.Fatal("unbound worker remains after removal")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 1 {
		t.Fatalf("routes after unbound removal = %d, want 1", got)
	}
}

func TestOrgSeatRemoveStalePaneIDWithoutClosingPane(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	edit, _, err := config.UpsertOrgSeatRole(paths, config.OrgRole{
		Name: "worker", Parent: "owner", Scope: []string{"*"}, Pane: "w1:p9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !edit.Changed() {
		t.Fatal("stale worker config edit did not change config")
	}
	addOrgSeatWorkerRoutes(t, paths)
	withOrgSeatFixtureProvider(t, &panes)
	originalBranchCheck := orgSeatBranchCheck
	orgSeatBranchCheck = func(context.Context, org.LivePane) error {
		t.Fatal("stale removal attempted a branch check")
		return nil
	}
	t.Cleanup(func() { orgSeatBranchCheck = originalBranchCheck })
	originalClose := orgSeatClosePane
	orgSeatClosePane = func(context.Context, string) error {
		t.Fatal("stale removal attempted to close a pane")
		return nil
	}
	t.Cleanup(func() { orgSeatClosePane = originalClose })

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "rm", "worker", "--home", home}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "no pane closed") ||
		!strings.Contains(stdout.String(), "ok role worker absent owned_routes=0") {
		t.Fatalf("stale seat rm code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); ok {
		t.Fatal("stale worker remains after removal")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 1 {
		t.Fatalf("routes after stale removal = %d, want 1", got)
	}
}

func TestOrgSeatRemoveClosesPaneAndEndsWithValidation(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	appendOrgSeatWorker(t, paths)
	addOrgSeatWorkerRoutes(t, paths)
	withOrgSeatFixtureProvider(t, &panes)
	originalBranchCheck := orgSeatBranchCheck
	orgSeatBranchCheck = func(context.Context, org.LivePane) error { return nil }
	t.Cleanup(func() { orgSeatBranchCheck = originalBranchCheck })
	originalClose := orgSeatClosePane
	orgSeatClosePane = func(_ context.Context, paneID string) error {
		for i, pane := range panes {
			if pane.PaneID == paneID {
				panes = append(panes[:i], panes[i+1:]...)
				return nil
			}
		}
		return errors.New("pane not found")
	}
	t.Cleanup(func() { orgSeatClosePane = originalClose })

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "rm", "worker", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("seat rm code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"pane w1:p2 closed",
		"role worker removed with 5 wake routes",
		"ok role worker absent owned_routes=0",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("seat rm output missing %q:\n%s", want, stdout.String())
		}
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); ok {
		t.Fatal("worker role remains after seat rm")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 1 {
		t.Fatalf("routes after seat rm = %d, want 1", got)
	}
}

func TestOrgSeatRemoveRestoresOwnedStateWhenPaneCloseFails(t *testing.T) {
	home, paths, panes := setupOrgSeatTestHome(t)
	appendOrgSeatWorker(t, paths)
	addOrgSeatWorkerRoutes(t, paths)
	withOrgSeatFixtureProvider(t, &panes)
	originalBranchCheck := orgSeatBranchCheck
	orgSeatBranchCheck = func(context.Context, org.LivePane) error { return nil }
	t.Cleanup(func() { orgSeatBranchCheck = originalBranchCheck })
	originalClose := orgSeatClosePane
	orgSeatClosePane = func(context.Context, string) error { return errors.New("close refused") }
	t.Cleanup(func() { orgSeatClosePane = originalClose })

	var stdout, stderr bytes.Buffer
	code := runOrg([]string{"seat", "rm", "worker", "--home", home}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "close refused") {
		t.Fatalf("seat rm close failure code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Role("worker"); !ok {
		t.Fatal("worker role was not restored after pane-close failure")
	}
	if got := len(listOrgSeatTestRules(t, paths)); got != 6 {
		t.Fatalf("routes after pane-close rollback = %d, want 6", got)
	}
}

func setupOrgSeatTestHome(t *testing.T) (string, config.Paths, []org.LivePane) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
pane = "Owner"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "owner-reply", OnKind: "reply", WakeRole: "owner",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return home, paths, []org.LivePane{
		{PaneID: "w1:p1", Label: "Owner"},
		{PaneID: "w1:p2", Label: "Worker"},
	}
}

func withOrgSeatFixtureProvider(t *testing.T, panes *[]org.LivePane) {
	t.Helper()
	original := newOrgProvider
	newOrgProvider = func(roles []config.OrgRole) org.Provider {
		return orgSeatFixtureProvider{roles: append([]config.OrgRole(nil), roles...), panes: panes}
	}
	t.Cleanup(func() { newOrgProvider = original })
}

func addOrgSeatCoordinator(t *testing.T, paths config.Paths, mergeRule string) {
	t.Helper()
	edit, _, err := config.UpsertOrgSeatRole(paths, config.OrgRole{
		Name: "coordinator", Parent: "owner", Scope: []string{"gitmoot/*"},
		MergeRule: mergeRule, Pane: "Coordinator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !edit.Changed() {
		t.Fatal("coordinator config edit did not change config")
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "coordinator-reply", OnKind: "reply", WakeRole: "coordinator",
		Scope: db.EventRuleScopeAddressed, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func appendOrgSeatWorker(t *testing.T, paths config.Paths) {
	t.Helper()
	edit, _, err := config.UpsertOrgSeatRole(paths, config.OrgRole{
		Name: "worker", Parent: "owner", Scope: []string{"*"},
		MergeRule: "owner", Pane: "Worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !edit.Changed() {
		t.Fatal("worker config edit did not change config")
	}
}

func addOrgSeatWorkerRoutes(t *testing.T, paths config.Paths) {
	t.Helper()
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRules(context.Background(), orgSeatDefaultRules("worker")); err != nil {
		t.Fatal(err)
	}
}

func listOrgSeatTestRules(t *testing.T, paths config.Paths) []db.EventRule {
	t.Helper()
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rules
}
