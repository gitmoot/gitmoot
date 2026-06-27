# Cross-repo pairwise round-trip fixture (#507/#508)

This directory holds a **fork-GENERATED** fixture for the cross-repo pairwise
round-trip E2E (`internal/cli/skillopt_pairwise_roundtrip_test.go`). Its purpose
is to catch a contract-shape mismatch between:

- the **producer** — the `gitmoot-skillopt` fork's live-pairwise emission (#507,
  `gitmoot_skillopt/pairwise.py:run_pairwise_eval`), which writes a blinded human
  review packet plus a SEPARATE secret unblinding map; and
- the **consumer** — the Go importer (#508,
  `gitmoot skillopt pairwise import`), which reads that packet + secret map +
  reviewer picks and unblinds each pick back to champion/challenger.

The two were built against an *assumed* shared shape on each side and never run
as one real round-trip. This fixture closes that gap.

The fixture contains **four** val items. With the fixed seed/run_id the fork's
per-item A/B placement RNG yields champion-on-**B** for two items and
champion-on-**A** for the other two. Both placements are required: if every item
shared one placement, a Go importer that ignored the per-item secret map and
hardcoded a single `A/B -> role` mapping would still pass every assertion, and
the round-trip would stop proving per-item secret-map consultation. `regen.py`
asserts both placements are present so a future seed/run_id change can't silently
revert to a degenerate single-placement (hollow) fixture.

## CI scope / what this does and does NOT catch

CI runs **Go only** and the JSON here is **committed/frozen** — `regen.py` is
never run in CI. So the round-trip test validates the *frozen* fork shape (as of
the last manual regen) against the Go importer. It does **not** automatically
catch a *new* fork-side shape change that lands without someone re-running
`regen.py` to refresh this fixture. The one drift axis pinned in CI without a
regen is the **contract version**:
`TestSkillOptPairwiseRoundTripContractVersionPinned` asserts the committed
`contract_version` equals the Go `skillopt.ContractVersion`, so a bump on either
side that leaves this fixture stale turns the build red. Whenever the fork's wire
shape changes, re-run `regen.py` (below).

## Files

| file | origin |
| --- | --- |
| `pairwise-review.json` | **fork-emitted** blinded review packet (verbatim) |
| `pairwise-secret-map.json` | **fork-emitted** secret unblinding map (only the volatile absolute temp-path *root* of the admin/debug trace paths is normalized to `/FIXTURE`; the Go importer never reads those fields) |
| `pairwise-picks.json` | authored reviewer picks (A/B labels only — the fork does not emit picks; they are the human's review input) |
| `expected.json` | ground-truth expected unblind outcome, computed from the fork's REAL secret map, used by the Go test to assert correct unblinding |

**The packet and secret map are NOT hand-authored.** They are produced by
executing the fork's real emission code. Hand-crafting JSON to match the Go
parser would be circular and would defeat the entire purpose of this E2E.

## Regenerating

Refresh the fixture whenever the fork's packet/secret-map shape changes:

```sh
cd /root/gitmoot-skillopt && pip install -e ".[dev]"   # first time / if deps missing
python3 internal/cli/testdata/pairwise_roundtrip/regen.py
```

`regen.py` stubs the live rollout, so it runs fully **offline** (no LLM, no
network) and is deterministic (the fork seeds A/B placement from a fixed
seed + run_id). It also asserts the regenerated fixture exercises both A/B
placements. CI never runs `regen.py`: the committed fixture means the Go
round-trip test runs with Go only, no Python required.
