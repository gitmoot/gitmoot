# Remote workspaces

A remote workspace (#2678) is a provider sandbox (E2B by default) bound to one
seat and one issue or pull request. The seat keeps its pane, session and
messages on the host; its checkout, builds and tests run in the VM. The seat
is still the implementer: gitmoot provisions and tears down the VM and never
edits code. The VM gets no GitHub token and none of the host's environment.

Slice 1 ships `open`, `show`, `ls`, `exec`, `ws sh` and `close`. File sync
(`changes`, `pull`, `push --files`, `mirror`, `read`, `write`, `stat`) and
`push <id>` (commits out) come in slice 2.

## Setup

```toml
[remote_exec]
e2b_api_key_file = "/path/to/e2b-api-key"
e2b_template = "gitmoot-omp-go126-2c4g-v1"  # needs git, tar and python3
cost_per_hour_usd = 0.2                       # required: the cap is priced by it

[workspaces]
enabled = true          # default false
provider = "e2b"        # or a configured sandboxd provider
max_total_usd = 50      # every workspace ever: settled cost + open reservations
max_workspace_usd = 10  # one workspace's reservations over its life
idle_timeout = "4h"     # close after this long without an exec
```

The section is read on every command and every daemon pass, so edits apply
without a restart. An invalid section refuses `open`; the daemon then keeps
enforcing the default caps.

The seat is the acting org role: `GITMOOT_ORG_ROLE`, else the calling pane's
registered seat.

## Commands

| Command | Contract |
|---|---|
| `gitmoot workspace open --repo O/R (--issue N \| --pr N) [--json]` | Provisions the VM and checks out the branch. `--pr`: the PR's head branch at its head commit. `--issue`: a new branch `ws/<seat>/<N>` from the default branch. One non-closed workspace per (seat, repo, issue\|pr). |
| `gitmoot workspace show <id> [--json]` | The record, plus `env`, the VM's base environment. |
| `gitmoot workspace ls [--all] [--json]` | Open workspaces (`--all`: closed and failed too), with `cost_reserved_usd`, `cost_accrued_usd`, `closed_at`, `close_reason`. |
| `gitmoot workspace exec <id> [--cwd REL] [--env K=V]... [--timeout S] [--stdin] [--status-file HOSTPATH] -- argv...` | Runs argv in the VM. |
| `gitmoot ws sh [--cwd REL] [--status-file HOSTPATH] '<cmd>'` | `exec $GITMOOT_WORKSPACE --cwd REL -- bash -lc '<cmd>'`. Exactly one command string; more is `invalid_args`. |
| `gitmoot workspace close <id> [--reason TEXT] [--json]` | Copies the logs out, destroys the VM, settles the ledger. Default reason `manual`. |

`open` and `show --json` print exactly these keys (`issue` and `pr` are `null`
when unset; `env` is in `show` only):

```json
{"id":"ws-1a2b3c4d5e6f","seat":"intern","repo":"o/r","issue":12,"pr":null,
 "branch":"ws/intern/12","base":"<40-hex commit>","root":"/home/user/work",
 "provider":"e2b","state":"ready","created_at":"2026-11-19T12:00:00Z",
 "last_active_at":"2026-11-19T12:00:00Z",
 "env":{"PATH":"/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "HOME":"/home/user","LANG":"C.UTF-8","GITMOOT_WORKSPACE_ROOT":"/home/user/work"}}
```

- `root` is the absolute work tree inside the VM. `base` is the commit the
  branch was checked out at; the branch tracks a local `gitmoot-base` branch at
  the merge base (PR) or the same commit (issue), so `git log @{upstream}..`
  shows the workspace's own commits. The VM's repository has no remote.
- `state` is `opening`, `ready`, `closing`, `closed` or `failed`.

### Errors

Every failure prints one line `{"error":{"code":"<code>","message":"…"}}` on
stderr (and on stdout too with `--json`) and exits **125**. Codes:
`not_found`, `not_ready`, `cost_cap`, `provider_unavailable`,
`cwd_outside_root`, `already_open`, `invalid_args`, `disabled`, `internal`.

### exec and ws sh

- **Exit status:** the command's own (0-255), 128+n when it died of signal n,
  124 when `--timeout` killed it. 125 with the error line on stderr when
  gitmoot itself failed before or around the command.
- **`--status-file`** is written last, atomically:
  `{"exit_code":N,"signal":n|null,"timed_out":bool,"error":{"code","message"}|null}`.
  `error` is non-null exactly when gitmoot failed, which is how a command that
  exits 125 (or 124) on its own is told apart.
- **Streaming:** stdout and stderr stream live, kept apart.
- **stdin:** `/dev/null` unless `--stdin`.
- **cwd:** relative to `root`; `..` out of it, an absolute path, or a symlink
  that resolves outside it is `cwd_outside_root`.
- **Environment:** exactly the base environment above plus `--env`. Nothing
  from the host is sent.
- **Cancellation:** SIGINT/SIGTERM to the CLI forwards that signal to the
  command's whole process group (SIGKILL 3 s later) and exits with its status.
  If the CLI dies outright (SIGKILL, lost connection), the VM-side watchdog
  kills the group within 15 s; see below.
- `last_active_at` moves on every exec (and each minute of a long one).

### How the VM-side watchdog works

envd keeps a process running when the request that started it goes away, so
gitmoot runs every command under a small supervisor (`python3 -c`, embedded in
the binary as `internal/cli/workspace_supervisor.py`). envd starts the
supervisor with a stdin pipe; the CLI feeds it through envd `SendInput` with
framed messages: a heartbeat every 2 s, the command's stdin (`--stdin`), and a
kill request on SIGINT/SIGTERM. The command runs in its own session and process
group. When no frame arrives for 15 s, or the pipe closes, the supervisor kills
the whole group with SIGKILL. When the command ends, anything left in its group
is killed too, so nothing outlives the exec. The supervisor's exit status is
the command's, and it reports `{exit_code, signal, timed_out, error}` as a
trailer on stderr that the CLI strips.

## Cost

Each workspace reserves one hour up front at the provider's
`cost_per_hour_usd` (`open` refuses with `cost_cap` if that would pass either
cap, before anything is provisioned). The provider deadline is set to the end
of what is reserved (`paid_until`). Ten minutes before it, the daemon settles
the accrued cost and reserves up to one more hour, then moves the provider
deadline. When a reservation would pass `max_total_usd` or
`max_workspace_usd`, the workspace is closed with reason `cost_cap`. Lowering
`max_total_usd` closes the newest workspaces until the committed total fits.
Without a running daemon a sandbox ends at `paid_until`: spend never exceeds
what was reserved.

Committed spend = settled cost of closed workspaces + full reservations of open
ones. Both caps are enforced inside the SQL statement that reserves, so
concurrent opens cannot both pass.

On cloud E2B a sandbox also ends at the team's maximum sandbox length; an exec
then finds it gone and the workspace closes with reason `provider_gone`.

## Lifecycle

The daemon (every 30 s, once per process however many repos it watches):

- closes a workspace whose pull request merged (`pr_merged`) or closed
  (`pr_closed`); an issue workspace follows the PR later opened from its
  branch;
- closes a ready workspace idle for `idle_timeout` (`idle_timeout`), measured
  from `last_active_at`;
- settles and renews (above), closing on `cost_cap`, `expired` (past
  `paid_until`) or `provider_gone`;
- fails an `open` that never finished within 15 minutes (`open_abandoned`) and
  retries any close whose teardown the provider did not confirm.

It keeps nothing in memory: every pass re-reads the `workspaces` table and
re-attaches to each sandbox by its recorded id, so a restarted daemon carries
on where the previous one stopped.

Every close first copies the VM's exec logs and a work-tree summary (git
status, commits since `gitmoot-base`, diff stat) to `<home>/workspaces/<id>/`
(`logs/*.log`, `close-summary.txt`, `close.json`), then destroys the VM, then
settles the ledger.
