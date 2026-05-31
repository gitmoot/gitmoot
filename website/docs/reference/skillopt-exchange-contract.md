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
gitmoot skillopt import --file candidate.json
```

The candidate package contains full agent-template Markdown with YAML
frontmatter, matching metadata, an optional eval report, and an optional summary.
Importing stores the candidate as a pending template version and never promotes
it automatically.

## Markdown Feedback Packet

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31
```

The packet contains `index.md`, one Markdown file per item, editable
`feedback.yml`, and hidden `.assignments.json` metadata that lets Gitmoot recover
the blind A/B mapping.

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
