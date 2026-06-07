---
title: SkillOpt Train Workflow
---

Use `gitmoot skillopt train` for the full human-feedback optimization loop for
an agent template. The lower-level `skillopt review`, `feedback`, `export`,
`import`, and `candidate` commands remain available for advanced debugging,
custom research runs, and recovery of individual steps.

Train mode keeps Gitmoot as the product/control layer. The external
`gitmoot-skillopt` optimizer stays outside the Gitmoot binary.

## Sessions And Iterations

A train session tracks one template and one human request. Each iteration pins a
base template version, owns an eval review run, records workspace and optional
preview repos, stores generated option artifacts, imports human feedback, runs
the optimizer, imports a pending candidate, and records a final decision.

The next iteration can start only after the previous iteration is promoted,
rejected with a reason, or abandoned. Promoted candidates become the next base
template snapshot. Rejected candidates never become current silently.

## Start And Continue

```sh
gitmoot skillopt train start \
  --template planner \
  --session planner-train \
  --repo owner/product \
  --workspace-repo owner/product-workspace \
  --preview-repo owner/product-previews \
  --request "Improve release planning answers from reviewer feedback" \
  --items-file train-items.yml \
  --mode explore \
  --exploration-level high \
  --options 4 \
  --preferred-gate hard_then_soft \
  --yes
```

Use `--dry-run` first to inspect the inferred session, item warnings, preferred
gate, and next action without writing state.

```sh
gitmoot skillopt train status --session planner-train
gitmoot skillopt train status --session planner-train --json --verbose
gitmoot skillopt train status --session planner-train --watch --poll 5s
gitmoot skillopt train continue --session planner-train
gitmoot skillopt train stop --session planner-train --reason "Request changed"
```

Verbose status reports the current phase, step, review issue, candidate state,
active work locks, item progress, and next action. JSON output is for
automation; watch mode is text-only and exits when the session waits for human
input or reaches a terminal phase.

The item file is YAML or JSON:

```yaml
items:
  - id: release-plan
    title: Release planning answer
    brief: Plan a small release with risk and verification steps.
    target_audience: maintainer
    output_type: markdown plan
  - id: review-followup
    title: Review follow-up answer
    brief: Turn reviewer feedback into a concise fix plan.
    target_audience: contributor
    output_type: markdown checklist
```

Starting with too few items fails. Homogeneous item sets warn and require
explicit confirmation.

## Preview Repos

Without `--preview-repo`, train sessions use `preview.mode=none` and publish
text-only review packets to the target repo. With `--preview-repo
owner/previews`, Gitmoot defaults to required Vue/Vite previews published
through GitHub Pages, and the expected review repo becomes `owner/previews`.
Register the preview checkout before publishing:

```sh
gitmoot repo add owner/previews --path /path/to/previews
```

GitHub-backed review operations require `gh` to be installed and authenticated
for the expected review repository. Check it before starting review publication
or watching:

```sh
gh auth status --hostname github.com
gh repo view owner/previews --json nameWithOwner
```

Gitmoot preflights GitHub issue/comment operations such as feedback publish,
feedback sync, candidate review publication, and review watching. Preview
publication itself still commits and pushes the Pages route before the review
issue is created, so a later `gh` auth failure can leave preview commits in the
preview repo even though the review issue was not posted.

The currently implemented renderer/publisher pairs are `none/none` and
`vue-vite/github-pages`. Required previews block inline fallback until every
generated option has a `preview_url`; optional previews use URLs when available
and fall back to inline Markdown only when preview publishing is unavailable.
Required Vue/Vite options are validated as soon as they are generated. An
actionable preview-bundle validation failure retries that option once while
keeping valid sibling options.

GitHub Pages publication records `preview_commit`, `preview_status`, and
`preview_status_reason` with each option. Gitmoot waits briefly for the pushed
commit and for matching queued/building builds to become ready or failed.
Review links render as `open`, `pending deployment`, `failed deployment`, or
`stale deployment` based on that status. LaTeX/PDF and other preview types are
future adapters.

## Review, Feedback, And Optimizer Gate

`train continue` generates options through Gitmoot-managed temporary agents and
publishes a concise GitHub review packet. For landing-page preview runs, call it
once to generate Vue/Vite bundles and a second time to build/publish previews
and create the review issue:

```sh
gitmoot skillopt train continue --session planner-train
gitmoot skillopt train continue --session planner-train
```

Low-level GitHub feedback publish/sync commands enforce the train run's
expected review repo, so preview reviews cannot accidentally publish to the
target product repo. Review issues include a fenced `yaml` block for copyable
feedback, and `train continue` auto-syncs GitHub comments when the review is
published and no feedback has been imported yet. Reviewers can use ranked
feedback with optional quality and phase hints:

```yaml
run_id: planner-train-review-001
reviewer: alice
items:
  - item_id: release-plan
    ranking:
      - C > A > D > B
    quality: acceptable
    continue_mode: refine
    reasoning: C is strongest, but A has a better risk summary.
```

Use `quality: poor` or `continue_mode: explore` when all options are weak and a
stable winner should not narrow the search yet.

For exploratory human-feedback optimization, prefer:

```sh
gitmoot skillopt train continue \
  --session planner-train \
  --skill-update-mode full_rewrite_minibatch \
  --optimizer-views 4 \
  --retry-optimizer-views auto
```

`--optimizer-views` runs independent optimizer perspectives over the same full
review feedback set before merge, while the compact update mode rewrites the
skill instead of only appending more prompt rules. `--retry-optimizer-views`
controls whether gate-reject retries also use multiple optimizer perspectives;
use `auto` to inherit the initial view count for full-rewrite minibatch retries
while keeping cheaper patch-mode retries at one view.

After feedback sync, train mode exports the training package, invokes
`gitmoot-skillopt optimize`, imports the returned candidate through the shared
candidate validator, and leaves the candidate pending. Use optimizer `--dry-run`
flags to validate command shape without model calls.

Evaluator profiles define artifact contracts and stages. Landing-page runs use
cheap-first Vue/Vite checks, optional render smoke, and then the LLM judge.
Structured failures include reasons, optimizer hints, failed checks, evidence,
and stage status so the optimizer can update the skill from failure classes. If
the optimizer returns the unchanged baseline, accepts no prompt edit, or returns
the same content hash, Gitmoot records `optimizer_completed_no_candidate` and
does not publish a candidate review.

## Candidate Review And Decision

Candidate reviews publish the candidate summary, preview/PR links when
available, GitHub links to the candidate skill files, and copyable decision
commands. The review repo file contract is:

- `skillopt/runs/<session>/<iteration>/<candidate>/best_skill.md`
- `skillopt/runs/<session>/<iteration>/<candidate>/base_skill.md`
- `skillopt/runs/<session>/<iteration>/<candidate>/candidate.diff.md`

These files let reviewers inspect the proposed skill, the baseline skill, and
the candidate diff directly in GitHub. Candidate reviews also separate selection
score, evaluator/test scores, gate status, no-op status, and promotability. If
metadata marks the candidate as no-op or not promotable, the review says
promotion is unavailable instead of showing a promote command.

Choose explicitly:

```sh
gitmoot skillopt train continue --session planner-train --promote planner@v2
gitmoot skillopt train continue --session planner-train --reject planner@v2 --reason "Too broad"
```

The supported decisions are promote, reject with reason, wait, and keep
improving. Waiting means taking no action while status keeps reporting the
candidate decision gate. Keep improving means rejecting with an actionable
reason, then starting the next iteration after the rejection is recorded:

```sh
gitmoot skillopt train continue --session planner-train --start-next
```

Manual append-style next iterations are not supported. The train state machine
creates the next iteration atomically from the resolved previous decision, its
eval run, and copied item plan.

## Smoke Test

```sh
scripts/skillopt-train-smoke.sh
```

The smoke script runs focused CLI tests with fake managed generation, fake
preview publication, fake `gitmoot-skillopt`, and fake GitHub publication. It
covers preview blocking, review-repo enforcement, the train loop, and candidate
decisions without real model calls or real GitHub mutation.

Manual smoke scenarios:

1. Candidate decision: publish a candidate review, confirm the review repo has
   `best_skill.md`, `base_skill.md`, and `candidate.diff.md`, then use status to
   confirm the decision gate before promoting or rejecting with a reason.
2. Preview publication status: publish Vue/Vite review options and confirm the
   issue links are labeled `open`, `pending deployment`, `failed deployment`,
   or `stale deployment` according to the GitHub Pages build state.

## Troubleshooting

- Missing `gh` auth: run `gh auth status --hostname github.com` and confirm
  `gh repo view owner/repo --json nameWithOwner` succeeds for the expected
  review repo before starting review publication. Preview publication can
  already have pushed Pages files before a later review issue preflight fails.
- Pending or failed preview links: pending means Pages had not completed within
  the bounded wait, failed includes the Pages error when available, and stale
  means the latest build still points at another commit. Existing review links
  keep their recorded label; `train continue` does not refresh old deployment
  status for options that already have a preview URL.
- Invalid generated preview bundles: required Vue/Vite options retry once for
  actionable validation errors; repeated failures stop with item, option,
  validation class, and retry count.
- Candidate waiting for decision: inspect the candidate files in the review repo
  and then promote, reject with a reason, keep waiting, or reject and
  `--start-next` to keep improving.
- Missing feedback import: reply with the fenced YAML block or raw YAML, then
  rerun `gitmoot skillopt train continue --session <id>` to auto-sync comments.
- Invalid YAML: sync output reports wrong `run_id`, missing item feedback,
  invalid ranking, unknown option, invalid signal value, no parseable YAML, or
  no comments.
- No candidate created: inspect `no_candidate_reason` with `train status
  --verbose`.
- Render adapter unavailable: install the required render dependency or use a
  profile that does not require render smoke.
- Evaluator skipped: cheap hard checks can intentionally stop render/LLM stages
  after an artifact-contract failure.
