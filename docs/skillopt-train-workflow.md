# SkillOpt Train Workflow

Use `gitmoot skillopt train` when a user wants Gitmoot to enforce the full
human-feedback optimization loop for an agent template. Use the lower-level
`gitmoot skillopt review`, `feedback`, `export`, `import`, and `candidate`
commands only for advanced debugging, custom research runs, or recovering one
step of an existing train session.

Train mode keeps Gitmoot as the product/control layer. The external
`gitmoot-skillopt` optimizer remains outside the Go binary and is invoked only
after Gitmoot has collected review items and feedback.

## Session Shape

A train session is the long-lived workflow for one template and request. Each
session has one or more iterations. An iteration has:

- a pinned base template version;
- an eval review run and review items;
- workspace and optional preview repos;
- preferred evaluation gate metadata;
- generated option artifacts;
- imported human feedback;
- an optimizer package and candidate package;
- an optional candidate review issue or PR link;
- a terminal decision: promoted, rejected with a reason, or abandoned.

The next iteration can start only after the prior iteration is promoted,
rejected with a reason, or abandoned. If the prior candidate was promoted, the
promoted candidate version becomes the next base template snapshot. Rejected
candidates never become current silently.

## High-Level Commands

Start with a request, target repo, pinned template, and item plan:

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

Use `--dry-run` first to inspect the inferred session id, request summary,
task kind, preferred gate, item warnings, and next action without writing state.
Without `--yes`, non-interactive runs print the exact confirmation command.

Inspect progress at any point:

```sh
gitmoot skillopt train status --session planner-train
gitmoot skillopt train status --session planner-train --verbose
gitmoot skillopt train status --session planner-train --json --verbose
gitmoot skillopt train status --session planner-train --watch --poll 5s
```

Verbose status separates the current phase, current step, review issue,
candidate state, active generation/review/optimizer locks, item progress, and
next action. JSON output is for automation. Watch mode is text-only and exits
when the session is waiting for human input or terminal.

Automation should read `status_phase` as the stable operational phase and keep
`current_phase` as the lower-level state-machine checkpoint. During normal
waiting states, `status_phase` can pass through the current train state, such
as `request_confirmed`, `workspace_ready`, `items_ready`, `options_generated`,
`review_published`, `feedback_synced`, `training_package_created`,
`optimizer_completed`, or `run_abandoned`. During optimizer operation,
candidate, and blocker states, it can report
`preflight_running`, `optimizer_running`, `optimizer_heartbeat_stale`,
`optimizer_completed_candidate`, `optimizer_completed_no_candidate`,
`recovery_available`, `blocked_config`, `blocked_stale_lock`, or
`failed_unrecoverable`. Verbose JSON also reports `recovery_available`,
`no_candidate_reason` when applicable, and `active_locks[]` with lock owner,
process, host, heartbeat, expiry, elapsed time, and content hash.

Advance the next required step:

```sh
gitmoot skillopt train continue --session planner-train
```

Stop instead of continuing:

```sh
gitmoot skillopt train stop --session planner-train --reason "Request changed"
```

## Items, Workspace, And Preview Repos

The item file is YAML or JSON. Each item should describe a distinct task or
audience so feedback is not overfit to one prompt.

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
explicit acceptance. `workspace-repo` is where managed agents can work on
generated outputs.

`preview-repo` enables review previews. Without it, train sessions use
`preview.mode=none`, `preview.renderer=none`, `preview.publisher=none`, and the
review repo defaults to the target repo. With `--preview-repo owner/previews`,
Gitmoot defaults to `preview.mode=required`, `preview.renderer=vue-vite`,
`preview.publisher=github-pages`, and `review.expected_repo=owner/previews`.
Register the preview checkout before publishing previews:

```sh
gitmoot repo add owner/previews --path /path/to/previews
```

The currently implemented renderer/publisher pairs are `none/none` and
`vue-vite/github-pages`. Vue/Vite generation is a constrained file-bundle
contract; Gitmoot supplies the trusted Vite scaffold, builds the bundle in a
temporary work directory, publishes only `dist/` into the preview repo under
`runs/{run_id}/{item_id}/{option_label}/`, pushes that route, and stores
`preview_url` on the generated option.

Required previews block inline fallback: Gitmoot will not publish the human
review issue until every generated option has a preview URL. Optional previews
prefer URLs but can fall back to inline Markdown if preview publishing is not
available. LaTeX/PDF, image, notebook, Storybook, and other preview types are
future adapters and are not implemented in this workflow.

Use previews only when the artifact has to be inspected as a rendered surface:

| Output type | Preview policy | Evaluator |
| --- | --- | --- |
| Vue landing page or UI | `preview.mode=required`, `vue-vite/github-pages` | `landing_page_v1` |
| Markdown, text plans, X/social post copy | `preview.mode=none` unless reviewers need a rendered page | text/LLM judge or fixture evaluator |
| LaTeX/PDF, image, notebook, Storybook | future preview adapter | future evaluator-specific contract |

GitHub review text is feedback for the optimizer. It is not a score. Optimizer
scores come from the evaluator result artifacts produced by
`gitmoot-skillopt`; if the evaluator is missing, fails, or returns invalid
JSON, the gate is blocked instead of treating the result as a numeric loss.

Evaluator profiles describe the artifact contract and evaluation stages for a
task. The landing-page profile uses cheap-first checks: validate the Vue/Vite
bundle contract, optionally run a render smoke adapter, and call the LLM judge
only after those checks pass. Failed checks produce structured failure packets
with `primary_reason`, `optimizer_hint`, failed checks, evidence, and stage
status so the optimizer can update the skill from the failure class instead of
only seeing a zero score.

## Review And Feedback

`train continue` generates options through Gitmoot-managed temporary agents,
stores artifact-backed review items/options, and publishes a concise GitHub
review packet when the review step is ready. For required Vue previews, run
`continue` once to generate bundles and a second time to build/publish previews
and create the review issue in the expected preview repo:

```sh
gitmoot skillopt train start \
  --template planner \
  --repo owner/product \
  --workspace-repo owner/product-workspace \
  --preview-repo owner/product-previews \
  --session landing-page-train \
  --request "Train landing-page review options" \
  --items-file train-items.yml \
  --yes

gitmoot skillopt train continue --session landing-page-train
gitmoot skillopt train continue --session landing-page-train
```

Low-level `skillopt feedback github publish` and `sync` enforce
`review.expected_repo` for train runs. If a preview train expects
`owner/previews`, publishing or syncing against `owner/product` fails instead of
posting to the wrong repository.

Review issues include a fenced `yaml` block so reviewers can copy, edit, and
submit parseable feedback. `train continue` automatically syncs GitHub comments
when the iteration is waiting in `review_published` and no feedback has been
imported yet. Raw YAML and fenced YAML are both supported.

Reviewers can provide ranked feedback with optional quality and phase hints:

```yaml
run_id: planner-train-review-001
reviewer: alice
items:
  - item_id: release-plan
    ranking:
      - C > A > D > B
    quality: acceptable
    continue_mode: refine
    useful_traits:
      C:
        - clearer verification sequencing
    rejected_traits:
      B:
        - too vague about rollback
    required_improvements:
      - clearer owner handoff
      - stronger rollout checks
    reasoning: C is strongest, but A has a better risk summary.
```

Field meanings:

- `quality`: reviewer confidence in the option set. Use `poor` when the whole
  set is weak, `acceptable` when there is a usable direction, and `strong` when
  the winner is strong enough to refine directly.
- `continue_mode`: requested next search phase. Use `explore` to widen the
  search, `refine` to improve the ranked winner, `distill` to simplify a strong
  direction, and `validate` when the output should mostly be checked.
- `promote`: human decision hint only. `yes` means the reviewer believes the
  candidate should become current after candidate review; `no` keeps it as
  optimizer feedback. Promotion still requires the explicit promote command.

Use `quality: poor` or `continue_mode: explore` when all options are weak and a
stable winner should not narrow the search yet. Gitmoot keeps feedback parsing
deterministic and stores imported events as canonical feedback for export.

## Optimizer And Candidate Gate

After feedback sync, `train continue` exports the training package, invokes the
configured `gitmoot-skillopt optimize` command, imports the returned candidate
package through the shared candidate validator, and leaves the candidate
pending. Use `--dry-run` only on a disposable or reset train session to validate
the package and optimizer command shape without model calls. If the dry-run
returns unchanged baseline content, Gitmoot records
`optimizer_completed_no_candidate` instead of publishing a candidate review.

```sh
gitmoot skillopt train continue \
  --session planner-train \
  --skillopt-bin /path/to/gitmoot-skillopt \
  --backend codex \
  --out-root .gitmoot/skillopt/planner-train \
  --dry-run
```

For the real landing-page optimizer pass on the production train session, do not
include `--dry-run`; request the Vue preview evaluator explicitly:

```sh
gitmoot skillopt train continue \
  --session landing-page-train \
  --skillopt-bin /path/to/gitmoot-skillopt \
  --out-root .gitmoot/skillopt/landing-page-train \
  --backend codex \
  --evaluator-id landing_page_v1 \
  --optimizer-model gpt-5.5 \
  --target-model gpt-5.5 \
  --evaluator-model gpt-5.5 \
  --skill-update-mode full_rewrite_minibatch \
  --num-epochs 1 \
  --batch-size 2 \
  --gate mixed
```

`--backend codex` resolves the user-facing optimizer, evaluator, and target
provider to `codex`; Gitmoot passes the internal SkillOpt target adapter as
`codex_exec`, so users do not need to remember that implementation detail.
Before training starts, `train continue` prints the resolved backend report,
config status, optimizer lock state, and recovery availability. Then
`gitmoot-skillopt` preflights the optimizer, target, and evaluator. The
canaries must prove the target can return the exact requested text and that the
evaluator can return structured hard/soft JSON. Optimizer failures record
blocked status and do not promote or partially install candidate templates. If
the optimizer selects the unchanged baseline, accepts no prompt edit, or returns
content with the same hash as the base template, Gitmoot records
`optimizer_completed_no_candidate` with `no_candidate_reason` and does not
create or publish a pending candidate review.

If the optimizer wrapper fails after writing completed artifacts, status reports
`status_phase: recovery_available`. Recover the artifacts through Gitmoot
instead of re-running blindly:

```sh
gitmoot skillopt train recover --session planner-train --out-root .gitmoot/skillopt/planner-train
```

Recovery validates the package state, imports a completed candidate through the
normal candidate gate, or records `optimizer_completed_no_candidate` with the
stored rejection reason. Incomplete or corrupted artifacts fail without
modifying the train state.

## Candidate Review And Next Iteration

The candidate review step publishes the candidate summary, template diff,
preview/PR links when available, and copyable decision commands. It separates
selection score, evaluator/test scores, gate status, no-op status, and
promotability so a candidate selected by the optimizer is not confused with a
candidate that passed evaluator gates. If stored metadata marks the candidate as
no-op or not promotable, the review body says promotion is unavailable instead
of showing a promote command.

Promote or reject explicitly:

```sh
gitmoot skillopt train continue --session planner-train --promote planner@v2
gitmoot skillopt train continue --session planner-train --reject planner@v2 --reason "Too broad"
```

After a decision, either stop or start the next iteration:

```sh
gitmoot skillopt train continue --session planner-train --start-next
```

Manual append-style next iterations are not supported. The train state machine
creates the next iteration atomically from the resolved previous decision, its
eval run, and copied item plan.

## Deterministic Smoke

Run the local train smoke script before shipping train-mode changes:

```sh
scripts/skillopt-train-smoke.sh
```

The script runs focused CLI smoke tests with fake managed generation, fake
`gitmoot-skillopt`, fake preview publication, and fake GitHub publication. It
covers local template creation, session setup, item/generation flow, required
preview blocking, expected review repo enforcement, preview URL review packets,
feedback-to-optimizer handoff, candidate import, candidate review publication,
optimizer recovery, stable status phases, promote/reject decisions, and
start-next gate enforcement without real model calls or real GitHub mutation.

## Troubleshooting

- Wrong repo: all train review publish/sync commands must target
  `review.expected_repo`. Use `gitmoot skillopt train status --session <id>` to
  confirm the expected review repo before publishing.
- Missing preview links: confirm the run has `--preview-repo`, the preview repo
  checkout is registered with `gitmoot repo add`, and the generated option
  output type is compatible with the current `vue-vite` renderer.
- Missing feedback import: reply with the fenced YAML block from the review
  issue or raw YAML. Then rerun `gitmoot skillopt train continue --session
  <id>`; it will attempt GitHub sync and report parse/import failures.
- Invalid YAML: status output names wrong `run_id`, missing item feedback,
  invalid ranking, unknown option, invalid signal values, no parseable YAML, or
  no comments.
- No candidate created: inspect `no_candidate_reason` in `train status
  --verbose`; usually the optimizer kept the initial skill, accepted no update,
  or produced content with the same hash as the base version.
- Optimizer wrapper failed after artifacts: if `status_phase` is
  `recovery_available`, run `gitmoot skillopt train recover --session <id>
  --out-root <optimizer-output-root>`.
- Stale optimizer lock: if `status_phase` is `blocked_stale_lock`, inspect
  `active_lock` owner, pid, host, heartbeat, and expiry in verbose status before
  clearing stale state or retrying the optimizer.
- Config blocker: if `status_phase` is `blocked_config`, fix the reported
  backend, credential, or model configuration and rerun `train continue`.
- Render adapter unavailable: install the profile's render dependency or run a
  profile/config that does not require render smoke. Required render profiles
  fail before the LLM judge so the optimizer receives a structured blocker.
- Missing evaluator: pass `--evaluator-id landing_page_v1` for landing-page
  runs. Text-only flows can rely on the package evaluator config or the default
  LLM judge.
- Evaluator skipped: check the evaluator profile and artifact contract. Cheap
  checks can intentionally skip expensive render/LLM stages after a hard
  contract failure.
- Invalid evaluator JSON: fix the evaluator prompt/model/backend and rerun
  `train continue`. Invalid hard/soft scores are blockers, not candidate
  rejections.
- Dry-run versus real optimizer: `--dry-run` checks package/candidate plumbing
  without model calls or evaluator scoring, but it still advances train state.
  Use a disposable/reset session for dry-runs; run the production session
  without `--dry-run` and expect preflight to require working
  credentials/backends.
