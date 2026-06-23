# Watching an orchestra: `gitmoot orchestrate --cockpit`

`gitmoot orchestrate --cockpit` renders one live [Herdr](https://github.com/jerryfane/herdr)
pane per delegation subagent so you can watch a fan-out as it runs — in the
terminal, and (through [herdres](https://github.com/jerryfane/herdr)) mirrored to
Telegram. It is **opt-in and fail-open**: with `--cockpit` off, or when Herdr is
absent or unreachable, orchestration is byte-identical to today. Gitmoot imports
no Herdr code; the cockpit is purely compositional, driving Herdr over its CLI
only.

## Turning it on

`--cockpit` (alias `--herdr`) is a flag on `gitmoot orchestrate`:

```sh
gitmoot orchestrate review-panel "Review PR #123 in this repo." --repo owner/repo --cockpit
```

When the flag is set, every child job in the delegation tree opens its own pane:
the coordinator splits a workspace for the run, each delegation subagent gets a
pane labeled `<agent> · d<depth> · <branch>`, and that pane streams the child's
progress while the job runs. Children inherit the cockpit setting from their
parent, so a single `--cockpit` on the root lights up the whole orchestra.

**Auto-detect:** running `gitmoot orchestrate` from inside a Herdr session
(`HERDR_ENV` set) turns the cockpit on automatically — no `--cockpit` needed. An
explicit flag works anywhere; `[orchestrate] cockpit_mode = off` is the host-level
veto; outside a Herdr session it stays off unless you pass the flag.

Pick where the run lands with `--cockpit-session`:

```sh
gitmoot orchestrate decompose-and-verify "Implement the export feature." \
  --repo owner/repo --cockpit --cockpit-session feature-export
```

### Terminal and Telegram

The same panes are visible two ways:

- **Terminal** — open the Herdr workspace to watch the panes split and update
  live as each subagent works.
- **Telegram** — with [herdres](https://github.com/jerryfane/herdr) bridging
  Herdr to Telegram, each pane surfaces as a forum topic / agent status you can
  read (and, in later phases, steer) from your phone. See the `herdres` skill for
  bridge setup.

## Close pane ≠ cancel job

A cockpit pane is a **view**, not the job. Closing a pane (in the terminal or
from Telegram) tears down the visible surface but does **not** cancel the
underlying job — the child keeps running in the daemon and its result is still
captured and synthesized. To actually stop work, cancel the job through the
record path:

```sh
gitmoot job list --repo owner/repo     # find the child job id
gitmoot job cancel <job-id>            # cancel via the record, pane teardown follows
```

This separation is deliberate: a herdr call must never fail or stall a job, so
panes are best-effort and disposable while the job lifecycle stays owned by the
engine.

## Configuration: the `[orchestrate]` section

Defaults live in `~/.gitmoot/config.toml` under `[orchestrate]` and can be
overridden per run by the flags above:

```toml
[orchestrate]
cockpit_mode = "auto"      # on | off | auto (auto = on when launched in a Herdr session)
cockpit_session = ""       # default Herdr session/workspace label ("" = per-run)
cockpit_max_panes = 4      # cap on simultaneous panes per run
cockpit_pane_key = "job"   # job (one pane per job) | seat (reuse a pane per role)
inline_artifact_bodies = false   # inline each child's artifact_body into the coordinator continuation
inline_artifact_max_bytes = 32768  # per-body cap (bytes) when inlining is on
max_delegation_token_budget = 0  # per-root delegation token budget (input+output); 0 = unlimited (off)
```

- `cockpit_mode = "off"` disables the cockpit even if `--cockpit` was passed;
  `"auto"` (the default) auto-enables it when `orchestrate` runs from inside a
  Herdr session and honors an explicit `--cockpit` anywhere. A pane is only opened
  when `herdr status` is ok; otherwise a requested run emits one
  `cockpit_unavailable` job event and proceeds without panes.
- `cockpit_pane_key = "job"` opens one pane per child job. `seat` mode reuses a
  single pane per logical role across phases (a later phase) so a long run does
  not accumulate panes.
- `inline_artifact_bodies` (default `false`) inlines each child's `artifact_body`
  into the coordinator continuation prompt as a fenced block, so the coordinator
  reads the briefs without re-fetching them from disk. Off by default, the
  continuation is byte-identical to before.
- `inline_artifact_max_bytes` (default `32768`, i.e. 32 KiB per body) caps how
  many bytes of each child's body are inlined; longer bodies are rune-safe
  truncated with a marker pointing at the full on-disk brief. A per-continuation
  aggregate cap also bounds the total inlined across all children.
- `max_delegation_token_budget` (default `0` = unlimited/off) bounds a delegation
  tree by **cost** in addition to depth/width/total-jobs/wall-clock. When set to a
  positive value, the whole tree under one root is capped at that many cumulative
  tokens (input + output, summed across every job in the tree). A coordinator that
  tries to fan out after the tree has already used at least the budget is refused
  with a `delegation_cost_exceeded` event and routed to the graceful finalize
  continuation (synthesize what completed, then stop). Token capture is
  **best-effort per runtime**: Claude reports usage via its `--output-format json`
  envelope, Kimi reports it when its stream emits a `usage` object, and Codex runs
  delivery without `--json` so it contributes `0` (the budget under-counts Codex
  rather than failing). Treat it as a coarse runaway-cost backstop, not a precise
  spend limit; the budget is in raw tokens (no `$` price table yet). Leaving it at
  `0` is byte-identical to before the knob existed.

## Constrained hosts

On a small box (few cores, limited terminal real estate, a shared daemon) the
pane count is what bites, not the jobs. Keep it bounded:

- **Lower `cockpit_max_panes`.** The default is `4`; set it to `2` (or `1`) on a
  constrained host. Beyond the cap, extra subagents run **status-only** — they
  still report state to Herdr but do not split a new pane, so the work fans out
  exactly as without the cockpit while the visible surface stays small.
- **Prefer `cockpit_pane_key = "seat"`** for long multi-phase runs so panes are
  reused per role instead of accumulating one-per-job.
- **The cockpit never changes the engine.** The delegation DAG, the result
  contract, the runtime-session locks, and the checkout keys are all unchanged
  whether the cockpit is on, off, or unavailable — so capping panes only changes
  what you *see*, never how the orchestra runs.

If Herdr is not installed or `herdr status` is not ok, `--cockpit` is a no-op
beyond a single `cockpit_unavailable` event on the root job; the run proceeds
unwrapped.

## Smoke test

An optional smoke script,
[`scripts/cockpit-smoke.sh`](../scripts/cockpit-smoke.sh), confirms the cockpit
is opt-in and fail-open. It runs against an isolated `--home`, exercises the
`--cockpit` wrap path when `herdr` is reachable, and **skips cleanly** (exit 0)
when `herdr` or `gitmoot` is unavailable, so it never fails a normal checkout
that has no Herdr installed. Set `GITMOOT_COCKPIT_SMOKE_AGENT=<coordinator>` for
a live end-to-end pane run.
