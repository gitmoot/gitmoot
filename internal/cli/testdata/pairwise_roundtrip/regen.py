#!/usr/bin/env python3
"""Regenerate the cross-repo pairwise round-trip fixture.

This script EXECUTES the gitmoot-skillopt fork's REAL emission code
(``gitmoot_skillopt.pairwise.run_pairwise_eval``) with a deterministic, OFFLINE
stubbed rollout (no live LLM, no network) and copies the fork-emitted artifacts
into this directory as Go testdata. The packet + secret map are therefore the
fork's ACTUAL wire shape, not JSON hand-authored to match the Go parser. That is
the whole point: the committed fixture is proof of the fork's output shape, so the
Go round-trip test (skillopt_pairwise_roundtrip_test.go) catches a contract-shape
mismatch between the fork (#507) and the Go importer (#508).

It writes four files next to itself:

  pairwise-review.json      fork-emitted blinded review packet (anonymized A/B)
  pairwise-secret-map.json  fork-emitted secret unblinding map (A/B -> roles)
  pairwise-picks.json       AUTHORED reviewer picks (A/B labels only; the fork
                            does not emit picks -- they are the human's input)
  expected.json             ground-truth expected unblind outcome, computed HERE
                            from the fork's REAL secret map, so the Go test can
                            assert the importer's unblind matches the fork's
                            intended champion/challenger mapping

Determinism: ``run_pairwise_eval`` seeds its A/B placement RNG from
(seed, run_id), both fixed here, so re-running this script is byte-stable.

Usage:
  python3 regen.py [--fork /root/gitmoot-skillopt] [--out <dir>]

Refresh the fixture whenever the fork's packet/secret-map shape changes:
  cd /root/gitmoot-skillopt && pip install -e ".[dev]"   # if needed
  python3 internal/cli/testdata/pairwise_roundtrip/regen.py
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path

SEED = 42
RUN_ID = "run-1"  # comes from the training package's eval_run.id below
# Four val items so the fork's per-item placement RNG (seeded from seed+run_id)
# yields BOTH champion-on-A and champion-on-B items. With seed=42/run-1 the
# placements are ['B','B','A','A'], so no single hardcoded A/B mapping can satisfy
# every item — the Go importer is forced to consult the per-item secret map. A
# two-item fixture happened to be ['B','B'] (degenerate), which a hardcoded
# importer would pass; see the regen-time guard in generate().
VAL_IDS = ["val-1", "val-2", "val-3", "val-4"]


def _stub_run_batch(*, items, skill_content, out_root, evaluator_config=None,
                    max_completion_tokens=4096, **kwargs):
    """Deterministic OFFLINE rollout stub.

    The fork rolls BOTH the promoted template and the candidate template through
    ``run_batch``; we distinguish them by content (the candidate template carries
    the marker word "Candidate") and emit a side-distinct, role-revealing response
    so the round-trip can verify the unblind join. We also set a role-revealing
    ``target_trace_path`` exactly as a real rollout does -- the fork must strip it
    from the blinded packet and keep it only in the secret map.
    """
    del evaluator_config, max_completion_tokens, kwargs
    is_candidate = "Candidate" in skill_content
    tag = "CANDIDATE" if is_candidate else "PROMOTED"
    role_dir = "candidate" if is_candidate else "promoted"
    results = []
    for item in items:
        item_id = str(item["id"])
        results.append(
            {
                "id": item_id,
                "response": f"{tag}-OUTPUT::{item_id}",
                "agent_ok": True,
                "target_status": "passed",
                "fail_reason": "",
                "n_turns": 1,
                "blocker": "",
                "token_usage": {"total_tokens": 100 if is_candidate else 90},
                # Role-revealing; must NEVER leak into the blinded packet.
                "target_trace_path": f"{out_root}/predictions/{item_id}/target_exec_raw.txt",
            }
        )
    return results


def _write_training_package(tmp_path: Path):
    """Build a four-val-item training package using the fork's own test helper."""
    from tests.test_gitmoot_dataloader import write_training_package

    package_path, artifact_root = write_training_package(tmp_path)
    package = json.loads(package_path.read_text(encoding="utf-8"))
    # The base helper emits one val item (val-1); add three more so the fixture
    # exercises multiple items AND both A/B placements. The fork seeds its per-item
    # placement RNG from (seed, run_id), so with seed=42/run-1 the four placements
    # are ['B','B','A','A'] — i.e. at least one champion-on-A and one champion-on-B
    # item. That forces the Go importer to consult the per-item secret map: no
    # single hardcoded "B=champion" mapping can satisfy every item.
    for n in range(2, len(VAL_IDS) + 1):
        package["items"].append(
            {
                "id": f"val-{n}",
                "title": f"Val item {n}",
                "baseline_artifact_id": "baseline",
                "candidate_artifact_id": "candidate",
                "metadata": {
                    "split": "val",
                    "mock_response": f"val {n}",
                    "expected_hard": False,
                    "expected_soft": 0.25,
                },
            }
        )
    package_path.write_text(json.dumps(package), encoding="utf-8")
    return package_path, artifact_root


def generate(fork: Path, out: Path) -> None:
    sys.path.insert(0, str(fork))
    from gitmoot_skillopt import pairwise

    # Stub the live rollout so emission runs fully offline and deterministically.
    pairwise.run_batch = _stub_run_batch  # type: ignore[assignment]

    with tempfile.TemporaryDirectory() as td:
        tmp = Path(td)
        package_path, artifact_root = _write_training_package(tmp)
        candidate_path = tmp / "candidate.md"
        candidate_path.write_text("# Candidate\n\nDifferent guidance.\n", encoding="utf-8")
        run_out = tmp / "out"

        summary = pairwise.run_pairwise_eval(
            training_package=str(package_path),
            artifact_root=str(artifact_root),
            candidate=str(candidate_path),
            out_root=str(run_out),
            seed=SEED,
        )

        artifacts = run_out / "artifacts"
        packet_raw = (artifacts / "pairwise-review.json").read_text(encoding="utf-8")
        secret_raw = (artifacts / "pairwise-secret-map.json").read_text(encoding="utf-8")

        # The secret map carries absolute, env-specific rollout trace paths
        # (.../rollout/promoted|candidate/...). They are admin/debug only and the
        # Go importer never reads them, but their value embeds the run's temp dir,
        # which changes every regen. Normalize ONLY that volatile absolute prefix
        # to a stable placeholder so the committed fixture is byte-reproducible;
        # the role-revealing .../rollout/<role>/... structure is preserved intact.
        # This touches no field the Go parser reads -- it is not reshaping to the
        # parser, just de-flaking the path root.
        secret_raw = secret_raw.replace(str(tmp), "/FIXTURE")
        packet = json.loads(packet_raw)
        secret = json.loads(secret_raw)

        # Copy the fork-EMITTED artifacts into the Go testdata dir (packet verbatim,
        # secret map with only the volatile temp-path root normalized as above).
        out.mkdir(parents=True, exist_ok=True)
        (out / "pairwise-review.json").write_text(packet_raw, encoding="utf-8")
        (out / "pairwise-secret-map.json").write_text(secret_raw, encoding="utf-8")

        secret_by_id = {s["item_id"]: s for s in secret["items"]}

        # Guard the load-bearing property of this fixture: it MUST contain BOTH
        # champion-on-A and champion-on-B items. Otherwise a Go importer that
        # ignores the per-item secret map and hardcodes a single A/B->role mapping
        # would still satisfy every assertion, and the round-trip would silently
        # stop proving per-item secret-map consultation. If a future seed/run_id
        # ever yields a degenerate single-placement set, fail loudly here rather
        # than committing a hollow fixture.
        placements = {s["champion_label"] for s in secret["items"]}
        assert placements == {"A", "B"}, (
            "fixture must exercise BOTH A/B placements (champion-on-A AND "
            f"champion-on-B); got champion_labels={sorted(placements)}. Adjust "
            "VAL_IDS/SEED/RUN_ID so the fork's placement RNG produces both."
        )

        # Author reviewer picks (A/B labels only) and compute the GROUND-TRUTH
        # expected unblind outcome from the fork's REAL secret map. To exercise
        # BOTH unblind directions we pick the champion's label on the first item
        # (champion should win) and the challenger's label on the second
        # (challenger should win). The expected winner is derived purely from the
        # fork's secret map semantics (mapping[label] == "promoted" -> champion).
        picks = []
        expected_items = []
        for idx, item in enumerate(packet["items"]):
            item_id = item["item_id"]
            sec = secret_by_id[item_id]
            if idx % 2 == 0:
                pick_label = sec["champion_label"]
                expected_winner = "champion"
            else:
                pick_label = sec["challenger_label"]
                expected_winner = "challenger"

            # Cross-check the mapping agrees with the champion/challenger labels.
            mapping = sec["mapping"]
            role_for_pick = mapping[pick_label]  # "promoted" | "candidate"
            assert (role_for_pick == "promoted") == (expected_winner == "champion"), (
                f"secret map mapping disagrees with labels for {item_id}"
            )

            # Resolve the role-true outputs from the blinded packet via the secret
            # map labels, so the Go test can verify the champion/challenger JOIN.
            by_label = {side["label"]: side["response"] for side in item["outputs"]}
            promoted_output = by_label[sec["champion_label"]]
            candidate_output = by_label[sec["challenger_label"]]
            assert promoted_output == f"PROMOTED-OUTPUT::{item_id}", promoted_output
            assert candidate_output == f"CANDIDATE-OUTPUT::{item_id}", candidate_output

            picks.append({"item_id": item_id, "pick": pick_label})
            expected_items.append(
                {
                    "item_id": item_id,
                    "pick": pick_label,
                    "expected_winner": expected_winner,
                    "promoted_output": promoted_output,
                    "candidate_output": candidate_output,
                }
            )

        picks_doc = {
            "kind": "gitmoot-skillopt-pairwise-picks",
            "run_id": packet["run_id"],
            "reviewer": "human",
            "picks": picks,
        }
        expected_doc = {
            "run_id": packet["run_id"],
            "template_id": packet["template_id"],
            "base_version_id": packet["base_version_id"],
            "candidate_content_hash": secret.get("candidate_content_hash", ""),
            "source": "live-pairwise",
            "feedback_source": "pairwise_valset",
            "items": expected_items,
        }
        (out / "pairwise-picks.json").write_text(
            json.dumps(picks_doc, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        (out / "expected.json").write_text(
            json.dumps(expected_doc, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )

        print("wrote fork-generated fixture to", out)
        print("  run_id:", packet["run_id"], "items:", [i["item_id"] for i in packet["items"]])
        print("  summary:", json.dumps(summary["mode"]))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fork", default="/root/gitmoot-skillopt", help="path to the gitmoot-skillopt fork checkout")
    parser.add_argument("--out", default=str(Path(__file__).resolve().parent), help="output testdata directory")
    args = parser.parse_args()
    generate(Path(args.fork).expanduser(), Path(args.out).expanduser())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
