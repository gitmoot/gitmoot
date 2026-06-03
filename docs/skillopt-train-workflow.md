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
```

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
    reasoning: C is strongest, but A has a better risk summary.
```

Use `quality: poor` or `continue_mode: explore` when all options are weak and a
stable winner should not narrow the search yet. Gitmoot keeps feedback parsing
deterministic and stores imported events as canonical feedback for export.

## Optimizer And Candidate Gate

After feedback sync, `train continue` exports the training package, invokes the
configured `gitmoot-skillopt optimize` command, imports the returned candidate
package through the shared candidate validator, and leaves the candidate
pending. Use `--dry-run` to validate the package and optimizer command shape
without model calls.

```sh
gitmoot skillopt train continue \
  --session planner-train \
  --skillopt-bin /path/to/gitmoot-skillopt \
  --out-root .gitmoot/skillopt/planner-train \
  --dry-run
```

Optimizer failures record blocked status and do not promote or partially install
candidate templates.

## Candidate Review And Next Iteration

The candidate review step publishes the candidate summary, eval report, template
diff, preview/PR links when available, and copyable decision commands. Promote
or reject explicitly:

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
promote/reject decisions, and start-next gate enforcement without real model
calls or real GitHub mutation.
