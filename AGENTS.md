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

It drives five runtimes (`codex`, `claude`, `kimi`, `omp`, `shell`; plus the
legacy `kimi-cli`). `agent start` supports codex/claude/kimi/omp; `shell` is a
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
- `internal/cli/` — the command surface (agent, template, skillopt, memory,
  dashboard) **and the `daemon` command wiring / worker loop**.
- `internal/daemon/` — the PR-watcher daemon package (poll/resume/revert logic).
- `internal/workflow/` — the job/delegation engine, mailbox, memory controller,
  and the `gitmoot_result` contract.
- `internal/runtime/` — the Codex/Claude/Kimi/omp/shell adapters.
- `internal/skillopt/` — the template auto-optimization loop.
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

`website/sidebars.ts` is manual — add new pages there. `website/static/llms.txt`
is hand-curated; `website/static/llms-full.txt` is generated by
`npm run build:llms` (part of `npm run build`).

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
  args (e.g. `skillopt candidate --home /tmp/h show <id>`, not after the id).
- `agent template add` needs a file with YAML frontmatter — use
  `agent template draft` to scaffold one. `template publish --create` makes a
  **private** repo; prompt bodies + metadata are stored/published **verbatim**,
  so point the remote at a private repo unless the prompts are meant to be public.
- An invalid `CLAUDE_CODE_OAUTH_TOKEN` 401s fresh claude sessions but `--resume`
  masks it; `gitmoot doctor` "auth ok" is set-not-valid (a false green).
- Killing a foreground `agent ask` / `skillopt train` strands a resource lock
  (runtime-session / generation); recover via `skillopt train recover` or clear
  the lock.
- codex ephemeral workers need `~/.codex/config.toml`
  `[sandbox_workspace_write] network_access=true` to push / open PRs.
- Agent permission policies gate Bash: `--policy workspace-write` auto-accepts
  **file edits only** and does **not** unblock Bash (`go`/`git`/`gh`). A full
  implement/push agent needs broader access; workspace-write alone is edits-only.
- The single static binary is sacred: no cgo, no runtime deps (modernc pure-Go
  SQLite).

## Agent jobs & the result contract

gitmoot runs agents through registered runtimes — **Codex, Claude Code, Kimi
Code, and omp** (`gitmoot agent start --runtime codex|claude|kimi|kimi-cli|omp`).
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
5. Config-only changes (e.g. `[memory]`/`[skillopt]`) usually need no restart
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
fix → verify on the integrated tree → owner-gated PR → deploy affected-only →
live-probe before close. The **OWNER holds merge authority**. Under ultracode,
orchestrate via the Workflow tool with opus sub-agents (protect the scarcer fable
quota).

## Workload mode

**Current mode: DRAIN.**

Update the line above whenever the owner switches modes. The mode changes how
much work may start; it never relaxes correctness, exact-head review, CI, or
owner merge authority.

### Throughput mode

- Start independent, issue-backed work when ownership and integration order are
  clear.
- Parallelize genuinely independent implementation and review lanes under the
  repository's normal safety and review rules.
- Stop opening new lanes when work queues behind shared files, unresolved
  integration order, or repeated review findings.

### Drain mode

- Finish and merge the active queue; do not expand it. Do not start new issues,
  PRs, experiments, speculative cleanup, or review panels unless a security,
  data-loss, or live-service incident requires it.
- Let work already in flight reach its next stable reviewed head. After that
  wave, cap concurrency at **two implementation seats and one independent
  reviewer**.
- Use one independent reviewer per corrected head. Do not fan reviews out.
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
  CI (`build / vet / test`) pass, then the owner **squash-merges**. One PR per
  issue, with deploy notes in the body.
- **Scope**: preserve existing behavior unless the change requires otherwise.
- For machine-local agent notes, use a gitignored `CLAUDE.local.md` rather than
  editing this shared file. Gitignored (local-only, not in the repo): `/GOALS/`,
  `/repos/` (vendored helper repos), `/dist/`, `/.gitmoot/evals/` — editing these
  never shows in `git status`.
