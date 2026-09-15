package workflow

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

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
// Renamed from ...ForwardsEveryEngineResolver, which OVERCLAIMED: it can only
// see EXPORTED func fields whose names and types match on both types. The #2188
// review demonstrated three blind spots, all now closed or declared:
//
//	a. PROVENANCE. Mutating `mb.RuntimeDefaultModel = e.RuntimeDefaultModel` to
//	   `= e.RuntimeDefaultEffort` (same signature) passed the old census and the
//	   whole package: asserting non-nil proves a field was SET, not that it was
//	   set from its counterpart. Each field now gets a stub that returns its own
//	   name, so a cross-wire is visible.
//	b. INVISIBLE FORWARDS. Renamed or unexported forwards - BlockerDeferrer ->
//	   deferBlocker above all - cannot be seen structurally. They are asserted
//	   explicitly below; a reflective test that silently omits them is how the
//	   deferBlocker drop survived to be caught only by a cli E2E.
//	c. SILENT SKIPS. The skip rule could not distinguish "not meant to be
//	   forwarded" from "somebody forgot the Mailbox field". Unmatched Engine func
//	   fields are now compared against a declared list, so a NEW one fails here
//	   until a human classifies it.
func TestEnqueueMailboxForwardsExportedEngineFuncFields(t *testing.T) {
	// Engine func fields with deliberately no same-named Mailbox counterpart.
	// Adding to this list is a decision; forgetting to is now a test failure.
	unforwardedByDesign := map[string]string{
		// Forwarded, but not visibly: installed through a parameter or onto an
		// unexported field, so reflection over matching names cannot see them.
		"ResolveDeliveryWorktree": "installed via the EnqueueMailbox delivery parameter, not inherited",
		"BlockerDeferrer":         "forwarded to the unexported deferBlocker; asserted explicitly below",
		"RouterContextEnabled":    "forwarded to unexported routerContextEnabled; asserted in TestEnqueueMailboxForwardsFieldsReflectionCannotSee",
		"ProduceCheckDir":         "forwarded to unexported produceCheckDir; asserted in TestEnqueueMailboxForwardsFieldsReflectionCannotSee",
		"ResultCheckMode":         "forwarded to unexported resultCheckMode; asserted in TestEnqueueMailboxForwardsFieldsReflectionCannotSee",

		// Engine-only. Verified mechanically rather than asserted: no
		// `mb.X = e.X` assignment exists for any of these in EnqueueMailbox's
		// copy list, and Mailbox declares no counterpart field. The census
		// listed all thirteen as silent skips before this list existed.
		"JobID":                         "engine-only",
		"PayloadRefresher":              "engine-only",
		"FixWorktreeAllocator":          "engine-only",
		"BeforeReadOnlyWorktreeCleanup": "engine-only",
		"OwnerPIDLive":                  "engine-only",
		"WorktreeHasLiveProcess":        "engine-only",
		"WorktreeLiveness":              "engine-only",
		"Now":                           "engine-only",
		"NativeReviewFanoutEnabled":     "engine-only",
		"ReviewBlockingSeverity":        "engine-only; the mailbox gets the engine's METHOD, not this field",
		"FindingsAdvisory":              "engine-only",
		"PullRequestSignals":            "engine-only",
		"ReviewChangedFiles":            "engine-only",
	}

	engineValue := reflect.New(reflect.TypeOf(Engine{})).Elem()
	engineType := engineValue.Type()
	mailboxType := reflect.TypeOf(Mailbox{})

	provenance := map[string]string{}
	var forwarded []string
	for i := range engineType.NumField() {
		field := engineType.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Func {
			continue
		}
		mailboxField, ok := mailboxType.FieldByName(field.Name)
		matched := ok && mailboxField.IsExported() && mailboxField.Type == field.Type
		if !matched {
			if _, declared := unforwardedByDesign[field.Name]; !declared {
				t.Fatalf("Engine.%s is a func field with no forwarded Mailbox counterpart and is not declared unforwarded-by-design: "+
					"either forward it in EnqueueMailbox or add it to the list with a reason", field.Name)
			}
			continue
		}
		name := field.Name
		fieldType := field.Type
		engineValue.Field(i).Set(reflect.MakeFunc(fieldType, func([]reflect.Value) []reflect.Value {
			provenance["last"] = name
			out := make([]reflect.Value, fieldType.NumOut())
			for j := range out {
				out[j] = reflect.Zero(fieldType.Out(j))
			}
			return out
		}))
		forwarded = append(forwarded, name)
	}
	if len(forwarded) < 2 {
		t.Fatalf("reflection found %d forwardable resolvers (%v); the census itself is broken", len(forwarded), forwarded)
	}

	engine := engineValue.Addr().Interface().(*Engine)
	engine.Store = openEngineStore(t)
	engine.ResolveDeliveryWorktree = UnavailableDeliveryWorktreeResolver("test")
	engine.BlockerDeferrer = func(context.Context, string, error) (bool, error) { return false, nil }

	deliveryScoped := map[string]bool{"CollectChangeSet": true, "ApplyChangeSet": true}

	inherited := reflect.ValueOf(engine.EnqueueMailbox(nil))
	for _, name := range forwarded {
		value := inherited.FieldByName(name)
		if value.IsNil() {
			t.Fatalf("EnqueueMailbox(nil) did not forward %s", name)
		}
		// PROVENANCE: call it and require ITS OWN stub to answer. A field wired
		// from a same-signature sibling answers with the sibling's name.
		callFuncWithZeroArgs(value)
		if provenance["last"] != name {
			t.Fatalf("Mailbox.%s is wired from Engine.%s, not from its counterpart", name, provenance["last"])
		}
	}

	// (b) the unexported forward the census structurally cannot see.
	if engine.EnqueueMailbox(nil).deferBlocker == nil {
		t.Fatal("BlockerDeferrer was not forwarded to deferBlocker: invisible to reflection, so asserted by hand")
	}

	sentineled := reflect.ValueOf(engine.EnqueueMailbox(UnavailableDeliveryWorktreeResolver("test")))
	for _, name := range forwarded {
		isNil := sentineled.FieldByName(name).IsNil()
		if deliveryScoped[name] && !isNil {
			t.Fatalf("%s survived a delivery sentinel: the producer refuses delivery but now carries the means to perform it", name)
		}
		if !deliveryScoped[name] && isNil {
			t.Fatalf("EnqueueMailbox dropped config-bearing %s alongside the delivery machinery", name)
		}
	}
	t.Logf("censused %d exported func forwards: %v", len(forwarded), forwarded)
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

// #2186 round 5, live survivor: the advisory's NEGATIVE direction was untested.
// A mutant emitting review_pool_unresolved on EVERY review survived both full
// packages - nothing failed when a review that HAD a pool was also told it had
// none. An advisory that fires unconditionally is noise, and noise is how a
// real "this review has no fallback" signal stops being read.
func TestPoolUnresolvedAdvisoryIsSilentWhenThePoolResolves(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "reviewer", []string{"review", "implement"}, "owner/repo")
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))
	mailbox.ReviewModelPool = func(string) []string { return []string{"sentinel/pool-a"} }

	for _, tc := range []struct {
		name         string
		action       string
		wantPool     bool
		wantAdvisory bool
	}{
		{"a review whose pool resolves is not warned", "review", true, false},
		{"a non-review job is neither pooled nor warned", "implement", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "job-" + strings.ReplaceAll(tc.name, " ", "-")
			prepared, err := mailbox.PrepareEnqueue(ctx, JobRequest{
				ID: id, Agent: "reviewer", Action: tc.action, Repo: "owner/repo",
				Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40), ReviewPurpose: "code",
			})
			if err != nil {
				t.Fatalf("PrepareEnqueue: %v", err)
			}
			payload, err := ParseJobPayload(prepared.Job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(payload.ReviewModelPool) > 0; got != tc.wantPool {
				t.Fatalf("pool present = %v, want %v", got, tc.wantPool)
			}
			advisory := false
			for _, event := range prepared.Events {
				if event.Kind == "review_pool_unresolved" {
					advisory = true
				}
			}
			if advisory != tc.wantAdvisory {
				t.Fatalf("advisory emitted = %v, want %v: an advisory that fires regardless says nothing", advisory, tc.wantAdvisory)
			}
		})
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

// forwardedEngineFields reads the ACTUAL copy list out of engine_types.go.
//
// #2188 round 2 (P2): `unforwardedByDesign` was TRUSTED, never verified - its
// "verified mechanically" comment was prose, not a mechanism. So it caught
// forgetting to classify a field and could not catch MIScflassifying one, which
// is how two of its entries came to describe forwards the reflective census
// cannot even see. A list whose correctness is asserted rather than checked,
// guarding against a defect whose signature is asserting more than you check.
//
// Reading the source is the cheapest thing that cannot drift: the census now
// compares its beliefs against the file that performs the forwarding.
func forwardedEngineFields(t *testing.T) map[string]string {
	t.Helper()
	source, err := os.ReadFile("engine_types.go")
	if err != nil {
		t.Fatalf("read engine_types.go: %v", err)
	}
	body := string(source)
	start := strings.Index(body, "func (e Engine) mailbox() Mailbox {")
	if start < 0 {
		t.Fatal("mailbox() not found: the census cannot read the copy list it audits")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("mailbox() has no terminator")
	}
	// The trailing group distinguishes a DIRECT field forward (`e.X`) from a
	// sub-field one (`e.Memory.injectBlock`). Collapsing them would have let the
	// census demand a hand assertion for a struct field, which is how an
	// exemption list grows entries that are really parser bugs.
	assignment := regexp.MustCompile(`mb\.(\w+)\s*=\s*[^\n]*\be\.(\w+)(\.\w+)?`)
	forwarded := map[string]string{}
	for _, match := range assignment.FindAllStringSubmatch(body[start:start+end], -1) {
		if match[3] != "" {
			continue
		}
		forwarded[match[2]] = match[1]
	}
	if len(forwarded) < 8 {
		t.Fatalf("parsed only %d forwards (%v); the parser is broken, not the code", len(forwarded), forwarded)
	}
	return forwarded
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

// The P2 itself: a declared exemption must not describe a field the copy list
// actually forwards, and every forward must be covered by SOMETHING.
func TestUnforwardedByDesignMatchesTheRealCopyList(t *testing.T) {
	forwarded := forwardedEngineFields(t)

	// Covered by TestEnqueueMailboxForwardsFieldsReflectionCannotSee above.
	handAsserted := map[string]bool{
		"BlockerDeferrer": true, "RouterContextEnabled": true,
		"ResultCheckMode": true, "ProduceCheckDir": true,
	}
	engineType := reflect.TypeOf(Engine{})
	mailboxType := reflect.TypeOf(Mailbox{})

	for source, destination := range forwarded {
		field, ok := engineType.FieldByName(source)
		if !ok || !field.IsExported() {
			continue // e.Memory.injectBlock and friends: not a direct field forward
		}
		if handAsserted[source] {
			continue
		}
		mailboxField, matched := mailboxType.FieldByName(source)
		reflectivelyCovered := matched && mailboxField.IsExported() && mailboxField.Type == field.Type &&
			field.Type.Kind() == reflect.Func
		if !reflectivelyCovered {
			t.Errorf("Engine.%s is forwarded to mb.%s but no census covers it: add a hand assertion, "+
				"because reflection over matching exported func fields cannot see this shape", source, destination)
		}
	}
}
