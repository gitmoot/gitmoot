package workflow

import (
	"context"
	"reflect"
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
	configured := []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}
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
func TestEnqueueMailboxForwardsEveryEngineResolver(t *testing.T) {
	// REFLECTIVE ON PURPOSE (#2186 round 5). The first version of this test was
	// named for exhaustiveness and asserted ONE field, which is the same
	// enumeration defect it exists to prevent, one level up: it would pass
	// forever while the tenth field went unforwarded. A test whose name claims
	// more than its assertions is worse than no test, because it answers the
	// question nobody re-asks.
	//
	// So: set EVERY func-typed exported field on Engine to a live stub, build the
	// mailbox, and require every identically-named, identically-typed exported
	// field on Mailbox to be non-nil. A new resolver added to both types is
	// covered the day it is added, with no edit here.
	engineValue := reflect.New(reflect.TypeOf(Engine{})).Elem()
	engineType := engineValue.Type()
	mailboxType := reflect.TypeOf(Mailbox{})

	var expected []string
	for i := range engineType.NumField() {
		field := engineType.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Func {
			continue
		}
		mailboxField, ok := mailboxType.FieldByName(field.Name)
		if !ok || !mailboxField.IsExported() || mailboxField.Type != field.Type {
			continue
		}
		fieldType := field.Type
		engineValue.Field(i).Set(reflect.MakeFunc(fieldType, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, fieldType.NumOut())
			for j := range out {
				out[j] = reflect.Zero(fieldType.Out(j))
			}
			return out
		}))
		expected = append(expected, field.Name)
	}
	if len(expected) < 2 {
		t.Fatalf("reflection found %d forwardable resolvers (%v); the census itself is broken", len(expected), expected)
	}

	engine := engineValue.Addr().Interface().(*Engine)
	engine.Store = openEngineStore(t)
	engine.ResolveDeliveryWorktree = UnavailableDeliveryWorktreeResolver("test")

	// Delivery MACHINERY is deliberately not inherited when a caller passes a
	// capability sentinel - that is the round-5 fix, and the reflective census
	// caught it on its first run rather than letting it read as an omission.
	deliveryScoped := map[string]bool{"CollectChangeSet": true, "ApplyChangeSet": true}

	inherited := reflect.ValueOf(engine.EnqueueMailbox(nil))
	for _, name := range expected {
		if inherited.FieldByName(name).IsNil() {
			t.Fatalf("EnqueueMailbox(nil) did not forward %s: a producer taking this mailbox silently loses it", name)
		}
	}

	sentineled := reflect.ValueOf(engine.EnqueueMailbox(UnavailableDeliveryWorktreeResolver("test")))
	for _, name := range expected {
		isNil := sentineled.FieldByName(name).IsNil()
		if deliveryScoped[name] && !isNil {
			t.Fatalf("%s survived a delivery sentinel: the producer refuses delivery but now carries the means to perform it", name)
		}
		if !deliveryScoped[name] && isNil {
			t.Fatalf("EnqueueMailbox dropped config-bearing %s alongside the delivery machinery", name)
		}
	}
	t.Logf("censused %d engine resolvers: %v", len(expected), expected)
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
