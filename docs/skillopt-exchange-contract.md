# Gitmoot-SkillOpt Exchange Contract

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

## Training Package

Export a local eval run:

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The exported package has:

- `kind: gitmoot-skillopt-training-package`
- `contract_version: 1`
- `template`: the logical template id, current or pinned version id, content
  hash, metadata, source, and exact template content
- `eval_run`: run id, target repo, state, metadata, and template version
- `items`: review/eval items with artifact references for source, baseline,
  candidate, preview, and diff artifacts
- `artifacts`: local artifact manifests with content hashes, media type, size,
  and driver
- `feedback_events`: canonical human feedback events when available
- `evaluator_config`: the run metadata used by the external optimizer

Artifact package entries reference local SHA256 blobs stored under Gitmoot home.
The export does not copy blobs into the repository by default.

## Candidate Package

Import a candidate produced by an external optimizer:

```sh
gitmoot skillopt import --file candidate.json
```

The imported package must have:

- `kind: gitmoot-skillopt-candidate-package`
- `contract_version: 1`
- `template_id`: an installed Gitmoot agent template id
- `base_version_id`: optional pinned version used by the optimizer
- `candidate.content`: full agent-template Markdown with YAML frontmatter
- `candidate.metadata`: metadata that exactly matches the candidate
  frontmatter
- `eval_report`: optional optimizer report
- `summary`: optional diff, score, and preference summary

Importing never promotes a candidate. Gitmoot stores it as a pending template
version so later review and promotion commands can decide whether it becomes
current.

## Markdown Feedback Packet

Generate a local blind A/B review packet:

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31
```

The packet contains:

- `index.md`: review instructions and item links
- `items/*.md`: one file per item with Option A and Option B
- `feedback.yml`: the editable response file
- `.assignments.json`: hidden A/B assignment metadata used by Gitmoot on import

Humans fill `feedback.yml` with choices:

```yaml
run_id: run-2026-05-31
reviewer: alice
items:
  - item_id: item-001
    choice: b
    reasoning: More concrete and easier to execute.
  - item_id: item-002
    choice: tie
```

Allowed choices are exactly `a`, `b`, `tie`, `neither`, and `skip`. Reasoning is
optional.

Import the completed feedback:

```sh
gitmoot skillopt feedback markdown import \
  --packet .gitmoot/evals/run-2026-05-31
```

Gitmoot validates the full response before writing any events. On import, it
uses `.assignments.json` to de-blind `a` and `b` so stored canonical feedback
events use `choice: a` for the baseline artifact and `choice: b` for the
candidate artifact. Each event includes `run_id`, `item_id`, `choice`, optional
`reasoning`, `reviewer`, `source`, optional `source_url`, and `created_at`.

## GitHub Feedback Collector

Publish a collaborative blind A/B review packet to a new GitHub issue:

```sh
gitmoot skillopt feedback github publish \
  --run run-2026-05-31 \
  --repo owner/reviews
```

To publish the packet as a comment on an existing PR instead:

```sh
gitmoot skillopt feedback github publish \
  --run run-2026-05-31 \
  --repo owner/repo \
  --pr 123
```

If `--repo` is omitted, Gitmoot resolves the target in this order: eval run
target repo, template source repo, configured `[feedback].repo`, then an error
asking for `--repo`.

Reviewers can reply with the issue body's YAML block or with short-form lines:

```text
run_id: run-2026-05-31
item-001: b - More concrete and easier to execute.
item-002: tie
```

Sync comments back into canonical feedback events:

```sh
gitmoot skillopt feedback github sync \
  --run run-2026-05-31 \
  --repo owner/reviews \
  --issue 42
```

For PR comment mode, use `--pr 123` instead of `--issue 42`. Sync ignores
unrelated comments and de-duplicates repeated imports by comment URL.
