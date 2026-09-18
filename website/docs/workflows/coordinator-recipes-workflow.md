# Coordinator Recipes Workflow

## Grouping work driven by an external coordinator

Add `--workflow <label>` when a coordinator outside Gitmoot needs to group
several jobs. It works on agent ask/run/review, `orchestrate`, and
`job open`; every delegation child and continuation inherits it.

```sh
gitmoot orchestrate planner "Run release checks." --repo owner/repo --workflow fable/release-42
gitmoot workflow describe fable/release-42 "Validate and ship release 42."
gitmoot workflow note fable/release-42 "Canary passed." --author operator
gitmoot workflow show fable/release-42
```

The read-only web dashboard clusters labeled jobs around workflow hubs in
Galaxy. The Workflows index and `/workflows/<label>` mission log combine
complete run trees, derived active/stalled/settled state, best-effort token
totals, the shared note journal, and the latest coordinator handoff. Labels may
use one namespace slash; each side remains a lowercase alphanumeric/single-hyphen
slug. `--pane`, `--session`, and `--workdir` on `workflow note` update the
handoff used by the dashboard's resume card. Inside Herdr, omitted handoff flags
default to the current pane label, full agent session UUID, and working
directory. Explicit flags override detection; `--no-auto` disables it. Detection
fails open without blocking the note and never invents an author. Resume
commands are emitted only for full UUID session ids. The workflow header carries
a stable auto-seeded description and live status. Override the former with
`workflow describe`; set the latter manually with `workflow note --status` when
there is no linked PR lifecycle to advance it. Linked PR transitions write
deduped `[auto:pr:...]` notes as author `daemon`.

Journal text and authors are stored verbatim. JSON keeps them verbatim, while
terminal text output sanitizes escapes/control bytes and caps each field to one
line.

Coordinator recipes are built-in agent templates that turn the
[Orchestra pattern](../reference/result-contract.md#delegations-orchestration)
into one-command workflows. Each recipe is a coordinator prompt that emits a
`delegations[]` of **ephemeral** workers — created on demand from an inline spec,
run once, then auto-disposed — so you do not pre-register any agents. Gitmoot runs
the panelists or legs in the daemon, then reconvenes their results in a single
continuation back to the coordinator.

There are two invocation styles. The primary one is the `--recipe` flag on
`gitmoot orchestrate` (also accepted on `gitmoot agent run`), which routes
**any existing coordinator agent** through the named built-in recipe prompt
without changing the agent's identity or registration (#477):

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier
```

The second style passes the recipe id as the agent positional —
`gitmoot orchestrate review-panel "..." --repo owner/repo` — but the positional
must resolve to a **registered agent** (or configured managed type), so it only
works after registering an agent under the recipe name (`agent start
review-panel --template review-panel …`). On a fresh install without that
registration it fails with "agent not found"; prefer `--recipe`.

Two recipes ship built in. Install or refresh any template the same way as any
built-in template:

```sh
gitmoot agent template show review-panel
```

## Review Panel

`review-panel` reviews a pull request or change by convening a panel of
independent reviewers, each looking through a different lens, then synthesizing
their findings into one verdict. It fans out three dep-free reviewers by default —
correctness and security; performance and maintainability; tests and edge cases —
so they review in parallel. Each panelist is an ephemeral worker with a
self-contained lens prompt, and the recipe mixes runtimes so the panel does not
share one model's blind spots (point a panelist at an installed review template
such as `thermo-nuclear-code-quality-review` only if you want).

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
```

Once every panelist is terminal, Gitmoot enqueues one continuation that
de-duplicates the findings, decides the verdict (`changes_requested` if any
reviewer raised a blocking issue, else `approved`), and reports which lenses drove
it.

## Decompose and Verify (retired)

`decompose-and-verify` was RETIRED because Gitmoot no longer dispatches
implementation (#2203). The recipe existed to split one implementation task into
file-disjoint subtasks and fan them out to ephemeral implementation workers
building in parallel in their own branch worktrees. `ask` and `review` are now
the only accepted delegation actions, so there is no action those legs could run
under and no branch worktree for them to build in.

What it taught still applies to any fan-out you write by hand: split into
**file-disjoint** subtasks so parallel legs cannot conflict with each other, and
end the DAG with a separate verify worker that `deps` on every leg instead of
trusting a producer's self-report. The `verifier` recipe below is the surviving
one-producer form of that idea.

The id stays reserved rather than unknown: `gitmoot agent template list` hides
it, `gitmoot agent template show <id>` and `gitmoot agent prompt <id>` refuse it
by name and point at a successor recipe, and an agent or managed type still
configured with it fails dispatch with that same named refusal — so a stale
script or a saved command gets a successor instead of "unknown template".

## Verifier

`verifier` is the minimal **produce vs. independent check** recipe: one producer
leg plus one independent verify leg.

A `synthesis_rule` (`summary`/`vote`/`quorum`) only reconciles what the producers
**self-report** — "I approve / I implemented it". That is *self-evaluation*, and
it inherits the producer's blind spots: the model that missed an edge case while
building tends to miss it while grading its own work. The `verifier` recipe adds a
separate **verify leg** — a read-only ephemeral `review` worker that `deps` on the
producer, runs on a **different runtime/model**, and checks the producer's
combined result against the original goal, re-running the build and tests itself
rather than trusting the producer's self-report. It returns `changes_requested`
with structured findings on any objective failure, else `approved`. That is
*cross-evaluation*, which the literature consistently finds beats self-evaluation:
a capable verifier catches failures the solver does not (the generator-verifier
gap), and LLM-as-judge graders show a self-preference bias toward their own
outputs that a different-model judge does not share. It is the same separation as
[ROMA](https://github.com/sentient-agi/ROMA)'s Verifier
(`(goal, candidate_output) -> verdict + feedback`), where a failed verdict drives
a re-plan rather than trusting the producer. `verifier` is the one-producer form
of that idea.

```sh
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier
```

A failed verdict routes through the verify leg's `failure_policy: escalate` back
to the coordinator continuation for autonomous correction (re-delegate a fixed
producer plus a fresh verify); set `failure_policy: escalate_human` instead for a
human-in-the-loop pause until someone runs `/gitmoot resume`. Either way the merge
gate independently blocks merge on the non-ready decision. No new engine primitive
is involved — `verifier` uses only the shipped `ephemeral` spec, `failure_policy`,
and merge gate.

## Coordinator-owned review

When a PR appears on a branch Gitmoot holds a lock for, the daemon's PR-watcher
fans it out to Gitmoot's native reviewers — the configured required reviewers,
or the ones passed for the task — so each reviewer runs as its own review job
before the merge gate. When a coordinator already plans review itself (for
example a `review-panel` leg, or a custom continuation that reconvenes its own
reviewers), that native fan-out duplicates work. Pass
`--skip-native-review-fanout` on `gitmoot orchestrate` or `gitmoot agent run`
to hand review orchestration to the coordinator:

```sh
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier --skip-native-review-fanout
gitmoot agent run lead --repo owner/repo --pr 12 --skip-native-review-fanout "Review this PR."
```

The flag is persisted on the job payload and on the branch lock, and the
PR-watcher reads the lock, so a PR it observes on that branch stays free of
native review fan-out. The engine's implement-advance arm reads the same flag,
but Gitmoot no longer dispatches implementation (#2203), so the PR-watcher is
the path that still exercises it. The flag defaults off; leaving it off keeps
the full native review fan-out.

A forge-reported draft does not park its task at `awaiting_human_merge`, because
no human merge decision has been requested yet. The `--draft`/`--ready` dispatch
flags went with `agent implement`, so mark the PR ready on the forge when it
should enter review and merge-gate processing.

## Ephemeral, leaf-only, bounded

In both recipes the delegations never set `agent`: `agent` and `ephemeral`
are mutually exclusive, and every panelist or leg here is ephemeral. Ephemeral workers
are **leaf-only** — they return findings, never their own delegations — so a
recipe's fan-out is exactly one level deep. The recipes run inside the same
delegation [termination bounds](../reference/result-contract.md#termination-bounds)
as any orchestra: a depth cap, a per-root job budget, a per-root wall-clock
budget, a per-coordinator width cap, and loop detection — all ending in one
graceful finalize continuation.

Inspect a run with the usual job and event commands:

```sh
gitmoot job list --repo owner/repo
gitmoot events --repo owner/repo
```

See the [Result Contract](../reference/result-contract.md) for the `ephemeral`
field reference and the
[registered agent vs. ephemeral worker](../concepts/agents-templates-jobs-locks.md#choosing-a-worker-registered-agent-vs-ephemeral-worker)
comparison.
