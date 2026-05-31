---
title: SkillOpt Exchange Contract
---

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

## Training Package

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The package contains the template snapshot, eval run, review items, artifact
manifests, canonical feedback events when available, and evaluator config.
Artifact entries reference local SHA256 blobs stored under Gitmoot home; blobs
are not copied into the repository by default.

## Candidate Package

```sh
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
```

The candidate package contains full agent-template Markdown with YAML
frontmatter, matching metadata, an optional eval report, an optional summary,
and optional artifact manifest entries. When artifact entries are present,
`--artifact-dir` is required; Gitmoot verifies relative paths and SHA256 hashes,
stores the blobs, and registers artifact metadata before creating the pending
candidate. Importing never promotes it automatically.

```sh
gitmoot skillopt candidate list --template planner
gitmoot skillopt candidate show planner@v2
gitmoot skillopt candidate promote planner@v2
gitmoot skillopt candidate reject planner@v3 --reason "Too broad"
```

`candidate show` includes the eval report, preference summary, and content diff.
Promotion updates the current template version; rejection records an audit reason
and keeps the rejected version out of `@latest`.

## Human Feedback Trial Happy Path

Create a run and add saved baseline/candidate outputs:

```sh
gitmoot skillopt review create --template planner --repo owner/repo --run run-2026-05-31
gitmoot skillopt review item add --run run-2026-05-31 --item item-001 --title "README planning task" --baseline baseline.md --candidate candidate.md --metadata-json '{"path":"README.md"}'
gitmoot skillopt review status --run run-2026-05-31
```

Export a blind packet, have the human edit `feedback.yml`, and import it:

```sh
gitmoot skillopt feedback markdown export --run run-2026-05-31 --output .gitmoot/evals/run-2026-05-31
# Human opens index.md, reviews items/*.md, sets reviewer, and edits feedback.yml.
gitmoot skillopt feedback markdown import --packet .gitmoot/evals/run-2026-05-31
```

Export training data and validate the external optimizer contract:

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
gitmoot-skillopt optimize --training-package training.json --artifact-root ~/.gitmoot/evals/blobs --out-root .gitmoot/skillopt/run-2026-05-31 --candidate-output candidate.json --dry-run
```

For real model-backed optimization, check `gitmoot-skillopt optimize --help`
and verify required backend/model environment variables for your installed
optimizer version. Importing the candidate keeps it pending until a human
explicitly promotes or rejects it.

## Markdown Feedback Packet

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31
```

The packet contains `index.md`, one Markdown file per item, editable
`feedback.yml`, and hidden `.assignments.json` metadata that lets Gitmoot recover
the blind A/B mapping. Keep `.assignments.json` untouched.

Humans fill `feedback.yml` with `a`, `b`, `tie`, `neither`, or `skip`:

```yaml
run_id: run-2026-05-31
reviewer: alice
items:
  - item_id: item-001
    choice: b
    reasoning: More concrete and easier to execute.
```

```sh
gitmoot skillopt feedback markdown import \
  --packet .gitmoot/evals/run-2026-05-31
```

Gitmoot validates the complete file before writing events. It uses the hidden
assignment metadata to de-blind `a` and `b`, so exported feedback events use
`a` for baseline and `b` for candidate.

## GitHub Feedback Collector

```sh
gitmoot skillopt feedback github publish \
  --run run-2026-05-31 \
  --repo owner/reviews
```

Use `--pr <number>` to publish the packet as a comment on an existing pull
request instead of creating a new issue.

If `--repo` is omitted, Gitmoot tries the eval run target repo, the template
source repo, and then configured `[feedback].repo = "owner/reviews"`.

Humans can reply with the YAML block in the issue body or short-form lines:

```text
run_id: run-2026-05-31
item-001: b - More concrete and easier to execute.
item-002: tie
```

```sh
gitmoot skillopt feedback github sync \
  --run run-2026-05-31 \
  --repo owner/reviews \
  --issue 42
```

For PR comment mode, sync with `--pr <number>`. Gitmoot ignores unrelated
comments and de-duplicates repeated imports by GitHub comment URL.

The complete saved-output review loop is: create a review run, add artifact
backed review items, collect feedback with either the Markdown packet or GitHub
collector, export training data, run the external optimizer, import the pending
candidate, inspect it with `gitmoot skillopt candidate show <version-id>`, then
promote or reject it.

## Future Live Pairwise Evaluation

The MVP compares candidates against saved baseline outputs so local review stays
deterministic and inexpensive. Future live pairwise evaluation is tracked in
[GitHub issue #77](https://github.com/jerryfane/gitmoot/issues/77): it would run
the current promoted template and pending candidate live for every validation
item before collecting blind A/B feedback. This is more faithful and protects
against stale baselines, but adds latency, token cost, and runtime/session
complexity.
