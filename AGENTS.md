# AGENTS.md — gitmoot

> Agent operating context for this repo (agents.md spec). Complements `README.md`
> (which is for humans). `CLAUDE.md` imports this file via `@AGENTS.md`.
>
> This is the filled-in **Project Map** for the `lead-engineer` work strategy.
> Accuracy is sacred: every claim here is verified against the repo or describes
> this host's live deployment. Don't add a claim you haven't checked.

## Project overview

gitmoot is a local-first coordinator for AI coding agents working across GitHub
repositories, pull requests, goals, reviews, and runtime workflows. It ships as a
single static Go binary plus a background daemon; workflow state lives in local
SQLite (the **modernc pure-Go** driver — no cgo). The single static binary with
**zero runtime dependencies** is a core invariant.

It drives five runtimes (`codex`, `claude`, `kimi`, `omp`, `shell`). `agent start` supports codex/claude/kimi/omp; `shell` is a
subscribe-only command runtime used mainly to drive engine-feature E2Es with no
LLM.

## Build, test, and verify (the gate)

Requires Go 1.26+ (see `go.mod`; CI resolves the version via
`go-version-file: go.mod`). On this host, pin the toolchain:

```sh
export GOTOOLCHAIN=local PATH=/root/.local/toolchains/go1.26.4/bin:$PATH
export GOCACHE=/tmp/gitmoot-go-build-cache
mkdir -p "$GOCACHE"
```

Run from the repo root and make these pass before committing — they mirror the CI
gate in `.github/workflows/ci.yml`:

```sh
go build -buildvcs=false ./...
go generate ./... && git diff --exit-code   # gitmoot_result contract is single-sourced + regenerated; stale artifact fails CI
go vet ./...
go test -timeout 25m ./...

# Race gate is scoped (not ./...). Use CI's package shard counts and its
# deterministic alternating partitioner so no growing package hits one monolithic
# timeout. Each compiled binary covers every package test exactly once.
(
  set -e
  race_dir="$(mktemp -d "${TMPDIR:-/tmp}/gitmoot-race.XXXXXX")"
  printf 'race artifacts: %s\n' "$race_dir"
  for spec in cli:8 pipeline:4 db:2 workflow:4 daemon:1; do
    package="${spec%%:*}"
    shards="${spec##*:}"
    bundle="$race_dir/$package"
    mkdir -p "$bundle/partitions"
    go test -c -race -o "$bundle/$package.test" "./internal/$package/"
    (
      cd "internal/$package"
      "$bundle/$package.test" -test.list '.*'
    ) >"$bundle/tests.list"
    scripts/partition-race-tests.sh \
      --tests "$bundle/tests.list" \
      --shards "$shards" \
      --out-dir "$bundle/partitions"
    for ((shard = 0; shard < shards; shard++)); do
      run_regex="$(cat "$bundle/partitions/shard-$shard.regex")"
      (
        cd "internal/$package"
        "$bundle/$package.test" \
          -test.run="$run_regex" \
          -test.timeout=20m
      )
    done
  done
)
```

See [`docs/testing-performance.md`](docs/testing-performance.md) before changing
test setup or CI partitioning. Writable test databases should copy the cached,
migration-keyed schema through `internal/db/dbtest`; tests that exercise
migration behavior must keep using the real `db.Open` path.

Managed-worktree runtime seats append `-buildvcs=false` to inherited `GOFLAGS`
so stray ancestor `.git` directories cannot confuse Go's VCS root detection.

The explicit temporary `GOCACHE` is also part of the host setup. Managed
worktrees can inherit a read-only `/root/.cache/go-build`; redirecting the cache
keeps build, vet, and test from failing during package setup before compilation.
This is documented instead of changing `/root/.cache` permissions because that
directory is host-global external state, while the gate must remain runnable in
each agent's environment.

The race block deliberately does not delete `race_dir`: recursive deletion is
policy-rejected for managed coordinators, and cleanup is not required for gate
correctness. The printed per-run directory and
`/tmp/gitmoot-go-build-cache` therefore persist for later owner-managed cleanup.

When the repository checkout itself is under `/tmp`,
`TestClaudeProduceHookAutoReadLandlockE2E` is a known host-environment confound:
it fails there on current `main` as well as feature branches, so that failure is
not evidence about the branch under test. In a `/tmp` checkout, run the non-race
gate with that one test explicitly skipped:

```sh
go test -timeout 25m -skip 'TestClaudeProduceHookAutoReadLandlockE2E' ./...
```

`-buildvcs=false` is required, not optional, inside a gitmoot worktree (#1209):
Go's VCS auto-stamp only recognizes a `.git` **directory** as a repo root
(`cmd/go/internal/vcs.vcsGit.RootNames`), but a linked worktree's `.git` is a
**file** (a `gitdir:` pointer). Go's root-detection walk-up skips past the
worktree's real root looking for any ancestor with a `.git`-shaped directory,
and can land on an unrelated one — hard-failing with `error obtaining VCS
status: exit status 128` even though `git status` itself works fine from the
same directory. This is a genuine Go toolchain limitation with linked
worktrees (confirmed by reading `cmd/go/internal/vcs/vcs.go`'s `FromDir` /
`isVCSRoot`), not something gitmoot's code or config can fix, and even the
non-failing cases can silently stamp the wrong VCS metadata (wrong commit,
wrong dirty bit) from whatever directory the walk-up happened to land on.
Disabling it here costs nothing real: release binaries get their version
info from the explicit `-ldflags -X ...Commit=$(git rev-parse HEAD)` recipe
in the deploy section below, never from Go's auto-stamp.

`-timeout 25m` on the plain `go test ./...` closes the same kind of gap
(#1210): Go's default test timeout is 600s **per package**, not per `./...`
invocation, and `internal/cli`'s suite alone can run past that on a clean
local clone. CI's own build+generate+vet+test job isn't at risk (it
completes in well under 10 minutes on its runners), so this was a
local-only gap — but a command documented as "run this before committing"
has to actually be able to finish.

The CLI entrypoint lives under `cmd/gitmoot/`. The CI gate is Go-only — it does
**not** build the website or run the live multi-runtime (codex/claude/kimi) E2E
(those need a Node build / runtime auth and stay manual).

Prefer driving engine-feature E2Es with **no LLM** via the `shell` runtime on an
isolated `/tmp` home, and test home-scoped daemon seams at the true runtime
boundary — component tests miss the home double-resolution bug class (#446/#459).

## Repository layout

- `cmd/gitmoot/` — CLI entrypoint.
- `internal/cli/` — the command surface (agent, template, memory,
  dashboard) **and the `daemon` command wiring / worker loop**.
- `internal/daemon/` — the PR-watcher daemon package (poll/resume/revert logic).
- `internal/pipeline/`: the pipeline engine. Start with:
  - `pipeline_run.go` for advancement, stage enqueueing, worktree isolation,
    settlement, run creation, and job-event timestamps.
  - `pipeline_trigger.go` for trigger evaluation.
  - `pipeline_service*.go` for service admission, finalization, and artifacts.
  - `pipeline_expose.go`, `pipeline_auto_merge.go`, and `pipeline_resume.go` for
    the remaining engine flows.
  - `spec.go`, `validate.go`, `state.go`, `env.go`, and `service_schema.go` for
    engine types, validation, state, environments, and schemas.
- `internal/workflow/` — the job/delegation engine, mailbox, memory controller,
  and the `gitmoot_result` contract.
- `internal/runtime/` — the Codex/Claude/Kimi/omp/shell adapters.
- `internal/config/` — config loaders (`init.go` holds the `DefaultConfig`
  template + per-section loaders).
- `internal/db/` — the SQLite store + the migrations slice.
- Other notable `internal/` packages: `agenttemplate`, `report` (bug reports),
  `presence`, `memory`, `doctor`, `cockpit`, `plugin*`.
- `skills/gitmoot/` — the packaged Agent Skill: `SKILL.md` + `references/`
  (`CLI.md`, `WORKFLOWS.md`, `RESULT_CONTRACT.md`, `SAFETY.md`, …) +
  `agent-templates/`.
- `docs/` — in-repo reference docs. `website/` — the Docusaurus site (separate
  tree). `scripts/` — repo scripts.

## Documentation — two independent trees

Docs live in **two places that do not auto-sync**, so a docs change usually needs
both:

1. **In-repo**: `docs/`, `skills/gitmoot/`, `README.md`, `CONTRIBUTING.md`.
2. **Website**: `website/docs/` (Docusaurus) — published to
   <https://gitmoot.io/docs>.

The website is **not auto-deployed**. It is served by nginx from
`/var/www/gitmoot-docs/` and published manually (see
`website/docs/operations/deployment.md`):

```sh
cd website && npm install && npm run build       # onBrokenLinks: throw — build fails on bad links/sidebar ids
rsync -a --delete build/ /var/www/gitmoot-docs/  # destructive; back up the target first
```

`website/sidebars.ts` is manual; add new pages there. `website/static/llms.txt`
is hand-curated. `website/static/llms-full.txt` is generated and ignored;
`npm run build:llms` recreates it as part of `npm run build`.

**Docs ship with code**: every user-facing change updates the skill / `CLI.md` /
site / `llms.txt` in the **same PR**, and you never document behavior you haven't
verified against the code — grep `main`, not a stale feature checkout.

## Runtime map / this host's live deployment

The facts below describe **this box's** running deployment (operator reality), not
portable code behavior.

- The live daemon runs as `systemd --user gitmoot-daemon`. Its token is supplied
  via a `chmod 600` EnvironmentFile at `/root/.config/gitmoot/daemon.env` (which
  also carries `PATH`); `loginctl enable-linger` keeps it alive. Manage it with
  `systemctl --user`, **not** `gitmoot daemon restart` (which spawns a 2nd
  daemon). Footgun: `daemon run` does not update `daemon.json`, so
  `gitmoot daemon status` / the dashboard can falsely read "stopped".
- Deployed binary: `/root/.local/bin/gitmoot`.
- `--home /x` resolves to `/x/.gitmoot`. The live daemon home is `/root/.gitmoot`.
  **Never touch `/root/.gitmoot` in tests** — use throwaway `/tmp` homes only.
- The daemon rebuilds its per-repo workflow engine each tick and warm-reloads
  runtime config on `SIGHUP` (#577), so many config edits (e.g. `[memory]`,
  worker count, poll interval) take effect without a full restart. Warm reload
  never re-execs (it preserves inherited runtime auth — the #559 lesson).
- Public read-only dashboard: <https://gitmoot.themartian.app> (a separate
  `gitmoot-dashboard-web` systemd service behind traefik). Docs site: gitmoot.io.
  That service runs its **own copy of the binary** at
  `/root/.local/bin/gitmoot-dashboard-web` (`ExecStart=… dashboard --web --addr
  172.17.0.1:8790`), so replacing `/root/.local/bin/gitmoot` alone leaves the
  public dashboard on the old build — see the deploy recipe.

## Hard rules (footguns)

- **Never touch `/root/.gitmoot`** in tests/E2E — isolated `/tmp` homes only.
- **Never re-resolve an already-resolved home** (`<home>/.gitmoot/.gitmoot` →
  silent nil; the #446/#459 bug class). Use the dual-mode resolver.
- Manage the live daemon via `systemctl --user`, never `gitmoot daemon restart`.
- In E2E/orchestrate set `HERDR_SOCKET_PATH=/tmp/throwaway` and unset `HERDR_ENV`
  or panes leak to the prod Telegram group.
- Global flags like `--home` use Go flag parsing, so they must precede positional
  args (e.g. `agent template --home /tmp/h show <id>`, not after the id).
- `agent template add` needs a file with YAML frontmatter — use
  `agent template draft` to scaffold one. `template publish --create` makes a
  **private** repo; prompt bodies + metadata are stored/published **verbatim**,
  so point the remote at a private repo unless the prompts are meant to be public.
- An invalid `CLAUDE_CODE_OAUTH_TOKEN` 401s fresh claude sessions but `--resume`
  masks it; `gitmoot doctor` "auth ok" is set-not-valid (a false green).
- Killing a foreground `agent ask` strands a runtime-session resource lock;
  clear the lock to recover.
- codex ephemeral workers need `~/.codex/config.toml`
  `[sandbox_workspace_write] network_access=true` to push / open PRs.
- Agent permission policies gate Bash: `--policy workspace-write` auto-accepts
  **file edits only** and does **not** unblock Bash (`go`/`git`/`gh`). A full
  implement/push agent needs broader access; workspace-write alone is edits-only.
- The single static binary is sacred: no cgo, no runtime deps (modernc pure-Go
  SQLite).

## Agent jobs & the result contract

gitmoot runs agents through registered runtimes — **Codex, Claude Code, Kimi
Code, and omp** (`gitmoot agent start --runtime codex|claude|kimi|omp`).
Jobs return a `gitmoot_result` JSON object, and agents can fan work out via a
validated `delegations[]` DAG with a coordinator continuation job (the
**Orchestra** pattern), bounded by depth, a per-root job budget, and loop
detection.
`gitmoot orchestrate <agent> "..." [--repo R]` is sugar for
`gitmoot agent run <agent> --background "..."`. Contracts:

- `skills/gitmoot/references/RESULT_CONTRACT.md` — `gitmoot_result` + the
  `delegations` fields and termination bounds.
- `skills/gitmoot/references/SAFETY.md` — checkout/runtime/branch locks and
  delegation termination bounds.
- `skills/gitmoot/SKILL.md` — the entry point for the Gitmoot agent skill.

## Deploy recipe (this host)

1. On `main` after merge, build with the pinned toolchain (above). Stamp the
   version the way `release.yml` does, or `gitmoot version` reports
   `commit: unknown`:

   ```sh
   PKG=github.com/gitmoot/gitmoot/internal/buildinfo
   CGO_ENABLED=0 go build -trimpath -ldflags \
     "-s -w -X $PKG.Version=dev-$(git rev-parse --short HEAD) \
      -X $PKG.Commit=$(git rev-parse HEAD) -X $PKG.Date=$(date -Iseconds)" \
     -o /root/.local/bin/gitmoot.new ./cmd/gitmoot
   ```

2. `mv`-rename the new binary into `/root/.local/bin/gitmoot` (same filesystem;
   the rename avoids `ETXTBSY`).
3. **Two services run two binaries.** The public dashboard has its own copy, so
   a deploy that touches only `gitmoot` silently leaves the dashboard stale:

   ```sh
   cp /root/.local/bin/gitmoot /root/.local/bin/gitmoot-dashboard-web.new
   mv /root/.local/bin/gitmoot-dashboard-web.new /root/.local/bin/gitmoot-dashboard-web
   ```

4. Restart at idle: confirm 0 running/queued **engine-dispatched** jobs
   first, not raw job count. Session-recorded jobs (`session-ask-*`/
   `session-implement-*`, #657) run entirely outside the daemon process — no
   subprocess, no lease — and may legitimately stay `running` for hours, so
   they don't belong in this check. #1125's reaper keeps genuinely abandoned
   session jobs from piling up forever, but on an hours-to-a-day timescale;
   that is a background-hygiene guarantee, not a substitute for checking
   right now:

   ```sh
   gitmoot job list --state running --json | jq '[.[] | select(.id | startswith("session-") | not)]'
   gitmoot job list --state queued --json | jq '[.[] | select(.id | startswith("session-") | not)]'
   ```

   Both empty (`[]`), then:
   `systemctl --user restart gitmoot-daemon gitmoot-dashboard-web`.
5. Config-only changes (e.g. `[memory]`) usually need no restart
   (re-read per tick / warm-reloaded on SIGHUP).
6. **Public releases need explicit OWNER sign-off** —
   `gh release create vX.Y.Z --latest` triggers `release.yml`. "Deploy locally"
   is not "cut a release".

## Live-probe

Prove a deploy: `gitmoot version` (commit/build), `gitmoot daemon status`,
`gitmoot doctor`. For engine features, a `shell`-runtime E2E on an isolated
`/tmp` home is the no-LLM smoke test.

`gitmoot version` only proves the binary **you** invoked, not what the services
run. Probe the deployed behavior instead: `/root/.local/bin/gitmoot-dashboard-web
version` for the dashboard service, and hit its API for a field the new build
changed (e.g. `curl -s http://172.17.0.1:8790/api/workflows`).

## Work strategy (lead-engineer)

Issue-first → isolated worktrees off `main` → implement → adversarial-review →
fix → verify on the integrated tree → reviewed PR → deploy affected-only →
live-probe before close. **Merge authority is whatever the org config records
for your role**: `merge_rule = "self"` means the coordinator merges its own
lane's PRs once one independent review is clean at the exact head and the PR is
mergeable with CI green; `merge_rule = "owner"` means the owner merges. The
field is **advisory** — `merge_gate.go` never reads it (`internal/config/org.go`
calls it "deliberately advisory in phase 1a"), so nothing mechanically stops a
merge you are not entitled to make; the gate enforces exact-head review, CI and
attribution, never role authority. The `[org.roles."<role>"]` block in the
gitmoot config is the authority of record and this file only describes it, so
settle any future disagreement with a config read — `gitmoot org chart` prints
`merge=<rule>` per role, `gitmoot org brief --role <role>` prints
`merge_rule: <rule>`, `gitmoot org status --json` carries the field, and
`[org.roles]` in `config.toml` is the source. Plain `gitmoot org status` has no
merge column, so it cannot answer this. **Public releases are unchanged and
still need explicit OWNER sign-off** — merge authority moved on 2026-08-31,
release authority did not.
Under ultracode, orchestrate via the Workflow tool with opus sub-agents
(protect the scarcer fable quota).

## Workload mode

**Current mode: DRAIN.**

A mode switch is a merged PR that changes the line above. It takes effect at
that PR's `mergedAt` time. At the next check-in, the coordinator must:

1. fetch `origin/main`, read the marker from `origin/main:AGENTS.md` rather than
   the seat's worktree, and steer every active seat to the merged mode;
2. comment on the merged mode-switch PR with this exact transition record:
   `[workload-mode-transition]`, `mode: <THROUGHPUT|DRAIN>`,
   `effective_commit: <40-character SHA>`, `observed_at: <RFC3339>`, zero or more
   `implementer: <seat> issue=<number> pr=<number|none> accepted_at=<RFC3339>`
   lines, zero or more `review: <job-id> pr=<number> created_at=<RFC3339>`
   lines, then
   `[/workload-mode-transition]`.

For DRAIN, the listed transition wave is derived from durable timestamps:
implementation assignments accepted before `mergedAt` that have not reached a
terminal handoff, plus review jobs created before `mergedAt` that were queued or
running then. `accepted_at` is the timestamp of the Herdr pane's first
`working` status event after the issue-backed assignment prompt; `created_at` is
the job-store timestamp. The merged PR always exists, so this record does not
depend on a pre-existing workflow. The mode changes how much work may start; it
never relaxes correctness, exact-head review, CI, or the merge authority
recorded in the org config.

Across both modes, use exactly one independent reviewer per corrected head.
Parallel review lanes mean different PRs, not multiple reviewers on one head.
Review panels and fanout require explicit, durable owner authorization for that
specific incident; an incident does not override this rule by itself.

### Throughput mode

- Start independent, issue-backed work when ownership and integration order are
  clear.
- Parallelize genuinely independent implementation lanes and reviews of
  different PRs under the repository's normal safety rules.
- Stop opening new lanes when work queues behind shared files, unresolved
  integration order, or repeated review findings.

### Drain mode

- Finish and merge the active queue; do not expand it. Do not start new issues,
  PRs, experiments, or speculative cleanup unless a security, data-loss, or
  live-service incident requires containment.
- Grandfathered transition-wave items may temporarily exceed the normal cap,
  but they count toward occupancy. No unlisted work may start while occupancy
  exceeds the cap. Each listed item loses its exemption at its first subsequent
  terminal handoff: review verdict, blocked or parked seat, merged PR, or
  explicit cancellation. Once occupancy reaches the cap, it must not rise above
  it again; never replace a completed transition-wave item with new work.
- After activation, cap the `gitmoot/*` scope at **two active implementers and
  one running reviewer**. An active implementer is a persistent seat currently
  changing code or a running engine implementation job.
- A DRAIN mode-switch PR is not merge-ready until the coordinator configures
  the shared daemon with `[daemon] workers = 1`; every active
  `[repos."gitmoot/*"].max_parallel` override must be absent, zero, or one.
  Apply the warm reload and verify both the effective global worker count and
  every effective per-repository limit. This is the atomic runtime gate shared
  by native PR fanout, heartbeats, and manual background reviews.
- Before that PR merges, every foreground or persistent-seat review already in
  progress must reach a terminal handoff; those reviews cannot be grandfathered.
  All DRAIN reviews then run as background engine jobs. Never bypass the shared
  gate with a foreground or persistent-seat reviewer.
- Before that PR merges, disable every `action=review` heartbeat and allow
  exactly one review-capable agent on each active `gitmoot/*` repository. For a
  PR with a branch lock, the native PR watcher is the sole producer; do not also
  dispatch a manual review. For a PR without a branch lock, native fanout cannot
  run, so only the coordinator may enqueue its single manual review.
- Prioritize merge-ready work and merge-gate integrity, then serial dependency
  chains, then resource-safety work. Rebase conflicted branches only after
  upstream merges settle. Keep drafts and backlog work parked.
- If another correction receives a new substantive P1, stop the patch loop and
  re-plan the defect class before writing more code.
- Run routine coordinator check-ins hourly. Owner messages, directives, and
  review verdicts remain immediate.
- Zero-model-token operational pipelines, including the hourly PR report, may
  continue.

## Escalation: ping your org parent, and ping again

When you need something from your **org parent** — a dispatch you cannot make, a
ruling, an unblock — **ping them.** A workflow note is durable but it is *not a
wake*: a coordinator sitting settled will not see one until something rouses it.

- **Need something from your parent → ping**, and leave the durable note too. The
  ping is the wake; the note is the record.
- **No reply within well under an hour → ping again.** Do not wait politely.
- **Only a delivery verdict of `submitted` proves delivery.** `written_to_pty` is
  delivery-*unknown* — do not blind-retry (that stacks duplicates); verify at the
  destination instead, by the recipient's status changing or by them acting.

Routing is unchanged: a seat asks its coordinator and the coordinator carries it
up. This is about being loud with your own parent, not about going around them.

**Why this is a rule and not a preference:** *reporting a blocker feels like
progress and is not.* A brief nobody dispatched is invisible to every channel
that shows work — including to the person who has to dispatch it. The
characteristic failure of an autonomous seat is ending a turn on the sentence
naming an action instead of the action; "blocked on X" is that sentence, and
pinging X is the action.

## PR & commit conventions

- **Commits**: Conventional Commits — `feat:`, `fix:`, `docs:`, `chore:`, `ci:`,
  `perf:`, optional scope (e.g. `feat(workflow): …`). Reference issues with
  `(#NNN)`.
- **Branches / PRs**: do **not** push directly to `main`. Branch, open a PR, let
  CI (`build / vet / test`) pass, get one clean independent review at the exact
  head, then whoever holds merge authority for that role in the org config
  **squash-merges** — the coordinator itself under `merge_rule = "self"`, the
  owner under `merge_rule = "owner"`. One PR per issue, with deploy notes in the
  body. Cutting a public release stays an OWNER decision either way.
- **Scope**: preserve existing behavior unless the change requires otherwise.
- For machine-local agent notes, use a gitignored `CLAUDE.local.md` rather than
  editing this shared file. Gitignored (local-only, not in the repo): `/GOALS/`,
  `/repos/` (vendored helper repos), `/dist/`, `/.gitmoot/evals/` — editing these
  never shows in `git status`.
