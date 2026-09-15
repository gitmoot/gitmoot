package workflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2180. The pool is resolved at the enqueue chokepoint every producer passes,
// not at each dispatch site. Per-site arming is the defect: `gitmoot review
// request` armed the pool at its own construction site and every other producer
// - `gitmoot agent review` and the engine's own review legs - enqueued with an
// EMPTY pool, which is indistinguishable from a pool with no alternatives and
// therefore made the provider fallback unreachable for them.
type testStoreHandle struct{ store *db.Store }

func (h *testStoreHandle) payload(t *testing.T, prepared PreparedEnqueue) JobPayload {
	t.Helper()
	payload, err := ParseJobPayload(prepared.Job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	return payload
}

func TestPrepareEnqueueResolvesAReviewPoolAndLeavesNonReviewJobsPoolLess(t *testing.T) {
	configured := []string{"sentinel/router-a", "sentinel/router-b"}
	var askedFor []string

	newMailbox := func(t *testing.T) (Mailbox, *testStoreHandle) {
		t.Helper()
		store := openEngineStore(t)
		seedAgent(t, store, "reviewer", []string{"review", "implement"}, "owner/repo")
		mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))
		mailbox.ReviewModelPool = func(purpose string) []string {
			askedFor = append(askedFor, purpose)
			return configured
		}
		return mailbox, &testStoreHandle{store: store}
	}

	t.Run("a review is armed with the configured pool for its purpose", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
			ReviewPurpose: "security",
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		payload := h.payload(t, prepared)
		if !slices.Equal(payload.ReviewModelPool, configured) {
			t.Fatalf("pool = %v, want %v", payload.ReviewModelPool, configured)
		}
		if !slices.Equal(askedFor, []string{"security"}) {
			t.Fatalf("resolver asked for %v, want the request's own purpose", askedFor)
		}
		// The pool arms the fallback; it must not choose a model for an agent
		// the operator named (#2186 review, F1).
		if payload.Model != "" {
			t.Fatalf("model = %q, want the agent's own", payload.Model)
		}
	})

	t.Run("a non-review job stays pool-less", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-implement", Agent: "reviewer", Action: "implement", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if pool := h.payload(t, prepared).ReviewModelPool; len(pool) != 0 {
			t.Fatalf("implement job acquired pool %v: the claim walk and the blocker both read this field", pool)
		}
		if len(askedFor) != 0 {
			t.Fatalf("resolver consulted for a non-review job: %v", askedFor)
		}
	})

	t.Run("a caller-supplied pool is preserved verbatim", func(t *testing.T) {
		askedFor = nil
		mailbox, h := newMailbox(t)
		chosen := []string{"kimi-code/k3"}
		prepared, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-chosen", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
			ReviewPurpose: "code", ReviewModelPool: chosen,
		})
		if err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if pool := h.payload(t, prepared).ReviewModelPool; !slices.Equal(pool, chosen) {
			t.Fatalf("pool = %v, want the caller's %v", pool, chosen)
		}
		if len(askedFor) != 0 {
			t.Fatalf("resolver consulted despite a caller pool: %v", askedFor)
		}
	})

	t.Run("an unstated purpose resolves the code chain", func(t *testing.T) {
		askedFor = nil
		mailbox, _ := newMailbox(t)
		if _, err := mailbox.PrepareEnqueue(context.Background(), JobRequest{
			ID: "job-nopurpose", Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40),
		}); err != nil {
			t.Fatalf("PrepareEnqueue: %v", err)
		}
		if !slices.Equal(askedFor, []string{"code"}) {
			t.Fatalf("resolver asked for %v, want the code default", askedFor)
		}
	})
}

// The engine half. Engine-produced review legs - native fanout, follow-up
// rounds, delegated reviews - never touch the CLI, so a resolver wired only
// into CLI dispatch leaves the fallback unreachable for every review the daemon
// produces on its own. Asserted through a REAL enqueue rather than by reading
// the field back, so the copy at engine_types.go and the resolution at the
// chokepoint are proved together.
func TestEngineProducedReviewsCarryTheResolvedPool(t *testing.T) {
	store := openEngineStore(t)
	seedAgent(t, store, "engine-reviewer", []string{"review"}, "owner/repo")
	// FIXTURE RULE, not a fixture fix (#2186 round 4, M6): this pool must differ
	// from the BUILT-IN default, or a test cannot tell COPY from INHERIT and the
	// mutant deleting the copy survives. Round 2 hit the identical vacuity with a
	// pool equal to the config pool; fixing that instance did not stop the next
	// test from repeating it, so the rule is now stated where the fixture is
	// written: review-pool fixtures use sentinel names no default can produce.
	configured := []string{"sentinel/engine-a", "sentinel/engine-b"}
	engine := Engine{
		Store:                   store,
		ResolveDeliveryWorktree: UnavailableDeliveryWorktreeResolver("test"),
		ReviewModelPool:         func(string) []string { return configured },
	}

	prepared, err := engine.mailbox().PrepareEnqueue(context.Background(), JobRequest{
		ID: "job-engine-review", Agent: "engine-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 7, HeadSHA: strings.Repeat("c", 40), ReviewPurpose: "code",
	})
	if err != nil {
		t.Fatalf("PrepareEnqueue: %v", err)
	}
	payload, err := ParseJobPayload(prepared.Job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(payload.ReviewModelPool, configured) {
		t.Fatalf("engine review pool = %v, want %v: the resolver does not reach engine-produced reviews", payload.ReviewModelPool, configured)
	}
}

// THE CLASS FIX, third attempt, and the first two are why this one is written
// this way (#2186 round 4).
//
// Round 3 armed the resolver per site across eleven files and the daemon's
// PR-comment producer was the site nobody armed. Round 3's remedy - defaulting
// the resolver at construction - was WORSE for that producer: it read the
// DEFAULT config home rather than the daemon's, and since the built-in router
// settings seed a pool, the job carried a plausible-but-wrong pool with no
// advisory. An empty pool that announces itself beats a wrong pool that does
// not.
//
// The property that ends the class: a holder of an Engine cannot enqueue
// through a LESS configured mailbox than the engine's own, because it does not
// build one. No field list to forget the next field from.
// THE CENSUS IS EXECUTED, NOT DECLARED (#2188 round 3).
//
// Three previous versions each replaced a declared list with a better declared
// list, and the reviewer's finding each time was the declaration itself:
//
//  1. a hand-enumerated list of two fields, named "every";
//  2. a reflective census plus `unforwardedByDesign`, which caught forgetting to
//     classify and could not catch MISclassifying;
//  3. a source-parsed copy list plus `handAsserted` - a SECOND trusted list, and
//     adding a name to it silenced the coverage failure with zero assertions
//     written. A guard satisfiable by DECLARING rather than by PROVING is not a
//     guard, and the reviewer predicted that specific mechanism in advance.
//
// So no list decides anything here. For each exported Engine field, this sets
// THAT FIELD ALONE to a distinctive non-zero value, builds a mailbox, and
// compares every Mailbox field - exported and unexported alike - against a
// mailbox built from an otherwise identical zero Engine. A field that reaches
// the mailbox is OBSERVED reaching it; a field that does not is observed not
// to. `expectedInert` below cannot create coverage: naming a field that DOES
// influence the mailbox fails, exactly as omitting one that does not.
//
// This also drops the source parser, with its hardcoded filename, its
// working-directory dependency, and its floor of eight against nineteen
// assignments that let a degraded parse look clean while seven forwards went
// unaudited.
func mailboxFieldSnapshot(t *testing.T, mailbox Mailbox) map[string]string {
	t.Helper()
	value := reflect.ValueOf(&mailbox).Elem()
	snapshot := map[string]string{}
	for i := range value.NumField() {
		field := value.Type().Field(i)
		cell := value.Field(i)
		if !cell.CanInterface() {
			cell = reflect.NewAt(cell.Type(), unsafe.Pointer(cell.UnsafeAddr())).Elem()
		}
		switch cell.Kind() {
		case reflect.Func, reflect.Map, reflect.Slice, reflect.Ptr, reflect.Interface, reflect.Chan:
			if cell.IsNil() {
				snapshot[field.Name] = "nil"
			} else {
				snapshot[field.Name] = fmt.Sprintf("set:%v", cell.Pointer())
			}
		default:
			snapshot[field.Name] = fmt.Sprintf("%v", cell.Interface())
		}
	}
	return snapshot
}

// distinctiveValues returns EVERY candidate worth trying for a field, not one.
//
// A single arbitrary string reports a false INERT wherever the forward
// normalises its input: `mb.resultCheckMode = normalizeResultCheckMode(...)`
// maps an unrecognised value to the zero mode, so "sentinel-12" arrives
// indistinguishable from unset. The probe must not conclude "not forwarded"
// from "my value was rejected", so it tries the domain's own vocabulary too and
// treats influence by ANY candidate as forwarded.
func distinctiveValues(fieldType reflect.Type, seed int) []reflect.Value {
	switch fieldType.Kind() {
	case reflect.Func:
		return []reflect.Value{reflect.MakeFunc(fieldType, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, fieldType.NumOut())
			for j := range out {
				out[j] = reflect.Zero(fieldType.Out(j))
			}
			return out
		})}
	case reflect.Bool:
		return []reflect.Value{reflect.ValueOf(true).Convert(fieldType)}
	case reflect.String:
		var values []reflect.Value
		for _, candidate := range []string{fmt.Sprintf("sentinel-%d", seed), "block", "warn", "on", "true"} {
			values = append(values, reflect.ValueOf(candidate).Convert(fieldType))
		}
		return values
	case reflect.Int, reflect.Int64, reflect.Int32:
		return []reflect.Value{reflect.ValueOf(int64(seed + 7)).Convert(fieldType)}
	default:
		return nil
	}
}

func TestEveryEngineFieldThatReachesTheMailboxIsObservedReachingIt(t *testing.T) {
	// SET EQUALITY IN BOTH DIRECTIONS (#2188 round 4). The previous version's
	// `mustForward` was obligation-only, which I argued made it safe. It did not:
	// DELETING a name passed silently, which is the same edit direction the three
	// deleted lists failed on - I had only closed the direction nobody uses.
	//
	// Worse, that rewrite REGRESSED two working properties. Round 3's source
	// parser, blind in four ways, still FAILED on a newly added forward; the
	// execution census did not, so "forgot to forward" was pinned only for the
	// names already written down. Deleting the exemption SEMANTICS was right;
	// deleting the mechanisms carrying them threw away detection and provenance.
	//
	// Every exported Engine field must therefore appear in EXACTLY ONE of these
	// three sets, and each set must match what execution observes, exactly. A new
	// field fails until classified, whichever way it behaves; a deleted name
	// fails because observation still reports it.
	mustForward := map[string]bool{
		"ResolveDeliveryWorktree": true, "CollectChangeSet": true, "ApplyChangeSet": true,
		"RequireWorkflowPolicy": true, "OrgPolicy": true, "ProduceCheckDir": true,
		"BlockerDeferrer": true, "RouterContextEnabled": true, "RuntimeDefaultModel": true,
		"ReviewModelPool": true, "RuntimeDefaultEffort": true, "ResultCheckMode": true,
	}
	// Observed inert: setting the field alone changes nothing on the mailbox.
	// Every name here was OBSERVED, not assumed - an entry that turns out to be
	// forwarded fails the set equality below, in that direction too.
	expectedInert := map[string]bool{
		"ArtifactRoot": true, "BeforeReadOnlyWorktreeCleanup": true, "DelegationCheckout": true,
		"FindingsAdvisory": true, "FixWorktreeAllocator": true, "Home": true,
		"InjectUpstreamDepContext": true, "InlineArtifactBodies": true, "JobID": true,
		"MaxDelegationNonProgressStreak": true, "MaxDelegationTokenBudget": true,
		"MaxInlineArtifactBytes": true, "MaxVerifyReplanAttempts": true,
		"NativeReviewFanoutEnabled": true, "Now": true, "OwnerPIDLive": true,
		"PayloadRefresher": true, "PullRequestSignals": true, "ReviewBlockingSeverity": true,
		"ReviewChangedFiles": true, "RiskLabelHigh": true, "RiskLabelRoutine": true,
		"RiskTiersEnabled": true, "WorktreeHasLiveProcess": true, "WorktreeLiveness": true,
	}
	// Kinds this generator cannot synthesise a distinguishing value for. Listing
	// one is a statement that it is UNPROBEABLE, not that it is uninteresting.
	unprobeable := map[string]bool{
		"DelegationTimeoutDefaults": true, "DelegationWorktrees": true, "EscalationNotifier": true,
		"EventSink": true, "HighRiskPaths": true, "ImplementationFinalizer": true,
		"LedgerResolvers": true, "MaxDelegationCostUSD": true, "Memory": true,
		"MergeGate": true, "RequiredReviewers": true,
	}

	store := openEngineStore(t)
	baseline := Engine{Store: store}
	baselineSnapshot := mailboxFieldSnapshot(t, baseline.EnqueueMailbox(nil))

	engineType := reflect.TypeOf(Engine{})
	observedForward := map[string]bool{}
	observedInert := map[string]bool{}
	observedUnprobeable := map[string]bool{}
	for i := range engineType.NumField() {
		field := engineType.Field(i)
		if !field.IsExported() || field.Name == "Store" {
			continue
		}
		candidates := distinctiveValues(field.Type, i)
		if len(candidates) == 0 {
			observedUnprobeable[field.Name] = true
			continue
		}
		changed := false
		for _, candidate := range candidates {
			probe := reflect.New(engineType).Elem()
			probe.FieldByName("Store").Set(reflect.ValueOf(store))
			probe.Field(i).Set(candidate)
			engine := probe.Addr().Interface().(*Engine)
			for name, got := range mailboxFieldSnapshot(t, engine.EnqueueMailbox(nil)) {
				if got != baselineSnapshot[name] {
					changed = true
					break
				}
			}
			if changed {
				break
			}
		}
		if changed {
			observedForward[field.Name] = true
		} else {
			observedInert[field.Name] = true
		}
	}

	requireSameSet(t, "forwarded", mustForward, observedForward)
	requireSameSet(t, "inert", expectedInert, observedInert)
	requireSameSet(t, "unprobeable", unprobeable, observedUnprobeable)

	// Method-derived and sub-field forwards, invisible to the value diff above.
	if baselineSnapshot["reviewBlockingSeverity"] == "nil" {
		t.Error("Mailbox.reviewBlockingSeverity is not wired: a method-derived forward is invisible to the diff")
	}
	withMemory := Engine{Store: store, Memory: &MemoryController{}}
	memorySnapshot := mailboxFieldSnapshot(t, withMemory.EnqueueMailbox(nil))
	for _, name := range []string{"injectMemory", "recordMemory"} {
		if memorySnapshot[name] == "nil" {
			t.Errorf("Mailbox.%s is nil with a non-nil Memory: the sub-field forward is gone", name)
		}
	}
}

// requireSameSet reports BOTH directions, because each catches a different
// edit: an unexpected member is a field nobody classified, and a missing member
// is a property that was proved once and has since been deleted.
func requireSameSet(t *testing.T, label string, declared, observed map[string]bool) {
	t.Helper()
	for name := range observed {
		if !declared[name] {
			t.Errorf("Engine.%s is observed %s but is classified nowhere: classify it as forwarded, inert or unprobeable", name, label)
		}
	}
	for name := range declared {
		if !observed[name] {
			t.Errorf("Engine.%s is declared %s but is not observed %s: the behaviour it pinned is gone", name, label, label)
		}
	}
}

// PROVENANCE, restored (#2188 round 4). Its deletion was a regression: cross-
// wiring `mb.RuntimeDefaultModel = e.RuntimeDefaultEffort` survived the whole
// package, because non-nil proves a field was SET, not that it was set from its
// counterpart. Two same-signature forwards can be swapped silently.
func TestForwardedFuncFieldsComeFromTheirOwnCounterpart(t *testing.T) {
	answered := ""
	engineValue := reflect.New(reflect.TypeOf(Engine{})).Elem()
	engineType := engineValue.Type()
	mailboxType := reflect.TypeOf(Mailbox{})

	var checked []string
	for i := range engineType.NumField() {
		field := engineType.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Func {
			continue
		}
		mailboxField, ok := mailboxType.FieldByName(field.Name)
		if !ok || !mailboxField.IsExported() || mailboxField.Type != field.Type {
			continue
		}
		name, fieldType := field.Name, field.Type
		engineValue.Field(i).Set(reflect.MakeFunc(fieldType, func([]reflect.Value) []reflect.Value {
			answered = name
			out := make([]reflect.Value, fieldType.NumOut())
			for j := range out {
				out[j] = reflect.Zero(fieldType.Out(j))
			}
			return out
		}))
		checked = append(checked, name)
	}
	if len(checked) < 5 {
		t.Fatalf("provenance covers only %d fields (%v); the probe is broken", len(checked), checked)
	}

	engine := engineValue.Addr().Interface().(*Engine)
	engine.Store = openEngineStore(t)
	engine.ResolveDeliveryWorktree = UnavailableDeliveryWorktreeResolver("test")
	mailbox := reflect.ValueOf(engine.EnqueueMailbox(nil))

	for _, name := range checked {
		answered = ""
		callFuncWithZeroArgs(mailbox.FieldByName(name))
		if answered != name {
			t.Errorf("Mailbox.%s is wired from Engine.%s, not from its counterpart", name, answered)
		}
	}
}

// #2188 review, M9: the PR's headline fix was UNPINNED. Deleting the line that
// installs the caller's sentinel passed every test, including the daemon ones -
// their `delivered` flag is unreachable because the resolver never fires during
// Enqueue, so they re-proved pool inheritance under a name that promised more.
//
// The assertion has to be in-package: resolveDeliveryWorktree is unexported, so
// the daemon package structurally cannot see what it is being handed. Driving
// the seam itself is the only honest check - invoke the delivery path and
// require the SENTINEL's refusal, not the engine's real resolver.
func TestEnqueueMailboxInstallsTheCallersDeliverySentinel(t *testing.T) {
	realResolverRan := false
	engine := Engine{
		Store: openEngineStore(t),
		ResolveDeliveryWorktree: func(context.Context, db.Job, JobPayload) (DeliveryWorktreeResolution, error) {
			realResolverRan = true
			return DeliveryWorktreeResolution{Path: "/tmp/engine-owned-checkout"}, nil
		},
	}

	mailbox := engine.EnqueueMailbox(UnavailableDeliveryWorktreeResolver("daemon comment enqueue"))
	_, err := mailbox.deliveryWorktree(context.Background(), db.Job{ID: "job-1"}, JobPayload{})
	if err == nil {
		t.Fatal("delivery resolved: the caller's refusal sentinel was not installed")
	}
	if !strings.Contains(err.Error(), "daemon comment enqueue") {
		t.Fatalf("error = %v, want the caller's sentinel naming its site", err)
	}
	if realResolverRan {
		t.Fatal("the engine's real delivery resolver ran: the sentinel was replaced, not installed")
	}

	// The inverse direction, so the test cannot pass by refusing everything: a
	// caller that passes no sentinel inherits the engine's real resolver.
	if _, err := engine.EnqueueMailbox(nil).deliveryWorktree(context.Background(), db.Job{ID: "job-2"}, JobPayload{}); err != nil {
		t.Fatalf("EnqueueMailbox(nil) refused delivery: %v", err)
	}
	if !realResolverRan {
		t.Fatal("EnqueueMailbox(nil) did not inherit the engine's delivery resolver")
	}
}

// The four forwards reflection structurally cannot see, each asserted on a
// distinctive value. Three of these mutants (routerContextEnabled,
// resultCheckMode, produceCheckDir) survived the entire package before this
// test existed, and ResultCheckMode had no entry in the declared list at all.
func TestEnqueueMailboxForwardsFieldsReflectionCannotSee(t *testing.T) {
	engine := Engine{
		Store:                   openEngineStore(t),
		ResolveDeliveryWorktree: UnavailableDeliveryWorktreeResolver("test"),
		BlockerDeferrer:         func(context.Context, string, error) (bool, error) { return false, nil },
		RouterContextEnabled:    true,
		ResultCheckMode:         ResultChecksBlock,
		ProduceCheckDir:         "/tmp/sentinel-produce-dir",
	}
	mailbox := engine.EnqueueMailbox(UnavailableDeliveryWorktreeResolver("test"))

	if !mailbox.routerContextEnabled {
		t.Error("RouterContextEnabled -> routerContextEnabled not forwarded")
	}
	if mailbox.resultCheckMode != ResultChecksBlock {
		t.Errorf("resultCheckMode = %q, want %q", mailbox.resultCheckMode, ResultChecksBlock)
	}
	if mailbox.produceCheckDir != "/tmp/sentinel-produce-dir" {
		t.Errorf("produceCheckDir = %q, want the engine's", mailbox.produceCheckDir)
	}
	if mailbox.deferBlocker == nil {
		t.Error("BlockerDeferrer -> deferBlocker not forwarded")
	}
}

// callFuncWithZeroArgs invokes fn with zero values for every parameter, so a
// provenance stub can report which field it belongs to.
func callFuncWithZeroArgs(fn reflect.Value) {
	fnType := fn.Type()
	args := make([]reflect.Value, fnType.NumIn())
	for i := range args {
		args[i] = reflect.Zero(fnType.In(i))
	}
	fn.Call(args)
}
