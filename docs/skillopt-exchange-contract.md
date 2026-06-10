# Gitmoot-SkillOpt Exchange Contract

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

For the guided product workflow, use `gitmoot skillopt train` instead of
assembling every low-level command manually. Train mode creates sessions and
iterations, manages review items, generates options, publishes review packets,
syncs feedback, exports the package, runs the external optimizer, imports a
pending candidate, publishes candidate review context, and starts follow-up
iterations only after an explicit decision. The low-level commands documented
here are still useful for advanced debugging, custom research runs, and
recovering individual train steps.

See [SkillOpt Train Workflow](skillopt-train-workflow.md) for the end-to-end
train-mode sequence.

## Training Package

Export a local eval run:

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The exported package has:

- `kind: gitmoot-skillopt-training-package`
- `contract_version: 1`
- `training_mode`: the Gitmoot training/review workflow mode, such as
  `explore`, `refine`, `distill`, or `validate`
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
- `evaluator_config`: evaluator and run metadata used by the external
  optimizer. Top-level workflow mode is exported as `training_mode`, not as
  `evaluator_config.mode`; `evaluator_config.mode` is reserved for evaluator
  implementation ids or drivers.

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
- `required_improvements`: JSON array of requested improvements not tied to one
  option
- `reasoning`: reviewer notes

Derived `pairwise_preferences` are provided so an optimizer can use simple
preference comparisons without reparsing every ranking. Each pairwise row
includes `ranked_event_id`, which matches the corresponding
`ranked_feedback_events[].id`. Trait notes remain attached to the ranked
feedback event so a future optimizer can combine useful traits across multiple
winning options rather than only copying the top option.

## Ranked Exploration Workflow

Use ranked exploration when the template is still ambiguous and humans need to
compare meaningfully different directions. Use A/B validation when the question
is whether a specific candidate should replace the current template.

1. `explore`: generate four to six diverse options for each review item. Ask
   reviewers to rank every option and name useful and rejected traits. Keep
   `exploration_level` set to `high` while the best direction is still unclear.
2. `refine`: use two to three candidates that combine the strongest traits
   discovered during exploration. Keep asking for rankings and trait notes, but
   focus the alternatives around the same product/workflow goal.
3. `distill`: convert accumulated ranked feedback into a candidate template
   update. This phase should not require broad new directions.
4. `validate`: compare the current template against the candidate on fresh
   review items. Use the A/B path by default for final promotion decisions.

`gitmoot skillopt review status --run <run-id>` reports the current mode,
feedback count, pairwise preference count, ranking stability, and recommended
next mode. Recommendations are advisory only. Gitmoot does not change the run
mode, import a candidate, or promote a template automatically.

Phase recommendations intentionally wait until every review item has imported
feedback. This prevents one heavily reviewed item from advancing a whole run
while other items are untouched. Blind Markdown and GitHub packets hide
outcome-bearing recommendation details after feedback exists so later reviewers
do not see the current winner before responding.

Do not run heavy SkillOpt optimization after every tiny feedback update unless
the user explicitly wants that. A practical cadence is:

- collect enough rankings to make the status recommendation stable;
- export the training package;
- run the external optimizer;
- import the candidate;
- review or validate the candidate with fresh items before promotion.

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
If `eval_report` or `summary.metadata` marks `promotable: false` with
`no_candidate_reason`, or if the candidate content hash matches the base
version, Gitmoot rejects the import as `optimizer produced no candidate` instead
of creating a fake pending version.
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

Generate a local ranked exploration packet by creating a ranked run and adding
repeated option artifacts:

```sh
gitmoot skillopt review create \
  --template landing-page-designer \
  --repo owner/gitmoot-web \
  --run landing-page-explore-001 \
  --mode explore \
  --exploration-level high \
  --options 4

gitmoot skillopt review item add \
  --run landing-page-explore-001 \
  --item hero-001 \
  --title "Gitmoot landing page hero" \
  --option a=previews/hero-a.md \
  --option b=previews/hero-b.md \
  --option c=previews/hero-c.md \
  --option d=previews/hero-d.md \
  --metadata-json '{"task":"landing-page","preview_url":"https://owner.github.io/gitmoot-previews/hero-001/"}'

gitmoot skillopt feedback markdown export \
  --run landing-page-explore-001 \
  --output .gitmoot/evals/landing-page-explore-001
```

Humans fill ranked feedback with ordered options and trait notes:

```yaml
run_id: landing-page-explore-001
reviewer: alice
items:
  - item_id: hero-001
    ranking:
      - C > A > D > B
    useful_traits:
      C:
        - explains what Gitmoot does before the fold
      A:
        - strongest mascot placement
    rejected_traits:
      B:
        - too generic for a developer tool
    required_improvements:
      - better mobile layout
      - stronger product visuals
    reasoning: C is clearest overall, but A has the better visual identity.
```

A non-visual text task uses the same structure:

```sh
gitmoot skillopt review create \
  --template x-post-writer \
  --repo owner/content-workflows \
  --run x-post-style-explore-001 \
  --mode explore \
  --options 5

gitmoot skillopt review item add \
  --run x-post-style-explore-001 \
  --item thread-hook-001 \
  --title "Launch-thread opening post" \
  --option a=posts/hook-a.txt \
  --option b=posts/hook-b.txt \
  --option c=posts/hook-c.txt \
  --option d=posts/hook-d.txt \
  --option e=posts/hook-e.txt
```

Rank every option from best to worst and use trait notes for style signals such
as pacing, specificity, voice, sentence length, and phrases to avoid.

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

Ranked GitHub review uses the same run and item setup as the Markdown ranked
workflow, then publishes to a review issue or PR:

```sh
gitmoot skillopt feedback github publish \
  --run landing-page-explore-001 \
  --repo owner/gitmoot-previews

gitmoot skillopt feedback github sync \
  --run landing-page-explore-001 \
  --repo owner/gitmoot-previews \
  --issue 42
```

Reviewers can reply with the YAML ranked block or a short ranking form:

```text
run_id: landing-page-explore-001
hero-001 ranking: C > A > D > B
best traits:
- C: clearest product explanation
- A: best mascot placement
reject:
- B: too generic
```

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

Complete train-mode path:

1. `gitmoot skillopt train start --template <id> --repo owner/repo --workspace-repo owner/workspace --request <text> --items-file items.yml --yes` (`--workspace-repo` is required; without it the session stays at `request_confirmed` and cannot reach option generation)
2. `gitmoot skillopt train status --session <session-id> --json --verbose` or `--watch` to inspect `status_phase`, item progress, active locks with owner and heartbeat, review issue, candidate, recovery availability, no-candidate reason, and next action. `status_phase` is the stable automation field; it can pass through normal train states such as `items_ready` and can also report generation/optimizer/blocker phases such as `generating_options`, `generating_options_heartbeat_stale`, `preflight_running`, `optimizer_running`, `optimizer_heartbeat_stale`, `optimizer_completed_candidate`, `optimizer_completed_no_candidate`, `recovery_available`, `blocked_config`, `blocked_stale_lock`, or `failed_unrecoverable`. While option generation runs, `status_phase` reports `generating_options` (the dashboard shows the same live phase), so polling does not look frozen at `items_ready`.
3. `gitmoot skillopt train continue --session <session-id>` to generate options and publish the review packet.
4. Human feedback is imported from raw or fenced YAML comments; `train continue` auto-syncs GitHub comments when the review is published and feedback is missing.
5. Evaluator profiles run cheap artifact checks first, optional render adapters second, and LLM judges last. Structured failures flow into optimizer input with reasons, hints, evidence, failed checks, and stage status.
6. `gitmoot skillopt train continue --session <session-id> --backend codex` to export the package, print the resolved backend/preflight report, run `gitmoot-skillopt`, and import the pending candidate, or record `optimizer_completed_no_candidate` if the optimizer produced no promotable content.
7. If the optimizer wrapper fails after writing completed artifacts and status reports `recovery_available`, run `gitmoot skillopt train recover --session <session-id> --out-root <optimizer-output-root>` to import a completed candidate or record the completed no-candidate result through the same gate.
8. `gitmoot skillopt train continue --session <session-id>` to publish candidate review context with separate selection score, evaluator/test scores, gate status, no-op status, and promotability.
9. `gitmoot skillopt train continue --session <session-id> --promote <version>` or `--reject <version> --reason <text>`.
10. `gitmoot skillopt train continue --session <session-id> --start-next` only after the prior iteration is resolved.

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
