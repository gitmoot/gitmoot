package memory

// Tests for the #804 groom additions: StableKey, the legacy-key rekey detector,
// and the cross-pool staleness detector. Pure functions, real-shaped fixtures.

import "testing"

// ---- StableKey --------------------------------------------------------------

func TestStableKeyStripsLegacySuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"runbook-md-deploy-a1b2c3d4", "runbook-md-deploy"}, // legacy ingest key
		{"runbook-md-deploy", "runbook-md-deploy"},          // already stable
		{"chat-standup-7-00ff11aa", "chat-standup-7"},       // legacy chat key
		{"ci-flake", "ci-flake"},                            // no suffix at all
		{"key-a1b2c3d", "key-a1b2c3d"},                      // 7 hex chars: not legacy
		{"key-a1b2c3d4e", "key-a1b2c3d4e"},                  // 9 hex chars: not legacy
		{"key-A1B2C3D4", "key-A1B2C3D4"},                    // uppercase: not legacy (slugs are lowercase)
		{"-a1b2c3d4", "-a1b2c3d4"},                          // stripping would leave nothing
	}
	for _, c := range cases {
		if got := StableKey(c.in); got != c.want {
			t.Fatalf("StableKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- rekey detector ---------------------------------------------------------

func TestDetectGroomRekeysKeepsNewestAndRetiresSiblings(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 1, Key: "notes-deploy-a1b2c3d4", Content: "old edition", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Key: "notes-deploy-ffee0011", Content: "newer edition", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-03-01T00:00:00Z"},
		{ID: 3, Key: "unrelated-fact", Content: "keeper", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-02-01T00:00:00Z"},
	}
	actions := DetectGroomRekeys(cands)
	if len(actions) != 1 {
		t.Fatalf("expected one rekey group, got %d: %+v", len(actions), actions)
	}
	a := actions[0]
	if a.KeepID != 2 || a.NewKey != "notes-deploy" {
		t.Fatalf("keeper should be the newest edition rekeyed to the stable form: %+v", a)
	}
	if len(a.Retire) != 1 || a.Retire[0].ID != 1 {
		t.Fatalf("older sibling should be retired: %+v", a.Retire)
	}
}

func TestDetectGroomRekeysPrefersExistingStableRow(t *testing.T) {
	// A post-#804 edit already spawned the stable-keyed row; it is the current
	// edition by construction even if a sibling carries a later timestamp.
	cands := []GroomCandidate{
		{ID: 1, Key: "notes-deploy-a1b2c3d4", Content: "legacy", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-05-01T00:00:00Z"},
		{ID: 2, Key: "notes-deploy", Content: "stable", OwnerKind: "agent", OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-04-01T00:00:00Z"},
	}
	actions := DetectGroomRekeys(cands)
	if len(actions) != 1 {
		t.Fatalf("expected one rekey group, got %d", len(actions))
	}
	a := actions[0]
	if a.KeepID != 2 || a.NewKey != "notes-deploy" || a.KeepKey != "notes-deploy" {
		t.Fatalf("stable row must be the keeper with no key rewrite: %+v", a)
	}
	if len(a.Retire) != 1 || a.Retire[0].ID != 1 {
		t.Fatalf("legacy sibling should be retired: %+v", a.Retire)
	}
}

func TestDetectGroomRekeysLoneLegacyRowRekeyedNoRetire(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 7, Key: "notes-setup-00aa11bb", Content: "only edition", OwnerKind: "agent", OwnerRef: "lead", Repo: "", Scope: "general", UpdatedAt: "2026-01-01T00:00:00Z"},
	}
	actions := DetectGroomRekeys(cands)
	if len(actions) != 1 {
		t.Fatalf("a lone legacy row must still be rekeyed, got %d actions", len(actions))
	}
	if actions[0].KeepID != 7 || actions[0].NewKey != "notes-setup" || len(actions[0].Retire) != 0 {
		t.Fatalf("lone legacy row: %+v", actions[0])
	}
}

func TestDetectGroomRekeysScopesGroupsAndIsDeterministic(t *testing.T) {
	cands := []GroomCandidate{
		// Same stripped key under a DIFFERENT owner: separate group.
		{ID: 1, Key: "notes-x-aaaa1111", Content: "a", OwnerKind: "agent", OwnerRef: "lead", Repo: "r/a", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Key: "notes-x-bbbb2222", Content: "b", OwnerKind: "agent", OwnerRef: "scout", Repo: "r/a", Scope: "repo", UpdatedAt: "2026-01-02T00:00:00Z"},
		// Same stripped key under a DIFFERENT repo: separate group.
		{ID: 3, Key: "notes-x-cccc3333", Content: "c", OwnerKind: "agent", OwnerRef: "lead", Repo: "r/b", Scope: "repo", UpdatedAt: "2026-01-03T00:00:00Z"},
	}
	first := DetectGroomRekeys(cands)
	if len(first) != 3 {
		t.Fatalf("cross-owner/cross-repo rows must not group together: %+v", first)
	}
	for _, a := range first {
		if len(a.Retire) != 0 {
			t.Fatalf("no siblings should be retired across scopes: %+v", a)
		}
	}
	rev := []GroomCandidate{cands[2], cands[1], cands[0]}
	second := DetectGroomRekeys(rev)
	if len(second) != len(first) {
		t.Fatalf("determinism: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].KeepID != second[i].KeepID || first[i].NewKey != second[i].NewKey {
			t.Fatalf("order-dependent output at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestDetectGroomRekeysIgnoresStableOnlyGroups(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 1, Key: "notes-deploy", Content: "fine", OwnerKind: "agent", OwnerRef: "lead", Repo: "r/a", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
	}
	if got := DetectGroomRekeys(cands); len(got) != 0 {
		t.Fatalf("stable-only keys must not propose anything: %+v", got)
	}
}

// ---- cross-pool staleness detector ------------------------------------------

func crossPoolFixture() []GroomCandidate {
	return []GroomCandidate{
		// Private edition NEWER than the shared one, same stable key: proposes.
		{ID: 1, Key: "notes-deploy", Content: "new private edition", OwnerKind: OwnerKindAgent, OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-06-01T00:00:00Z"},
		{ID: 2, Key: "notes-deploy-a1b2c3d4", Content: "stale shared edition", OwnerKind: OwnerKindShared, OwnerRef: SharedOwnerRef, Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
		// Shared NEWER than private: must NOT propose.
		{ID: 3, Key: "other-fact", Content: "old private", OwnerKind: OwnerKindAgent, OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: 4, Key: "other-fact", Content: "fresh shared", OwnerKind: OwnerKindShared, OwnerRef: SharedOwnerRef, Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-06-01T00:00:00Z"},
		// Same stable key but a DIFFERENT repo: must NOT propose.
		{ID: 5, Key: "notes-deploy", Content: "other repo private", OwnerKind: OwnerKindAgent, OwnerRef: "lead", Repo: "acme/z", Scope: "repo", UpdatedAt: "2026-06-01T00:00:00Z"},
	}
}

func TestDetectCrossPoolStalenessStableKeyEquality(t *testing.T) {
	actions := DetectCrossPoolStaleness(crossPoolFixture(), nil)
	if len(actions) != 1 {
		t.Fatalf("expected exactly one stable-key proposal, got %d: %+v", len(actions), actions)
	}
	a := actions[0]
	if a.PrivateID != 1 || a.SharedID != 2 || a.Basis != CrossPoolBasisStableKey {
		t.Fatalf("unexpected proposal: %+v", a)
	}
}

func TestDetectCrossPoolStalenessRejectsWeakBM25(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 1, Key: "private-fact", Content: "private", OwnerKind: OwnerKindAgent, OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-06-01T00:00:00Z"},
		{ID: 2, Key: "shared-fact", Content: "shared", OwnerKind: OwnerKindShared, OwnerRef: SharedOwnerRef, Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
	}
	// Strong score but NO link: composite evidence requires both.
	unlinked := []GroomCrossPoolSignal{{PrivateID: 1, SharedID: 2, Score: CrossPoolBM25Strong + 10, Linked: false}}
	if got := DetectCrossPoolStaleness(cands, unlinked); len(got) != 0 {
		t.Fatalf("bm25 alone must never propose: %+v", got)
	}
	// Linked but weak score: still no.
	weak := []GroomCrossPoolSignal{{PrivateID: 1, SharedID: 2, Score: CrossPoolBM25Strong - 1, Linked: true}}
	if got := DetectCrossPoolStaleness(cands, weak); len(got) != 0 {
		t.Fatalf("a weak bm25 match must never propose even when linked: %+v", got)
	}
	// Strong AND linked: proposes with the bm25-link basis.
	strong := []GroomCrossPoolSignal{{PrivateID: 1, SharedID: 2, Score: CrossPoolBM25Strong + 1, Linked: true}}
	got := DetectCrossPoolStaleness(cands, strong)
	if len(got) != 1 || got[0].Basis != CrossPoolBasisBM25Link {
		t.Fatalf("strong linked match should propose via bm25-link: %+v", got)
	}
}

func TestDetectCrossPoolStalenessSecondaryRequiresNewerPrivate(t *testing.T) {
	cands := []GroomCandidate{
		{ID: 1, Key: "private-fact", Content: "private", OwnerKind: OwnerKindAgent, OwnerRef: "lead", Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Key: "shared-fact", Content: "shared", OwnerKind: OwnerKindShared, OwnerRef: SharedOwnerRef, Repo: "acme/w", Scope: "repo", UpdatedAt: "2026-06-01T00:00:00Z"},
	}
	strong := []GroomCrossPoolSignal{{PrivateID: 1, SharedID: 2, Score: CrossPoolBM25Strong + 5, Linked: true}}
	if got := DetectCrossPoolStaleness(cands, strong); len(got) != 0 {
		t.Fatalf("an OLDER private edition must never replace a newer shared one: %+v", got)
	}
}

func TestDetectCrossPoolStalenessPrimaryBeatsSecondaryPerSharedRow(t *testing.T) {
	cands := crossPoolFixture()
	// A strong linked signal against the same shared row the primary already
	// claimed: only one action per shared row, primary basis wins.
	signals := []GroomCrossPoolSignal{{PrivateID: 1, SharedID: 2, Score: CrossPoolBM25Strong + 5, Linked: true}}
	actions := DetectCrossPoolStaleness(cands, signals)
	if len(actions) != 1 || actions[0].Basis != CrossPoolBasisStableKey {
		t.Fatalf("primary must claim the shared row first: %+v", actions)
	}
}
