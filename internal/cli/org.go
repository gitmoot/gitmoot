package cli

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// orgPolicyResolver accepts a raw --home, while the Root variant below accepts
// the already resolved <home>/.gitmoot path used by daemon engines.
func orgPolicyResolver(home string) func(string) workflow.OrgEnforcement {
	if strings.TrimSpace(home) == "" {
		paths, err := pathsFromFlag("")
		if err != nil {
			return func(string) workflow.OrgEnforcement { return workflow.OrgEnforcement{LoadErr: err} }
		}
		return orgPolicyResolverPaths(paths)
	}
	return orgPolicyResolverPaths(config.PathsForHome(home))
}

func orgPolicyResolverRoot(root string) func(string) workflow.OrgEnforcement {
	return orgPolicyResolverPaths(config.Paths{ConfigFile: configFileAtRoot(root)})
}

func orgPolicyResolverPaths(paths config.Paths) func(string) workflow.OrgEnforcement {
	return func(string) workflow.OrgEnforcement {
		cfg, err := config.LoadOrg(paths)
		if err != nil {
			return workflow.OrgEnforcement{LoadErr: err}
		}
		return workflow.OrgEnforcement{
			Enabled: cfg.Enabled(), Enforce: cfg.Enforce(),
			Role: func(name string) (workflow.OrgRole, bool) {
				r, ok := cfg.Role(name)
				return workflow.OrgRole{Name: r.Name, Parent: r.Parent, Scope: append([]string(nil), r.Scope...), MergeRule: r.MergeRule}, ok
			},
			ScopeMatches: config.ScopeMatches,
		}
	}
}

// preflightOrgScope prevents an org-blocked implement/task-run request from
// allocating a task worktree or branch lock before Mailbox.Enqueue can reject.
// Warn mode intentionally leaves the durable event to the enqueue chokepoint.
func preflightOrgScope(policy workflow.OrgEnforcement, repo, actingRole string, operatorOrigin bool) error {
	if !operatorOrigin {
		return nil
	}
	_, err := workflow.OrgScopeDecision(policy, actingRole, repo)
	return err
}

func fixedOrgPolicy(policy workflow.OrgEnforcement) func(string) workflow.OrgEnforcement {
	return func(string) workflow.OrgEnforcement { return policy }
}

var newOrgProvider = func(roles []config.OrgRole) org.Provider { return cockpit.NewHerdrOrgProvider(roles) }

var orgDoctorRunner subprocess.Runner = subprocess.ExecRunner{}

var orgSeatRunner subprocess.Runner = subprocess.ExecRunner{}

var orgSeatBranchCheck = checkOrgSeatBranches

var orgSeatClosePane = closeOrgSeatPane

var orgRecycleAdvisoryWriter io.Writer = os.Stderr

var orgRecycleOverdueEventWriter io.Writer = os.Stderr

var orgRecycleOverdueEventSink = enabledBlockedSinceEventSink

var orgRecycleOverdueEpisodeEmitter = emitRecycleOverdueEpisode

func runOrg(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgUsage(stdout)
		return 0
	}
	switch args[0] {
	case "validate", "show":
		return runOrgValidateOrShow(args, stdout, stderr)
	case "init":
		return runOrgInit(args[1:], stdout, stderr)
	case "brief":
		return runOrgBrief(args[1:], stdout, stderr)
	case "chart":
		return runOrgChart(args[1:], stdout, stderr)
	case "status":
		return runOrgStatus(args[1:], stdout, stderr)
	case "recycle":
		return runOrgRecycle(args[1:], stdout, stderr)
	case "escalate":
		return runOrgEscalate(args[1:], stdout, stderr)
	case "directive":
		return runOrgDirective(args[1:], stdout, stderr)
	case "await":
		return runOrgAwait(args[1:], stdout, stderr)
	case "events":
		return runOrgEvents(args[1:], stdout, stderr)
	case "seat":
		return runOrgSeat(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org command %q\n\n", args[0])
		printOrgUsage(stderr)
		return 2
	}
}

func printOrgUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org validate [--home PATH]")
	fmt.Fprintln(w, "  gitmoot org show [--home PATH]")
	fmt.Fprintln(w, "  gitmoot org init [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org brief --role NAME [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org chart [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org status [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org recycle ROLE --kind KIND --handoff NOTE [--pane ID] [--json] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org seat add NAME [--pane ID_OR_LABEL] [--parent ROLE] [--scope REPO,...] [--merge-rule owner|self|none] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org seat rm NAME [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org escalate --to ROLE --workflow LABEL [--org-role ROLE] [--repo OWNER/REPO] [--json] [--home DIR] \"QUESTION\"")
	fmt.Fprintln(w, "  gitmoot org escalate resolve NOTE_ID [--by ROLE] [--note ANSWER_NOTE_ID] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive send --to ROLE --workflow LABEL (--stdin | -F FILE | TEXT) [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive ack ID [--by ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive cancel ID [--by ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org await review --repo OWNER/REPO --pr NUMBER --head SHA --ttl DURATION [--role ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org await list [--role ROLE] [--state waiting|satisfied|expired] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org events rule add --on KIND [--match FILTER | --repo SUBSTRING] --wake ROLE [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org events rule list [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org events rule set-scope [--home DIR] ID observer|addressed")
	fmt.Fprintln(w, "  gitmoot org events rule rm [--home DIR] ID")
}

var orgSeatDefaultRouteKinds = []string{"blocked", "directive", "escalation", "fact", "reply"}

const orgSeatExternalTimeout = 10 * time.Second

func runOrgSeat(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgSeatUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runOrgSeatAdd(args[1:], stdout, stderr)
	case "rm":
		return runOrgSeatRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org seat command %q\n", args[0])
		printOrgSeatUsage(stderr)
		return 2
	}
}

func printOrgSeatUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org seat add NAME [--pane ID_OR_LABEL] [--parent ROLE] [--scope REPO,...] [--merge-rule owner|self|none] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org seat rm NAME [--home DIR]")
}

func runOrgSeatAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org seat add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	paneFlag := fs.String("pane", "", "live Herdr pane id or unique exact label to claim; omit to create an unbound role")
	parentFlag := fs.String("parent", "", "parent role (defaults to GITMOOT_ORG_ROLE, then owner)")
	scopeFlag := fs.String("scope", "", "comma-separated repository scope (defaults to the acting role's scope)")
	mergeRuleFlag := fs.String("merge-rule", "", "merge authority to grant explicitly: owner, self, or none")
	nameArg, flagArgs := leadingOrgSeatName(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if nameArg == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "org seat add requires exactly one NAME")
			return 2
		}
		nameArg = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org seat add requires exactly one NAME")
		return 2
	}
	name := strings.ToLower(strings.TrimSpace(nameArg))
	if !validOrgSeatName(name) {
		fmt.Fprintf(stderr, "org seat add: invalid role name %q; use lowercase letters, digits, and hyphens\n", nameArg)
		return 2
	}
	paneRef := strings.TrimSpace(*paneFlag)
	paneSet := hasFlag(flagArgs, "pane")
	if paneSet && paneRef == "" {
		fmt.Fprintln(stderr, "org seat add: --pane must not be empty; omit the flag to create an unbound role")
		return 2
	}
	parentSet := hasFlag(flagArgs, "parent")
	scopeSet := hasFlag(flagArgs, "scope")
	mergeRuleSet := hasFlag(flagArgs, "merge-rule")
	requestedScope, err := parseOrgSeatScope(*scopeFlag, scopeSet)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: %v\n", err)
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: resolve paths: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "org seat add: initialize home: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: load org registry: %v\n", err)
		return 1
	}
	if current, exists := cfg.Role(name); exists {
		var changedPolicyFlags []string
		if parentSet && strings.ToLower(strings.TrimSpace(*parentFlag)) != strings.ToLower(strings.TrimSpace(current.Parent)) {
			changedPolicyFlags = append(changedPolicyFlags, "--parent")
		}
		if scopeSet && !slices.Equal(requestedScope, current.Scope) {
			changedPolicyFlags = append(changedPolicyFlags, "--scope")
		}
		requestedMergeRule := strings.ToLower(strings.TrimSpace(*mergeRuleFlag))
		currentMergeRule := strings.ToLower(strings.TrimSpace(current.MergeRule))
		if requestedMergeRule == "" {
			requestedMergeRule = "none"
		}
		if currentMergeRule == "" {
			currentMergeRule = "none"
		}
		if mergeRuleSet && requestedMergeRule != currentMergeRule {
			changedPolicyFlags = append(changedPolicyFlags, "--merge-rule")
		}
		if len(changedPolicyFlags) != 0 {
			fmt.Fprintf(stderr, "org seat add: role %q already exists; %s differ from its existing policy\n", name, strings.Join(changedPolicyFlags, ", "))
			return 2
		}
	}
	ctx := context.Background()
	var snapshot org.Snapshot
	var pane org.LivePane
	if paneRef != "" {
		snapshotCtx, cancel := context.WithTimeout(ctx, orgSeatExternalTimeout)
		snapshot, err = orgProviderSnapshot(snapshotCtx, cfg)
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "org seat add: inspect live Herdr panes: %v\n", err)
			return 1
		}
		pane, err = uniqueOrgSeatPaneByReference(snapshot.Panes, paneRef)
		if err != nil {
			fmt.Fprintf(stderr, "org seat add: %v\n", err)
			return 2
		}
		for roleName, binding := range snapshot.PaneBindings {
			if roleName != name && strings.TrimSpace(binding.PaneID) == pane.PaneID {
				fmt.Fprintf(stderr, "org seat add: pane %s (%q) is already claimed by role %q\n", pane.PaneID, pane.Label, roleName)
				return 2
			}
		}
	}

	desired, roleExists, err := orgSeatDesiredRole(cfg, snapshot, name, paneRef, pane.PaneID, orgSeatRoleOptions{
		ActingRole: strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")),
		Parent:     *parentFlag,
		Scope:      requestedScope,
		ScopeSet:   scopeSet,
		MergeRule:  *mergeRuleFlag,
	})
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: %v\n", err)
		return 2
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: open store: %v\n", err)
		return 1
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		store.Close()
		fmt.Fprintf(stderr, "org seat add: list event rules: %v\n", err)
		return 1
	}
	expectedRules := orgSeatDefaultRules(name)
	missingRules, existingRuleIDs, err := missingOrgSeatRules(expectedRules, rules)
	if err != nil {
		store.Close()
		fmt.Fprintf(stderr, "org seat add: %v\n", err)
		return 2
	}
	rebindFromPane := ""
	if current, ok := cfg.Role(name); ok {
		currentPane := strings.TrimSpace(current.Pane)
		desiredPane := strings.TrimSpace(desired.Pane)
		if currentPane != "" && desiredPane != "" && currentPane != desiredPane {
			rebindFromPane = currentPane
		}
	}
	configEdit, roleCreated, err := config.UpsertOrgSeatRole(paths, desired, rebindFromPane)
	if err != nil {
		store.Close()
		fmt.Fprintf(stderr, "org seat add: write role: %v\n", err)
		return 1
	}
	if err := store.AddEventRules(ctx, missingRules); err != nil {
		restoreErr := configEdit.Restore()
		store.Close()
		fmt.Fprintf(stderr, "org seat add: add wake routes: %v\n", errors.Join(err, restoreErr))
		return 1
	}
	if err := store.Close(); err != nil {
		fmt.Fprintf(stderr, "org seat add: close store: %v\n", err)
		return 1
	}

	if paneRef != "" {
		fmt.Fprintf(stdout, "pane %s claimed label=%q\n", pane.PaneID, pane.Label)
	}
	roleStatus := "existed"
	if roleCreated || !roleExists {
		roleStatus = "created"
	} else if configEdit.Changed() {
		roleStatus = "repaired"
	}
	fmt.Fprintf(stdout, "role %s %s pane=%q\n", name, roleStatus, desired.Pane)
	for _, rule := range expectedRules {
		status := "created"
		if existingRuleIDs[rule.ID] {
			status = "existed"
		}
		fmt.Fprintf(stdout, "route %s %s on=%s wake=%s scope=%s\n", rule.ID, status, rule.OnKind, rule.WakeRole, rule.Scope)
	}
	fresh, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add: reload org registry: %v\n", err)
		return 1
	}
	if strings.TrimSpace(desired.Pane) == "" {
		fmt.Fprintf(stdout, "role %s is unbound; bind with gitmoot org seat add %s --pane ID_OR_LABEL\n", name, name)
		return runOrgSeatValidateAdded(ctx, paths, fresh, name, false, stdout, stderr)
	}
	return runOrgSeatValidateAdded(ctx, paths, fresh, name, true, stdout, stderr)
}

func runOrgSeatRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org seat rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	nameArg, flagArgs := leadingOrgSeatName(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if nameArg == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "org seat rm requires exactly one NAME")
			return 2
		}
		nameArg = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org seat rm requires exactly one NAME")
		return 2
	}
	name := strings.ToLower(strings.TrimSpace(nameArg))
	if !validOrgSeatName(name) {
		fmt.Fprintf(stderr, "org seat rm: invalid role name %q\n", nameArg)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm: load org registry: %v\n", err)
		return 1
	}
	role, ok := cfg.Role(name)
	if !ok {
		fmt.Fprintf(stderr, "org seat rm: unknown org role %q\n", name)
		return 2
	}
	if children := cfg.Children(name); len(children) != 0 {
		names := make([]string, len(children))
		for i, child := range children {
			names[i] = child.Name
		}
		fmt.Fprintf(stderr, "org seat rm: role %q still parents %s\n", name, strings.Join(names, ", "))
		return 2
	}

	ctx := context.Background()
	var pane org.LivePane
	hasLivePane := false
	unresolvedDetail := "Herdr pane binding is unset"
	if strings.TrimSpace(role.Pane) != "" {
		snapshotCtx, cancel := context.WithTimeout(ctx, orgSeatExternalTimeout)
		snapshot, snapshotErr := orgProviderSnapshot(snapshotCtx, cfg)
		cancel()
		if snapshotErr != nil {
			fmt.Fprintf(stderr, "org seat rm: inspect live Herdr panes: %v\n", snapshotErr)
			return 1
		}
		binding := snapshot.PaneBindings[role.Name]
		unresolvedDetail = firstNonEmpty(binding.Detail, "no live pane binding")
		if strings.TrimSpace(binding.PaneID) == "" {
			fmt.Fprintf(
				stderr,
				"org seat rm: role %q pane is unresolved: %s; rebind with gitmoot org seat add %s --pane ID_OR_LABEL\n",
				role.Name, unresolvedDetail, role.Name,
			)
			return 1
		}
		pane, ok = orgSeatPaneByID(snapshot.Panes, binding.PaneID)
		if !ok {
			fmt.Fprintf(stderr, "org seat rm: resolved pane %s is absent from the live snapshot\n", binding.PaneID)
			return 1
		}
		if err := orgSeatBranchCheck(ctx, pane); err != nil {
			fmt.Fprintf(stderr, "org seat rm: refusing to remove %q: %v\n", role.Name, err)
			return 1
		}
		hasLivePane = true
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm: open store: %v\n", err)
		return 1
	}
	configEdit, _, err := config.RemoveOrgSeatRole(paths, role.Name)
	if err != nil {
		store.Close()
		fmt.Fprintf(stderr, "org seat rm: remove role: %v\n", err)
		return 1
	}
	removedRules, err := store.DeleteEventRulesForRole(ctx, role.Name)
	if err != nil {
		restoreErr := configEdit.Restore()
		store.Close()
		fmt.Fprintf(stderr, "org seat rm: remove wake routes: %v\n", errors.Join(err, restoreErr))
		return 1
	}
	if hasLivePane {
		closeCtx, closeCancel := context.WithTimeout(ctx, orgSeatExternalTimeout)
		closeErr := orgSeatClosePane(closeCtx, pane.PaneID)
		closeCancel()
		if closeErr != nil {
			rulesRestoreErr := store.AddEventRules(ctx, removedRules)
			configRestoreErr := configEdit.Restore()
			store.Close()
			fmt.Fprintf(stderr, "org seat rm: close pane %s: %v\n", pane.PaneID, errors.Join(closeErr, rulesRestoreErr, configRestoreErr))
			return 1
		}
	}
	if err := store.Close(); err != nil {
		fmt.Fprintf(stderr, "org seat rm: close store: %v\n", err)
		return 1
	}
	if hasLivePane {
		fmt.Fprintf(stdout, "pane %s closed\n", pane.PaneID)
	} else {
		fmt.Fprintf(stdout, "role %s had no resolved live pane (%s); no pane closed\n", role.Name, unresolvedDetail)
	}
	fmt.Fprintf(stdout, "role %s removed with %d wake routes\n", role.Name, len(removedRules))
	fresh, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm: reload org registry: %v\n", err)
		return 1
	}
	closedPaneID := ""
	if hasLivePane {
		closedPaneID = pane.PaneID
	}
	return runOrgSeatValidateRemoved(ctx, paths, fresh, role.Name, closedPaneID, stdout, stderr)
}

func leadingOrgSeatName(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func validOrgSeatName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func uniqueOrgSeatPaneByReference(panes []org.LivePane, reference string) (org.LivePane, error) {
	if pane, ok := orgSeatPaneByID(panes, reference); ok && pane.PaneID != "" {
		return pane, nil
	}
	var match org.LivePane
	count := 0
	for _, pane := range panes {
		if pane.PaneID != "" && pane.Label == reference {
			match = pane
			count++
		}
	}
	switch count {
	case 0:
		return org.LivePane{}, fmt.Errorf("no live Herdr pane has id or exact label %q", reference)
	case 1:
		return match, nil
	default:
		return org.LivePane{}, fmt.Errorf("%d live Herdr panes have label %q; refusing ambiguous adoption", count, reference)
	}
}

func orgSeatPaneByID(panes []org.LivePane, paneID string) (org.LivePane, bool) {
	for _, pane := range panes {
		if pane.PaneID == paneID {
			return pane, true
		}
	}
	return org.LivePane{}, false
}

type orgSeatRoleOptions struct {
	ActingRole string
	Parent     string
	Scope      []string
	ScopeSet   bool
	MergeRule  string
}

func orgSeatDesiredRole(cfg config.OrgConfig, snapshot org.Snapshot, name, paneReference, paneID string, options orgSeatRoleOptions) (config.OrgRole, bool, error) {
	if current, ok := cfg.Role(name); ok {
		if paneID == "" {
			return current, true, nil
		}
		if strings.TrimSpace(current.Pane) == "" {
			current.Pane = paneID
			return current, true, nil
		}
		binding := snapshot.PaneBindings[current.Name]
		if strings.TrimSpace(binding.PaneID) == "" {
			if binding.Ambiguous {
				return config.OrgRole{}, true, fmt.Errorf(
					"role %q binding %q is ambiguous (%s); disambiguate live pane labels before rebinding",
					current.Name, current.Pane, firstNonEmpty(binding.Detail, "multiple live panes match"),
				)
			}
			current.Pane = paneID
			return current, true, nil
		}
		if binding.PaneID != paneID {
			return config.OrgRole{}, true, fmt.Errorf(
				"role %q already binds %q (resolved to %s), not pane %s referenced by %q",
				current.Name, current.Pane, binding.PaneID, paneID, paneReference,
			)
		}
		if strings.TrimSpace(current.Pane) != paneID {
			current.Pane = paneID
		}
		return current, true, nil
	}
	if name == "owner" {
		if cfg.Enabled() {
			return config.OrgRole{}, false, errors.New("owner role is missing from a non-empty registry")
		}
		return config.OrgRole{Name: name, Scope: []string{"*"}, MergeRule: "owner", Pane: paneID}, false, nil
	}

	actingRoleName := strings.ToLower(strings.TrimSpace(options.ActingRole))
	if actingRoleName == "" {
		actingRoleName = "owner"
	}
	actingRole, ok := cfg.Role(actingRoleName)
	if !ok {
		return config.OrgRole{}, false, fmt.Errorf("acting role %q is not declared", actingRoleName)
	}

	parentName := strings.ToLower(strings.TrimSpace(options.Parent))
	if parentName == "" {
		parentName = actingRoleName
	}
	if !validOrgSeatName(parentName) {
		return config.OrgRole{}, false, fmt.Errorf("invalid parent role name %q", options.Parent)
	}
	if orgSeatParentCreatesCycle(cfg, name, parentName) {
		return config.OrgRole{}, false, fmt.Errorf("parent role %q would create a cycle for %q", parentName, name)
	}
	parent, ok := cfg.Role(parentName)
	if !ok {
		return config.OrgRole{}, false, fmt.Errorf("parent role %q is not declared", parentName)
	}

	scope := append([]string(nil), actingRole.Scope...)
	if options.ScopeSet {
		scope = append([]string(nil), options.Scope...)
	}
	if !config.ScopeSubset(scope, scope) {
		return config.OrgRole{}, false, fmt.Errorf("invalid --scope %q", strings.Join(scope, ","))
	}
	if !config.ScopeSubset(scope, actingRole.Scope) {
		return config.OrgRole{}, false, fmt.Errorf("requested scope %q exceeds acting role %q scope", strings.Join(scope, ","), actingRoleName)
	}
	if !config.ScopeSubset(scope, parent.Scope) {
		return config.OrgRole{}, false, fmt.Errorf("requested scope %q exceeds parent role %q scope", strings.Join(scope, ","), parentName)
	}

	mergeRule := strings.ToLower(strings.TrimSpace(options.MergeRule))
	if mergeRule != "" && mergeRule != "owner" && mergeRule != "self" && mergeRule != "none" {
		return config.OrgRole{}, false, fmt.Errorf("invalid --merge-rule %q; use owner, self, or none", options.MergeRule)
	}
	if !orgSeatMergeRuleAllows(actingRole.MergeRule, mergeRule) {
		return config.OrgRole{}, false, fmt.Errorf("merge rule %q exceeds acting role %q authority", mergeRule, actingRoleName)
	}

	return config.OrgRole{
		Name: name, Parent: parentName, Scope: scope,
		MergeRule: mergeRule, Pane: paneID,
	}, false, nil
}

func parseOrgSeatScope(raw string, set bool) ([]string, error) {
	if !set {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	scope := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		entry := strings.ToLower(strings.TrimSpace(part))
		if entry == "" {
			return nil, errors.New("--scope must contain one or more comma-separated repository scopes")
		}
		if seen[entry] {
			return nil, fmt.Errorf("--scope contains duplicate %q", entry)
		}
		seen[entry] = true
		scope = append(scope, entry)
	}
	return scope, nil
}

func orgSeatParentCreatesCycle(cfg config.OrgConfig, name, parent string) bool {
	seen := map[string]bool{}
	for current := parent; current != ""; {
		if current == name || seen[current] {
			return true
		}
		seen[current] = true
		role, ok := cfg.Role(current)
		if !ok {
			return false
		}
		current = role.Parent
	}
	return false
}

func orgSeatMergeRuleAllows(granter, requested string) bool {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "", "none":
		return true
	case "self":
		return granter == "self" || granter == "owner"
	case "owner":
		return granter == "owner"
	default:
		return false
	}
}

func orgSeatDefaultRules(role string) []db.EventRule {
	rules := make([]db.EventRule, 0, len(orgSeatDefaultRouteKinds))
	for _, kind := range orgSeatDefaultRouteKinds {
		rules = append(rules, db.EventRule{
			ID: "org-seat-" + role + "-" + kind, OnKind: kind, WakeRole: role,
			Scope: db.EventRuleScopeAddressed, Enabled: true,
		})
	}
	return rules
}

func missingOrgSeatRules(expected, current []db.EventRule) ([]db.EventRule, map[string]bool, error) {
	byID := make(map[string]db.EventRule, len(current))
	for _, rule := range current {
		byID[rule.ID] = rule
	}
	missing := make([]db.EventRule, 0, len(expected))
	existing := make(map[string]bool, len(expected))
	for _, want := range expected {
		got, ok := byID[want.ID]
		if !ok {
			missing = append(missing, want)
			continue
		}
		if !sameOrgSeatRule(got, want) {
			return nil, nil, fmt.Errorf("route id %q already exists with different semantics", want.ID)
		}
		existing[want.ID] = true
	}
	return missing, existing, nil
}

func sameOrgSeatRule(got, want db.EventRule) bool {
	return strings.EqualFold(strings.TrimSpace(got.OnKind), want.OnKind) &&
		strings.TrimSpace(got.MatchFilter) == want.MatchFilter &&
		strings.EqualFold(strings.TrimSpace(got.WakeRole), want.WakeRole) &&
		got.Scope == want.Scope && got.Enabled == want.Enabled
}

func closeOrgSeatPane(ctx context.Context, paneID string) error {
	result, err := orgSeatRunner.Run(ctx, "", "herdr", "pane", "close", paneID)
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func checkOrgSeatBranches(ctx context.Context, pane org.LivePane) error {
	directories := []string{strings.TrimSpace(pane.ForegroundCWD), strings.TrimSpace(pane.CWD)}
	roots := map[string]struct{}{}
	reported := 0
	for _, directory := range directories {
		if directory == "" {
			continue
		}
		reported++
		result, err := orgSeatRunner.Run(ctx, directory, "git", "rev-parse", "--show-toplevel")
		if err != nil {
			if strings.Contains(strings.ToLower(result.Stderr), "not a git repository") {
				continue
			}
			return fmt.Errorf("inspect pane checkout %q: %w", directory, err)
		}
		root := strings.TrimSpace(result.Stdout)
		if root == "" {
			return fmt.Errorf("pane checkout %q returned an empty git root", directory)
		}
		roots[filepath.Clean(root)] = struct{}{}
	}
	if reported == 0 {
		return fmt.Errorf("pane %s reports neither cwd nor foreground_cwd; branch safety is unverified", pane.PaneID)
	}
	if len(roots) == 0 {
		return fmt.Errorf("pane %s reports no Git checkout; branch safety is unverified", pane.PaneID)
	}
	for root := range roots {
		status, err := orgSeatRunner.Run(ctx, root, "git", "status", "--porcelain")
		if err != nil {
			return fmt.Errorf("inspect worktree %q: %w", root, err)
		}
		if dirty := strings.TrimSpace(status.Stdout); dirty != "" {
			first, _, _ := strings.Cut(dirty, "\n")
			return fmt.Errorf("checkout %s has uncommitted work (%s)", root, first)
		}
		branchResult, branchErr := orgSeatRunner.Run(ctx, root, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
		branch := strings.TrimSpace(branchResult.Stdout)
		if branchErr != nil {
			branch = "detached HEAD"
		}
		baseResult, baseErr := orgSeatRunner.Run(ctx, root, "git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
		base := strings.TrimSpace(baseResult.Stdout)
		if baseErr != nil || base == "" {
			if _, err := orgSeatRunner.Run(ctx, root, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); err != nil {
				return fmt.Errorf("checkout %s has no resolvable origin default branch; branch safety is unverified", root)
			}
			base = "origin/main"
		}
		_, err = orgSeatRunner.Run(ctx, root, "git", "merge-base", "--is-ancestor", "HEAD", base)
		if err == nil {
			continue
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return fmt.Errorf("checkout %s branch %s has commits not merged into %s", root, branch, base)
		}
		return fmt.Errorf("compare checkout %s branch %s with %s: %w", root, branch, base, err)
	}
	return nil
}

func runOrgValidateOrShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "org %s does not accept positional arguments\n", args[0])
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: resolve paths: %v\n", args[0], err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", args[0], err)
		return 1
	}
	if args[0] == "validate" {
		return runOrgValidateReality(context.Background(), paths, cfg, stdout, stderr)
	}
	for _, role := range loadOrgRoster(context.Background(), nil, cfg).Members() {
		fmt.Fprintf(stdout, "%s\tparent=%s\tscope=%s\tmerge_rule=%s\n", role.Name, role.Parent, strings.Join(role.Scope, ","), firstNonEmpty(role.MergeRule, "none"))
	}
	return 0
}

func runOrgSeatValidateAdded(ctx context.Context, paths config.Paths, cfg config.OrgConfig, roleName string, requireBound bool, stdout, stderr io.Writer) int {
	role, ok := cfg.Role(roleName)
	if !ok {
		fmt.Fprintf(stderr, "org seat add validation: role %q is absent after write\n", roleName)
		return 1
	}
	var binding org.PaneBinding
	if requireBound {
		snapshot, err := orgProviderSnapshot(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "org seat add validation: Herdr snapshot unavailable: %v\n", err)
			return 1
		}
		binding = snapshot.PaneBindings[role.Name]
		if strings.TrimSpace(binding.PaneID) == "" {
			fmt.Fprintf(stderr, "org seat add validation: role %q pane is unresolved: %s\n", role.Name, firstNonEmpty(binding.Detail, "no live pane binding"))
			return 1
		}
	} else if strings.TrimSpace(role.Pane) != "" {
		fmt.Fprintf(stderr, "org seat add validation: role %q unexpectedly binds pane %q\n", role.Name, role.Pane)
		return 1
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add validation: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add validation: list event routes: %v\n", err)
		return 1
	}
	missing, _, err := missingOrgSeatRules(orgSeatDefaultRules(role.Name), rules)
	if err != nil {
		fmt.Fprintf(stderr, "org seat add validation: %v\n", err)
		return 1
	}
	if len(missing) != 0 {
		ids := make([]string, len(missing))
		for i, rule := range missing {
			ids[i] = rule.ID
		}
		fmt.Fprintf(stderr, "org seat add validation: role %q is missing routes %s\n", role.Name, strings.Join(ids, ", "))
		return 1
	}
	if !requireBound {
		fmt.Fprintf(stdout, "ok role %s unbound enabled_routes=%d\n", role.Name, len(orgSeatDefaultRouteKinds))
		return 0
	}
	fmt.Fprintf(stdout, "ok role %s pane=%s enabled_routes=%d\n", role.Name, binding.PaneID, len(orgSeatDefaultRouteKinds))
	return 0
}

func runOrgSeatValidateRemoved(ctx context.Context, paths config.Paths, cfg config.OrgConfig, roleName, closedPaneID string, stdout, stderr io.Writer) int {
	if _, ok := cfg.Role(roleName); ok {
		fmt.Fprintf(stderr, "org seat rm validation: role %q remains after removal\n", roleName)
		return 1
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm validation: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org seat rm validation: list event routes: %v\n", err)
		return 1
	}
	ownedRoutes := 0
	for _, rule := range rules {
		if strings.EqualFold(strings.TrimSpace(rule.WakeRole), roleName) {
			ownedRoutes++
		}
	}
	if ownedRoutes != 0 {
		fmt.Fprintf(stderr, "org seat rm validation: role %q still owns %d routes\n", roleName, ownedRoutes)
		return 1
	}
	if closedPaneID != "" {
		snapshot, snapshotErr := orgProviderSnapshot(ctx, cfg)
		if snapshotErr != nil {
			fmt.Fprintf(stderr, "org seat rm validation: Herdr snapshot unavailable: %v\n", snapshotErr)
			return 1
		}
		if _, present := orgSeatPaneByID(snapshot.Panes, closedPaneID); present {
			fmt.Fprintf(stderr, "org seat rm validation: pane %s remains live after close\n", closedPaneID)
			return 1
		}
	}
	fmt.Fprintf(stdout, "ok role %s absent owned_routes=%d\n", roleName, ownedRoutes)
	return 0
}

type orgValidationSummary struct {
	roles                int
	livePanes            int
	enabledRoutes        int
	unresolvedRoles      int
	rolesWithoutRoutes   int
	unclaimedLabeledPane int
}

func runOrgValidateReality(ctx context.Context, paths config.Paths, cfg config.OrgConfig, stdout, stderr io.Writer) int {
	snapshot, err := orgProviderSnapshot(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "org validate: Herdr snapshot unavailable: %v\n", err)
		return 1
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org validate: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org validate: list event routes: %v\n", err)
		return 1
	}
	missedWakes, err := store.ListRoleMissedWakes(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org validate: list missed wakes: %v\n", err)
		return 1
	}

	routesByRole := make(map[string]int)
	enabledRoutes := 0
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		enabledRoutes++
		routesByRole[strings.ToLower(strings.TrimSpace(rule.WakeRole))]++
	}
	missedByRole := make(map[string]int, len(missedWakes))
	for _, missed := range missedWakes {
		missedByRole[strings.ToLower(strings.TrimSpace(missed.Role))] = missed.Consecutive
	}
	claimedPanes := make(map[string]string)
	roster := loadOrgRoster(ctx, store, cfg)
	summary := orgValidationSummary{
		roles: len(roster.Members()), livePanes: len(snapshot.Panes), enabledRoutes: enabledRoutes,
	}
	var issues []string
	for _, role := range roster.Members() {
		name := strings.ToLower(strings.TrimSpace(role.Name))
		binding := snapshot.PaneBindings[name]
		if strings.TrimSpace(binding.PaneID) == "" {
			summary.unresolvedRoles++
			detail := strings.TrimSpace(binding.Detail)
			if detail == "" {
				detail = "provider returned no binding decision"
			}
			issues = append(issues, fmt.Sprintf(
				"role %s pane unresolved: %s (paths=presence,event-wake,role-unavailable-nudge missed_wakes=%d)",
				role.Name, detail, missedByRole[name],
			))
		} else {
			claimedPanes[binding.PaneID] = role.Name
		}
		if routesByRole[name] == 0 {
			summary.rolesWithoutRoutes++
			issues = append(issues, fmt.Sprintf("role %s has no enabled wake route", role.Name))
		}
	}
	for _, pane := range snapshot.Panes {
		if strings.TrimSpace(pane.Label) == "" {
			continue
		}
		if _, claimed := claimedPanes[pane.PaneID]; claimed {
			continue
		}
		summary.unclaimedLabeledPane++
		issues = append(issues, fmt.Sprintf("labeled pane %q (%s) is not claimed by an org role", pane.Label, firstNonEmpty(pane.PaneID, "no pane id")))
	}
	sort.Strings(issues)
	if len(issues) == 0 {
		fmt.Fprintf(stdout, "ok %d roles, %d live panes, %d enabled routes\n", summary.roles, summary.livePanes, summary.enabledRoutes)
		return 0
	}
	fmt.Fprintf(stderr,
		"org validate: unresolved_roles=%d roles_without_routes=%d unclaimed_labeled_panes=%d\n",
		summary.unresolvedRoles, summary.rolesWithoutRoutes, summary.unclaimedLabeledPane,
	)
	for _, issue := range issues {
		fmt.Fprintf(stderr, "- %s\n", issue)
	}
	return 1
}

const starterOrgConfig = `

# Organization registry (#1042). enforce = "warn" logs a durable event without
# rejecting; set "block" to reject out-of-scope dispatches (see gitmoot org validate).
[org]
enforce = "warn"

[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
`

func runOrgInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org init accepts no positional arguments")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org init: resolve paths: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "org init: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org init: load org registry: %v\n", err)
		return 1
	}
	if !cfg.Enabled() {
		file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "org init: open config: %v\n", err)
			return 1
		}
		_, writeErr := io.WriteString(file, starterOrgConfig)
		closeErr := file.Close()
		if writeErr != nil {
			fmt.Fprintf(stderr, "org init: write config: %v\n", writeErr)
			return 1
		}
		if closeErr != nil {
			fmt.Fprintf(stderr, "org init: close config: %v\n", closeErr)
			return 1
		}
		cfg, err = config.LoadOrg(paths)
		if err != nil {
			fmt.Fprintf(stderr, "org init: validate scaffold: load org registry: %v\n", err)
			return 1
		}
	}
	check := doctor.CheckHerdrVersion(context.Background(), orgDoctorRunner, doctor.OrgMinimumHerdrVersion)
	if !check.OK {
		fmt.Fprintf(stderr, "org init: %s\n", check.Detail)
		return 1
	}
	if _, err := orgProviderSnapshot(context.Background(), cfg); err != nil {
		fmt.Fprintf(stderr, "org init: Herdr snapshot unavailable: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "initialized organization registry at %s\n", paths.ConfigFile)
	fmt.Fprintf(stdout, "herdr: %s\n", check.Detail)
	return 0
}

type orgBriefOutput struct {
	Role            string             `json:"role"`
	Parent          string             `json:"parent,omitempty"`
	Pane            string             `json:"pane,omitempty"`
	Children        []string           `json:"children"`
	Path            []string           `json:"path"`
	Scope           []string           `json:"scope"`
	MergeRule       string             `json:"merge_rule"`
	Model           string             `json:"model"`
	LastSeenAt      string             `json:"last_seen_at,omitempty"`
	LastCommand     string             `json:"last_command,omitempty"`
	ProviderState   org.LifecycleState `json:"provider_state"`
	ProviderDetail  string             `json:"provider_detail,omitempty"`
	ObservedAt      time.Time          `json:"observed_at,omitempty"`
	ProviderVersion string             `json:"provider_version,omitempty"`
	// TODO(#1058): add bounded open escalations only after creation, resolution,
	// and correlation identifiers have a frozen store contract.
}

type orgStatusOutput struct {
	Role              string             `json:"role"`
	Parent            string             `json:"parent,omitempty"`
	Pane              string             `json:"pane,omitempty"`
	Depth             int                `json:"depth,omitempty"`
	Scope             []string           `json:"scope"`
	MergeRule         string             `json:"merge_rule"`
	Model             string             `json:"model"`
	ActiveJobs        int                `json:"active_jobs,omitempty"`
	LastSeenAt        string             `json:"last_seen_at,omitempty"`
	LastSeenAge       string             `json:"last_seen_age,omitempty"`
	LastCommand       string             `json:"last_command,omitempty"`
	ProviderState     org.LifecycleState `json:"provider_state"`
	ProviderDetail    string             `json:"provider_detail,omitempty"`
	ObservedAt        time.Time          `json:"observed_at,omitempty"`
	ProviderVersion   string             `json:"provider_version,omitempty"`
	RecycleStatus     string             `json:"recycle,omitempty"`
	RecycleAfter      string             `json:"recycle_after,omitempty"`
	MissedWakes       int                `json:"missed_wakes,omitempty"`
	Flagged           bool               `json:"flagged,omitempty"`
	FlagReason        string             `json:"flag_reason,omitempty"`
	UnavailableReason string             `json:"unavailable_reason,omitempty"`
	UnavailableUntil  string             `json:"unavailable_until,omitempty"`
}

func runOrgBrief(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org brief", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleName := fs.String("role", strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")), "organization role to brief")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*roleName) == "" {
		fmt.Fprintln(stderr, "org brief requires --role NAME")
		return 2
	}
	ctx := context.Background()
	cfg, presence, store, err := loadOrgCommandState(ctx, *home)
	if err != nil {
		fmt.Fprintf(stderr, "org brief: %v\n", err)
		return 1
	}
	defer store.Close()
	role, ok := cfg.Role(*roleName)
	if !ok {
		fmt.Fprintf(stderr, "org brief: unknown org role %q\n", strings.TrimSpace(*roleName))
		return 1
	}
	if err := store.TouchOrgRolePresence(ctx, role.Name, "org brief"); err != nil {
		fmt.Fprintf(stderr, "org brief: touch presence: %v\n", err)
		return 1
	}
	updatedPresence, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "org brief: reload presence: %v\n", err)
		return 1
	}
	for _, row := range updatedPresence {
		presence[row.Role] = row
	}
	snapshot, snapshotErr := orgProviderSnapshot(ctx, cfg)
	out := buildOrgBriefOutput(cfg, presence, role, snapshot, snapshotErr)
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "org brief: %v\n", err)
			return 1
		}
		return 0
	}
	printOrgBrief(stdout, out)
	return 0
}

func buildOrgBriefOutput(cfg config.OrgConfig, presence map[string]db.OrgRolePresence, role config.OrgRole, snapshot org.Snapshot, snapshotErr error) orgBriefOutput {
	live := org.RoleLiveState{State: org.StateUnknown}
	if snapshotErr != nil {
		live.Detail = snapshotErr.Error()
	} else if value, exists := snapshot.States[role.Name]; exists {
		live = value
	} else {
		live.Detail = "provider snapshot omitted this role"
	}
	children := cfg.Children(role.Name)
	childNames := make([]string, 0, len(children))
	for _, child := range children {
		childNames = append(childNames, child.Name)
	}
	row := presence[role.Name]
	return orgBriefOutput{
		Role: role.Name, Parent: role.Parent, Pane: role.Pane, Children: childNames, Path: cfg.Path(role.Name), Scope: role.Scope,
		MergeRule: role.MergeRule, Model: role.Model, LastSeenAt: row.LastSeenAt, LastCommand: row.LastCommand,
		ProviderState: live.State, ProviderDetail: live.Detail, ObservedAt: snapshot.ObservedAt, ProviderVersion: snapshot.ProviderVersion,
	}
}

func runOrgChart(args []string, stdout, stderr io.Writer) int {
	return runOrgOverview("chart", args, stdout, stderr)
}

func runOrgStatus(args []string, stdout, stderr io.Writer) int {
	return runOrgOverview("status", args, stdout, stderr)
}

func runOrgOverview(command string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "org %s accepts no positional arguments\n", command)
		return 2
	}
	ctx := context.Background()
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", command, err)
		return 1
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", command, err)
		return 1
	}
	defer store.Close()
	shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(stderr, "org %s: %v\n", command, err)
		return 1
	}
	for _, warning := range shared.Warnings {
		fmt.Fprintf(stderr, "org %s: %s\n", command, warning)
	}
	reportedWarnings := len(shared.Warnings)
	rows, err := buildOrgStatusRows(ctx, &shared, herdrOrgLiveSource, command, command == "status")
	if err != nil {
		var liveErr *orgLiveSourceError
		if errors.As(err, &liveErr) {
			fmt.Fprintf(stderr, "org %s: Herdr snapshot unavailable: %v\n", command, liveErr.err)
		} else {
			fmt.Fprintf(stderr, "org %s: %v\n", command, err)
		}
		return 1
	}
	for _, warning := range shared.Warnings[reportedWarnings:] {
		fmt.Fprintf(stderr, "org %s: %s\n", command, warning)
	}
	if *jsonOutput {
		if err := writeJSON(stdout, rows); err != nil {
			fmt.Fprintf(stderr, "org %s: %v\n", command, err)
			return 1
		}
		return 0
	}
	if command == "chart" {
		for _, row := range rows {
			fmt.Fprintf(stdout, "%s%s · %s%s · scope=%s · merge=%s · model=%s · seen=%s%s\n", strings.Repeat("  ", row.Depth), row.Role, row.ProviderState, orgUnavailableFlag(row), strings.Join(row.Scope, ","), dash(row.MergeRule), dash(row.Model), dash(row.LastSeenAge), orgMissedWakeFlag(row))
		}
		return 0
	}
	fmt.Fprintln(stdout, "ROLE\tSTATE\tLAST SEEN\tAGE\tLAST COMMAND\tDETAIL\tRECYCLE")
	for _, row := range rows {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\trecycle=%s%s\n", row.Role, row.ProviderState, dash(row.LastSeenAt), dash(row.LastSeenAge), dash(row.LastCommand), dash(row.ProviderDetail), firstNonEmpty(row.RecycleStatus, "off"), orgMissedWakeFlag(row))
	}
	return 0
}

func orgUnavailableFlag(row orgStatusOutput) string {
	if row.ProviderState != org.StateUnavailable {
		return ""
	}
	return fmt.Sprintf(" ⚠ UNAVAILABLE reason=%s until=%s", dash(row.UnavailableReason), dash(row.UnavailableUntil))
}

func orgMissedWakeFlag(row orgStatusOutput) string {
	if !row.Flagged {
		return ""
	}
	return fmt.Sprintf(" ⚠ flagged (%d missed wakes)", row.MissedWakes)
}

func printOrgBrief(w io.Writer, brief orgBriefOutput) {
	fmt.Fprintf(w, "role: %s\n", brief.Role)
	fmt.Fprintf(w, "parent: %s\n", dash(brief.Parent))
	fmt.Fprintf(w, "children: %s\n", dash(strings.Join(brief.Children, ", ")))
	fmt.Fprintf(w, "path: %s\n", dash(strings.Join(brief.Path, " > ")))
	fmt.Fprintf(w, "scope: %s\n", dash(strings.Join(brief.Scope, ", ")))
	fmt.Fprintf(w, "merge_rule: %s\n", dash(brief.MergeRule))
	fmt.Fprintf(w, "model: %s\n", dash(brief.Model))
	fmt.Fprintf(w, "last_seen: %s\n", dash(brief.LastSeenAt))
	fmt.Fprintf(w, "last_command: %s\n", dash(brief.LastCommand))
	fmt.Fprintf(w, "provider: %s\n", brief.ProviderState)
	fmt.Fprintf(w, "provider_detail: %s\n", dash(brief.ProviderDetail))
}

type orgRecycleOutput struct {
	Role       string `json:"role"`
	Pane       string `json:"pane"`
	Kind       string `json:"kind"`
	AgentName  string `json:"agent_name"`
	WorkflowID string `json:"workflow_id"`
}

const orgRecycleSnapshotTimeout = 10 * time.Second

func runOrgRecycle(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org recycle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	kindFlag := fs.String("kind", "", "Herdr agent kind for the successor session")
	handoffFlag := fs.String("handoff", "", "handoff note for the successor session")
	paneFlag := fs.String("pane", "", "Herdr pane id (overrides the role's configured pane)")
	jsonOutput := fs.Bool("json", false, "print JSON")
	roleArg := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		roleArg, flagArgs = args[0], args[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if roleArg == "" {
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "org recycle requires exactly one ROLE")
			return 2
		}
		roleArg = fs.Arg(0)
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org recycle requires exactly one ROLE")
		return 2
	}
	handoff := strings.TrimSpace(*handoffFlag)
	if handoff == "" {
		fmt.Fprintln(stderr, "org recycle requires a non-empty --handoff note")
		return 2
	}
	kind := strings.ToLower(strings.TrimSpace(*kindFlag))
	if !validOrgRecycleKind(kind) {
		fmt.Fprintf(stderr, "org recycle requires a valid --kind (got %q)\n", strings.TrimSpace(*kindFlag))
		return 2
	}

	ctx := context.Background()
	cfg, presence, store, err := loadOrgCommandState(ctx, *home)
	if err != nil {
		fmt.Fprintf(stderr, "org recycle: %v\n", err)
		return 1
	}
	defer store.Close()
	role, ok := cfg.Role(roleArg)
	if !ok {
		fmt.Fprintf(stderr, "org recycle: unknown org role %q\n", strings.TrimSpace(roleArg))
		return 2
	}
	pane := strings.TrimSpace(*paneFlag)
	if pane == "" {
		pane = strings.TrimSpace(role.Pane)
	}
	if pane == "" {
		fmt.Fprintf(stderr, "org recycle: role %q has no bound pane; set [org.roles.%q].pane or pass --pane\n", role.Name, role.Name)
		return 2
	}

	workflowID := "org/" + role.Name
	if err := workflow.ValidateWorkflowID(workflowID); err != nil {
		fmt.Fprintf(stderr, "org recycle: role lifecycle workflow: %v\n", err)
		return 1
	}
	noteBody := workflow.FormatOrgHandoffNote(role.Name, handoff)
	if noteBody == "" || len(noteBody) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "org recycle handoff must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: workflowID, Author: role.Name, Body: noteBody}); err != nil {
		fmt.Fprintf(stderr, "org recycle: journal handoff: %v\n", err)
		return 1
	}

	provider := newOrgProvider([]config.OrgRole{role})
	if provider == nil {
		fmt.Fprintf(stderr, "org recycle: organization provider is not configured (handoff journaled in workflow %s)\n", workflowID)
		return 1
	}
	snapshotCtx, cancelSnapshot := context.WithTimeout(ctx, orgRecycleSnapshotTimeout)
	snapshot, snapshotErr := provider.Snapshot(snapshotCtx)
	cancelSnapshot()
	brief := buildOrgBriefOutput(cfg, presence, role, snapshot, snapshotErr)
	var boot strings.Builder
	printOrgBrief(&boot, brief)
	fmt.Fprintf(&boot, "\nhandoff: %s\n", handoff)
	req := org.RecycleRequest{Role: role.Name, Pane: pane, Kind: kind, AgentName: role.Name, Model: role.Model, BootPrompt: boot.String()}
	if err := provider.Recycle(ctx, req); err != nil {
		fmt.Fprintf(stderr, "org recycle: %v (handoff journaled in workflow %s)\n", err, workflowID)
		return 1
	}
	out := orgRecycleOutput{Role: role.Name, Pane: pane, Kind: kind, AgentName: role.Name, WorkflowID: workflowID}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "org recycle: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "recycled org role %s as %s in pane %s; handoff journaled in workflow %s\n", role.Name, kind, pane, workflowID)
	return 0
}

func validOrgRecycleKind(kind string) bool {
	return slices.Contains([]string{
		"pi", "claude", "codex", "gemini", "cursor", "devin", "agy", "cline", "omp", "mastracode", "opencode",
		"copilot", "kimi", "kiro", "droid", "amp", "grok", "hermes", "kilo", "qodercli", "maki",
	}, kind)
}

func loadOrgCommandState(ctx context.Context, home string) (config.OrgConfig, map[string]db.OrgRolePresence, *db.Store, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return config.OrgConfig{}, nil, nil, err
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return config.OrgConfig{}, nil, nil, fmt.Errorf("load org registry: %w", err)
	}
	if !cfg.Enabled() {
		return config.OrgConfig{}, nil, nil, errors.New("organization registry is disabled; run `gitmoot org init`")
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return config.OrgConfig{}, nil, nil, err
	}
	rows, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		store.Close()
		return config.OrgConfig{}, nil, nil, err
	}
	presence := make(map[string]db.OrgRolePresence, len(rows))
	for _, row := range rows {
		presence[row.Role] = row
	}
	return cfg, presence, store, nil
}

func orgProviderSnapshot(ctx context.Context, cfg config.OrgConfig) (org.Snapshot, error) {
	provider := newOrgProvider(loadOrgRoster(ctx, nil, cfg).Members())
	if provider == nil {
		return org.Snapshot{}, errors.New("organization live-state provider is not configured")
	}
	return provider.Snapshot(ctx)
}

// validateAndTouchActingOrgRole is the shared local job ingress for --org-role.
// Validation happens before dispatch mutation; invalid/disabled config creates
// neither a presence row nor a job.
func validateAndTouchActingOrgRole(ctx context.Context, store *db.Store, home, role, command string) error {
	role = strings.TrimSpace(role)
	if role == "" {
		return nil
	}
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		return fmt.Errorf("load org registry: %w", err)
	}
	if !cfg.Enabled() {
		return errors.New("--org-role requires an enabled organization registry; run `gitmoot org init`")
	}
	configuredRole, ok := cfg.Role(role)
	if !ok {
		return fmt.Errorf("unknown org role %q", role)
	}
	if err := refuseUnavailableOrgRole(ctx, store, configuredRole.Name, time.Now().UTC()); err != nil {
		return err
	}
	// Recycle enforcement only applies to operator-origin --org-role dispatches:
	// this ingress (dispatchLocalAgentJob) is the sole path passing a non-empty
	// ActingOrgRole, and today only agent ask/run/implement/orchestrate
	// (OperatorOrigin) do so. Delegated/engine children enqueue via the mailbox
	// and never reach here, so an inherited role cannot be refused mid-tree.
	touchRole := role
	if mode := cfg.RecycleEnforce(); mode != "off" {
		// Read/touch under the registry's canonical name; TouchOrgRolePresence
		// also canonicalizes the key, so a stale case-variant presence row cannot
		// let an overdue role slip past the enforcement read.
		touchRole = configuredRole.Name
		recycleAfter := cfg.RecycleAfterFor(configuredRole.Name)
		if recycleAfter > 0 {
			presence, found, err := store.GetOrgRolePresence(ctx, configuredRole.Name)
			if err != nil {
				return fmt.Errorf("read org role %q presence: %w", configuredRole.Name, err)
			}
			if found {
				now := time.Now().UTC()
				age, known, overdue := orgRecycleAge(presence.LastSeenAt, now, recycleAfter)
				if known {
					overdueSince := time.Time{}
					if overdue {
						lastSeen, _ := parseOrgPresenceTime(presence.LastSeenAt)
						overdueSince = lastSeen.UTC().Add(recycleAfter)
					}
					updateRecycleOverdueEpisodeBestEffort(ctx, store, home, configuredRole.Name, overdueSince, recycleAfter, now)
				}
				if known && overdue {
					message := fmt.Sprintf("org role %q is overdue for recycling (idle %s ≥ recycle_after %s); journal a handoff note and recycle before dispatching new work", configuredRole.Name, age, formatOrgRecycleAfter(recycleAfter))
					if mode == "block" {
						return errors.New(message)
					}
					fmt.Fprintf(orgRecycleAdvisoryWriter, "warning: %s\n", message)
				}
			}
		}
	}
	return store.TouchOrgRolePresence(ctx, touchRole, command)
}

// updateRecycleOverdueEpisodeBestEffort mirrors the blocked-since episode
// pattern without coupling the CLI ingress to its daemon evaluator. A zero
// overdueSince means the role is fresh and closes any prior episode. Every
// failure is advisory-only so event bookkeeping can never change dispatch.
func updateRecycleOverdueEpisodeBestEffort(ctx context.Context, store *db.Store, home, role string, overdueSince time.Time, repeatAfter time.Duration, now time.Time) {
	if store == nil || repeatAfter <= 0 {
		return
	}
	// ALWAYS clear on a fresh dispatch, regardless of whether notifications are
	// enabled, so a prior episode can't linger stale across a rule toggle and
	// later mis-report overdue_since or wrongly suppress a legitimate emit.
	if overdueSince.IsZero() {
		if err := store.ClearRecycleOverdueEpisode(ctx, role); err != nil {
			fmt.Fprintf(orgRecycleOverdueEventWriter, "warning: org role %q recycle-overdue episode clear failed: %v\n", role, err)
		}
		return
	}
	// The overdue episode is a notification-dedup record (read only by this emit
	// path), so open/refresh it and emit only when an org event rule is enabled
	// (a nil sink otherwise). The wake/webhook is fire-and-forget; on a
	// short-lived --background/orchestrate dispatch the process may exit before
	// delivery, so the notification is best-effort (reliable on foreground
	// `agent ask`). Reliable background delivery is a tracked follow-up.
	if orgRecycleOverdueEventSink == nil {
		return
	}
	sink, err := orgRecycleOverdueEventSink(ctx, store, home)
	if err != nil {
		fmt.Fprintf(orgRecycleOverdueEventWriter, "warning: org role %q recycle-overdue event sink unavailable: %v\n", role, err)
		return
	}
	if sink == nil {
		return
	}
	if err := store.UpsertRecycleOverdueEpisode(ctx, role, overdueSince, now); err != nil {
		fmt.Fprintf(orgRecycleOverdueEventWriter, "warning: org role %q recycle-overdue episode upsert failed: %v\n", role, err)
		return
	}
	episodes, err := store.ListRecycleOverdueEpisodes(ctx)
	if err != nil {
		fmt.Fprintf(orgRecycleOverdueEventWriter, "warning: org role %q recycle-overdue episode read failed: %v\n", role, err)
		return
	}
	for _, episode := range episodes {
		if episode.Subject != role {
			continue
		}
		if err := orgRecycleOverdueEpisodeEmitter(ctx, store, sink, episode, repeatAfter, now); err != nil {
			fmt.Fprintf(orgRecycleOverdueEventWriter, "warning: org role %q recycle-overdue event emit failed: %v\n", role, err)
		}
		return
	}
}

// emitRecycleOverdueEpisode marks before emitting and carries the stable first
// overdue instant so consumers can distinguish a repeat from a fresh episode.
func emitRecycleOverdueEpisode(ctx context.Context, store *db.Store, sink events.Sink, episode db.RecycleOverdueEpisode, repeatAfter time.Duration, now time.Time) error {
	if store == nil || sink == nil || repeatAfter <= 0 {
		return nil
	}
	now = now.UTC()
	overdueSince, err := time.Parse(db.BlockedEpisodeTimeLayout, episode.OverdueSince)
	if err != nil {
		return fmt.Errorf("parse overdue_since %q: %w", episode.OverdueSince, err)
	}
	if last := strings.TrimSpace(episode.EmittedAt); last != "" {
		if lastEmitted, err := time.Parse(db.BlockedEpisodeTimeLayout, last); err == nil && now.Sub(lastEmitted) <= repeatAfter {
			return nil
		}
	}
	if err := store.MarkRecycleOverdueEpisodeEmitted(ctx, episode.Subject, now); err != nil {
		return fmt.Errorf("mark recycle-overdue episode emitted: %w", err)
	}
	overdueFor := now.Sub(overdueSince)
	if overdueFor < 0 {
		overdueFor = 0
	}
	detail := fmt.Sprintf("role %s overdue for recycling %s (since %s)", episode.Subject, overdueFor.Round(time.Second), overdueSince.UTC().Format(time.RFC3339))
	ev := events.NewEvent(events.EventOrgRecycleOverdue, episode.Subject, episode.Subject, "", "overdue", detail, now, workflow.RedactCommentText)
	ev.Cause = "recycle_overdue"
	events.EmitEvent(ctx, sink, ev)
	return nil
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func orgPresenceAge(value string, now time.Time) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	observed, ok := parseOrgPresenceTime(value)
	if !ok {
		return "unknown"
	}
	age := now.Sub(observed.UTC())
	if age < 0 {
		age = 0
	}
	return age.Round(time.Second).String()
}

func parseOrgPresenceTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		observed, err := time.Parse(layout, value)
		if err == nil {
			return observed, true
		}
	}
	return time.Time{}, false
}

func orgRecycleStatus(lastSeen string, now time.Time, state org.LifecycleState, activeJobs int, recycleAfter time.Duration) string {
	if recycleAfter <= 0 {
		return "off"
	}
	_, known, overdue := orgRecycleAge(lastSeen, now, recycleAfter)
	if !known {
		return "off"
	}
	if !overdue {
		return "fresh"
	}
	if activeJobs > 0 {
		return "overdue"
	}
	switch state {
	case org.StateIdle, org.StateDone, org.StateUnknown:
		return "eligible"
	default:
		return "overdue"
	}
}

func orgRecycleAge(lastSeen string, now time.Time, recycleAfter time.Duration) (age time.Duration, known, overdue bool) {
	if recycleAfter <= 0 {
		return 0, false, false
	}
	observed, ok := parseOrgPresenceTime(lastSeen)
	if !ok {
		return 0, false, false
	}
	age = now.Sub(observed.UTC())
	if age < 0 {
		age = 0
	}
	return age, true, age >= recycleAfter
}

func formatOrgRecycleAfter(value time.Duration) string {
	switch {
	case value%time.Hour == 0:
		return fmt.Sprintf("%dh", value/time.Hour)
	case value%time.Minute == 0:
		return fmt.Sprintf("%dm", value/time.Minute)
	case value%time.Second == 0:
		return fmt.Sprintf("%ds", value/time.Second)
	default:
		return value.String()
	}
}

type orgEscalateOutput struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Workflow string `json:"workflow"`
	Question string `json:"question"`
}

func runOrgEscalate(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "resolve" {
		return runOrgEscalateResolve(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("org escalate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	toFlag := fs.String("to", "", "ancestor or descendant role to ask")
	workflowID := fs.String("workflow", "", "workflow label for the organization question note")
	fromFlag := fs.String("org-role", "", "acting organization role")
	repo := fs.String("repo", "", "repository binding for the escalation note")
	jsonOutput := fs.Bool("json", false, "print the escalation as JSON")
	question, flagArgs, ok := orgEscalateQuestionAndFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "org escalate requires exactly one question")
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org escalate requires exactly one question")
		return 2
	}
	if question == "" {
		fmt.Fprintln(stderr, "org escalate question must be non-empty")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org escalate: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 1
	}
	if !cfg.Enabled() {
		fmt.Fprintln(stderr, "org escalate requires an [org] registry")
		return 2
	}
	from := strings.ToLower(strings.TrimSpace(*fromFlag))
	if from == "" {
		from = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if _, ok := cfg.Role(from); !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", from)
		return 2
	}
	to := strings.ToLower(strings.TrimSpace(*toFlag))
	if to == "" {
		fmt.Fprintln(stderr, "org escalate requires --to")
		return 2
	}
	if _, ok := cfg.Role(to); !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", to)
		return 2
	}
	downwardAsk := false
	switch {
	case slices.Contains(cfg.Ancestors(from), to):
		// Preserve the established upward escalation path.
	case slices.Contains(cfg.Ancestors(to), from):
		downwardAsk = true
	case from == to:
		fmt.Fprintf(stderr, "--to %q must differ from acting role %q\n", to, from)
		return 2
	default:
		// There is no configured peer-question policy. Fail closed until one
		// exists rather than inventing a cross-branch routing rule here.
		fmt.Fprintln(stderr, "peer questions are not configurable and are refused")
		return 2
	}
	label := strings.TrimSpace(*workflowID)
	if label == "" {
		fmt.Fprintln(stderr, "org escalate requires --workflow")
		return 2
	}
	if err := workflow.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 2
	}
	body := workflow.FormatOrgEscalateNote(from, to, label, question)
	if body == "" || len(body) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "org escalate question must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	_, addressedTarget, _, _, parsed := workflow.ParseOrgEscalateNote(body)
	if !parsed {
		fmt.Fprintln(stderr, "org escalate: formatted escalation could not be parsed")
		return 1
	}
	if err := withStore(*home, func(store *db.Store) error {
		count, err := store.CountJobsByWorkflow(context.Background(), label)
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("workflow %q has no jobs; refusing note to guard against a typo", label)
		}
		_, err = store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: label, Author: from, Body: body, Repo: strings.TrimSpace(*repo),
			AddressedTarget: addressedTarget,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "org escalate: %v\n", err)
		return 1
	}
	out := orgEscalateOutput{From: from, To: to, Workflow: label, Question: question}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintf(stderr, "org escalate: %v\n", err)
			return 1
		}
		return 0
	}
	if downwardAsk {
		fmt.Fprintf(stdout, "asked from %s to %s in workflow %s\n", from, to, label)
	} else {
		fmt.Fprintf(stdout, "escalated from %s to %s in workflow %s\n", from, to, label)
	}
	return 0
}

func runOrgEscalateResolve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org escalate resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	byFlag := fs.String("by", "", "organization role resolving the escalation")
	answerNoteFlag := fs.String("note", "", "workflow note id containing the answer")
	// A bare --help/-h anywhere in args is a skippable flag to the id-extraction
	// scan below (it never sets idIndex), which previously fell through to the
	// generic "requires exactly one escalation note id" error before flag.Parse
	// ever ran — so help never printed. Detect it first and let the flag package
	// handle it the normal way, regardless of where it appears among the other args.
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			_ = fs.Parse([]string{"--help"})
			return 0
		}
	}
	escalationIDText, flagArgs, ok := orgEscalateResolveIDAndFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "org escalate resolve requires exactly one escalation note id")
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org escalate resolve requires exactly one escalation note id")
		return 2
	}
	escalationNoteID, err := strconv.ParseInt(escalationIDText, 10, 64)
	if err != nil || escalationNoteID <= 0 {
		fmt.Fprintf(stderr, "org escalate resolve: invalid escalation note id %q\n", escalationIDText)
		return 2
	}
	var answerNoteID int64
	if value := strings.TrimSpace(*answerNoteFlag); value != "" {
		answerNoteID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || answerNoteID <= 0 {
			fmt.Fprintf(stderr, "org escalate resolve: invalid answer note id %q\n", value)
			return 2
		}
	}

	var workflowID string
	var unaddressedResolution bool
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		target, err := store.GetWorkflowNote(ctx, escalationNoteID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("escalation note %d not found", escalationNoteID)
		}
		if err != nil {
			return err
		}
		asker, to, parsed := orgEscalationResolveParties(target.Body)
		if !parsed {
			return fmt.Errorf("note %d is not an org escalation", escalationNoteID)
		}
		resolvedBy := strings.ToLower(strings.TrimSpace(*byFlag))
		if resolvedBy == "" {
			resolvedBy = to
		}
		body := workflow.FormatOrgEscalateResolvedNote(escalationNoteID, resolvedBy, answerNoteID)
		if body == "" {
			return fmt.Errorf("invalid resolving role %q", resolvedBy)
		}
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID:      target.WorkflowID,
			Author:          resolvedBy,
			Body:            body,
			Repo:            target.Repo,
			AddressedTarget: asker,
		}); err != nil {
			return err
		}
		workflowID = target.WorkflowID
		unaddressedResolution = asker == ""
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "org escalate resolve: %v\n", err)
		return 1
	}
	if unaddressedResolution {
		fmt.Fprintf(stderr, "org escalate resolve: warning: escalation note %d has no identifiable asker; resolution recorded without wake\n", escalationNoteID)
	}
	fmt.Fprintf(stdout, "resolved escalation %d in workflow %s\n", escalationNoteID, workflowID)
	return 0
}

// orgEscalationResolveParties reads the asker and target from a typed
// escalation. The strict parser owns the normal schema. The compatibility path
// accepts only that same schema with its from field absent or empty, allowing a
// legacy resolution to be recorded without inventing an addressed target.
func orgEscalationResolveParties(body string) (asker, to string, ok bool) {
	asker, to, _, _, ok = workflow.ParseOrgEscalateNote(body)
	if ok {
		return asker, to, true
	}
	if !strings.HasPrefix(body, workflow.OrgEscalatePrefix) {
		return "", "", false
	}
	end := strings.IndexByte(body, ']')
	if end < 0 {
		return "", "", false
	}
	fields := strings.Fields(body[len(workflow.OrgEscalatePrefix):end])
	withoutAsker := make([]string, 0, len(fields))
	missingAsker := true
	for _, field := range fields {
		switch {
		case field == "from=":
			continue
		case strings.HasPrefix(field, "from="):
			missingAsker = false
		}
		withoutAsker = append(withoutAsker, field)
	}
	if !missingAsker || len(withoutAsker) != 2 {
		return "", "", false
	}
	reconstructed := workflow.OrgEscalatePrefix +
		strings.Join(withoutAsker, " ") +
		" from=unidentified" +
		body[end:]
	_, to, _, _, ok = workflow.ParseOrgEscalateNote(reconstructed)
	if !ok {
		return "", "", false
	}
	return "", to, true
}

func orgEscalateResolveIDAndFlags(args []string) (string, []string, bool) {
	needsValue := map[string]bool{"--home": true, "--by": true, "--note": true}
	idIndex := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if needsValue[arg] {
			i++
			if i >= len(args) {
				return "", nil, false
			}
			continue
		}
		if arg == "-h" || arg == "--help" || strings.HasPrefix(arg, "--home=") || strings.HasPrefix(arg, "--by=") || strings.HasPrefix(arg, "--note=") || strings.HasPrefix(arg, "-") {
			continue
		}
		if idIndex >= 0 {
			return "", nil, false
		}
		idIndex = i
	}
	if idIndex < 0 {
		return "", nil, false
	}
	flagArgs := make([]string, 0, len(args)-1)
	flagArgs = append(flagArgs, args[:idIndex]...)
	flagArgs = append(flagArgs, args[idIndex+1:]...)
	return strings.TrimSpace(args[idIndex]), flagArgs, strings.TrimSpace(args[idIndex]) != ""
}

func orgEscalateQuestionAndFlags(args []string) (string, []string, bool) {
	needsValue := map[string]bool{"--home": true, "--to": true, "--workflow": true, "--org-role": true, "--repo": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if needsValue[arg] {
			i++
			if i >= len(args) {
				return "", nil, false
			}
			continue
		}
		if arg == "--json" || strings.HasPrefix(arg, "--home=") || strings.HasPrefix(arg, "--to=") || strings.HasPrefix(arg, "--workflow=") || strings.HasPrefix(arg, "--org-role=") || strings.HasPrefix(arg, "--repo=") {
			continue
		}
		if i != len(args)-1 {
			return "", nil, false
		}
		return strings.TrimSpace(arg), args[:i], strings.TrimSpace(arg) != ""
	}
	return "", nil, false
}

var eventRuleKinds = map[string]struct{}{
	"escalation":         {},
	"attention":          {},
	"guard":              {},
	"job-terminal":       {},
	"review-verdict":     {},
	"blocked":            {},
	"recycle-overdue":    {},
	"pane_input_pending": {},
	"reply":              {},
	"directive":          {},
	"fact":               {},
}

func runOrgEvents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgEventRuleUsage(stdout)
		return 0
	}
	if args[0] != "rule" {
		fmt.Fprintf(stderr, "unknown org events command %q\n", args[0])
		return 2
	}
	if len(args) == 1 || args[1] == "-h" || args[1] == "--help" {
		printOrgEventRuleUsage(stdout)
		return 0
	}
	switch args[1] {
	case "add":
		return runOrgEventRuleAdd(args[2:], stdout, stderr)
	case "list":
		return runOrgEventRuleList(args[2:], stdout, stderr)
	case "set-scope":
		return runOrgEventRuleSetScope(args[2:], stdout, stderr)
	case "rm":
		return runOrgEventRuleRemove(args[2:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org events rule command %q\n", args[1])
		return 2
	}
}

func printOrgEventRuleUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org events rule add --on <kind> [--match <filter> | --repo <filter>] --wake <role> [--scope addressed|observer] [--home path]")
	fmt.Fprintln(w, "  gitmoot org events rule list [--home path]")
	fmt.Fprintln(w, "  gitmoot org events rule set-scope [--home path] <id> observer|addressed")
	fmt.Fprintln(w, "  gitmoot org events rule rm [--home path] <id>")
}

func runOrgEventRuleAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	onKind := fs.String("on", "", "event kind: escalation, attention, guard, job-terminal, review-verdict, blocked, recycle-overdue, pane_input_pending, reply, directive, or fact")
	// A slash makes the repository comparison exact while job IDs always retain
	// case-insensitive substring matching. Without a slash, repositories also use
	// substring matching. An empty filter matches every event of the selected kind.
	match := fs.String("match", "", "slash filters match owner/repo exactly and job IDs by substring; other filters use repo or job-ID substrings; empty matches all")
	repo := fs.String("repo", "", "repository alias; owner/name is exact against repo while job-ID substring matching remains active")
	wake := fs.String("wake", "", "organization role to wake")
	scopeFlag := fs.String("scope", string(db.EventRuleScopeAddressed), "rule scope: addressed or observer")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org events rule add does not accept positional arguments")
		return 2
	}
	matchFilter := strings.TrimSpace(*match)
	repoFilter := strings.TrimSpace(*repo)
	if matchFilter != "" && repoFilter != "" {
		fmt.Fprintln(stderr, "org events rule add: --match and --repo cannot both be set; choose one")
		return 2
	}
	if repoFilter != "" {
		matchFilter = repoFilter
	}
	scope := db.EventRuleScope(strings.ToLower(strings.TrimSpace(*scopeFlag)))
	if scope != db.EventRuleScopeAddressed && scope != db.EventRuleScopeObserver {
		fmt.Fprintf(stderr, "unknown event rule scope %q; want addressed or observer\n", scope)
		return 2
	}
	kind := strings.ToLower(strings.TrimSpace(*onKind))
	if _, ok := eventRuleKinds[kind]; !ok {
		fmt.Fprintf(stderr, "unknown event rule kind %q; want escalation, attention, guard, job-terminal, review-verdict, blocked, recycle-overdue, pane_input_pending, reply, directive, or fact\n", kind)
		return 2
	}
	roleName := strings.ToLower(strings.TrimSpace(*wake))
	if roleName == "" {
		fmt.Fprintln(stderr, "org events rule add requires --wake")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: %v\n", err)
		return 1
	}
	role, ok := cfg.Role(roleName)
	if !ok {
		fmt.Fprintf(stderr, "unknown org role %q\n", roleName)
		return 2
	}
	id, err := newEventRuleID()
	if err != nil {
		fmt.Fprintf(stderr, "org events rule add: generate id: %v\n", err)
		return 1
	}
	rule := db.EventRule{ID: id, OnKind: kind, MatchFilter: matchFilter, WakeRole: role.Name, Scope: scope, Enabled: true}
	if err := withStore(*home, func(store *db.Store) error {
		return store.AddEventRule(context.Background(), rule)
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added %s\n", id)
	return 0
}

func runOrgEventRuleList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org events rule list does not accept positional arguments")
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		rules, err := store.ListEventRules(context.Background())
		if err != nil {
			return err
		}
		for _, rule := range rules {
			fmt.Fprintf(stdout, "%s\ton=%s\tmatch=%s\twake=%s\tscope=%s\tenabled=%t\n", rule.ID, rule.OnKind, rule.MatchFilter, rule.WakeRole, rule.Scope, rule.Enabled)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule list: %v\n", err)
		return 1
	}
	return 0
}

func runOrgEventRuleSetScope(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule set-scope", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 2 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "org events rule set-scope requires a rule id and observer|addressed; place --home before the id")
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	scope := db.EventRuleScope(strings.ToLower(strings.TrimSpace(fs.Arg(1))))
	if scope != db.EventRuleScopeAddressed && scope != db.EventRuleScopeObserver {
		fmt.Fprintf(stderr, "unknown event rule scope %q; want addressed or observer\n", scope)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		return store.UpdateEventRuleScope(context.Background(), id, scope)
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule set-scope: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "updated %s scope=%s\n", id, scope)
	return 0
}

func runOrgEventRuleRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org events rule rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "org events rule rm requires exactly one rule id; place --home before the id")
		return 2
	}
	id := strings.TrimSpace(fs.Arg(0))
	if err := withStore(*home, func(store *db.Store) error {
		return store.DeleteEventRule(context.Background(), id)
	}); err != nil {
		fmt.Fprintf(stderr, "org events rule rm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s\n", id)
	return 0
}

func newEventRuleID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "event-rule-" + hex.EncodeToString(raw[:]), nil
}
