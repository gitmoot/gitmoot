package workflow

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
	return distinctiveValuesAtDepth(fieldType, seed, 0)
}

// distinctiveValuesAtDepth bounds recursion. A self-referential field type used
// to CRASH the whole test binary rather than fail (#2188 round 7, P3): a future
// Engine field of a cyclic type would have taken down every test in the package
// while reporting nothing. Past the bound the type is unsynthesisable, which is
// a loud failure in the census rather than a silent skip.
func distinctiveValuesAtDepth(fieldType reflect.Type, seed, depth int) []reflect.Value {
	if depth > 4 {
		return nil
	}
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
	case reflect.Float64, reflect.Float32:
		return []reflect.Value{reflect.ValueOf(float64(seed) + 0.5).Convert(fieldType)}

	// #2188 round 5 (P2): `unprobeable` was decided by GENERATOR CAPABILITY, not
	// by behaviour - so a forward FROM an unprobeable field was invisible by
	// construction, and the check then passed HONESTLY rather than by anyone's
	// oversight. Demonstrated live: `mb.produceCheckDir = e.HighRiskPaths[0]`
	// left the whole package green.
	//
	// The exemption did not disappear when the declared lists went; it moved
	// from human declaration to tool limitation. Widening the generator is the
	// only remedy that removes it rather than relocating it again: composite
	// kinds are now synthesised from their own element types, recursively.
	case reflect.Slice:
		element := distinctiveValuesAtDepth(fieldType.Elem(), seed+1, depth+1)
		if len(element) == 0 {
			return nil
		}
		slice := reflect.MakeSlice(fieldType, 1, 1)
		slice.Index(0).Set(element[0])
		return []reflect.Value{slice}
	case reflect.Map:
		key := distinctiveValuesAtDepth(fieldType.Key(), seed+2, depth+1)
		value := distinctiveValuesAtDepth(fieldType.Elem(), seed+3, depth+1)
		if len(key) == 0 || len(value) == 0 {
			return nil
		}
		mapping := reflect.MakeMap(fieldType)
		mapping.SetMapIndex(key[0], value[0])
		return []reflect.Value{mapping}
	case reflect.Pointer:
		return []reflect.Value{reflect.New(fieldType.Elem())}
	case reflect.Struct:
		// #2188 round 6 (the tenth instance): this case USED TO return the ZERO
		// struct, which is indistinguishable from unset - so DelegationTimeoutDefaults
		// and LedgerResolvers moved from honestly-admitted `unprobeable` to
		// falsely-claimed "observed inert", and a forward from either was
		// invisible while the census reported coverage.
		//
		// THE WIDENING TRADED AN ADMITTED GAP FOR A FALSE PROOF, which is
		// strictly worse: an admitted gap is a known limit, a false proof
		// removes the reason to look. Fields are now filled recursively, and a
		// struct whose fields cannot be filled reports UNFILLABLE rather than
		// inert.
		// ALL-OR-NOTHING (#2188 round 8, P2). This used to accept a PARTIAL fill:
		// one field set was enough, so a struct whose remaining fields hit the
		// depth bound was probed with an incomplete value and reported "observed
		// inert" with confidence. That is the zero-struct false proof from two
		// rounds earlier, one level deeper - a bound added to stop a CRASH
		// created a new channel for a wrong observation inside the stated reach.
		//
		// An incompletely synthesisable struct is now unsynthesisable, which is
		// a loud census failure rather than a quiet inert.
		filled := reflect.New(fieldType).Elem()
		exported := 0
		for i := range fieldType.NumField() {
			if !fieldType.Field(i).IsExported() {
				continue
			}
			exported++
			inner := distinctiveValuesAtDepth(fieldType.Field(i).Type, seed+i+11, depth+1)
			if len(inner) == 0 {
				return nil
			}
			filled.Field(i).Set(inner[0])
		}
		if exported == 0 {
			return nil
		}
		return []reflect.Value{filled}

	// The five interface-typed fields were exempted as "unsynthesisable". That
	// is a claim about the generator, and it is FALSE INSIDE THIS PACKAGE: the
	// stubs already existed. Using them deletes the exemption set rather than
	// relocating it - the first time in this lineage that closing a gap did not
	// create a new place for the class to live.
	case reflect.Interface:
		for _, stub := range []any{
			&recordingSink{}, &recordingNotifier{}, &fakeImplementationFinalizer{},
			&fakeMergeGate{}, &fakeWorktreeManager{},
		} {
			candidate := reflect.ValueOf(stub)
			if candidate.Type().Implements(fieldType) {
				return []reflect.Value{candidate}
			}
		}
		return nil
	default:
		return nil
	}
}

// NAME STATES THE REACH (#2188 round 7). The previous name claimed "every
// engine field", and that was FALSE in three ways at once: Engine.Now reaches
// the mailbox only inside the emitTerminal closure, ReviewBlockingSeverity is
// forwarded as a METHOD VALUE - always non-nil, fixed code pointer, so it reads
// as inert - and Store was skipped by name.
//
// The premise is a single-field probe read from ONE post-construction snapshot.
// Four shapes are outside it BY CONSTRUCTION: closures, method values,
// conditionals and name-skips. Rather than widen the census a twelfth time,
// each is covered by a direct behavioural test and named here:
//
//   - Engine.Now            -> TestEmittedEventTimestampsComeFromTheEnginesClock
//   - ReviewBlockingSeverity -> the reviewBlockingSeverity wiring assertion below
//   - Memory sub-fields      -> the Memory residual assertions below
//   - Store                  -> now probed like everything else
func TestSnapshotVisibleEngineFieldsAreClassifiedByObservation(t *testing.T) {
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
		"Store":          true,
		"ApplyChangeSet": true, "BlockerDeferrer": true, "CollectChangeSet": true,
		"EventSink": true, "Memory": true, "OrgPolicy": true,
		"ProduceCheckDir": true, "RequireWorkflowPolicy": true, "ResolveDeliveryWorktree": true,
		"ResultCheckMode": true, "ReviewModelPool": true, "RouterContextEnabled": true,
		"RuntimeDefaultEffort": true, "RuntimeDefaultModel": true,
	}
	// Observed inert: setting the field alone changes nothing on the mailbox.
	// Every name here was OBSERVED, not assumed - an entry that turns out to be
	// forwarded fails the set equality below, in that direction too.
	expectedInert := map[string]bool{
		"ArtifactRoot": true, "BeforeReadOnlyWorktreeCleanup": true, "DelegationCheckout": true,
		"DelegationTimeoutDefaults": true, "DelegationWorktrees": true, "EscalationNotifier": true,
		"FindingsAdvisory": true, "FixWorktreeAllocator": true, "HighRiskPaths": true,
		"Home": true, "ImplementationFinalizer": true, "InjectUpstreamDepContext": true,
		"InlineArtifactBodies": true, "JobID": true, "LedgerResolvers": true,
		"MaxDelegationCostUSD": true, "MaxDelegationNonProgressStreak": true, "MaxDelegationTokenBudget": true,
		"MaxInlineArtifactBytes": true, "MaxVerifyReplanAttempts": true, "MergeGate": true,
		"NativeReviewFanoutEnabled": true, "Now": true, "OwnerPIDLive": true,
		"PayloadRefresher": true, "PullRequestSignals": true, "RequiredReviewers": true,
		// METHOD-VALUE forward: always non-nil with a fixed code pointer, so the
		// snapshot reads it as inert. Pinned by the wiring assertion below.
		"ReviewBlockingSeverity": true,
		"ReviewChangedFiles":     true, "RiskLabelHigh": true,
		"RiskLabelRoutine": true, "RiskTiersEnabled": true, "WorktreeHasLiveProcess": true,
		"WorktreeLiveness": true,
	}
	// Kinds this generator cannot synthesise a distinguishing value for. Listing
	// one is a statement that it is UNPROBEABLE, not that it is uninteresting.
	// NO EXEMPTION SET. It was five interface-typed fields, justified as
	// "unsynthesisable" - a claim about the generator that was FALSE inside this
	// package, where recordingSink, recordingNotifier, fakeImplementationFinalizer,
	// fakeMergeGate and fakeWorktreeManager already existed. Using them made all
	// five real observations, and EventSink turned out to be FORWARDED, not
	// merely unprobed - the same error the hand-audit made on Memory, found the
	// same way. Every exported Engine field is now observed.

	store := openEngineStore(t)
	baseline := Engine{Store: store}
	baselineSnapshot := mailboxFieldSnapshot(t, baseline.EnqueueMailbox(nil))
	// Store was SKIPPED BY NAME for seven rounds - a one-field exemption in the
	// most literal form available, sitting in plain sight while three exemption
	// lists were deleted for being exemptions. It is probed like everything else
	// now; the probe simply supplies a DIFFERENT store so the diff is real.
	otherStore := openEngineStore(t)

	engineType := reflect.TypeOf(Engine{})
	observedForward := map[string]bool{}
	observedInert := map[string]bool{}
	for i := range engineType.NumField() {
		field := engineType.Field(i)
		if !field.IsExported() {
			continue
		}
		candidates := distinctiveValues(field.Type, i)
		if len(candidates) == 0 {
			// No exemption remains, so this is a FAILURE rather than a category:
			// a field the generator cannot synthesise is a field nothing checks.
			t.Errorf("Engine.%s cannot be synthesised by the probe: extend distinctiveValues "+
				"(a stub for its type), or this field is unchecked", field.Name)
			continue
		}
		changed := false
		for _, candidate := range candidates {
			probe := reflect.New(engineType).Elem()
			probe.FieldByName("Store").Set(reflect.ValueOf(store))
			if field.Name == "Store" {
				probe.Field(i).Set(reflect.ValueOf(otherStore))
			} else {
				probe.Field(i).Set(candidate)
			}
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
			// DO NOT RECOMMEND THE OBSERVATION AS THE ANSWER (#2188 round 8).
			// The snapshot cannot see a forward through a closure, a method
			// value, or a conditional on another field, so "observed inert" is
			// exactly the wrong answer for those shapes - and telling the reader
			// to paste it is the mechanism HANDING OVER a wrong classification
			// rather than merely permitting one. It also offered "unprobeable",
			// a classification that no longer exists.
			hint := ""
			if label == "inert" {
				hint = " - before classifying it inert, check engine_types.go for a forward through a CLOSURE, a METHOD VALUE, " +
					"or a CONDITIONAL on another field: this probe cannot see those, and each has already hidden a real " +
					"forward on this file (Engine.Now, ReviewBlockingSeverity, Memory)"
			}
			t.Errorf("Engine.%s is observed %s but is classified nowhere%s", name, label, hint)
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
// Renamed (#2188 round 8, P3): "ReflectionCannotSee" was accurate when written
// and became FALSE as the probe improved - the snapshot census sees all four
// now. A name that decayed into a claim its assertions no longer carry is the
// same defect this file has been bitten by three times.
func TestUnexportedAndTransformedForwardsCarryTheirEngineValues(t *testing.T) {
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

// #2188 round 7, and the ONLY finding in this lineage over PRODUCTION
// behaviour rather than test machinery.
//
// Engine.Now reaches the mailbox solely inside the emitTerminal CLOSURE, guarded
// by EventSink != nil, so the per-field census - one field, one post-construction
// snapshot - cannot see it by construction. The mutant `e.now()` ->
// `time.Now().Add(24h)` survived the entire package while making EVERY EMITTED
// EVENT TIMESTAMP WRONG.
//
// A closure-captured receiver is one of four shapes outside that census's reach
// (closures, method values, conditionals, name-skips). This asserts the
// behaviour directly instead of widening the census a twelfth time.
func TestEmittedEventTimestampsComeFromTheEnginesClock(t *testing.T) {
	frozen := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	sink := &recordingSink{}
	engine := Engine{
		Store:                   openEngineStore(t),
		ResolveDeliveryWorktree: UnavailableDeliveryWorktreeResolver("test"),
		EventSink:               sink,
		Now:                     func() time.Time { return frozen },
	}

	mailbox := engine.EnqueueMailbox(nil)
	if mailbox.emitTerminal == nil {
		t.Fatal("emitTerminal is not wired with an EventSink set")
	}
	mailbox.emitTerminal(context.Background(), "job-clock", JobSucceeded, JobPayload{Repo: "owner/repo"})

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(sink.events))
	}
	if got, want := sink.events[0].Timestamp, frozen.Format(time.RFC3339); got != want {
		t.Fatalf("event timestamp = %q, want the engine's clock %q: every emitted timestamp is wrong if this drifts", got, want)
	}
}

// #2188 round 9 (P3): the all-or-nothing struct fill was CORRECT AND UNDEFENDED -
// reverting it survived the whole package, because no current Engine field is
// partially fillable. A guard whose subject does not exist in production cannot
// be tested by production code, which is this lineage's thirteenth shape.
//
// So the subject is constructed here. These types exist only to be probed: one
// fully fillable, one whose tail cannot be synthesised. The guard's contract is
// that the first is OBSERVED and the second is refused rather than reported as
// an incomplete-value probe.
type zzFillable struct {
	Name  string
	Count int
	Inner zzFillableInner
}

type zzFillableInner struct{ Flag bool }

type zzPartiallyFillable struct {
	Name string
	Ch   chan int // unsynthesisable: no distinguishing value exists
}

func TestStructSynthesisIsAllOrNothing(t *testing.T) {
	fillable := distinctiveValues(reflect.TypeOf(zzFillable{}), 1)
	if len(fillable) != 1 {
		t.Fatalf("a fully fillable struct produced %d candidates, want 1 - the guard rejects valid input", len(fillable))
	}
	// MUST-SUCCEED HALF: every field actually carries a distinguishing value,
	// so the probe cannot report a zero struct as "filled".
	got := fillable[0].Interface().(zzFillable)
	if got.Name == "" || got.Count == 0 || !got.Inner.Flag {
		t.Fatalf("fully fillable struct came back partly zero: %+v", got)
	}

	// REFUSAL HALF: one unsynthesisable field makes the whole struct
	// unsynthesisable. Accepting a partial fill is what manufactured a
	// confident "observed inert" for a field the probe never really set.
	if partial := distinctiveValues(reflect.TypeOf(zzPartiallyFillable{}), 2); len(partial) != 0 {
		t.Fatalf("a partially fillable struct produced %d candidates: the census would report it inert on an incomplete probe", len(partial))
	}
}
