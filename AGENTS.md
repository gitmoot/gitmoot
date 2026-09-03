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
**file** (a `gitdir:` pointer), so the root-detection walk-up skips the
worktree's real root and keeps going up. What happens next depends on what the
walk finds, and it is stated once in the deploy recipe below rather than twice
here: see "Deploy recipe (this host)", step 1. The short version is that the
quiet outcome is the dangerous one, not the `exit status 128` failure. This is a
Go toolchain behavior with linked worktrees, not something gitmoot's code or
config can fix.

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
  `presence`, `memory`, `doctor`, `cockpit` (the herdr wake client + org
  provider), `plugin*`.
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

1. On `main` after merge, build with the pinned toolchain (above), from a clean
   detached worktree at the exact tip rather than the shared `/root/gitmoot`
   checkout, which usually carries uncommitted work. Stamp the version the way
   `release.yml` does, or `gitmoot version` reports `commit: unknown`.
   `-buildvcs=false` is required for the same reason as the test gate above: in
   a linked worktree `.git` is a **file**, which the pinned toolchain does not
   treat as a repository root, so it walks up to the parent directories. What
   that walk finds decides the outcome. Measured with a throwaway module on
   go1.26.4:

   - **no repository anywhere above it**: the build succeeds and simply omits
     the stamp. This case is tolerated, not an error;
   - **a `.git` DIRECTORY above it that git rejects as a repository**: the build
     **fails** with `error obtaining VCS status: exit status 128`. A `.git`
     **file** never produces this: it is skipped, and the walk continues past
     it. This is not hypothetical here: `/tmp/.git` exists on this host as a
     directory (`ls -A` returns nothing, and `git -C /tmp status` fails), so a
     deploy worktree anywhere under `/tmp` hits it, and it will not be the only
     such directory on any given host;
   - **a working unrelated repository above it**: the build **succeeds** and
     stamps that repository's metadata. A binary built from commit `1f76f143`
     embedded the ancestor's `vcs.revision 60f8282c` and `vcs.modified true`.

   The third case is the one to design against, because it is silent. The
   `-ldflags` below override `Commit`, so `gitmoot version` still prints the
   right value and the wrong stamp stays hidden in the embedded build info.
   Drop the ldflags and `buildinfo.Current` falls back to that VCS revision
   (`internal/buildinfo/buildinfo.go`), so an unstamped binary reports an
   ancestor's commit as its own and the daemon build-skew check compares a
   false identity rather than an unknown one.

   Treat the `.git`-file behavior as version-specific rather than permanent:
   Go's handling of it is being changed upstream, and `-buildvcs=false` is what
   makes all three outcomes moot.

   ```sh
   git worktree add --detach /root/gitmoot-deploy "$(git rev-parse HEAD)"
   cd /root/gitmoot-deploy
   PKG=github.com/gitmoot/gitmoot/internal/buildinfo
   CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags \
     "-s -w -X $PKG.Version=dev-$(git rev-parse --short HEAD) \
      -X $PKG.Commit=$(git rev-parse HEAD) -X $PKG.Date=$(date -Iseconds)" \
     -o /root/.local/bin/gitmoot.new ./cmd/gitmoot
   ```

   Remove the deploy worktree when the deploy is done:
   `git worktree remove /root/gitmoot-deploy`.

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
for your role**: `merge_rule = "self"` lets the coordinator merge its own
lane's PRs only under the standing delegation and safeguards below;
`merge_rule = "owner"` leaves the merge with the owner. THREE DATED EVENTS, kept
distinct because collapsing them into one date is what made this section
contradict itself: the role field was set to `"self"` on **2026-08-31**
(`config.toml` records jarvis setting it on the owner's direct instruction);
owner row **107983** granted the standing merge delegation on **2026-09-02**,
with the six safeguards below; and owner row **115499**, relayed to this lane as
directive **115517**, removed the approval GATE on **2026-09-04** - the
merge-authorized role merges without asking jarvis or the owner once ALL SIX
safeguards below hold. The full condition is not summarised here on purpose: an
earlier draft compressed it to "clean head plus one independent verdict", which
silently dropped the non-empty `tests_run` evidence bar and the immediate
pre-merge re-read - the two that caught a verdict which executed nothing and
every head that moved under a review. One review is sufficient; another model
family is preferred when available but never a gate. The `merge_rule`
field is **advisory** — `merge_gate.go` never reads it (`internal/config/org.go`
calls it "deliberately advisory in phase 1a"), so nothing mechanically stops a
merge you are not entitled to make; the gate enforces exact-head review, CI and
attribution, never role authority. Advisory cuts the other way too: **an armed
engine merge gate may merge on its own approval-plus-green conditions, with no
`merge_rule` holder acting at all**, so a holder MUST NOT rely on parking a local
commit to protect a merge window. #1731 merged itself at 2026-09-02T11:23:26Z as
squash `250b3fad` ("Gitmoot merge review-pr-1731-3f3a1026", committer GitHub);
`merge_gates` row 963 flipped to `state=merged` four seconds later and six
seconds *before* the approving verdict comment posted, and four fixes held back
to protect the holder's window were excluded by that merge. Land work or record
it durably; a window you do not control is not a queue. The
gitmoot config is the authority of record and this file only describes it, so
settle any future disagreement with a config read — `gitmoot org chart` prints
`merge=<rule>` per role, `gitmoot org brief --role <role>` prints
`merge_rule: <rule>`, `gitmoot org status --json` carries the field, and
`[org.roles]` in `config.toml` is the source. Plain `gitmoot org status` has no
merge column, so it cannot answer this. **Public releases are unchanged and
still need explicit OWNER sign-off** — the three events above moved MERGE
authority; release authority never moved.

For the `gitmoot` role, owner standing-delegation row **107983** makes every
self-merge conditional on all six checks below:

1. The PR is OPEN, MERGEABLE, and CLEAN. Every check has succeeded; none is
   pending or failing.
2. A review job has succeeded with `decision=approved` at the current PR head.
3. The verdict has non-empty `tests_run`; an evidence-free approval does not
   qualify.
4. The reviewer is neither the implementer nor the lead. One independent
   review is enough; same-family review is valid when reported as such.
5. Immediately before merging, re-read the head and every check. Abort if the
   head moved or any check is not successful.
6. In-session work with no implement job is an **attribution gap**. Name it in
   the merge note; do not report it as an independence failure or skip it
   silently.

The delegation does **not** authorize releases, `gh release create`, deploys,
service restarts, force-pushes to `main`, or merging work outside the PR's
issue. Those actions remain owner-gated.

**A green, mergeable PR is not necessarily a REVIEWED one — check two axes
independently: did the head move, and did the base move?**
`mergeable: MERGEABLE` / `mergeStateStatus: CLEAN` is a claim about *git* — that
the branch applies with no textual conflict. It says nothing about whether
anyone reviewed the tree that will land.

CI is a **stronger** instrument than a file-level argument, but it has a
staleness hazard. `.github/workflows/ci.yml` runs on `pull_request` with no
`ref:` override, so each `actions/checkout` resolves `github.ref` =
`refs/pull/N/merge` and GitHub **does** build the branch merged into the base —
measured on this file's own PR, whose run log reads
`git checkout --force refs/remotes/pull/1783/merge` /
`HEAD is now at 7057f594 Merge 4b245ac5… into c3785d6d…`. But GitHub recomputes
that merge ref when the base moves and **does not re-run CI**, and the
check-runs are reported against the *branch head*. So a green check is a claim
about `merge(head, base AS OF THAT RUN)` displayed against the head — which is
exactly what makes a stale green look current. (Whether a manual "Re-run all
jobs" re-resolves the merge ref against the new base or replays the original
merge SHA is **unmeasured** — do not rely on either.)

The two axes — the head axis has two shapes — and both axes can be true at once:

- **The head moved — new commits were pushed.** The verdict is void: it
  described a tree that no longer exists. Re-review at the new head, scoped to
  the delta since the approved head, not the whole PR again. Verify the branch
  carries only what you intended: `git diff origin/<baseRefName>..HEAD` and a
  tree read confirming no file you never touched reappears or vanishes.
  - **Sub-case of the head axis: the head moved BACK** — a force-push restored
    an earlier approved SHA. `ensureFinalReviewCaptured` binds approval to
    `head_sha`; it does not persist the reviewed base SHA. If the base stayed
    unchanged, the restored head and merge tree are identical and discarded
    intermediate commits are irrelevant. The current gate cannot prove that
    invariant, so treat the restored head as unreviewed as a conservative
    fail-closed policy, not because the base necessarily changed.
- **The base moved.** The verdict still names a commit that still exists and git
  still reports CLEAN, yet nobody reviewed `base` + branch, which is what lands.
  **The sound instrument is to build and test the merge result** — re-trigger CI
  at the current base and read that run. To re-trigger it, **push to the
  branch** (rebase onto the base, or merge the base in): that fires
  `pull_request: synchronize`, which produces a fresh merge ref AND a fresh run.
  Do not rely on the GitHub UI's "Re-run all jobs" — whether it re-resolves the
  merge ref against the new base or replays the original merge SHA is unmeasured
  here, so it may report green for the tree you were trying to leave behind.

  **A green merge build is necessary, not sufficient.** It proves the merged tree
  compiles and its tests pass; it cannot notice an assertion that stopped
  existing. Where the base delta and the PR touch the same region, a clean
  textual merge silently produces wrong code: two migrations appended to the same
  slice merge without conflict and can still renumber, and two edits to one test
  function merge with one assertion dropped — after which the merged tree builds
  and every remaining test passes, so CI is green and reports nothing. That is
  what file/package overlap is for: it is useful only to scope how much of the
  diff a human re-reads, and it is **not** a merge predicate. It has no direction and no
  transitivity, while "depends on" has both: a PR changing a signature in
  `internal/workflow` and a base commit adding a call site in `internal/daemon`
  share zero files and zero packages, pass any overlap test, and produce a tree
  that does not compile. Same shape when a PR deletes a symbol the base has just
  begun to reference — live risk during a reduction epic.

Measured on the first push of this very PR: it was stacked on a PR whose own
base predated a large deletion; that parent was **squash**-merged, which makes
the reviewed branch commits non-ancestors of the base, so the stack silently
kept the parent's stale base. The branch reported MERGEABLE, no conflict, green
CI — while its diff against the moved base re-added ~10,200 deleted lines, a
mass revert of a merge from half an hour earlier. Stacking on a PR that will be
squash-merged always requires a rebuild on the base once the parent lands.

Where a tree comparison is the right tool, use **trees, not diff output**, with a
positive control on every instrument. #1731 was waived on "only `AGENTS.md`
changed" by the role that benefited from the waiver; reversing that bought a
content-addressed proof instead — `git ls-tree -r` over all 1173 tracked files,
pairwise-equal subtree hashes, identical blob hashes for all 57 PR-changed
files, each instrument shown to report differences on a known-different pair.

And **`CLEAN` beside an empty check rollup is not green** — a head whose checks
have not registered yet reports zero check-runs and still reads CLEAN. An empty
check list right after a push means "not started", never "not required". Read
the count, not the label.

Under ultracode, orchestrate via the Workflow tool with opus sub-agents
(protect the scarcer fable quota).

## Workload mode

**Current mode: THROUGHPUT.**

A mode decision is an append-only
`[operating-mode repo=<owner/repo> mode=<THROUGHPUT|STEADY|DRAIN>]` workflow
note from the owner (or a coordinator acting on a recorded owner instruction).
A PR that changes the marker above is a second materialization path, but it
cannot invent a later decision merely by merging later. A merged marker PR takes
effect at its `mergedAt` only after the pre-merge reconciliation below, and the
later valid source wins. The note path's precedent is STEADY, decided by owner
instruction in note 107313 on 2026-09-02 and since superseded; the marker above
is whatever `origin/main:AGENTS.md` currently says, which is the only line a
seat should trust.

Before a mode-marker PR is marked ready, post
`[workload-mode-reconciliation repo=<owner/repo> pr=<number> head=<40-character-reviewed-head> mode=<mode> decision_note=<id|none>]`.
For a PR-sourced decision, use `decision_note=none`; the reconciliation row's
`created_at` is then `decided_at`. Otherwise `decision_note` names the newest
operating-mode note the exact head implements, and that note's `created_at` is
`decided_at`. A later operating-mode note supersedes the reconciliation and
requires a new exact-head row.

This is mechanically enforced, not a pre-merge memory check. Both the native
`PolicyMergeGate` and the separate `PipelineAutoMerger` read the paginated PR
file list. A changed `+`/`-` line containing the exact `**Current mode:` marker
requires a matching reconciliation row. An `AGENTS.md` entry whose patch is
missing or ambiguous fails closed. `PipelineAutoMerger.Merge` re-reads the PR
and both note streams at the final merge boundary; an older reconciled PR that
merges after a newer owner decision cannot override the newer decision.

Four properties of that enforcement are worth knowing before you write a row.
Enforcement is keyed on the repository OWNER, so every `gitmoot/*` repo is held
until a row exists, human-requested merges included. Repository names are
compared case-insensitively on every stream, so a note recorded as
`Gitmoot/gitmoot` still counts. A newest operating-mode note that cannot be
read — a malformed field list, or a `mode` that is not a workload mode —
SUPERSEDES like any other decision instead of reading as no decision: a row
filed before it no longer reconciles, and the hold names the note and says to
file a new exact-head row. It is a recency boundary, not a veto, so a fresh row
that agrees with the PR's own marker still merges, and correcting the note is
optional rather than the only exit. The gate only refuses outright when nothing
readable remains, meaning the note is unreadable AND the PR's marker patch is
missing or ambiguous. A reconciliation hold at the merge boundary is retryable:
the pipeline gate RELEASES its at-most-once merge claim, records the cause as a
`pipeline_auto_merge_held` job event, and re-attempts on a later scan, so the
row landing merges. If the gate stage sets a `timeout` the park summary carries
that cause; a stage with no timeout waits indefinitely, which is documented
pipeline policy and is only safe because the hold retries and is recorded.

Resolve the active decision from both durable sources: read the marker from
`origin/main:AGENTS.md`, never a seat worktree, and read the newest typed
operating-mode/reconciliation note. Decisions are ordered by `decided_at`
(the decision row's `created_at`), not by when their materialization later
activates. Name both sources in the handoff.

Record every activation with:
`[workload-mode-transition]`,
`mode: <THROUGHPUT|STEADY|DRAIN>`,
`decision_ref: workflow-note:<id>`,
`change_ref: none|pr:<owner/repo>#<number>@<reviewed-head>`,
`activation_ref: workflow-note:<id>|commit:<merge-sha>`,
`decided_at: <RFC3339>`,
`effective_at: <RFC3339>`,
`observed_at: <RFC3339>`, zero or more
`implementer: <seat> issue=<number> pr=<number|none> accepted_at=<RFC3339>`
lines, zero or more `review: <job-id> pr=<number> created_at=<RFC3339>` lines,
then `[/workload-mode-transition]`. A direct STEADY/THROUGHPUT note may be both
the decision and activation reference. A PR activates at its merge commit. A
note-sourced DRAIN uses a later activation-marker note, so no field claims a
commit that does not exist.

A DRAIN decision freezes new admissions at `decided_at`; the previously active
mode remains active while DRAIN prerequisites are installed. The transition
wave is the non-terminal implementation assignments accepted before
`decided_at` plus review jobs created before it that were queued or running
then. DRAIN becomes active only at `effective_at`: `mergedAt` for a reconciled
PR after its prerequisites, or the activation-marker note's `created_at` for a
note source. A later owner decision cancels an unactivated DRAIN.
`accepted_at` is the Herdr pane's first `working` event after the issue-backed
assignment prompt; review `created_at` is the job-store timestamp. Mode changes
never relax correctness, exact-head review, CI, or org merge authority.

Across ALL THREE modes, use exactly one independent reviewer per corrected head.
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

### Steady mode

Between throughput and drain: the **cap stays, the admission gate goes**. Set by
the owner on 2026-09-02, because drain's cost was never its concurrency limit but
its permission step — a seat sat on four ready, gated fixes through two
escalations, and an armed merge gate then merged the PR without them.

- At most **four implementation seats and two independent reviewers** at once.
  The owner gave those numbers illustratively ("4 implementations and 2 reviews
  for example"); note 107313 hardened them, and a rules file needs a definite
  cap rather than an example.
- **No admission gate.** A seat that finishes takes the next item itself: no
  escalation for permission, no waiting for an authorization row.
- Work comes off an **ordered list**, not a free choice — for a reduction epic,
  that epic's own dependency-sorted sub-issue order. **Nothing outside the
  scoped list is in scope**: with no admission gate, exclusivity is the only
  clause that bounds what a finishing seat may pick up (owner instruction, note
  107313: "Nothing outside #1762 is in scope").
- **Claim before editing.** A seat posts a one-line claim note naming issue,
  seat and branch BEFORE its first edit. No permission, no wait, no reply — it
  is a record other seats can read. Removing the admission gate removed the
  permission step, not the coordination signal: on 2026-09-02 two seats built
  incompatible answers to one issue (#1757, PRs #1786 and #1789 sharing 18 files
  and treating `NoopClient` two ways) because the second seat checked for a
  claim, correctly found none, and started.
- **One in, one out**: no second PR from a seat while its first is unmerged.
  This is what keeps the queue from growing, without anyone deciding.
- Escalate only for a **P2-or-worse finding**, a **scope boundary wider than the
  assigned item**, or **live-service impact**. Everything else is the seat's call.
- Steady mode names its own **exit condition and its destination**: when the
  scoped list is merged, `gitmoot/*` returns to **DRAIN**. The coordinator first
  completes every DRAIN activation precondition below and only then posts the
  drain marker itself rather than waiting to be told (owner instruction, note
  107313: "we need to stop and go back to DRAIN mode once those are merged").

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
- A DRAIN decision freezes admission immediately, but DRAIN is not active until
  the coordinator configures the shared daemon with `[daemon] workers = 1`;
  every active `[repos."gitmoot/*"].max_parallel` override must be absent, zero,
  or one. Apply the warm reload and verify the live worker setting plus every
  effective per-repository limit; plain `gitmoot daemon status` renders process
  arguments and is not proof of a warm-reloaded value. For a PR source, finish
  these prerequisites before marking the reconciled PR ready and merging it.
  For a note source, finish them before posting the separate activation marker.
- Before DRAIN activates, every foreground or persistent-seat review already in
  progress must reach a terminal handoff; those reviews cannot be grandfathered.
  All DRAIN reviews then run as background engine jobs. Never bypass the shared
  gate with a foreground or persistent-seat reviewer.
- Before DRAIN activates, disable every `action=review` heartbeat and allow
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
  **squash-merges** — the coordinator under `merge_rule = "self"` only after
  satisfying the row-107983 safeguards above, or the owner under
  `merge_rule = "owner"`. One PR per issue, with deploy notes in the body.
  Cutting a public release stays an OWNER decision either way.
- **Scope**: preserve existing behavior unless the change requires otherwise.
- For machine-local agent notes, use a gitignored `CLAUDE.local.md` rather than
  editing this shared file. Gitignored (local-only, not in the repo): `/GOALS/`,
  `/repos/` (vendored helper repos), `/dist/`, `/.gitmoot/evals/` — editing these
  never shows in `git status`.
