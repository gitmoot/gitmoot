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
- `eval_run`: run id, target repo, state, mode, exploration level, option
  count, metadata, and template version
- `items`: review/eval items with artifact references for source, baseline,
  candidate, preview, diff, and ranked option artifacts
- `artifacts`: local artifact manifests with content hashes, media type, size,
  and driver
- `feedback_events`: canonical human feedback events when available
- `ranked_feedback_events`: canonical N-way ranked feedback events when
  available, including the ordered ranking, optional winner, trait notes, and
  reasoning
- `pairwise_preferences`: derived pairwise preferences expanded from ranked
  feedback, for example `C > A > D > B` becomes six ordered preferences
- `evaluator_config`: the run metadata used by the external optimizer

Artifact package entries reference local SHA256 blobs stored under Gitmoot home.
The export does not copy blobs into the repository by default.

A/B validation runs keep the existing `feedback_events` shape. Ranked
exploration data is additive: optimizers that only consume A/B validation can
ignore `items[].options`, `ranked_feedback_events`, and
`pairwise_preferences`.

## Ranked Exploration Export

Ranked runs use `mode` to tell the optimizer how broad the update should be:

- `explore`: learn broad directions from four to six diverse options.
- `refine`: combine winning traits into a smaller set of stronger candidates.
- `distill`: update the template body from accumulated feedback.
- `validate`: compare the current template against a candidate on fresh review
  items.

The `exploration_level` field describes how much variation the optimizer should
try in the next candidate set:

- `high`: prioritize wider exploration and visibly different outputs.
- `medium`: combine promising traits while still testing alternatives.
- `low`: make narrow refinements and prepare for validation.

Each ranked item exports `options` with the blind option label, artifact id,
role, and optional metadata such as preview URLs. Ranked feedback exports:

- `ranking`: canonical option labels ordered best to worst
- `winner`: optional first-place option
- `useful_traits`: JSON object keyed by canonical option label
- `rejected_traits`: JSON object keyed by canonical option label
- `reasoning`: reviewer notes

Derived `pairwise_preferences` are provided so an optimizer can use simple
preference comparisons without reparsing every ranking. Each pairwise row
includes `ranked_event_id`, which matches the corresponding
`ranked_feedback_events[].id`. Trait notes remain attached to the ranked
feedback event so a future optimizer can combine useful traits across multiple
winning options rather than only copying the top option.

## Candidate Package

Import a candidate produced by an external optimizer:

```sh
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
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
- `artifacts`: optional candidate artifact manifest entries with `id`, relative
  `path`, SHA256 `hash`, `media_type`, `driver`, and optional `size_bytes`

Importing never promotes a candidate. Gitmoot stores it as a pending template
version so later review and promotion commands can decide whether it becomes
current.
If `artifacts` is present, `--artifact-dir` is required. Gitmoot rejects
absolute paths, path traversal, missing files, hash mismatches, duplicate
artifact ids, and invalid `summary.diff_artifact_id` references before creating
the pending candidate version. Verified blobs are stored in Gitmoot's
content-addressed artifact store and registered in SQLite.

Review pending candidates:

```sh
gitmoot skillopt candidate list --template planner
gitmoot skillopt candidate show planner@v2
```

`candidate show` includes the candidate state, source, content hash, base
version, optimizer score, preference summary, eval report JSON, and a content
diff against the base/current template version. It does not expose hidden A/B
assignment mappings while blind reviews are active.

Promote or reject after human review:

```sh
gitmoot skillopt candidate promote planner@v2
gitmoot skillopt candidate reject planner@v3 --reason "Too broad for the current workflow"
```

Promotion updates the template's current version. Rejection records an audit
reason and prevents the rejected candidate from being selected by `@latest`.

## Human Feedback Trial Happy Path

Create an eval review run and add saved baseline/candidate outputs:

```sh
gitmoot skillopt review create \
  --template planner \
  --repo owner/repo \
  --run run-2026-05-31

gitmoot skillopt review item add \
  --run run-2026-05-31 \
  --item item-001 \
  --title "README planning task" \
  --baseline baseline.md \
  --candidate candidate.md \
  --metadata-json '{"path":"README.md"}'

gitmoot skillopt review status --run run-2026-05-31
```

Then export a blind local packet, collect human feedback, and import it:

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31

# Human opens index.md, reviews items/*.md, sets reviewer, and edits feedback.yml.

gitmoot skillopt feedback markdown import \
  --packet .gitmoot/evals/run-2026-05-31
```

When every review item has imported feedback, export the training package for
the external optimizer:

```sh
gitmoot skillopt review status --run run-2026-05-31
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

Use `--dry-run` first to validate the exchange contract without model calls:

```sh
gitmoot-skillopt optimize \
  --training-package training.json \
  --artifact-root ~/.gitmoot/evals/blobs \
  --out-root .gitmoot/skillopt/run-2026-05-31 \
  --candidate-output candidate.json \
  --dry-run
```

For real model-backed optimization, verify the installed optimizer contract and
environment before running it:

```sh
gitmoot-skillopt --help
gitmoot-skillopt optimize --help
for name in OPENAI_API_KEY ANTHROPIC_API_KEY GITMOOT_SKILLOPT_BACKEND; do
  if [ -n "${!name:-}" ]; then
    printf '%s=set\n' "$name"
  else
    printf '%s=missing\n' "$name"
  fi
done
```

Use the backend, model, and budget flags shown by your installed
`gitmoot-skillopt optimize --help`. Do not assume flag names without checking
the local optimizer version.

Import and review the candidate. Importing never promotes automatically:

```sh
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> --reason "Needs narrower instructions"
```

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

Complete local review path:

1. `gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>`
2. `gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md`
3. `gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>`
4. Human opens `index.md`, reviews `items/*.md`, sets `reviewer`, and fills `feedback.yml`.
5. `gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id>`
6. `gitmoot skillopt export --run <run-id> --output training.json`
7. `gitmoot-skillopt optimize --training-package training.json --artifact-root ~/.gitmoot/evals/blobs --out-root .gitmoot/skillopt/<run-id> --candidate-output candidate.json --dry-run`
8. `gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]`
9. `gitmoot skillopt candidate show <version-id>`
10. `gitmoot skillopt candidate promote <version-id>` or `gitmoot skillopt candidate reject <version-id>`

Complete GitHub review path:

1. `gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]`
2. `gitmoot skillopt feedback github publish --run <run-id> --repo owner/reviews`
3. Humans reply in GitHub comments using the run-scoped YAML or short-form block.
4. `gitmoot skillopt feedback github sync --run <run-id> --repo owner/reviews --issue <number>`
5. `gitmoot skillopt candidate show <version-id>`
6. `gitmoot skillopt candidate promote <version-id>` or `gitmoot skillopt candidate reject <version-id>`

## Future Live Pairwise Evaluation

The MVP exchange contract compares candidates against saved baseline outputs.
This keeps local review deterministic and avoids rerunning every baseline for
each candidate import.

Future live pairwise mode is tracked in
[GitHub issue #77](https://github.com/jerryfane/gitmoot/issues/77). That mode
would run the current promoted template and the pending candidate live for every
validation item before collecting blind A/B feedback. The tradeoff is more
faithful comparisons and better protection against stale baseline outputs, at
the cost of higher latency, token spend, and runtime/session complexity.
