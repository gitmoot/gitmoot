# Gitmoot CLI Reference

Use these commands from an agent session only when the user asks for Gitmoot
setup, status, agent coordination, or PR-comment workflow help.

## Install And Update

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gitmoot update --check
gitmoot update --restart-daemon
```

Verify GitHub access before PR workflows:

```sh
gh auth status
```

## Runtime Plugins

Install Gitmoot's Agent Skill into Codex or Claude Code when the user wants the
runtime to discover Gitmoot workflow guidance from its plugin system:

```sh
gitmoot plugin install codex
gitmoot plugin install claude
gitmoot plugin doctor
```

Inspect or build packages without installing:

```sh
gitmoot plugin build codex
gitmoot plugin build claude
gitmoot plugin path codex
gitmoot plugin path claude
gitmoot plugin doctor codex
gitmoot plugin doctor claude
```

Claude scopes are supported with `--scope user|project|local`. Codex ignores
`--scope` because the current Codex plugin install command does not use it.

## Repo And Daemon Status

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot daemon start --repo owner/repo --poll 30s --workers 1
gitmoot daemon status
gitmoot daemon logs
gitmoot daemon stop
```

Use `daemon start` for the background daemon. Use `daemon run` only when the
user explicitly wants a foreground process. Keep the default `--workers 1`
unless the Gitmoot home has multiple independent runtime sessions or managed
agent types with `max_background` greater than one.

`gitmoot dashboard` shows local state — daemon health, repos, agents and runtime
sessions, jobs by state, worktrees, branch locks, SkillOpt train phase/candidate,
and pending interactive prompts.

On a real terminal (stdin and stdout both a TTY) and with no other output/mutation
flag, `gitmoot dashboard` launches an **interactive TUI**: a sidebar of pages
(Attention, Trains, Agents, Sessions, Jobs, Locks) that auto-refreshes. Navigate
with `tab`/`shift+tab` or `←/→`; on the Attention page select a pending prompt
with `↑/↓` and `a` to answer it inline (a choice list or a text field) or `d` to
dismiss it; on the Trains page `enter` opens a per-session detail with its locks;
`r` refreshes, `q` quits. Inline answers/dismissals use the same store APIs as
`gitmoot interactive answer` / `clear`.

Everywhere else — pipes, redirects, CI, `--plain`, `--json`, `--all`, `--watch`,
`--answer`, `--dismiss` — it prints the one-shot snapshot instead, unchanged. Set
`GITMOOT_NO_TUI=1` or `TERM=dumb` to force the non-interactive path globally.

```sh
gitmoot dashboard                  # interactive TUI on a terminal
gitmoot dashboard --plain          # one-shot snapshot on a terminal
gitmoot dashboard --json
gitmoot dashboard --all
gitmoot dashboard --answer <prompt-id> --value <value>
gitmoot dashboard --dismiss <prompt-id>
gitmoot dashboard --watch          # plain redraw until Ctrl-C (terminal only)
gitmoot dashboard --watch --interval 2s
```

In the one-shot styled output the dashboard leads with a "needs attention" block,
colors and truncates long lists, and groups near-identical runtime sessions;
`--all` shows everything. `--watch` redraws on an interval (default 5s) and cannot
be combined with `--json`, `--answer`, or `--dismiss`.

## Agent Setup

Start a new runtime session managed by Gitmoot:

```sh
gitmoot agent start reviewer \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --role reviewer \
  --capability ask \
  --capability review \
  --start-daemon
```

Subscribe an existing runtime session:

```sh
gitmoot agent subscribe reviewer \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --role reviewer \
  --capability ask \
  --capability review
```

Inspect agents:

```sh
gitmoot agent list
gitmoot agent show reviewer
gitmoot agent show reviewer --json
gitmoot agent repos reviewer
gitmoot agent doctor reviewer
```

Delegate to a registered agent from the current local chat:

```sh
gitmoot agent run project-planner --repo owner/repo "Return the plan status."
gitmoot agent run lead --repo owner/repo --task task-001 --background "Implement this task."
gitmoot agent run reviewer --repo owner/repo --pr 12 --background "Review this PR."
gitmoot agent review reviewer --repo owner/repo --pr 12 "Review this PR."
gitmoot agent implement lead --repo owner/repo --task task-001 "Implement this task."
gitmoot agent ask project-planner --repo owner/repo "Return the plan status."
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan and goal file."
gitmoot job watch <job-id>
```

This uses the same agent registry, repo access grants, cached template snapshot,
runtime adapter, and local job history as PR-comment jobs. `agent run` is the
default coordinator-safe entrypoint because it routes to `ask`, `review`, or
`implement` and keeps branch, worktree, commit, push, PR, and workflow lifecycle
inside Gitmoot. `agent ask` is for analysis, planning, and questions only; it
blocks obvious branch/commit/push/PR orchestration unless `--force` is supplied.
The runtime
plugin helps Codex or Claude Code discover Gitmoot guidance, but it does not
replace the Gitmoot CLI. Synchronous jobs and queued jobs both use the same
runtime session locks.

Configure managed background agent types:

```sh
gitmoot agent type list
gitmoot agent type show planner
gitmoot agent type set planner --runtime codex --template planner --max-background 2 --idle-timeout 20m
gitmoot agent gc
```

## Agent Templates

Install or refresh the built-in thermo review template:

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Install or refresh the built-in full planner/goal template:

```sh
gitmoot agent template update planner
gitmoot agent start project-planner \
  --runtime codex \
  --repo owner/repo \
  --path . \
  --template planner \
  --start-daemon
```

For fast current-chat planning, use the Gitmoot skill with the same packaged
`agent-templates/planner.md` instructions instead of starting a background job:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

The current chat can also import any cached custom agent or template prompt:

```sh
gitmoot agent prompt frontend-reviewer
gitmoot agent prompt frontend-reviewer --json
```

This prints the prompt content for the current chat to apply locally. It does
not create a job, start a daemon, resume a runtime session, or post a PR
comment.

Draft and validate a captured template before installing it:

```sh
gitmoot agent template draft release-planner
gitmoot agent template validate .gitmoot/templates/release-planner.md
gitmoot agent template add release-planner --file .gitmoot/templates/release-planner.md
```

`agent template draft` only creates the standard markdown structure. For
current-chat capture, the active Codex or Claude chat reads
`references/TEMPLATE_CAPTURE.md` and fills that structure from visible
conversation context. Gitmoot does not extract hidden runtime memory.

Create a local custom prompt template:

```sh
mkdir -p agents
gitmoot agent template draft frontend-reviewer --output agents/frontend-reviewer.md
$EDITOR agents/frontend-reviewer.md
gitmoot agent template validate agents/frontend-reviewer.md
gitmoot agent template add frontend-reviewer --file agents/frontend-reviewer.md
gitmoot agent start frontend-reviewer \
  --runtime codex \
  --repo owner/repo \
  --template frontend-reviewer \
  --role reviewer \
  --capability ask \
  --capability review
```

After editing a local template file, refresh Gitmoot's cached snapshot:

```sh
gitmoot agent template diff frontend-reviewer
gitmoot agent template update frontend-reviewer
```

Template updates are versioned locally. `gitmoot agent template show <id>`
prints the current version, content hash, source commit, and promotion state.
Agents use the current promoted version by default, or a pinned version when
configured with a reference such as `--template frontend-reviewer@v1`.
Queued jobs keep the exact template content snapshot they were created with.

Discover templates by metadata:

```sh
gitmoot agent template list --runtime codex --output goal_file
gitmoot agent template list --tag review --capability ask
gitmoot agent template show frontend-reviewer
```

## Goals

Print the standard Gitmoot goal prompt template:

```sh
gitmoot goal template
```

Import a goal file into local Gitmoot state:

```sh
gitmoot goal import --file GOAL-feature.md --repo owner/repo
```

Start a task in its dedicated branch worktree and inspect task state:

```sh
gitmoot task run task-001 --repo owner/repo --owner lead --base main
gitmoot task list --repo owner/repo
gitmoot task list --repo owner/repo --state implementing --json
```

`task run` stores the deterministic task worktree path under
`$GITMOOT_HOME/worktrees/<owner>--<repo>/<task-id>/` and leaves the registered
checkout on its current branch.

## PR Comments

Use GitHub PR comments as the public audit trail:

```text
/gitmoot help
/gitmoot status
/gitmoot <agent> review [instructions]
/gitmoot <agent> implement [instructions]
/gitmoot ask <agent> [question]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
```

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job watch <job-id>
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cancel <job-id>
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

Merge-gate retries are automatic while the daemon is running. Retryable states,
such as a busy base-branch merge queue or a GitHub branch update in progress,
are retried on the next daemon poll tick. The default poll interval is `30s`
unless the daemon was started with a different `--poll`.

## SkillOpt Exchange

```sh
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>
gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md [--title text]
gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id> --mode explore --exploration-level high --options 4
gitmoot skillopt review item add --run <run-id> --item <item-id> --option a=option-a.md --option b=option-b.md [...]
gitmoot skillopt review status --run <run-id>
gitmoot skillopt export --run <run-id> [--output training.json]
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
gitmoot skillopt candidate list [--template id]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> [--reason text]
gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>
gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]
gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]
gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)
gitmoot skillopt train start --template <id> --repo owner/repo --request <text> --items-file items.yml [--workspace-repo owner/workspace] [--preview-repo owner/previews] [--preview-mode none|optional|required] [--preview-renderer none|vue-vite] [--preview-publisher none|github-pages] [--preview-route-template template] [--create-repos] [--yes]
gitmoot skillopt train status --session <id>
gitmoot skillopt train run [--config path | --session <id>] [--plain]
gitmoot skillopt train continue --session <id> [--generator-type skillopt-generator | --generator-agent name] [--skillopt-bin path] [--dry-run] [--promote version|--reject version --reason text] [--start-next]
gitmoot skillopt train stop --session <id> --reason <text>
```

On a real terminal, `skillopt train run` opens an interactive view of one session
(resolved from `--session` or the newest session of a `--config`): a phase bar
plus a single keypress per step — `enter` advances the current phase (the long
generate/optimizer steps run in a detached background process so `q` leaves the
run going), `p`/`x` promote or reject a candidate, `n` starts the next iteration.
Review-blocked phases show the GitHub issue link to continue from the browser.
`--plain`, a piped stdin, or `GITMOOT_NO_TUI`/`TERM=dumb` print a one-shot status
snapshot instead. `train status`/`continue` print a `continue_from_github:` line
at review-blocked phases. `train start --create-repos` (or the prompt in the
`train init` form) creates a missing target/workspace/review repo on GitHub.

Use `skillopt train` for the product workflow. It pins the template version,
tracks sessions and iterations, validates item diversity, generates temporary
agent options, publishes review packets, syncs feedback, hands off to the
external optimizer, imports pending candidates, publishes candidate review
context, and starts follow-up iterations only after a promoted/rejected/abandoned
decision. Use `skillopt review`, `feedback`, `export`, `import`, and
`candidate` directly only for advanced debugging, custom research runs, or
recovering one step of a train session.

`skillopt review create` starts a review run for a template and target repo.
Use the default A/B shape for validation, or pass `--mode explore|refine|distill`
with `--options N` for ranked exploration. `skillopt review item add` stores
saved baseline/candidate outputs as artifact-backed A/B review items, or
repeated `--option label=path` artifacts for ranked N-way items. `skillopt
review status` reports whether the run has items, complete artifacts, imported
feedback for every item, ranking stability, pairwise preference count, and a
recommended next mode. Recommendations are advisory; Gitmoot never changes mode,
imports a candidate, or promotes a template automatically.

`skillopt export` writes a JSON training package with the template snapshot,
eval run, review items, artifact manifests, feedback events when present, and
evaluator config. Use `gitmoot-skillopt optimize --training-package
training.json --artifact-root ~/.gitmoot/evals/blobs --out-root
.gitmoot/skillopt/<run-id> --candidate-output candidate.json --dry-run` first
to validate the contract without model calls.
Before real model-backed optimization, check `gitmoot-skillopt optimize --help`
and verify required model/backend environment variables for the installed
optimizer version. `skillopt import` validates a candidate package and stores
the candidate template as a pending version; it never promotes the candidate
automatically. If the candidate package includes new artifact manifest entries,
pass `--artifact-dir` so Gitmoot can verify relative paths and SHA256 hashes
before storing blobs. `skillopt candidate show` displays candidate metadata, eval
report JSON, preference summary, and a content diff against the base/current
version. `skillopt candidate promote` makes a pending candidate current, while
`skillopt candidate reject` records an auditable rejection and prevents that
version from being selected by `@latest`.

The Markdown feedback collector writes blind A/B review packets with `index.md`,
per-item Markdown files, editable `feedback.yml`, and hidden assignment metadata
that Gitmoot uses to validate the full response and import de-blinded canonical
feedback events. Open `index.md`, review every file in `items/*.md`, set
`reviewer`, edit `feedback.yml` with exactly one of `a`, `b`, `tie`, `neither`,
or `skip` for every item, and leave `.assignments.json` untouched.
Ranked packets use the same files, but `feedback.yml` contains ordered rankings
plus optional `useful_traits`, `rejected_traits`, and reasoning. After feedback
exists, packet summaries hide outcome-bearing phase details so later blind
reviewers do not see the current winner before responding.

The GitHub feedback collector publishes the same blind A/B review packet to a
new issue by default, or to an existing PR when `--pr <number>` is provided.
Repository resolution uses `--repo`, then the eval run target repo, then the
template source repo, then optional `[feedback].repo = "owner/reviews"` in
Gitmoot config. Reviewers can reply with full YAML or run-scoped short-form
lines such as `run_id: run-1` followed by `item-001: b - More concrete.`.
`github sync` imports valid comments into canonical feedback events and ignores
unrelated comments safely.
Ranked GitHub comments can use `item-001 ranking: C > A > D > B` plus trait
notes. Use the ranked workflow for exploration/refinement and return to A/B
validation for final promotion decisions on fresh items.
