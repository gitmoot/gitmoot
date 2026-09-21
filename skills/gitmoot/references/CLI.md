# Gitmoot CLI Reference

Use these commands from an agent session only when the user asks for Gitmoot
setup, status, agent coordination, or PR-comment workflow help.

## Install And Update

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot version
gitmoot update --check
gitmoot update --restart-daemon
gitmoot doctor [--home <path>] [--repo <path>] [--json]
```

Verify GitHub access before PR workflows:

```sh
gh auth status
```

`gitmoot doctor` is the environment preflight: it validates `gh auth` (with an
actionable remediation hint) and live-probes the Claude credential selected by
`runtime-auth.env`, so a bad credential is caught before jobs stall. Run it
after install and before starting the daemon. It also reports delegation
worktree count and logical disk size, warning at 10 stale worktrees or 1 GB and
distinguishing aged-final reclaimable owners from pinned non-final owners. A
non-zero quarantined cleanup-obligation count is also a worktree warning;
inspect those rows with `gitmoot job cleanup list --state quarantined`. A running
job whose directly recorded runtime PID is confirmably dead is a
required `stuck jobs` failure; legacy jobs with no recorded PID and hosts where
process identity cannot be verified are neutral and produce no ghost-job
finding.
Doctor validates `[remote_exec]` through the same configuration loader used by
job dispatch. An absent section passes because remote execution is opt-in; an
invalid backend, identity pair, numeric identity, or local root is a required
failure carrying the loader's exact error. Use `--home /x` to preflight
`/x/.gitmoot/config.toml` without reading the default home.
For organization roles with an effective `recycle_after`, doctor also reads the
durable `org_role_presence.last_seen_at` signal. It warns when a specific role
has never been observed or has been inactive past that bound; an unreadable,
malformed, or future timestamp is reported as **unverified**, never healthy.
This deterministic absence check does not enqueue an agent job and is separate
from `gitmoot agent heartbeat`, whose configured schedules do enqueue jobs.
It also reports the SQLite auto-vacuum mode. New homes use bounded incremental
reclaim automatically. A legacy home remains a non-blocking warning until an
operator deliberately converts it during an idle maintenance window:

A runtime that is installed and contract-valid is not necessarily SERVING, and
`doctor` reports the difference (#1558). When the engine is currently holding a
job off a provider refusal - a `runtime_quota` or `runtime_auth` blocker whose
recorded retry time is still in the future - the `runtime serving` check warns
and names the runtime, the blocker class, the retry time and the job id. The
verdict is the engine's own forward-looking hold, not a second opinion about the
provider: a hold whose retry time has passed, a job that reached a terminal
state, a blocker row carrying no retry time, and a job whose runtime cannot be
attributed are all neutral, so a stale row can never be read as a current
refusal. It is a warning rather than a failure because the binary and its
contract are fine; what the operator needs is to not put that runtime on a
review panel today.

```sh
sqlite3 "$HOME/.gitmoot/gitmoot.db" \
  'PRAGMA auto_vacuum=INCREMENTAL; VACUUM;'
```

The one-time command fully rewrites the database; the daemon never runs a full
`VACUUM` automatically. `sqlite3` is an optional operator tool, not a Gitmoot
runtime dependency. For a non-default home, use the exact path printed by
`gitmoot doctor`.

One-shot onboarding: `gitmoot setup` registers the repo and an agent in one
command (`--repo owner/repo --agent <name> --runtime codex|claude|shell
--session <ref> [--role <role>] [--path .] [--start-daemon]`). `--repo`,
`--agent`, `--runtime`, and `--session` are all **required** — setup errors out
if any is missing; `--session` takes a runtime session reference, `last`, or a
shell command.
`--watch-issues` is **on by default** in setup, so the daemon comes up
tagging-ready for `@<agent>` issue mentions.

Home and config: local state lives in the Gitmoot home (default `~/.gitmoot`) —
the SQLite store, `logs/`, `workspaces/`, `evals/`, `artifact_blobs/`, and
`config.toml`. `gitmoot init` creates it, `gitmoot config path` prints the
config file location, and `gitmoot config show` prints the effective config.
Set `GITMOOT_HOME` (or pass the global `--home <path>` flag, accepted by nearly
every command) to relocate everything — useful for isolated test homes.

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
gitmoot plugin codex-launch --repo .
gitmoot plugin codex-launch --config-snippet
```

Claude scopes are supported with `--scope user|project|local`. Codex ignores
`--scope` because the current Codex plugin install command does not use it.
Use `plugin codex-launch` when Codex needs sandbox access to the resolved
Gitmoot home on Linux, macOS, or Windows. It prints a `codex-face --cd ...
--add-dir ... -s workspace-write` launch command, or a persistent config
snippet with `--config-snippet`.

## Runtime Metadata Registry

Gitmoot drives five built-in runtimes (`codex`, `claude`, `kimi`, `omp`, plus the
subscribe-only `shell`). Each carries
declarative metadata — advertised capabilities, a default model, an advisory list
of known-valid models, and a descriptor of where token usage is read from. Inspect
the resolved registry:

```sh
gitmoot runtime list
gitmoot runtime list --json
```

The values come from the compiled built-in defaults, overlaid with any
`[runtimes.<name>]` overrides in the config file. Override a built-in runtime's
recorded metadata **without recompiling** — for example to retarget its default
model/effort or record its known models:

```toml
[runtimes.codex]
default_model = "gpt-5.5-codex"
default_effort = "high"
models = ["gpt-5.5-codex", "gpt-5.4-codex"]
capabilities = ["review", "implement", "ask"]
usage_source = "codex exec --json turn.completed usage"
```

Two fields are **behavioral**. `default_model` is the model fallback when neither
the agent nor the job pins `--model`: agent/job `--model`, then `default_model`,
then the runtime CLI's own default. `default_effort` follows the same precedence
after job/agent `--effort`; for Codex, Gitmoot emits
`-c model_reasoning_effort=<value>`, and for omp it becomes `--thinking <level>`
whenever the resolved value is one of omp's accepted levels
(`off|minimal|low|medium|high|xhigh|max|auto`) — anything else is **dropped**, so
a typo falls back to omp's own default instead of silently downgrading the seat.
Claude and Kimi do not expose a reasoning
effort argument, so the resolved value is a no-op for those adapters. Every other
field is **inspection-only**, surfaced by `gitmoot runtime list`
but changing nothing at runtime: `models` is **advisory** (Gitmoot never rejects a
`--model` based on it), and `capabilities` gates nothing at dispatch. Adapter
behavior (auth, sandbox policy, session resume, stream parsing) always stays in Go.
With no `[runtimes.*]` section, and with both defaults unset, no model or effort is
forced.

A `[runtimes.<name>]` section can only tweak a **built-in** runtime's metadata; it
cannot add a new first-class runtime (that requires a code change). An unknown
runtime name is a config error surfaced by `gitmoot runtime list`.

Before an engine job launches, Gitmoot lazily checks the compiled runtime
contract against the installed CLI. Required argv flags are read from bounded
`<binary> --help` probes and environmental restrictions are declared beside the
argv that triggers them. Results are `supported`, `unsupported`, or `unknown`:
only a positively `unsupported` contract blocks on the contract itself; missing
binaries, timeouts, and unparseable help stay `unknown` and emit
`runtime_contract_unknown`. A missing binary is additionally refused BEFORE any
job row or worktree is created, but only when the dispatcher explicitly declares
that this dispatch builds a real adapter which will exec that declared CLI and
the execution backend runs on the local host. Injected or fake adapters, remote
or attached backends, and present binaries all stay dispatchable, and the
classification remains `unknown` for reporting either way.
Parsed help results are cached by resolved executable path, size, and mtime;
unknown results use a 60-second TTL before probing again, while an in-place CLI
update immediately invalidates either cached result. `gitmoot doctor --json`
reports the tri-state value in `state` alongside status, installed version,
answering instrument, and exact missing flag or precondition. Doctor probes its
own foreground `PATH`, which can differ from the daemon's EnvironmentFile
`PATH`; compare the reported `resolved_path` with the daemon's executable
resolution before treating the foreground verdict as the daemon's.

A read-only seat has a second, separate capability precondition, because the
contract preflight probes the dispatching host's `PATH` while a seat executes a
daemon-staged copy. When the daemon cannot stage a runtime it publishes that
name as an engine-owned exit-126 command; if that runtime is the seat's own,
seat setup ends the job `blocked` and records a `seat_runtime_unavailable`
event naming the agent, the runtime and the staging cause, before any adapter
is composed or model token spent. A sibling runtime published unavailable stays
a daemon log line and refuses nothing.

## Runtime Ambient Credential Hygiene

Claude runtime auth has one authoritative source:
`~/.gitmoot/runtime-auth.env` (mode `0600`). Manage it without putting secrets
on argv:

```sh
claude setup-token
gitmoot auth set claude             # reads the token from stdin
gitmoot auth status                 # local, masked, no paid runtime call
gitmoot auth probe claude           # paid fresh-session liveness check
gitmoot auth unset claude           # writes an explicit empty file
```

`auth set claude --var ANTHROPIC_API_KEY` and `--var
ANTHROPIC_AUTH_TOKEN` select another managed variable. `--from-env` atomically
copies currently set managed variables. The file is re-read for every Claude
adapter build, so rotation takes effect on the next foreground or daemon
delivery without a restart. If the file selects any managed variable, Gitmoot
injects all three names and explicitly blanks absent ones; this prevents an
ambient API key from outranking a file-selected OAuth token. An explicitly
empty file injects nothing and allows Claude's normal ambient/credential-store
fallback. The first adapter build imports legacy `daemon-runtime.env` when the
new file is absent, otherwise it seeds from ambient managed variables once;
existing authoritative files are never overwritten.

Shared injected pipeline keys use a separate operator-owned keychain file,
defaulting to `<base-home>/.config/gitmoot/keychain.env` (override with
`[credentials] keychain_path`). The CLI manages names and grants only; edit the
`0600` file at the path it prints to set or rotate values:

```sh
gitmoot key path [--json]
gitmoot key add <NAME> --mode injected|proxied [--json]
gitmoot key configure <NAME> --upstream <https-url> --auth bearer|header:<HeaderName> [--json]
gitmoot key list [--json]
gitmoot key show <NAME> [--json]
gitmoot key grant <NAME> (--pipeline <pipeline> | --agent <seat>) [--json]
gitmoot key revoke <NAME> (--pipeline <pipeline> | --agent <seat>) [--json]
gitmoot key rm <NAME> [--force] [--json]
```

There is deliberately no value flag, stdin value, or value-derived hash.
`proxied` keys must be configured with a pinned HTTPS origin/base path and
bearer or approved custom-header placement before they can be granted.
`rm --force` removes metadata and grants but leaves the file entry untouched.

Runtime-child environment curation is off by default. Enable it in
`config.toml`:

```toml
[credentials]
env_curation = true
env_passthrough = ["GOCACHE", "NPM_*"]
github = "deny"
model_gateway = false
model_gateway_allow_hosts = ["api.anthropic.com"]
# keychain_path = "/absolute/operator/path/keychain.env"
```

Set `model_gateway = true` to opt local Claude into the daemon-owned loopback
model gateway and to make broker material available to remote shell jobs. Each local Claude delivery receives a random job-scoped placeholder and
`ANTHROPIC_BASE_URL`; only the gateway holds the snapshotted real credential and
it forwards only to an exact allowlisted hostname. Gateway startup, credential,
and allowlist failures are fail-closed. The option is off by default and does
not enable Codex, Claude, Kimi, or omp on the remote backend. Remote shells get
only `GITMOOT_CREDENTIAL_GATEWAY_CURL_CONFIG`, whose owner-only file carries a
lease-bounded mTLS identity and capability; the provider key remains host-side.

With `env_curation = false`, runtime subprocesses inherit the full foreground or
daemon environment exactly as before. With it enabled, the base allowlist is:
`PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `TMP`, `TEMP`, `TZ`,
`LANG`, `LANGUAGE`, `TERM`, `COLORTERM`, `NO_COLOR`, all `LC_*` names,
`XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`,
`GOTOOLCHAIN`, `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`,
`GIT_COMMITTER_EMAIL`, and `GITMOOT_HOME`.

Codex additionally receives `CODEX_HOME`. Claude receives `CLAUDE_CONFIG_DIR`;
its three managed auth names are then resolved from `runtime-auth.env` at the
adapter seam described above. omp receives **routing plumbing only** —
`OMP_PROFILE`, `PI_PROFILE`, `PI_CODING_AGENT_DIR`, `PI_SMOL_MODEL`,
`PI_SLOW_MODEL`, `PI_PLAN_MODEL`, `OMP_AUTH_BROKER_URL`, and
`OMP_AUTH_BROKER_TOKEN` — and **no** raw provider key, so under curation a
provider key reaches omp only through omp's own profile auth storage or an
explicit `env_passthrough` entry. Two caveats: the broker URL/token pair is
**indivisible** (omp throws when the URL is set and no token is available, so
dropping only the token turns an inherited URL into a hard failure), and
`--profile` selects omp's auth/state store without isolating process
environment, so a passed-through key is visible to every omp profile this daemon
runs. Kimi and shell add nothing.
Gitmoot-owned relay, shell-stage, pipeline, and upstream-file variables are
appended after the base and remain available.

`env_passthrough` accepts exact names or a single trailing-`*` prefix glob.
Names containing `=` or NUL and non-trailing `*` forms are invalid. The base
deliberately excludes `SSH_AUTH_SOCK`, proxy variables, GitHub variables, and
toolchain caches such as `GOCACHE`; pass through required non-secret operational
variables explicitly.

With curation enabled, `github = "deny"` is the default. All ambient `GH_*` and
`GITHUB_*` values are omitted, including the four token variables. Gitmoot sets
`GH_PROMPT_DISABLED=1` and gives each delivery a fresh empty `0700`
`GH_CONFIG_DIR`, removed when the runtime exits on success, failure, timeout, or
cancellation. `github = "inherit"` is the explicit opt-out: ambient `GH_*` and
`GITHUB_*` variables pass through and Gitmoot adds neither GitHub variable.

Two limits apply: env-var routing is cooperative, not a hard egress boundary —
a malicious agent can unset it; this buys credential custody/policy/attribution,
not enforcement. The strong "agents never hold real credentials" claim also
requires Landlock read-rules for `runtime-auth.env` (same-UID read is currently
possible) — that is P3. Codex/Kimi custody and hard egress enforcement also
remain P3.

## Execution Backend

`[remote_exec] backend = "local"` is the default. `remote` provisions E2B for
engine-driven shell and OMP review jobs. The retained implement execution arm is
not dispatchable; ask and other job types refuse before provider allocation.
Engine-driven daemon jobs provision one job-scoped detached worktree, run the
runtime there with streaming preserved, and then destroy the instance. A review
returns findings in its result envelope and imports no change set. The instance
persists across Mailbox repair deliveries; cancellation destroys it, and daemon
startup reaps instances whose recorded owner process is gone.

Remote review admission runs after exact-head checkout binding and before cost
reservation or provider calls. It re-reads the PR head; deduplicates
repo/PR/head/purpose through the durable review claim; checks runtime/backend
support; and allows only the first cloud attempt unless an operator retry or a
provider create conflict that proves no allocation authorized the new lifecycle
generation. Cancellation releases the review claim. Refusals emit durable
`remote_review_admission_avoided_<reason>` events for `stale`, `duplicate`,
`unsupported`, `red_ci`, and `retry`.

Set `[review] remote_require_ci_green = true` to require at least one successful
current-head check and no pending or failed checks before remote provisioning.
The default is `false`. A `[repos."owner/repo".review]` value overrides the
global policy for that repository.

Local worktrees use Git's absolute gitdir pointer successfully because they share
the host filesystem, so bundle/base-ref hydration is reserved for a future remote
provider. Foreground dispatch remains on the host path. Unknown backend names and
explicit blank selectors fail loudly; a job payload's `exec_backend` overrides
the config for that job.

Remote broker access additionally requires paired
`credential_gateway_listen` and `credential_gateway_url` values. The first is
the daemon bind address and the second is the HTTPS origin reachable from the
sandbox. The listener is mTLS-only; credentials are revoked before teardown.

Set numeric `[remote_exec] local_uid` and `local_gid` together to run local
backend agent commands as that non-root identity. Gitmoot never invents an
account, and a failed credential application fails the command without a root
fallback. The workspace is handed to the configured identity after sync;
collection/import remain daemon-side and imported files therefore retain the
daemon user's ownership. If the default backend root is below a root-only
parent (for example a root daemon using `/root/.gitmoot`), set absolute
`local_root` to a dedicated operator-managed path whose parents the configured
identity can traverse; filesystem roots are rejected. Gitmoot keeps the backend
and instance roots daemon-owned, assigns them to `local_gid`, and uses mode
`0710` so the configured command group can traverse without setting execute for
the Unix `other` class. This is a group boundary: use a dedicated `local_gid`
whose only member is the configured account when unrelated local users must be
excluded. Gitmoot validates paired, non-zero numeric IDs but cannot portably
verify host group exclusivity. Omitting uid/gid preserves the daemon identity.

The runtime executable must be traversable by the configured uid too. If a
root daemon resolves `claude` or `codex` below `/root`, install the runtime at
an accessible location and put it first in the daemon's `PATH`; do not loosen
`/root` permissions. An inaccessible executable fails startup with no root
fallback.

For daemon jobs that own an execution-backend lifecycle, runtime contract
preflight evaluates UID-dependent requirements against configured `local_uid`.
This lets a root daemon dispatch Claude with `danger-full-access` to a non-root
local backend without weakening Claude's root refusal. Host-only paths such as
`gitmoot job run` do not provision that lifecycle and still evaluate the host
process identity. When `local_uid` is absent, daemon jobs do the same.

To prove a parallel local wave, use an isolated `--home`, one distinct
`fresh:<suffix>` session per Claude leg, and a daemon started with `--parallel
N`. Dispatch all legs together with `--background` and
`--skip-native-review-fanout`. Each parsed `gitmoot_result` should report UID,
GID, workspace, start/end timestamps, and visible markers. The gate passes only
when peak overlap is N, workspace paths are distinct, each leg sees only its own
marker, and the isolated ledger contains zero remote execution attempts.

## Transcript Retention

Runtime transcript retention is default-on. Every engine delivery appends its
stdout and stderr to a private per-job log; set `enabled = false` only for an
explicit opt-out. Missing or invalid configuration uses the safe defaults:

```toml
[transcripts]
enabled = true
retain = "168h"
max_total_bytes = 2147483648
```

Enabled capture appends every engine-delivered job attempt (foreground, daemon,
temporary session, ephemeral, and delegated jobs) to a private canonical log
under `<home>/logs/jobs/`. Externally driven session jobs have no runtime
subprocess and therefore no log. A home-scoped sweep removes settled logs after
`retain`, then evicts the oldest settled logs when the total exceeds
`max_total_bytes`; queued/running jobs and recently finalized jobs are protected.
Seat logs remain transient. Expect roughly 440 MB/week at this host's observed
rate, though workload output varies. `retain` has a hard 24-hour floor; shorter
or malformed retention falls back to the 168-hour default.

Raw retained logs are mode `0600` and **unredacted on disk**. Treat the Gitmoot
home as sensitive. JSONL exports redact known credential patterns best-effort,
but that redaction is not a vault and cannot guarantee removal of every secret.

## Runtime Launch Sandbox

```sh
gitmoot sandbox probe
```

`sandbox probe` prints whether this Linux host can enforce Gitmoot's strict
Landlock launch sandbox and includes the detected ABI. The probe runs the real
hidden re-exec shim and verifies both an allowed write and a denied outside write;
unsupported kernels return non-zero. Claude/Kimi `produce` pipeline stages require
this probe to pass and otherwise retain the explicit Codex-only refusal. Codex
produce remains on its own native sandbox. Landlock confines filesystem writes but
does not govern network access; network policy remains the runtime CLI's. Wrapped
Claude may write its runtime-owned `$HOME/.claude` state and
`$XDG_CACHE_HOME/claude-cli-nodejs` cache; wrapped Kimi may write its runtime-owned
`$HOME/.kimi-code` state. Apart from runtime state/cache and standard device nodes,
only declared data paths, the disposable workdir, and temp roots are writable.
`omp` never reaches this wrapper: it is applied by runtime **name**, to Claude
and Kimi only. omp also does not advertise the `produce` capability, because its
Bun binary's Landlock interaction is unprobed and advertising an unproven
capability would turn an unknown into a silent stage failure.

## Repo And Daemon Status

```sh
gitmoot status --repo owner/repo
gitmoot events --repo owner/repo
gitmoot repo add owner/repo --path <path> [--poll <duration>]
gitmoot repo list
gitmoot repo set-interval owner/repo (<duration>|default)
gitmoot repo set-interval --all (<duration>|default)
gitmoot repo remove owner/repo
gitmoot repo doctor owner/repo
gitmoot repo collisions owner/repo [--limit N] [--json]
gitmoot daemon start --poll 30s --workers 1
gitmoot daemon start --session <root-job-id>
gitmoot daemon start
gitmoot daemon status
gitmoot daemon logs
gitmoot daemon restart
gitmoot daemon stop
```

For structured local state, use `gitmoot dashboard --json` or
`gitmoot task list --repo owner/repo --json`. `gitmoot status --json` and
`gitmoot task show` are not valid commands.

`gitmoot repo collisions owner/repo` inspects at most the newest `--limit` open
pull requests (default 25, maximum 100), compares each selected pair's current
changed-filename sets, and prints one warning per non-empty intersection with
both PR numbers and the sorted shared paths. It exits 1 when collisions exist, 0
when every inspected pair is disjoint, and supports structured output with
`--json`; a clean result is the iterable empty array `[]`. Rename history is not
available, so a concurrent edit of a renamed path's old name may be missed.

`gitmoot daemon status` always prints the configured daemon log path. For a
running daemon, it also compares that file's last write with the daemon's
recorded start time. A missing file or an older last write produces a warning
with both times and suggests
`journalctl --user -u gitmoot-daemon -f` if the daemon runs under systemd.
`gitmoot doctor` reports the same confirmed condition as a non-fatal
`daemon log` check. Fresh, stopped, and indeterminate cases add no output.

### Build skew (upgraded but not restarted)

The daemon is a long-lived process: replacing the binary does **not** change the
code it is executing. It keeps running the old build until you restart it.

The daemon records the build it started from in `<home>/.gitmoot/daemon.json`
(`version` + `commit`). `gitmoot daemon status` prints that build and compares it
against the build of the binary **now sitting at the daemon's own path** — the
one a restart would load:

```
build: dev-cd43a49 (cd43a495)
WARNING: daemon running dev-cd43a49 (cd43a495); /root/.local/bin/gitmoot is dev-56ba1c7 (56ba1c74) — restart the daemon to pick it up
```

`gitmoot doctor` reports the same comparison as a non-fatal `build` check. Note
it compares the **daemon** against the **daemon's binary** — not against whatever
binary you happen to be invoking, which may not be the daemon's at all.

Unknown is never reported as skew, and never reported as agreement either. The
comparison is skipped when the daemon is not running, when it was started by an
older gitmoot (no recorded build), or when either side is an **unidentifiable**
build. A build is identifiable if it was stamped (any release, and the documented
deploy recipe) or if Go's VCS stamping supplied a commit — which a plain
`go build` in a git tree provides. Two unstamped builds with no commit are both
just `dev`: indistinguishable, so comparing them would prove nothing.

The web dashboard's `/api/health` reports the daemon's **recorded** build (what
the process is actually running — not the version of whatever binary now sits at
its path) plus, separately, the serving dashboard process's own build. Its
`daemon.versionSource` is `recorded` when `daemon.version` came from daemon
startup metadata, or `unknown` when an older daemon recorded no build; in the
latter case `daemon.version` is empty and must never be treated as either skew or
agreement. This keeps a stale dashboard or daemon visible rather than silently
wrong. The update badge stays relative to the binary on disk, since that is what
an update replaces.

The `gitmoot repo` commands manage the **watched-repo registry**: one daemon
per Gitmoot home supervises every **enabled** registered repo. An omitted
`repo add --poll` stores the `inherit` sentinel, so the repo follows the daemon's
resolved `--poll` / `[daemon].poll` cadence; an explicit `--poll` stores a
per-repo override. `repo list` renders the sentinel as `inherit`.
`repo set-interval owner/repo <duration>` changes an override, `default` restores
inheritance, and `--all` applies either value to every registered repo.
A changes-requested review reports its verdict to the requester and never
creates an implement job: Gitmoot does not dispatch implementation (#2203).
The seat named by `--lead` (or the requester's own session) does the fix work
and records it with `gitmoot job record --type implement`.
`repo doctor owner/repo` checks a single repo's checkout/config health. If the
registered checkout is missing or is no longer a Git worktree, Gitmoot verifies
the recorded primary checkout, repairs the registration, and reports the
self-heal. Implicit registration from inside a linked task worktree pins the
repo to its primary checkout; an existing valid linked checkout remains usable.

Use `daemon start` for the background daemon. Use `daemon run` only when the
user explicitly wants a foreground process. Keep the default `--workers 1`
unless the Gitmoot home has multiple independent runtime sessions or managed
agent types with `max_background` greater than one.

`daemon start --repo owner/repo` **scopes** the daemon to a single repo: it
polls only that repo's PRs and claims only that repo's queued jobs. Omit
`--repo` to supervise every enabled registered repo from one daemon (#581). Do
not start one daemon per repo on the same home expecting parallel isolation: a
second daemon on the same home is **refused** (`daemon already running with pid
…`; a stale pidfile from a dead owner is liveness-checked, so restarts work
cleanly). To cap one repo's parallelism on a shared (no-`--repo`) daemon, use
the per-repo config keys below instead.

Both `daemon run` and `daemon start` accept `--session <root-job-id>` (alias
`--root`) to pin the worker to one orchestration run. With `--session` set, the
worker runs only jobs whose `root_job_id` matches that value plus the root
coordinator job itself, and ignores every other queued job. Leaving it empty
keeps the default behavior of matching all jobs.

Both `daemon run` and `daemon start` also accept three opt-in flags (all off by
default, so leaving them unset is byte-identical to before): `--watch-issues`
watches open **issues** for `@<agent> ask …` mentions and routes them to jobs,
mirroring the PR-comment watcher; `--scheduler pool` selects the continuous
worker-pool scheduler that re-queries the queue as workers free and reactively
isolates a contended same-repo read job into an ephemeral worktree (fixing a
same-repo dependent-job deadlock), versus the default `--scheduler barrier`.
(Independently of the scheduler, background **read-only ask** jobs — `agent ask
--background` and heartbeat asks — are each given their
own detached committed-tip worktree **at dispatch** (#739), so they parallelize
across same-repo seats under either scheduler with ≥2 workers.)
To run a repo's queued jobs N-wide, use `--parallel N` (sugar for `--workers N
--scheduler pool`; it cannot be combined with `--workers` or `--scheduler`).
Raising `--workers` above 1 without an explicit `--scheduler` now **auto-selects
`pool`** (multiple workers under `barrier` serialize same-repo jobs anyway); an
explicit `--scheduler barrier` is still honored. `gitmoot daemon status` reports
the live scheduler mode and worker count (e.g. `scheduler: pool, workers: 5`), and
the daemon logs a preflight warning — with the exact relaunch command — when ≥2
parallelizable jobs are queued under a serializing config. Same-repo parallelism
is bounded by **distinct runtime sessions** as well as distinct checkouts.

One repo's concurrency can also be capped **from config, without any relaunch**
(#576): a `[repos."owner/repo"]` section with `max_parallel = N` caps that
repo's in-flight jobs (`0` or unset = use the global worker count), and an
optional `scheduler = "pool"|"barrier"` overrides that repo's scheduler. The
keys are re-read every tick, so edits apply live.

`org chart` and `org status` render the provider's last completed **turn**
alongside `seen=` (#1702). `org status --json` carries it as `last_turn`.

The turn measures PROGRESS; `seen=` measures RECENCY, and they answer different
questions - a seat can be inside one long turn with a stale note age and be
perfectly healthy. Measured on one host, two seats read 27h and 45h by note age
while each had completed a turn minutes earlier.

`last_turn` is ABSENT rather than zero when the provider reported no turn
activity, and the text surfaces render `-` for that case. A reported turn of `0`
is a real value and renders as `0`; a rendered `0` for silence would invent a
stalled seat. An unavailable role keeps its reported turn, because an
unavailability incident says whether a role may be dispatched to, not whether
the provider reported anything.
The same `[repos."owner/repo"]` section declares a repo's **staged review**
verdict agent (#1821):

```toml
[repos."owner/repo"]
staged_review_verdict_agent = "gm-review-opus"
```

A staged review splits one review into a cheap preflight stage that answers
*can this review be performed here* and a **verdict** stage whose result is the
review. `staged_review_verdict_agent` names the agent that would run the
verdict stage.

Local `agent review` and review-resolved `agent run` dispatch honor this key:
they run the requested reviewer as a **preflight** parent. The parent checks
whether the review can run and delegates exactly once to the configured verdict
agent. The preflight is marked so its result cannot be consumed as the final
review verdict. Daemon fanout, heartbeat, pipeline, comment-command, and
externally recorded session reviews do not consult this key; they keep their
ordinary review paths.

It is **off by default** and there is deliberately **no default and no fallback
list**. A repo that has not declared one uses the ordinary, unstaged review
path, and having a `[repos.*]` section for some
other key is **not** a declaration. That is the campaign's non-fallback rule
applied to the choice of reviewer itself - a strong reviewer that cannot be
identified must never degrade to *the cheap stage approved it* - so the
reviewer is an operator's decision per repo rather than a constant in the
Gitmoot source.

Job kill deadlines are independent from stale-running detection. Configure the
daemon defaults with:

```toml
[daemon]
job_timeout_default = "4h"
job_timeout_max = "8h"
quiet_kill_after = "45m"
```

The effective deadline resolves in this order: a positive `job_timeout` in the
job payload, the review class floor below, the registered agent type's
`[agents.<type>].job_timeout`, then `job_timeout_default`. `job_timeout_max` is
a hard ceiling (default `8h`) applied last and clamping every source including
an explicit payload value; a larger request is clamped and the job receives a
`job_timeout_clamped` event recording the requested and applied durations. This
prevents a delegation tree from granting itself an unbounded run. These keys are
read when a job dispatches, so edits affect newly started jobs without changing
an in-flight deadline.

Reviews carry their own floor, because a review's duration is a property of the
prompt class rather than of whichever agent happens to answer it:

```toml
[review_router]
job_timeout = "3h"   # default 3h; must be positive
```

A review job gets at least this long even when the answering agent's own
`job_timeout` is shorter, so rotating between reviewers cannot shorten a review.
A longer agent setting still wins, and an explicit payload `job_timeout` still
wins over both — a caller that deliberately caps one review keeps that cap. A
job counts as a review when its stored job type is `review`, independent of
whether the payload carries a review purpose or model pool.

Two advisories report deadline inputs that would otherwise be ignored in
silence: an unparseable or non-positive payload `job_timeout` lands a
`job_timeout_payload_invalid` event on the job and falls through to the next
source, and a `[review_router]` section that cannot be read lands a
`review_class_deadline_default` event naming the parse error and the deadline
used instead. A non-positive `job_timeout` in the config file is refused at load
rather than accepted.

`quiet_kill_after` controls the transcript-silence leg of the liveness
conjunction (default `45m`, hard floor `5m`). The 30-minute stale-running
threshold remains only an age predicate; neither value is a job kill deadline.

Reconfigure the running daemon without a restart: `kill -HUP <daemon-pid>`
re-reads the `[daemon]` config section (`poll`, `workers`, `scheduler`,
parallelism, `idle_grace_ticks`, `idle_max_multiplier`, `quiet_kill_after`) live
(#577) — no teardown, no dropped jobs, no environment
re-inheritance. Values pinned by explicit launch flags win over the re-read
config. Prefer SIGHUP over a restart when only tuning throughput.

The default-on `[disk_guard]` section pauses normal queued-job dispatch when the
filesystem holding the Gitmoot home and worktrees has less than either
`min_free_bytes` (default `2147483648`, 2 GiB) or `min_free_percent` (default
`5`) available. Both checks apply when both are non-zero, so the more
conservative floor wins:

```toml
[disk_guard]
enabled = true
min_free_bytes = 2147483648
min_free_percent = 5
```

The guard fails closed: an unreadable config, missing path, `statfs` error, or
invalid filesystem measurement pauses dispatch instead of assuming the disk is
healthy. Paused jobs remain `queued` and retry automatically on the next healthy
daemon pass. The daemon writes a greppable
`DISK GUARD REFUSED JOB DISPATCH` log and a
`dispatch_refused_disk_guard` job event containing the measured free space,
configured floors, and measured path. `gitmoot daemon status` always reports the
current measurement and prints `UNHEALTHY, dispatch paused` when the measurement
cannot be established or a floor is breached. The guard applies only to normal
agent-job dispatch; daemon maintenance/reconciliation remains runnable so an
internal reclaim pass can free space.

A queued job that the dispatcher examined and could not claim records a
`dispatch_held_back` job event naming the reason: an admission-budget refusal
(including the never-fit case, which names the cap), a checkout key held by an
in-flight job, a runtime session lock with its holder and lease expiry, or - when
the pass cannot attribute the wait - the bare fact that the job was not selected
this pass. Before this the reason reached daemon stdout only, so
`gitmoot job events <job-id>` could not distinguish a job waiting two hours from
one about to start. The event is throttled to one row per job per distinct reason
every five minutes, matching the log line it accompanies.

Claude runtime auth is independent of daemon restarts. Use `gitmoot auth set
claude` to rotate the owner-only `runtime-auth.env`; the next delivery observes
it. Use `gitmoot auth unset claude` to write the explicit-empty state. Do not
delete the file to unset auth, because a missing file is eligible for one-time
legacy/environment bootstrap.

An opt-in, off-by-default `[admission]` config section adds a host-global
concurrency budget the daemon applies **before** starting each agent session,
on top of `--workers`/pool and the per-repo locks (#365):
`max_concurrent_sessions` caps total in-flight sessions across all repos, and
`max_memory_gb` caps the summed per-runtime RAM estimate (tunable priors:
`codex_memory_gb`, `claude_memory_gb`, `kimi_memory_gb`, `default_memory_gb`).
With both caps `0` (the default) it is disabled. A job that does not fit is
left **queued** and retried next tick — never failed — so on a small host
"jobs stay queued" can mean the admission budget is holding them. The budget is
enforced per daemon process.

A `[github]` config section installs a **GitHub call budget + secondary-rate-limit
backoff** over the `gh`/API calls gitmoot issues **from the daemon process** —
polling, comments, merges, status (#683). It is enforced **per daemon process**
(like the admission budget), so it does not reach separate foreground processes (a
foreground `gitmoot orchestrate`/`pool`/`review`/`pr comment`) or the `gh` calls a
codex/claude runtime subprocess makes on its own. GitHub's **secondary**
(abuse-detection) rate limit fires on burstiness/concurrency — not total volume —
so a busy daemon plus concurrent in-process calls can trip it (HTTP 403 "secondary
rate limit") and freeze all GitHub ops even while the primary quota is fine. The limiter smooths bursts and, on a
secondary hit, **pauses all GitHub calls process-wide** (respecting `Retry-After`,
else exponential backoff) instead of retry-storming the abuse detector. Knobs:
`max_concurrent` caps in-flight `gh` calls (0 = unlimited, the default),
`min_interval` spaces successive call starts (0 = off; accepts a Go duration or a
bare integer of seconds), `secondary_backoff` toggles the reactive pause (default
`true`), and `backoff_base`/`backoff_max` bound the exponential fallback
(defaults `60s`/`5m`). `conditional_requests` defaults to `true` and adds ETag
validators to the four per-tick polling reads; a `304 Not Modified` replays the
cached raw response at zero GitHub quota cost. `calls_per_hour_warn` defaults to
`0` (off) and logs when this daemon process crosses the configured sliding-hour
count. The count is approximate and daemon-local: foreground commands and
agent-owned `gh` processes are outside it. **Safe defaults:** the proactive caps are off (single-call
latency and steady-state throughput unchanged) and only the invisible reactive
backoff is on. Set `max_concurrent` (e.g. `6`) and/or a small `min_interval`
(e.g. `250ms`) to also smooth bursts proactively on a busy host. Calls are never
dropped — they queue/delay. `gitmoot daemon status` shows the configured budget
(`github limiter: max_concurrent=… min_interval=… secondary_backoff=… conditional_requests=… calls_per_hour_warn=…`).

The **primary** limit is a different failure and now reports itself as one. When
a call fails on primary exhaustion, Gitmoot re-issues that same request once with
headers and reports the window its own response named: the resource, how empty it
is, the reset timestamp and the remaining wait, wrapped around the original `gh`
text. Waiting is the only remedy, because a primary window carries no
`Retry-After` and cannot be shortened.

It probes the **failed request**, never `GET /rate_limit`, and that is measured
rather than stylistic. On one credential and one host, seconds apart,
`/rate_limit` reported core `4999/5000 used=1` while a real core request returned
403 with `remaining=0 used=5000`, and their resets were thirteen minutes apart;
minutes later it reported a completely fresh `5000/5000 used=0` while real calls
were still refused, and the 403 header's reset was accurate throughout. **Do not
use `/rate_limit` as a precondition**: it reads full while every real call fails.
A secondary hit is deliberately not probed, since it answers to `Retry-After` and
already pauses process-wide.

After `idle_grace_ticks` consecutive successful polls in which every conditional
read is a 304, a repo's GitHub poll cadence decays to 2x and then up to
`idle_max_multiplier` (default `4`; `1` disables decay). Any response-body miss,
poll error, queued repo job, or in-flight repo job resets/promotes it immediately.
Repos with open PRs stay at base cadence because their per-PR comment reads are
deliberately non-conditional. Idle decay gates only GitHub calls; heartbeat,
pipeline, and other supervisor maintenance still wake at the resolved base
interval.

`gitmoot dashboard` prints a styled one-shot snapshot of local state — daemon
health, repos, agents and runtime sessions, jobs by state, worktrees, and branch
locks. It prints the same snapshot everywhere: terminal, pipe, or CI.

```sh
gitmoot dashboard                  # styled one-shot snapshot
gitmoot dashboard --json
gitmoot dashboard --all
gitmoot dashboard --watch          # redraw until Ctrl-C (terminal only)
gitmoot dashboard --watch --interval 2s
gitmoot dashboard --web [--addr 127.0.0.1:8080]
```

`gitmoot dashboard --web` serves the **read-only web dashboard** (a live
orchestration/delegation graph with run summaries and prompt/output inspection)
until interrupted; `--addr` sets the listen address (default
`127.0.0.1:8080`). Use it when the user wants a browser view of a running
orchestration. The Overview and Org pages also show the fleet activity strip:
live Herdr sessions are counted separately from engine jobs and unresolved
escalations. Org tree nodes retain the hierarchy while adding session status,
terminal task title, turn age, and last-completed-turn detail. No session,
Herdr unavailable, and an empty filter are distinct labeled states. One shared
server-side poller feeds all viewers through `/api/fleet/activity/events`; the
surface remains read-only and exposes no transcript content.

The web dashboard's `/comms` route is a read-only operator inbox for typed org
escalations and workflow engine markers. Org-note bodies are operator-visible
on this page. Open escalations float above resolved traffic, and `?note=<id>`
deep-links to the note's workflow conversation. The page never resolves an
escalation itself; use `gitmoot org escalate resolve` from the CLI.

The styled output leads with a "needs attention" block, colors and truncates
long lists, and groups near-identical runtime sessions; `--all` shows
everything. `--watch` redraws on an interval (default 5s) and cannot be
combined with `--json`.

## Event Stream (Webhooks)

To notify an external system when jobs finish, configure the off-by-default
webhook transport in the `[events]` section of the Gitmoot config — it is **not**
in the generated default config, so this documentation is its discovery surface:

```toml
[events]
webhook_url = "https://example.com/gitmoot-events"  # empty (default) = OFF
timeout = "2s"                                       # per-POST timeout
# socket_path = ""                                   # reserved, unused today
```

With `webhook_url` set, Gitmoot POSTs a small, versioned (`schema_version = 1`),
redacted JSON event to that endpoint for: `job.finished`, `job.failed`,
`job.blocked`, `job.needs_attention` (an `escalate_human` pause), and
`job.deferred`. Delivery is best-effort
(bounded buffer + timeout; drops are recorded as a local `event_sink_drop` job
event, never blocking the job). Consumer rule: **treat a `job.failed` as final
only when it is NOT immediately followed by a `job.deferred` for the same job
id.** Since #532 slice E a deferred run emits `job.deferred` as a first-class
transition **instead of** `job.failed` (no preceding `job.failed`); the rule
still holds and is forward-compatible with the older `job.failed`→`job.deferred`
flap. See `docs/events.md` for the full contract.

## Review Router

`gitmoot review request` is the front door for asking for an independent review.
The requester names the pull request; Gitmoot picks the reviewer, runtime and
model, deduplicates on the exact head, and wakes the requester when the verdict
is saved.

**`gitmoot agent review` now routes THROUGH this command.** It used to dispatch
beside it, which is why the router's machinery went unused: measured 2026-09-16,
687 of 688 reviews on one box came through `agent review`, so exact-head dedup,
the delta baseline, availability-aware runtime choice and the verdict wake were
reachable in principle and unused in practice. The lower-level form keeps its
own surface — it NAMES a reviewer, carries a review message, and takes
`--lead` — and those inputs are forwarded rather than discarded.

```sh
gitmoot review request --pr 2170 [--repo owner/repo] [--purpose code|security|ui|architecture] \
    [--head <40-hex>] [--branch <name>] [--role <org-role>] [--ttl 12h] [--reviewer <agent>] \
    [--runtime <name>] [--exec-backend local|remote] [--model <provider/model>] \
    [--effort <level>] [--workflow <id>] [--session <ref>] [--lead <implementer>] \
    [--full] [--allow-prompt-head-mismatch] [--json] \
    [-- "review instructions"]
gitmoot review status --pr 2170 [--repo owner/repo] [--json]
```

`--lead` names the implementer a changes-requested verdict routes to; with no
`--lead` the request dispatches with no fix target and says so. The positional
message is appended to the router's own brief under a labelled header, so a
reviewer can tell operator instructions from generated framing — pass it after
`--` if it starts with a dash.

`--exec-backend local|remote` selects where this review's runtime executes. It
is persisted on the job before enqueue and affects no other queued or future
job. Omit it for local execution; process-wide `[remote_exec].backend` does not
reroute reviews. Remote reviews currently support only `shell` and `omp`;
Gitmoot refuses any other runtime/backend pair before enqueue and names both
operands. `agent review` forwards the same flag through the review router.

Two things `agent review` reports that are easy to miss:

- **A dispatch that cannot be delegated RECORDS why.** No `--org-role` (the
  router requires a verdict recipient), `--foreground`, or any flag the router
  cannot express keeps the direct path and writes a `router_bypassed` job event
  naming the reason — read it months later with `gitmoot job events <id>` (NOT
  `job show`, which prints the row and its payload and never lists events). The
  unexpressible-flag case also prints the reason to stderr; the other two arms
  never NAME the reason on stderr, which is exactly why the durable event
  exists — they may still emit unrelated warnings there, so stderr silence is
  not the discriminator. A silently different review is worse than a refused one.
  **Counting unrouted reviews:** `router_bypassed` alone UNDERCOUNTS, because
  `agent run --action review` and `orchestrate --pr` never reach that gate and
  record their reason in `route_selected` instead. The complete query is
  `route_selected NOT LIKE '%review_request%'`.
- **An ATTACHING request is told what it lost.** A second request at a head that
  already holds a claim attaches to the running review rather than spending a
  second reviewer — and prints which of your `--reviewer`, instructions,
  `--lead`, `--model` or `--runtime` were discarded, because a caller told only
  that it is "awaiting a verdict" would wait on a review it never commissioned.

`--runtime` overrides the omp pin. The router pins omp because it SELECTS the
reviewer, so the runtime is its choice rather than an agent's identity — but a
pin with no escape is a dead end: a role carrying a runtime-scoped
`org_role_unavailable` hold is refused dispatch when the hold names the runtime
it selected, and the caller has nothing else to reach for. `gitmoot agent
review` takes the same flag and pins nothing, because a REGISTERED reviewer's
runtime carries its auth profile and session.

What one request does, in order:

1. Resolves the pull request's current head (or binds `--head`, which must be the
   full 40 hex characters) and the requester role (`--role`, default
   `GITMOOT_ORG_ROLE`; it must exist in the organization chart because it is the
   notification destination).
2. Claims `(repo, PR, head, purpose)` in `review_requests`. THE CLAIM ROW, NOT THE
   JOB ROW, IS THE MUTUAL EXCLUSION: dispatch does real work (read-only worktree
   allocation, runtime preflight, GitHub reads) before the job is enqueued, so
   for a few seconds the winner holds a claim whose job id is not yet readable.
   A requester arriving in that window returns `state: attached` with
   `job_state: dispatching` rather than concluding the holder is dead. Takeover
   asks LIVENESS, not elapsed time: the claim records the dispatching process
   (pid, its `/proc` start-time identity, and the boot id), so a claim is taken
   over when its job ended without a verdict, when that process is provably
   gone, or when the claim was recorded on an earlier boot. A dispatch that is
   merely slow — a cold PR-ref fetch, a stopped process — keeps its claim; a
   time bound applies only where liveness cannot be evaluated at all. A
   synthetic `failed` result, which the daemon's dead-runtime recovery writes,
   is NOT a verdict, and neither is a verdict stored on a job that did not
   succeed: the awaited fact is satisfied only from a succeeded transition, so
   the router refuses to call anything else `verdict_exists`. A claim standing
   on a real `approved`/`changes_requested` verdict returns
   `state: verdict_exists` and spends nothing. A claim whose job DELEGATED the
   answer — a staged-review preflight whose own result is a fan-out — is held
   while its verdict child still runs. A different `--purpose` is a different
   question and runs in parallel: review-loop detection matches PURPOSE as well
   as agent and head, so a security request is not refused because a code
   review already happened — including with one eligible reviewer or an
   explicit `--reviewer`, where substituting a different agent is not an
   option. A repeat of the SAME purpose by the same agent at the same head is
   still refused, and everything outside the router carries no purpose on
   either side, so its comparison is the historical one.
3. Selects a registered agent with the `review` capability, WITHOUT `implement`,
   scoped to the repository (omp-native agents first, then by name), or the
   `--reviewer` you name. The job runs as a background review-only job on `omp`
   with a fresh per-job session, `--no-fix-target` semantics, and the first
   model of the purpose's pool; merge-gate independence rules apply unchanged.
   Selection is PURPOSE-SCOPED: an agent holding a verdict of a different
   purpose at this head is still eligible, and the refusal fires only when every
   candidate already answered THIS purpose.
4. Subscribes the requester to the exact-head verdict FOR THAT PURPOSE. The
   subscription key is `owner/repo#N@sha|purpose`, so a `code` verdict cannot
   terminally satisfy a `security` request at the same head — enforced on BOTH
   doors: the producer resolves the purposed and bare keys in one transaction,
   and the subscribe-time recheck matches purpose as well as head, so a waiter
   registering after a different-purpose verdict is not satisfied by it.
   `gitmoot org await review` keeps the bare `owner/repo#N@sha` key and its
   any-purpose meaning, and a routed review inherits its purpose into REVIEW
   delegation children so a staged review still answers the purposed wait —
   scoped to review legs, because a non-review leg carrying a review purpose is
   read by consumers keyed on review type. The wake fires from the reviewing job's own
   state transition when the verdict is persisted, before and independently of
   gate advancement, and it carries the decision, findings count, executed-check
   count, evidence declaration and the `gitmoot job show <id>` command. A second
   request by the same role keeps its original wait; two concurrent requests for
   the same role and subject both ATTACH to the single live wait rather than one
   failing. When the wait's `--ttl`
   elapses it expires to the role's parent, as every awaited fact does.

The request prints every hold it can see rather than leaving a requester to
infer one: the daemon not running, the disk guard pausing dispatch, and any
head-blind review that cannot satisfy an exact-head wait at all. `review status`
lists each review job with head, model, verdict, evidence and the daemon's hold
reason, and states the GATE's own capability in words — whether native
auto-merge is enabled, or disabled by the operator kill switch, in which case
the gate publishes status and merges nothing. An absent or not-applied gate
marker is never an approval.

Model pools are configured per purpose; an unconfigured purpose uses `code`:

```toml
[review_router]
code         = ["devin/swe-2", "openai-codex/gpt-5.6-sol"]
security     = ["anthropic/claude-opus-4-6", "devin/swe-2"]
```

Every entry is a provider-qualified omp model. When a delivery fails on a
PROVIDER quota or auth error before any verdict, the daemon advances the job to
the next pool entry and re-queues it immediately (event
`review_model_fallback`) instead of waiting out the provider's window; with the
pool exhausted the ordinary timed operational hold applies, and the shared
attempt budget bounds the whole sequence. A verdict, a finding, or a product
failure never changes the model: fallback exists for operational failure only.

## Review Policy

Review severity controls whether a reported finding restarts the fix loop. The
default preserves the existing block-all behavior:

```toml
[review]
blocking_severity = "P3"

[repos."themartianapp/keephair".review]
blocking_severity = "P1"
```

The threshold is inclusive. `P1` blocks `P0` and `P1`; `P2` and `P3` still post
their findings and record the raw `changes_requested` result, but Gitmoot treats
the round as approved-with-notes and does not dispatch a fix. The global default
is `P3`, so every valid finding blocks unless a repository overrides it.

A blocking verdict must also say WHERE. If a `changes_requested` result blocks at
the threshold, carries findings, and no finding at or above the threshold names a
location — `evidence_locator`, `locator`, `file`, `location`, cited `evidence`, or
a path like `internal/db/store.go:88` inside its prose — the round resolves as
approved-with-notes instead of blocking. The findings are still stored and posted;
only the block is withdrawn, because a merge cannot be stopped on a defect nobody
can open. The `review_approved_with_notes` event names this reason, distinct from
the below-threshold fold.

This is deliberately conservative and keeps blocking whenever the gate cannot see
the whole picture: no findings at all, findings that are empty objects, a finding
that does not decode, or a severity Gitmoot cannot rank.
Configured values must be `P0`, `P1`, `P2`, or `P3`.
An invalid `blocking_severity` value falls back to `P3` while other valid review
fields remain active. Any other review-policy parse or read error rejects the
entire applied review policy, restoring `P3` with native fanout and risk tiers
off.

The threshold applies to native review rounds only. A pipeline review stage is
report-only — the pipeline advancer owns folding its verdict — so its raw
`changes_requested` keeps blocking the merge gate at every threshold and never
counts toward required-reviewer approval.

### Fan-outs are not verdicts

A review result that declares `delegations` is a coordinator **fan-out**: it
announces a panel. Gitmoot dispatches a result's delegations *after* the result
is stored, so such a row was written before any delegate could report, and it is
not an answer about the code (#1685). This is the shape the shipped
`review-panel` agent template produces, and it is legitimate — what is not
legitimate is counting it as a verdict.

Every surface that reads a review decision applies the same rule:

- the **merge gate** excludes a fan-out row from the verdict population. It
  neither satisfies nor blocks the reviewer slot, so an independent verdict at
  the same head still decides the PR;
- if the panel **reported**, the gate walks nested fan-outs to their leaf
  verdicts and decides that slot on those leaves — at least one approving leaf,
  no blocking verdict, no crashed, abstaining or still-running node, and every
  declared delegation accounted for. A coordinating child is still an
  announcement, not a verdict. Identity, family and
  `merge_gate_approval_evidence` records use the
  actual approving leaves. Only the LATEST attempt of each delegation counts, so
  an approved retry supersedes a failed original instead of being poisoned by it;
- if the panel was announced and **never dispatched**, the gate reports
  `no review verdict at evaluated head: <agent> (job <id>, N declared) declared
  delegations that never reported` rather than merging or parking;
- **pipeline auto-merge** refuses a fan-out. A pipeline stage has its executable
  `delegations` stripped at the mailbox seam so a leaf can never spawn children,
  and the classification is preserved across that strip — stripping the
  instructions must not erase what the row IS;
- **required-reviewer counting**, the **review-verdict wake**, the **canonical
  same-head verdict history** (`SucceededReviewVerdicts`) and **awaited
  review-verdict facts** all skip fan-outs, so an announcement neither satisfies
  a reviewer slot nor wakes a waiter nor suppresses the retry that would produce
  a real verdict;
- the **proof projector** mints no approval claim for a fan-out, and the
  rendered proof shows `no verdict` rather than counting it as approved;
- `blocked` and `failed` reviews are never fan-outs. They already refuse on their
  own terms and keep reporting their own cause.

A coordinator is therefore dispatchable into a review slot, by CLI and by
heartbeat. Gitmoot judges the panel it convenes, not the announcement.

### Risk-Tiered Adaptive Review

Set `risk_tiers_enabled = true` in `[review]` to scale review depth to a
change's blast radius:

```toml
[review]
risk_tiers_enabled = true                     # empty/false (default) = OFF
# high_risk_paths matched against the PR's changed files (** = any path depth):
high_risk_paths = ["**/auth/**", "**/security/**", "**/payment/**", "**/migration/**", "go.mod"]
risk_label_high = "risk:high"                 # PR label that forces the high tier
risk_label_routine = "risk:routine"           # PR label that forces the routine tier
```

With `risk_tiers_enabled = true`, each opened PR is classified: **explicit PR
label > changed-path glob match > default routine** (a `risk:high`/`risk:routine`
label wins over paths; a high label wins a label tie). A `routine` PR keeps the
unchanged single-reviewer fan-out. A `high` PR instead fans out a delegation
batch of **refutation-framed lens reviewers** (correctness, security, and, with
three or more configured reviewers, regression), each prompted to *disprove*
the change and return structured findings `{lens, refuted, severity, confidence,
evidence}` in `gitmoot_result.findings`. The lenses are synthesized by the
existing delegation `synthesis_rule = quorum` engine: **any blocking refutation
fails the quorum and blocks the merge**; the configured quorum of effective
approvals satisfies it. The resolved tier is recorded as a
`risk_tier_resolved` job event so an escalation is explainable in the
report/dashboard. With `risk_tiers_enabled`
off, PR review uses the single-reviewer path. The competition tier (two
implementations + a judge) is a planned follow-up.

## Bug Reports

Use `gitmoot report bug` to build a redacted GitHub-ready issue from local
Gitmoot error state. Job reports are fully supported; daemon, dashboard, and
train selectors are reserved and return clear unsupported-source errors until
their source collectors are implemented.

```sh
gitmoot report bug --job <job-id> [--preview]
gitmoot report bug --job <job-id> --create --yes
gitmoot report bug --source daemon --preview
gitmoot report bug --source dashboard --preview
gitmoot report bug --train <session-id> --create --yes
```

Default behavior is preview. Agents should run preview first, show or summarize
the redacted draft, and create an issue only when the user explicitly asks or
the active workflow policy already permits filing reports. Non-interactive
creation requires `--create --yes`.

Created reports target `gitmoot/gitmoot`, include the labels
`gitmoot-dashboard-report` and `bug`, and carry a fingerprint marker in the
body so duplicate open issues can be reused instead of creating another report.
If duplicate search fails in the CLI path, Gitmoot prints a warning and still
creates the issue; dashboard creates fail closed and keep the preview open so
the user can retry.

After creation, report the printed issue URL back to the user. If Gitmoot says
an existing issue was found, report that URL instead of presenting it as a new
issue.

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
  --model gpt-5-codex \
  --effort high \
  --start-daemon
```

`--runtime` accepts `codex`, `claude`, `kimi`, or `omp`. `kimi` is
the current Kimi Code CLI. Run `kimi login` first and restart
the Gitmoot daemon so it inherits the session. `agent subscribe` additionally
accepts `--runtime shell`, the deterministic no-LLM adapter whose `--session`
is a **command** (the job prompt arrives as `$1`; stdout must carry the
`gitmoot_result` envelope) — the workhorse for deterministic E2E tests.

`omp` is the oh-my-pi CLI (#1428): a multi-provider **routing harness** rather
than one vendor's CLI, so which provider answers is a property of the omp profile
the daemon runs under. Practical consequences worth knowing before you register
an omp seat:

- **Capabilities are `review`, `implement`, `ask` — not `produce`** (its Bun
  binary's Landlock interaction is unprobed).
- **Every job is a fresh session.** omp is stateless in v1 and never resumes:
  `omp --resume` relocates the working directory to the previous session's cwd
  and overwrites `--cwd`, so a resumed job would edit the *old* worktree and
  leave the job's own worktree clean — a green job with an empty diff. A
  `--session` ref may be a session UUID or `fresh:<suffix>`, never `last`.
- **Exit code 0 is not success.** In `--mode=json` omp exits 0 even on a failed
  turn, so Gitmoot decides success by parsing the NDJSON stream, and an empty
  assistant answer, a stream that ends mid-retry, or a run **cut off mid-work**
  fails loudly rather than reporting an empty success. The cut-off case is the
  quiet one: when the `--max-time` deadline or the provider's output cap stops
  the run, the envelope is complete and only the final answer is missing, so the
  parser reads the FINAL assistant message rather than the last one that carried
  text — an earlier work note is never handed back as the job's answer.
- **`--policy` selects the ordinary `--approval-mode` value,** and the flag is
  always present for **determinism** - omitting it would inherit whatever
  `tools.approvalMode` the host config carries. `read-only` maps to
  `always-ask`, `workspace-write` maps to `write`, and `auto` plus
  `danger-full-access` map to `yolo`. Measured on omp 17.2.4 headless,
  `always-ask` lets `read`/`grep`/`glob` succeed and refuses
  `bash`/`write`, with the process exiting 0 and a full `agent_end`, so it
  **restricts** omp rather than breaking it (#1721). A kernel-enforced
  `ReadOnlySeat` is the exception: Gitmoot wraps the delivery in Landlock and
  passes `yolo` so headless shell and test tools can run. The adapter declares
  that override `widened`; Landlock remains the write boundary. Non-seat
  `read-only` keeps `always-ask`. The other three explicit policy mappings are
  declared `applied`, while `auto` is `widened`.
- **OMP runtime-family diversity uses runtime-reported upstream-provider
  evidence.** A successful delivery whose final runtime message identifies its
  provider and model records that provider in the append-only event ledger; the
  OMP wrapper remains `effective_runtime=omp`. Providers shared with native
  adapters compare as the same family: `openai` and `openai-codex` map to
  Codex, `anthropic` maps to Claude, and `kimi-code` maps to Kimi. Providers
  without a native adapter stay namespaced, for example `omp:devin`. Different
  models and agent names on one provider remain the same family. The comparison
  is advisory, not an independence gate: same-family comparisons emit
  `merge_gate_family_advisory`, while unresolved comparisons retain the
  `merge_gate_family_unresolved` event; neither disqualifies a substantive
  approval from a reviewer whose identity is not an implementer. For a review
  fan-out, every coordinating parent is an announcement rather than a verdict,
  including nested fan-outs. The gate applies identity and family checks to each
  approving leaf and records approval evidence on that leaf. Requested model
  text alone and failed delivery are not provider evidence.
- **Authentication depends on the seat policy.** Ordinary omp jobs use the
  profile, provider keys, or auth broker visible to the daemon. Read-only review
  and ask seats require `OMP_AUTH_BROKER_URL` as an HTTPS or loopback HTTP
  origin, an owner-only token file named by `OMP_AUTH_BROKER_TOKEN_FILE`, and a
  provider-qualified effective model such as `kimi-code/k3`. An upstream
  `OMP_AUTH_BROKER_TOKEN` in the daemon environment
  is refused because the runtime requires seat-readable `/proc`. Gitmoot writes
  the random job-local token and loopback URL only into the seat's minimal
  `config.yml`; no broker secret enters the child environment. The proxy exposes
  only that provider, forwards credential refreshes, and keeps shared broker
  writes out of reach. Gitmoot drops the operator profile and ambient provider
  keys, runs a daemon-staged OMP binary, and supplies job-private
  `PI_CONFIG_DIR` and `PI_CODING_AGENT_DIR` roots. Missing or unsafe broker
  configuration refuses before runtime staging; an unqualified effective model
  refuses before OMP launches. Restart the daemon after changing its broker
  environment; its `PATH` also comes from the systemd EnvironmentFile.

`agent start`, `agent subscribe`, and `agent type set` accept an optional
`--model <name>` flag that sets the agent's default runtime model. It is a
free-form, runtime-scoped string (a Codex, Claude Code, Kimi Code, or omp
model name) with no allow-list; both `--model X` and `--model=X` are accepted.
A per-job `--model` (or a delegation's `model` field) overrides this default,
and an omitted model preserves the runtime's own default. The same default can
be set in config under `[agents.<type>].model`.

The same commands accept `--effort <value>` as the agent's default reasoning
effort, and `agent run`, `ask`, `review`, and `orchestrate` accept it
as a per-job override. The resolution order mirrors model selection: job effort,
agent effort, `[runtimes.<runtime>].default_effort`, then no explicit override.
Values are free-form pass-through strings. Codex receives
`-c model_reasoning_effort=<value>`; omp receives `--thinking <level>` when the
resolved value is one of `off|minimal|low|medium|high|xhigh|max|auto` and no flag
at all otherwise; Claude and Kimi ignore the setting.

`agent start` and `agent subscribe` accept `--policy` (default `auto`). The policy
maps to the runtime permission mode and decides what a headless job may do:

| `--policy` | Claude `--permission-mode` | Headless capability |
|---|---|---|
| `read-only` | `plan` | inspect/report only, no writes |
| `workspace-write` | `acceptEdits` | file edits only — does NOT unblock Bash (`go`/`git`/`gh`) |
| `danger-full-access` | `bypassPermissions` | full implementation: file writes plus Bash |
| `auto` (default) | *(no flag)* | non-deterministic — inherited from ambient Claude config |

Gitmoot also records **permission-policy instrumentation** for engine-dispatched
jobs. Each adapter derives one of `applied`, `widened`, or `not-applied` while it
builds argv. A live agent whose runtime does not resolve is `unresolved`; a
missing agent row also produces an `unresolved` warning for that individual job.
`not-applied` means only that Gitmoot supplied no permission-policy flag. It does
**not** mean the process was unsandboxed: host runtime configuration may still
constrain it, and Gitmoot does not read that configuration. `not-applied` and
`unresolved` produce a structured `permission_policy_not_applied` job event,
coalesced once per `(agent, runtime, policy, capability)` per 24-hour window.
This is visibility, not protection: the warning never refuses, blocks, or
changes a job. At completion, the one winning sampled job for that window is
updated in place with `checkout_dirty`, `branch_pushed`, and
`payload_had_pull_request`. Git-derived booleans are
`null` when Gitmoot could not determine them; `branch_pushed_instrument` records
whether the answer came from the local upstream, `ls-remote`, the branchless
payload, or remained unavailable. `payload_had_pull_request` states only whether
the job payload already carried a PR number at capture; it does not claim the
sampled job opened that PR. Capture failures are logged and never change the job
outcome.

The live-fleet ratchet is off by default. Set
`[daemon].permission_policy_observation_enabled = true` to record the first
home-store baseline and compare later daemon ticks against it. Any newly
introduced configuration emits a `permission_policy_baseline_exceeded` event,
even when removals leave the total unchanged or lower; only a strict subset
lowers the stored baseline. `gitmoot doctor` reports current, baseline, and
delta for fixable live configurations. Durable jobs whose agent identity no
longer resolves are excluded from that ratchet and reported separately through
the event's `unresolved_job_agents` count and a healthy informational doctor
line. This baseline intentionally lives in the Gitmoot home store, not in source
control or CI, because only that store contains the fleet inventory.

Because of this, an agent that carries the `implement` capability **must** be
started/subscribed with a write policy. Gitmoot fails closed: `--capability
implement` with `auto`/empty or `read-only` is refused at `agent start`, at
`agent subscribe`, and when `--lead` names that agent on a review dispatch,
each with an actionable message. Set
`--policy danger-full-access` for full headless implementation (file writes plus
`go`/`git`/`gh`), or `--policy workspace-write` for edits-only (Bash stays
blocked). See `references/SAFETY.md` for the full mapping and rationale.

Implement jobs own the commit contract: Gitmoot commits and delivers the
worktree's changes after the job finishes, and every rendered implement prompt
carries one deterministic sentence telling the worker not to run `git commit`
or `git push`. Ask and review prompts are unchanged. On the Codex runtime, a
`workspace-write` job whose checkout is a linked `git worktree` also gets the
worktree's resolved git directory (`<main-repo>/.git/worktrees/<name>`) added
to the sandbox writable roots via `--add-dir`, so routine git metadata writes
(an index refresh from `git status`, or `git add`) work inside the sandbox.
The grant is additive and leaves operator-configured `writable_roots` intact;
read-only and danger-full-access sandboxes and primary (non-worktree)
checkouts are unchanged.

Subscribe an existing runtime session:

```sh
gitmoot agent subscribe reviewer \
  --runtime codex \
  --session <session-id-or-last> \
  --repo owner/repo \
  --role reviewer \
  --capability ask \
  --capability review \
  --model gpt-5-codex \
  --effort high

# Deterministic shell runtime: the session is a command, not a session id.
gitmoot agent subscribe stub-agent \
  --runtime shell \
  --session '/path/to/answer.sh' \
  --repo owner/repo \
  --role agent \
  --capability ask
```

Inspect and manage agents:

```sh
gitmoot agent list
gitmoot agent show reviewer
gitmoot agent show reviewer --json
gitmoot agent repos reviewer
gitmoot agent doctor reviewer
gitmoot agent restart reviewer
gitmoot agent remove reviewer
```

### `agent policy`: change a registered agent's autonomy policy in place

```
gitmoot agent policy <name> --policy auto|read-only|workspace-write|danger-full-access
```

**This writes the plane dispatch reads.** `agent type set --policy` writes the
config plane only, and most registered agents have no config section at all, so
for them it refuses outright. `agent start` and `agent subscribe` carry
`--policy` but mean re-registration, which replaces the runtime session.

Dispatch resolves a registered agent through `GetAgent`, which reads **the
`agents` row if one exists and otherwise the `agent_instances` row**. This verb
updates **both** rows in one transaction rather than only the one that outranks
the other today. A same-name `agent_instances` row can OUTLIVE the `agents` row
that outranked it, because `agent remove` deletes `agents` and not
`agent_instances`; a stale instance left behind would then become authoritative
and silently re-widen an agent you had tightened. Only a name present in
neither plane is unregistered.

**When a config type also exists, both planes are written**, and the command
reports each:

```
agent: appkit-omp
policy: workspace-write -> danger-full-access
config_type: danger-full-access          # or: none (registry-only agent; nothing to keep in step)
```

**Failure semantics are all-or-nothing, enforced by a held transaction.** Both
the agents row and the config type can authorise dispatch on their own: explicit
managed-type routing never consults the row. So no ordering is safe, because
whichever plane is written first is already live before the second can fail. The
database transaction is the coordinator instead. The rows are updated, the
config write runs as a barrier inside that still-open transaction, and the
commit lands only if the config write succeeded. A config failure rolls the rows
back, so nothing moved on any plane. If the commit itself fails after config
succeeded, the config write is rolled back too and the command reports that
nothing changed; only if that rollback also fails does it report the planes as
inconsistent, naming both values so you know what to correct.

**Every plane that can become effective is written**, not only the one that
decides today: a same-name `agent_instances` row is updated alongside the
`agents` row, because `agent remove` deletes only the latter and a surviving
instance would otherwise silently re-widen an agent you had tightened.

`agent type show` returns **non-zero** when a registered agent's policy differs
between planes, or when the effective policy cannot be determined; it prints
`policy_effective:` alongside the config `policy:` so neither a human nor a
stdout parser can read the config value as the effective one.

`gitmoot agent show <name>` keeps the existing `runtime_ref: <id>` line unchanged
and makes concrete session pinning explicit on a separate
`runtime_session: pinned (last successful use: <age>)` line. The extra line is
omitted for an unpinned `runtime_ref: last`. The age comes from the newest
succeeded job whose newest `effective_runtime` or `runtime_override` event
resolved to that exact runtime and session. An event for another runtime with
the same ref does not count, and an older event from a failed attempt is ignored
when the same job later succeeds on a retry. A pin with no matching successful
job prints `last successful use: never`. JSON keeps the raw `runtime_ref`
unchanged and adds `runtime_ref_pinned` plus, only for a pin,
`runtime_ref_last_successful_use`.

`gitmoot agent restart <name>` abandons the agent's runtime session and binds a
fresh one **in place** — the fix for a dead or stranded session that would
otherwise tempt a re-register. It refuses while the session is live or the
agent has in-flight jobs (finish or cancel those first). `gitmoot agent remove
<name>` unregisters the agent.

Delegate to a registered agent from the current local chat:

```sh
gitmoot agent run project-planner --repo owner/repo "Return the plan status."
gitmoot agent run reviewer --repo owner/repo --pr 12 --lead lead --background "Review this PR."
gitmoot agent review reviewer --repo owner/repo --pr 12 --lead lead "Review this PR."
gitmoot agent ask project-planner --repo owner/repo "Return the plan status."
gitmoot agent ask project-planner --repo owner/repo --background "Write the implementation plan."
gitmoot agent run reviewer --repo owner/repo --pr 12 --model gpt-5-codex "Review this PR."
gitmoot agent run reviewer --repo owner/repo --pr 12 --effort xhigh "Review this PR."
gitmoot job watch <job-id>
```

**Gitmoot does not dispatch implementation by any route a user can reach
(#2203).** There is no `agent implement` verb, no `--action implement`, and no
`--task`/`--base`/`--draft`/`--ready` implement flags; an `implement` leg in a
result's `delegations[]` is refused with the action named, `/gitmoot <agent>
implement` is not a recognized PR-comment command, and an `implement` heartbeat
is refused at config validation. The one remaining exception is the pipeline
`action: implement` stage kind, which still VALIDATES but has no allocator to
run in; whether `pipeline add` should refuse it is tracked in
[#2213](https://github.com/gitmoot/gitmoot/issues/2213).

What was removed in each case is the writable-worktree ALLOCATION plus the
enqueue — precisely what would have to come back for dispatch to return. The
removal followed the traffic rather than leading it: 568 of 570 implement rows
in a 14-day window were seats recording their own sessions, delegation-origin
implement legs total 47 lifetime with zero in the last 30 days, no PR comment
has ever carried an implement command, no heartbeat is configured with that
action, and no pipeline has ever created an implement stage (lifetime stage
jobs: ask 5,229, produce 92, implement zero). Seats implement in their own
session and record the work themselves:

```sh
gitmoot job record --agent lead --repo owner/repo --type implement \
  --decision implemented --pr 12 --head-sha <sha> \
  --title "Fix the findings on #12" --summary "What changed and why."
```

A `changes_requested` verdict therefore reports to the requester and to the
`--lead` seat; it never mints a fix job.

`agent review` and review-resolved `agent run` with `--org-role` queue the job
for daemon ownership by default. This keeps a review running if the calling
seat or its command runner exits. Use `--foreground` only when synchronous
ownership is intentional. Unattributed reviews retain their synchronous
default; `--background` remains accepted.

`agent run --action ask|review` explicitly selects the job action and wins
before the usual inference order (`--pr`/review `--head-sha` -> review, then
message heuristics, else ask). `--type <name>` has a separate meaning: it
selects a managed agent type. The flags can be used together. Invalid actions
and contradictions are rejected before enqueue; notably, `--action review`
requires `--pr`, and `implement` is no longer an accepted action.

Review admission is evidence-gated before Gitmoot creates a new review job.
For local `agent review` / `agent run` requests that resolve to review, and for
native engine review fan-out, Gitmoot reads succeeded review jobs for the same
repository and pull request. If every succeeded verdict at the requested exact
head agrees on one decision, dispatch is refused: the local CLI returns a hard
error, the engine blocks the task, and the matched succeeded job receives one
idempotent `review_loop_detected` event. A new head proceeds, and mixed decisions
at one head also proceed because the earlier claim is unstable. The loop guard
allows an empty head only before any succeeded review history exists for that
repo/PR; after that it fails closed until the caller supplies the current head.
The local CLI still requires a concrete head before dispatch. Each admitted
local review gets its own detached, read-only per-job worktree at that exact
commit; the stable per-PR Task row remains lifecycle metadata and no longer owns
the review checkout. Consequently, `gitmoot task list` shows an empty worktree
column for review Task rows. This is expected: each review job owns its own path,
so there is no single Task-level worktree path to display. The requested
`head_sha` stays on the job payload. Review
allocation fails closed rather than falling back to the registered checkout,
and dispatch refuses before Task mutation when the Gitmoot filesystem has less
than 5 GiB free or free-space measurement is unavailable. The prior verdict is
escalation evidence only and is never served as the new result.

A review dispatch is also bound to its own head by its PROMPT. Before the
read-only worktree is allocated and before any job row exists, Gitmoot resolves
every commit-shaped token in a review's instructions and classifies it against
the dispatch head: the head itself, an ancestor of it (which covers prior heads
on the branch and the branch base), a head this pull request recorded at some
earlier dispatch, or none of those. Only the last refuses, with a non-zero exit
naming both SHAs and, when the store knows it, the pull request whose head the
cited commit actually is. `--allow-prompt-head-mismatch` dispatches such a
request deliberately.

Three arms allow on purpose, because a prompt naming another commit is usually
how it states provenance. The recorded-head arm reads an append-only store fact
rather than git, so a legitimate prior head still passes after a force push has
left it unreachable. A citation whose relationship cannot be established at all,
because the dispatching checkout resolves neither commit, also dispatches: that
is a fact about the checkout rather than about the citation.

`review request` dispatches on the **omp** runtime because it SELECTS the
reviewer itself, so the runtime is its choice rather than an agent's identity.
`--runtime NAME` overrides that pin. The escape is not cosmetic: a role
carrying a runtime-scoped `org_role_unavailable` hold is refused dispatch when
the hold names the runtime it selected, and a pin with no override leaves the
caller nothing to reach for (gitmoot/gitmoot#2181). `gitmoot agent review`
takes the same flag, but pins nothing: a REGISTERED reviewer keeps its own
runtime, which carries its auth profile and session.

`prompt_head_warning` IS still emitted for a review, for two relations rather
than one. The normal-path relation is a RECORDED PRIOR HEAD THAT IS NOT AN
ANCESTOR. The second is a citation whose ancestry the classifier could not
establish: `promptCitationAncestry` returns `known=false` when `IsAncestor`
errors, and that lands on `promptCommitUnresolved`, which the filter also
retains. That arm is an INSTRUMENT FAILURE rather than a statement about the
prompt, and it is deliberately reachable - when the classifier cannot judge a
citation, nobody has judged it.

Retention is PER CITATION: a warning is kept only when the retained relation is
the one that warning is about, matched on its leading
`prompt references commit <token>,` clause. That is what makes the two relations
above exhaustive - an earlier version tested for the token anywhere in the
warning text, and since every warning names the dispatch head twice, an unjudged
token that happened to be a hex run from inside that sha could retain a
DIFFERENT citation's warning.

Two things are silent. A token the scan itself could not resolve produces no
warning of its own, because `dispatchPromptHeadContradictionWarnings` discards it
before any classification. And on the resolvable path ancestry is checked BEFORE
the recorded-head arm, so a commit that is both a prior head and an ancestor is
silent - the ordinary scoped re-review, citing its own previous head.
Ancestor provenance is silent, because naming the branch base is how a prompt
states where the work sits. The prior-head arm is the force-push shape - the
commit really was a head of this pull request and no longer is - so a prompt
naming it as its target is reviewing a tree that is gone, and that is worth one
line to its operator even though the dispatch proceeds. Ask keeps the blanket
warning, because no refusal runs in front of it and it is its only head check.

`--head-sha` must be the FULL 40 hex characters for a review. An abbreviated
value used to dispatch and then be cancelled by the daemon's staleness check,
which compared it against the pull request's full head and reported the same
commit as a move; it is now refused at dispatch, before the read-only worktree,
the review task row and the job row. The refusal rejects EVERY non-empty value
that is not exactly 40 hex characters, including a revision expression such as
`<sha>^` or `<sha>~1`: the daemon binds this value by EQUALITY against the pull
request's head, so no other shape can bind whatever it looks like. It runs after
the review-loop and reviewer-identity refusals, so those still name their own
preconditions first, and before the review task is upserted, so a refused
dispatch leaves no durable state behind.

`--no-fix-target` dispatches a REVIEW-ONLY review: the reviewer need not be able
to implement and no `--lead` is required. The lead exists so a
`changes_requested` verdict has an implementer to route to, so declining one is
a decision, and it is recorded as a `review_no_fix_target` job event stating
that the dispatching operator owns the follow-up. An UNSTATED absence still
refuses, and `--no-fix-target` with `--lead` is refused as mutually exclusive.

This exact `(repo, PR, head_sha, decision)` evidence key is intentional: the
#1419 review panel rejected round counters and other instruments, so this guard
does not infer a loop from a numeric threshold. Direct PR-comment review ingress
is unchanged here and remains advance-time guarded until #1433; cached-verdict
serving remains out of scope for #1415/#1423.
Local review dispatches accept `--lead <implementer>` on `agent review` and on
`agent run` when it resolves to review. A `changes_requested` verdict routes its
fix WORK to that lead, not to the reviewer: Gitmoot wakes the lead's org role
and never mints a fix job (#2203). Before creating a review job or
starting its runtime session, Gitmoot loads the lead from the agents database
and requires that it exist, can access the repository, has `implement`
capability, and uses a write-granting policy (`workspace-write` or
`danger-full-access`). Without `--lead`, the reviewer is the fallback lead and
must pass the same checks. Managed-type review dispatches also require an
explicit DB-backed lead. `--lead` is rejected whenever `agent run` resolves to
anything but review, and is not accepted by `orchestrate`.

A strict review-only agent therefore needs either an explicit implementer or
`--no-fix-target`, which declares that this review has NO fix target and the
dispatching operator owns the follow-up. That declaration is carried on the job
payload, not only as an event, so the ledger records who owns an open
`changes_requested` finding: Gitmoot dispatches no fix either way (#2203), and a
review with no lead has no org role to wake, which is exactly what the operator
is taking on. `--no-fix-target` is rejected outside review and is mutually
exclusive with `--lead`.

This dispatch-time lead validation applies only to local CLI reviews started by
`gitmoot agent review` or review-resolved `gitmoot agent run`. Reviews routed
from PR comments continue to validate their fix target when the workflow
advances until [gitmoot#1433](https://github.com/gitmoot/gitmoot/issues/1433)
adds the corresponding ingress preflight.

When an engine review returns `changes_requested`, Gitmoot persists the verdict
and wakes the requester's org role without creating an implement job. That is
the terminal behaviour, not a default: there is no per-PR opt-in that converts a
verdict into a fix dispatch, and no writable fix clone is allocated (#2203).
The lead implements in its own session and records the work with `gitmoot job
record --type implement` — the rows the merge gate's implementer attribution
and reviewer-independence check read.

Before delivery, these dispatch commands scan commit-shaped tokens against the
target repository. Ask preserves its existing scanner input;
review scans its newly allocated exact-head worktree with the requested head
still bound. If a token resolves to a commit other than the dispatch head,
Gitmoot prints an advisory warning such as
`prompt references commit <referenced>, but the dispatch head is <head>; Gitmoot
will use dispatch head <head>`. The job still runs because prompts may
legitimately discuss historical commits. Hex strings that do not resolve to a
commit, including mutation-hygiene SHA-256 restore hashes, do not warn.

**The scan covers the instructions you pass on the command line, and nothing
else.** Text contributed by a recipe template (`--recipe`) or by the selected
agent's own template is not scanned, so a stale commit cited inside a template
body dispatches with no warning. If you drive dispatches through templates,
treat this warning as covering your instructions only.

A PR the forge reports as a DRAFT is an author-controlled hold: it does not move
its task to `awaiting_human_merge`, because no human merge decision has been
requested. Marking the PR ready for review lets normal merge-gate advancement
resume, and an unknown draft state fails toward NOT parking. Gitmoot no longer
opens implementation PRs itself and the `--draft`/`--ready` dispatch flags went
with `agent implement` (#2203), so this hold now describes whatever PR the gate
observes — including one a seat opened by hand.

A repository's default branch comes from the registered checkout's local
`origin/HEAD` symbolic ref. Git does not refresh that ref when the upstream
repository renames its default branch. Run `git remote set-head origin -a` in
the registered checkout after such a rename; until then Gitmoot treats the
cached ref as authoritative and may reconcile `repos.default_branch` back to
the stale name.

`gitmoot agent run`, `ask`, and `review` (and `orchestrate`) accept
an optional `--model <name>` flag that pins the runtime model for that one job,
overriding the agent's configured default. It is a free-form, runtime-scoped
string (a Codex, Claude Code, Kimi Code, or omp model name) with no allow-list;
an omitted `--model` leaves the agent's default model in effect. Both `--model X`
and `--model=X` are accepted.

`--effort <value>` and `--effort=<value>` select reasoning effort for one job
with the same job-over-agent-over-registry precedence. Gitmoot does not validate
an allow-list; Codex validates the forwarded value.

The same commands accept an optional per-job `--runtime
codex|claude|kimi|omp|shell` override: that ONE job runs through the
named runtime while the agent's registered default runtime stays untouched
(`agent show` is unchanged afterwards). An overridden job never resumes — and
never writes back to — the agent's default-runtime session: it runs on a fresh
session of the override runtime, or on an explicit `--session <ref>` (a
Codex/Claude session id, a Kimi session id, an omp session UUID or
`fresh:<suffix>`, or — required for `shell` — a command; `last` is rejected
because it resumes whichever session is most
recent rather than a concrete one), and its runtime-session lock names the
override runtime so it cannot collide with the default session's lock. Model
rule: `--model` combined with `--runtime` is interpreted for the OVERRIDE
runtime; an override without `--model` uses the override runtime's default
model — the agent's configured default model is never applied to a different
runtime. The same rule applies to `--effort`: an explicit job value belongs to
the override runtime, while the agent's default effort does not cross runtimes.
An unknown `--runtime` fails before any job is enqueued, background (daemon) jobs honor
the override identically to foreground, and a coordinator's delegation-tree
continuations (synthesis, corrective, replan, finalize) inherit the override,
so an `orchestrate --runtime` tree stays on the override runtime across
generations:

```sh
# Retry a hard review through Claude without re-registering the reviewer:
gitmoot agent review reviewer --repo owner/repo --pr 123 "Re-review this PR." --runtime claude
gitmoot agent ask reviewer "Compare the approaches." --repo owner/repo --runtime kimi --model kimi-k2

# Route one job through omp's router; --effort becomes omp's --thinking level:
gitmoot agent ask reviewer "Summarize the risk in this diff." --repo owner/repo --runtime omp --effort high
```

`gitmoot orchestrate` and `agent run` also accept an optional
`--skip-native-review-fanout` flag. It states that the coordinator owns review
orchestration, so a PR on this branch must not be fanned out to Gitmoot's
native reviewers (the configured required reviewers, or the ones passed for the
task). The flag is persisted on the job payload and on the branch lock, and the
daemon's GitHub PR-watcher reads the lock, so a PR it observes on that branch
stays free of native review fan-out. The engine's implement-advance arm reads
the same flag, but no implement job can be dispatched while #2203 stands, so
the PR-watcher is the path that still exercises it. The flag defaults off;
leave it off for the full native review fan-out.

When a synchronous `agent run`/`ask`/`review`/`orchestrate` job
delivers and **succeeds terminally** but a benign *post-success* advancement step
errors — for example a merge-gate block on the freshly-opened PR, or a 422
"a pull request already exists" race — the command no longer discards the result.
It **exits 0** with the agent result on stdout (in JSON mode this includes an
additive `advance_error` field carrying the advance warning, omitted when there
is none), prints `advance warning: …` to stderr, and shows an `advance_error:`
line in human output. Genuine non-terminal failures (the job did not reach
`succeeded`) still exit non-zero as before. A normal success with no advance error
is byte-identical to prior behavior — no `advance_error` field is emitted.

**Review resilience under branch churn.** Newly dispatched local reviews are
pinned to the requested PR head in a per-job worktree, so a later branch push or
registered-checkout movement cannot change what they review. The daemon keeps
the older re-sync behavior only for legacy/fallback review jobs that lack an
owned read-only worktree: when their shared checkout advances and the PR remains
open on the same branch, it re-targets the payload and records
`review_head_resynced`; closed/merged, dirty, or wrong-branch checkouts fail.
A re-target requires the checkout head to have the dispatched head as an ancestor, so a review queued before an amend or a rebase force-push will **not** follow the branch: it refuses (recording `review_head_resync_refused`) and keeps the original wrong-head error, which a **non-delegation** review job defers and auto-retries within the shared blocker budget before failing terminally, while a **delegation-child** leg (a high-risk lens child, say) is routed terminally by its own DAG on the first tick — either way the remedy is to dispatch a **new** review at the new head, which is what exact-head review does anyway.
Any dispatched head that git resolves to the checkout's own commit — an abbreviation of any length, a case-differing 40-character SHA, a rev expression, a ref name — is recorded as `review_head_normalized` and is never a re-sync, while a dispatched head this checkout cannot resolve at all leaves the re-sync refused and keeps the original wrong-head failure, deferrable on the same non-delegation terms.
Relatedly,
when a foreground `agent review` finds the agent's serialized runtime session
**busy**, the review is now **left queued** for the daemon to run when the session
frees (a `requeued_runtime_busy` event is recorded) instead of being cancelled and
dropped; `agent ask` stays synchronous and keeps its existing
busy-session cancel behavior.

Start an orchestra of agents with `gitmoot orchestrate`:

```sh
gitmoot orchestrate project-planner "Plan and split this work across agents." --repo owner/repo
gitmoot orchestrate project-planner "Plan and split this work." --repo owner/repo --model gpt-5-codex
gitmoot orchestrate project-planner "Plan and split this work." --repo owner/repo --effort high
```

The built-in coordinator recipes `review-panel` and
`verifier` run a coordinator that fans work out to ephemeral workers and
reconvenes them in a continuation, with no agent pre-registration. The primary
invocation is the `--recipe` flag (also accepted on `agent run`), which routes
**any existing coordinator agent** through the named built-in recipe prompt
without changing the agent's identity or registration:

```sh
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier
```

`decompose-and-verify` was RETIRED because Gitmoot no longer dispatches
implementation (#2203). The recipe existed to split one implementation task
into parallel implementation legs, and there is no writable delegation action
left for those legs — `ask` and `review` are the only accepted ones. Its
teaching survives in WORKFLOWS.md § Multi-Model Delegation: decompose, keep the
legs file-disjoint, and verify with a separate worker rather than trusting a
producer's self-report.

The id stays reserved rather than unknown: `agent template list` hides it,
`agent template show <id>` and `agent prompt <id>` refuse it by name and point
at a successor recipe, and an agent or managed type still configured with it
fails dispatch with that same named refusal — so a stale script or a saved
command gets a successor instead of "unknown template".

The bare `gitmoot orchestrate <recipe-id> "..."` form also works, but the
positional argument must resolve to a **registered agent** (or configured
managed type) — so it requires an agent registered under the recipe name
(e.g. `agent start review-panel --template review-panel …` against an installed
`review-panel` row). On a fresh install without that registration it
fails with "agent not found"; prefer `--recipe`.

`gitmoot orchestrate <agent> "..." [--repo R]` is sugar for
`gitmoot agent run <agent> --background "..."`. It starts a conductor
(coordinator) that returns a `delegations[]` score; the players (child agents)
then run in parallel or in dependency order, and a finale (continuation)
reconvenes and synthesizes the results.

This uses the same agent registry, repo access grants, cached template snapshot,
runtime adapter, and local job history as PR-comment jobs. `agent run` is the
default coordinator-safe entrypoint because it routes to `ask` or `review` and
keeps branch, worktree, commit, push, PR, and workflow lifecycle
inside Gitmoot. `agent ask` is for analysis, planning, and questions only; it is
read-only, so when the message reads like branch/commit/push/PR orchestration it
prints a non-fatal note and still runs (pass `--force` to suppress the note).
The runtime
plugin helps Codex or Claude Code discover Gitmoot guidance, but it does not
replace the Gitmoot CLI. Synchronous jobs and queued jobs both use the same
runtime session locks.

Configure managed background agent types:

```sh
gitmoot agent type list
gitmoot agent type show planner
gitmoot agent type set planner --runtime codex --template planner --max-background 2 --idle-timeout 20m
gitmoot agent type set planner --model gpt-5-codex
gitmoot agent type set planner --effort high
gitmoot agent gc
```

`agent type set --model <name>` (or `[agents.<type>].model` in config) sets the
default runtime model for that managed agent type.

`agent type set --effort <value>` (or `[agents.<type>].effort`) sets its default
reasoning effort.

Schedule recurring agent work (heartbeats, off by default):

```sh
gitmoot agent heartbeat add repo-maintainer daily-status \
  --repo owner/repo --interval 24h --prompt "Daily status report." --enabled
# --runtime pins a runtime for this schedule.
gitmoot agent heartbeat add reviewer stale-prs \
  --repo owner/repo --interval 12h --action review --runtime codex \
  --prompt "Review stale open PRs and summarize blockers."
gitmoot agent heartbeat list
gitmoot agent heartbeat show repo-maintainer daily-status
gitmoot agent heartbeat enable|disable repo-maintainer daily-status
gitmoot agent heartbeat remove repo-maintainer daily-status
```

A heartbeat enqueues a normal background job on its `interval`. The actions are
read-only: `ask` (default) and `review` (`review` needs the agent's `review`
capability). `implement` is refused at config validation (#2203) — the
heartbeat's writable-worktree allocator and its enqueue are gone, so an
`implement` schedule could only have produced a branchless job with no checkout
to resolve. Schedule read-only work and let the seat record its own
implementation with `gitmoot job record --type implement`. An optional
`--runtime codex|claude|kimi|omp` runs the scheduled job on that runtime (fresh
session) instead of the agent default — the accepted set is derived from the
adapter registry, so `omp` joined it the moment the runtime was registered, and
an enabled heartbeat pointed at omp will spend whatever credential its profile
resolves. `gitmoot daemon status` surfaces each
schedule's last-run/next-due/last-status. See `docs/heartbeats.md` for the full
reference.

A registered single instance **shadows** a managed type of the same name:
dispatch resolves `gitmoot agent <name>` to a registered single instance before a
type, so force the type with `--type <name>` (or do not register a single
instance of that name). Since **v0.5.1** a foreground `gitmoot agent ask <type>`
(the `ask` action) dispatches to the managed type synchronously; background
`run`/`review` to a type and `[parallel_sessions]` temp-session
forking use the **background** path. See WORKFLOWS.md → "Running one agent's jobs in parallel".

## Agent Templates

Agent templates are **read-only installed data** (#2204). Gitmoot inspects them
and reads an installed row's content into a job payload so it reaches the
agent's prompt. It cannot author, update, or distribute one: there is no
`draft`, `validate`, `add`, `export`, `publish`, `pull`, `remote`, `diff`,
`revert`, or `update` verb, and no template-remote config section. A
template that must change is edited or re-seeded directly in its
`agent_templates` store row. That is the accepted trade, not an oversight.

The two surviving verbs are read-only inspection:

```sh
gitmoot agent template list [--capability <cap>] [--runtime <runtime>] [--tag <tag>] [--output <output>]
gitmoot agent template show <template-id>
```

`list` prints every built-in definition and every installed row: `available`
means a built-in is registered but has no store row, `installed@<commit>` means
a row exists. `show` prints the definition and its metadata and, when installed,
the current version, content hash, source commit, promotion state, and content.

Discover templates by metadata:

```sh
gitmoot agent template list --runtime codex --output plan
gitmoot agent template list --tag review --capability ask
gitmoot agent template show frontend-reviewer
```

Start an agent against an installed template:

```sh
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Templates are versioned in the store. An agent uses the current version by
default, or a pinned version when configured with a reference such as
`--template frontend-reviewer@v1`. Queued jobs keep the exact template content
snapshot they were created with. The dashboard's Agents page shows a template's
version history.

The built-in coordinator recipes (`review-panel`, `verifier`) are coordinator prompts for the Orchestra pattern, selected per
invocation with `--recipe <id>` rather than started as long-lived agents:

```sh
gitmoot agent template show review-panel
gitmoot orchestrate project-planner "Review PR #123 in this repo." --repo owner/repo --recipe review-panel
gitmoot orchestrate project-planner "Produce the export-feature migration plan and prove it is complete." --repo owner/repo --recipe verifier
```

For fast current-chat planning, use the Gitmoot skill with the same packaged
`agent-templates/planner.md` instructions instead of starting a background job:

```text
Use the Gitmoot planner here. Write the implementation plan.
```

The current chat can also import any installed agent or template prompt:

```sh
gitmoot agent prompt frontend-reviewer
gitmoot agent prompt frontend-reviewer --json
```

This prints the prompt content for the current chat to apply locally. It does
not create a job, start a daemon, resume a runtime session, or post a PR
comment — a free read-only peek.

To track the here-method work by default, add `--record`: it opens a session
job on import (see "Session jobs" below) and returns the prompt with a header
line naming the job id, so the imported work shows in `job list` / the dashboard
once you clock out:

```sh
gitmoot agent prompt frontend-reviewer --record [--repo owner/repo] [--type ask|review|implement] [--json]
# prints:  [gitmoot session job <id> — when this work is complete, run:
#           gitmoot job close <id> --decision <approved|changes_requested|implemented|blocked|failed|skipped> --summary "..."]
# followed by the prompt body.
```

`--record` accepts either a **registered agent** or a bare **template id**:

- Registered agent: the repo comes from `--repo`, else the agent's `repo_scope`
  (error if neither is set); the session job records the agent name.
- Bare template (no agent of that name registered, e.g. the packaged `planner`):
  `--repo owner/repo` is **required** — a template has no `repo_scope` to fall
  back on — and the session job records the **template id** as its agent identity
  (#673). The repo must be tracked (`gitmoot repo add owner/repo` first).

`--type` defaults to `implement`. When the imported work is done, close the job
with `gitmoot job close <id> --decision …`. `--json` includes the opened
`job_id`. Without `--record`, behavior is unchanged (no job).

## Organization registry and scoped dispatch

Organization mode is opt-in. Initialize a starter registry and verify the
required Herdr provider (`>=0.7.5`) with:

```sh
gitmoot org init
gitmoot org brief [--role owner] [--json]
gitmoot org chart [--json]
gitmoot org status [--json]
```

`gitmoot org validate [--home PATH]` validates the optional local `[org]`
registry against the live Herdr snapshot and event-rule store. It fails when a
role has no live pane, has no enabled wake route, or a labeled live pane is not
claimed by any role; each failure includes category counts and a reason.
`org brief --role` defaults to `GITMOOT_ORG_ROLE`; an explicit flag wins.
`gitmoot org show [--home PATH]` prints the resolved role table.

`gitmoot org seat add <name> [--pane ID_OR_LABEL] [--parent ROLE]
[--scope REPO,...] [--merge-rule owner|self|none] [--home DIR]` creates or
repairs a role and installs addressed `reply`, `review-verdict`, `blocked`,
`directive`, `escalation`, and `fact` routes with stable IDs
`org-seat-<name>-<kind>`.

With `--pane`, Gitmoot resolves a literal live pane id first, then a unique
exact live label, and stores the resolved pane id. Existing label-based commands
therefore keep working; re-running one for the same resolved pane canonicalizes
that existing binding to the pane id. Later cosmetic label changes do not
retarget or break bindings written by this command. Duplicate labels, unknown
references, and a supplied but empty `--pane` hard-fail before any role or route
is written.
Omit `--pane` to create an unbound role and the same routes; re-run the command
with `--pane ID_OR_LABEL` to bind that role later. If a configured binding stops
resolving, for example after Herdr recreates a pane with a new id, the same
command can rebind the role to a live unclaimed pane while preserving its policy
and routes. A configured binding that still resolves is immutable. An ambiguous
label binding must be disambiguated in Herdr before rebinding. Every successful
add validates the affected role and six routes. Bound creation and repair also
validate the live binding without making unrelated intentionally unbound roles
decide the exit code. Unbound creation reports the deferred bind command and an
`ok role NAME unbound enabled_routes=6` verdict. The global `org validate` command
continues to report every unbound role until it is attached.

For a new non-owner seat, the acting role comes from `GITMOOT_ORG_ROLE` and
falls back to `owner` only when the variable is unset. The new seat inherits
that role as its parent and copies its scope. `--parent` must name an existing
role and cannot create a cycle. An explicit `--scope` must stay within both the
acting role's scope and the selected parent's scope. Merge authority defaults
to empty; an explicit `--merge-rule` cannot exceed the acting role's authority.
The empty-registry `owner` bootstrap keeps its `*` scope and `owner` merge rule.
The three policy flags initialize new seats only. An existing role accepts an
explicit policy value only when it matches the stored value; `--scope` matches
as an unordered set, and a changed value is rejected instead of being silently
ignored. An invalid `--merge-rule` is always rejected as invalid, on new and
existing roles alike. Re-running with matching values or without those flags
repairs missing owned pieces, fills an empty pane binding, canonicalizes a
matching label binding, or replaces a non-empty binding only when its former
target no longer resolves. It does not rewrite existing policy or duplicate
routes.

`gitmoot org seat rm <name> [--force] [--home DIR]` resolves the role's live pane
when it has a binding and checks every distinct Git checkout reported by that
pane's `cwd` and `foreground_cwd`. It refuses a dirty checkout or a branch whose
`HEAD` is not merged into the locally known `origin/HEAD` (falling back to
`origin/main`); unreadable branch state also fails closed. A safe removal deletes
the role and all of its wake routes, closes a resolved live pane, and validates
that the role, routes, and closed pane are absent. A role with no configured
binding has no pane to inspect or close, so removal deletes only that role and
its routes. A configured binding that is stale, absent, or ambiguous fails
closed before mutation and reports the `org seat add` rebind command. A provider
error for a configured binding also fails closed. Roles that still parent
another role cannot be removed.

`--force` retires a seat whose configured pane will never resolve again, which
is otherwise unreachable: rebinding is impossible when the pane is gone, so the
role and its wake routes cannot be deleted at all. Per #1175 an unbound role is
a defect with two remedies, bind it or remove it, and `--force` is the second
one. It applies ONLY to an unresolved binding, and the removal prints
`pane check skipped under --force` with the reason the binding did not resolve.
A stale id may still name a LIVE pane holding work that the check cannot
inspect, so forcing a stale binding asserts that pane is gone. An ambiguous
label warning names every matching live pane ID; removal does not guess which
one belongs to the role and therefore closes none of them. Inspect or close
those panes separately before forcing the removal. `--force` does not weaken
anything else: a pane that DOES resolve is still refused when its branch check
fails, and a provider error still fails closed rather than being forced through.

The six provisioned routes are enabled, addressed, and have an empty match
filter. Remove one by its stable ID with `org events rule rm` to quiet that kind;
this is destructive and re-running `seat add` recreates it. There is currently
no non-destructive event-rule disable verb.

After upgrading an existing installation, re-run `org seat add <role>` for each
seat to provision default routes introduced by the new release, including
`review-verdict`. The repair preserves the seat's existing policy and routes.

The registry uses `[org] enforce = "warn"|"block"` and
`[org.roles."name"]` entries with `parent`, `scope`, `merge_rule`, an optional
cosmetic `display_name`, an optional `model` runtime pin, an optional per-role
`recycle_after` duration override, and an optional `pane` Herdr binding (used by
live presence and org event-rule wakes).
For backward compatibility, a configured binding resolves as a literal pane id
or a unique exact live label. A literal id tracks one pane; a label tracks
whichever current pane uniquely carries that cosmetic value. `org seat add`
canonicalizes new bindings to ids. Roles without a binding report unknown live
presence, and event wakes for them are skipped with an observable log and
increment the role's missed-wake counter rather than being inferred from a pane
label. There is
exactly one root named `owner`; accepted scopes are `*`, `owner/*`, and
`owner/repo`, and each child scope must be covered by its parent. Malformed
org configuration fails closed and loudly. `brief` records passive last-seen
presence for its role and can render static context with provider state
`unknown` during an outage; `chart` and `status` require a live compatible
Herdr snapshot. When configured, `brief --json` and `status --json` include the
role's `pane` binding. `chart` and `status` also show a `⚠ flagged (N missed
wakes)` marker once a role reaches the positive
`[orchestrate].max_consecutive_missed_wakes` threshold; their JSON rows expose
`missed_wakes`, `flagged`, and `flag_reason`. The threshold defaults to `0`, so
flagging is off. A missed-wake row more than 24 hours old is omitted from this
flag calculation; its stored consecutive counter remains unchanged for the
next real delivery attempt. `status --json` also exposes `active_jobs`, the live
queued-plus-running job count attributed to the role through `ActingOrgRole`
(#1057); it is distinct from daily or historical job counts. Escalations can be
resolved with `gitmoot org escalate resolve`; correlation beyond the optional
`--note` link remains deferred to #1058.

For Claude-runtime jobs attributed with `ActingOrgRole`, an explicit provider
weekly-quota rejection marks that role `unavailable` until the provider's
stated reset time. `org status` prints `⚠ UNAVAILABLE`, `reason=quota`, and the
UTC reset instant in the role detail; `org chart` appends the same warning, and
their JSON rows expose `provider_state: "unavailable"`,
`unavailable_reason`, and `unavailable_until`. Enforcement is scoped to the runtime that hit the
wall (#1641): new operator dispatches to the role are refused, and already-queued
jobs for it stay held, only when the job's selected runtime is the walled one, so
a Claude wall never blocks the role's Codex or Kimi work. A per-job `--runtime`
override decides this, in both directions. An incident with no recorded runtime
(written before per-runtime attribution) still holds the whole role, and an
unrecognized recorded or selected runtime refuses rather than dispatches. The incident sends
one best-effort direct wake to the role's configured parent, then clears at the
reset instant or on that role's first subsequent successful Claude-runtime job,
whichever happens first. Success on another runtime cannot clear the Claude
wall. If Claude supplies no parseable reset, Gitmoot uses the existing bounded
15-minute quota fallback. Codex and Kimi quota-message detection are not part of
this phase.

Archived seats (herdrup#173, #1635): when Herdr archives an agent
(`herdr agent archive`), the daemon's org lane observes the `archived` block on
`herdr agent list` each tick and drops that seat from `org chart`, `org status`,
health counts, sweeps, nudge ladders, and live presence — at one shared roster
choke-point, not per consumer. Herdr owns archive state; Gitmoot only reads it,
and a missing block means ACTIVE, so either side can deploy first. The seat's
open directives are parked (nudge ladders suspended — not marked done, which
would assert a deliverable exists, and not cancelled, which would discard the
obligation) and return to the live sweep with fresh TTL anchors when an
unarchive is observed. If Herdr is unreachable the last observation holds —
exclusions are preserved rather than expiring toward inclusion — and
`gitmoot doctor` warns once archived seats are held on a stale observation
(default 15 minutes).

The read-only Org page consumes `GET /api/org` for the store-backed role tree,
health strip, typed escalations, and current signal feed, plus
`GET /api/org/role/{name}` for one role's identity, presence, recycle history,
and today's job counts. These endpoints open SQLite read-only and never contact
Herdr. Responses are cached for at most 15 seconds; `data_as_of` is the newest
persisted source timestamp, not the request time. `detection_enabled` is true
only when `blocked_role_wake_after` is positive and at least one org event rule
is enabled; otherwise `detection_hint` explains why an empty signal feed is not
evidence that every role is healthy. The enabled blocked-role evaluator also
persists its latest Herdr snapshot for these endpoints. Only observations from
the last five minutes are rendered as `blocked`, `working`, or `idle`; stale,
missing, `done`, and `unknown` observations render as `never-seen`. An active
provider quota incident renders separately as `unavailable`, including its
reason and reset boundary in `presence_detail`; it is never collapsed into
`never-seen`.

**Session lifecycle (phase 3).** `[org] recycle_after = "24h"` (a duration,
per-role overridable via `[org.roles."name"] recycle_after`) marks a role
recycle-overdue once it has been idle at least that long — surfaced read-only in
the `recycle` column of `org status` (`off | fresh | eligible | overdue`).
`[org] recycle_enforce = "off" | "warn" | "block"` (default `off`) then, for a
role past its `recycle_after`, either refuses (`block`) new `--org-role`
dispatches with an actionable error or logs a one-line advisory (`warn`) — the
"overgrown sessions become impossible" economics; journaling a handoff note is
never blocked, so an overdue role can always hand off. `recycle_enforce` needs a
configured `recycle_after` to have any effect. Both fields are **binary-first**:
a binary predating them fails closed on a config that uses them, so deploy the
binary before any config sets them. When `recycle_enforce` is not `off`, an
overdue refusal or warning also emits a repeating (once per `recycle_after`)
`org.recycle_overdue` event through the org event sink; route it to a wake with
`org events rule add --on recycle-overdue --wake <role>`. Notification delivery
is best-effort — reliable from a foreground `agent ask`, but a short-lived
`--background`/`orchestrate` dispatch may exit before the wake fires.

`gitmoot org recycle <role> --kind <kind> --handoff "<note>" [--pane <id>]
[--json] [--home <dir>]` journals a typed handoff in the role-lifecycle workflow
`org/<role>`, builds the successor's boot prompt from `org brief` plus that
handoff, and starts the requested Herdr agent kind in `--pane` or the role's
configured `pane`. A pane binding and non-empty handoff are required. For safety,
recycle does not kill or send exit keys to the old agent: the pane must already
be at its interactive shell prompt. The Herdr start wait is bounded to 30
seconds; a failed start leaves the durable handoff note available for recovery.
When a role configures `model`, recycle passes `--model <value>` to the successor
only for the verified Herdr kinds `codex`, `claude`, and `kimi`; other accepted
`--kind` values silently ignore the pin without an error or warning. Deploy this
binary before adding the fail-closed field to config. Brief and chart surface
the configured pin, but live-vs-pinned drift detection awaits a running-model
signal from Herdr.

Fresh local `agent ask`, `agent run`, `agent review`, and `orchestrate`
dispatches accept `--org-role <name>` (or the
narrow `GITMOOT_ORG_ROLE` fallback). The role is validated and touched before
dispatch, stored as `acting_org_role` in the job payload for provenance, and
its scope is enforced at enqueue. `[org] enforce = "block"` rejects violations;
`"warn"` queues and records an `org_scope_violation` event.

`gitmoot org escalate --to <role> --workflow <label> [--org-role
<from-role>] [--repo <owner/repo>] "<question>"` records an escalation as a
workflow journal note. The acting role comes from `--org-role` (which takes
precedence) or `GITMOOT_ORG_ROLE`; it must be a configured role. An ancestor
target preserves the upward escalation behavior; a descendant target records a
downward ask. Both directions use the same typed schema
`[org:escalate to=<to> from=<from> wf=<workflow>] <question>`, set the from-role
as author, and can be rendered as JSON with `--json`. The same role is invalid.
Peer questions are refused by a safe command-level default because Gitmoot has
no configurable peer-question policy. This formalizes the earlier ad-hoc
practice of typing organization questions into notes or panes; there is no
code-level marker to migrate. The note and a `pending` wake outbox row commit
atomically. With an opt-in `reply` rule, a daemon tick wakes the addressed role
through its configured Herdr pane.

`gitmoot org message send --to <role> --workflow <label> [--org-role
<from-role>] [--repo <owner/repo>] [--json] "<message>"` records a durable
sender-attributed heads-up between two distinct configured roles. The roles may
message each other if and only if their non-empty `parent` values are equal.
Repository scope does not grant this channel, and `owner` has no special case.
The typed note
`[org:message to=<to> from=<from> wf=<workflow>] <message>` and its addressed
`reply:<role>` wake row commit atomically. The wake includes the exact
`gitmoot workflow show-note <id>` retrieval command; that command renders the
citable row's workflow, author, optional repository, timestamp, and body, or
returns the row as JSON. **Plain output prints the whole body**: a single-note
view is the one place a body must not be cut, and the 512-rune line cap applies
to timeline and list lines instead. Control characters and ANSI escapes are
still scrubbed from plain output; JSON preserves the stored body byte for byte.
Messages create no directive, acknowledgment, completion, TTL, or nag
obligation.

`gitmoot org escalate resolve <escalation-note-id> [--by <role>] [--note
<answer-note-id>] [--home <dir>]` appends a typed resolution marker to the same
workflow journal. `--by` defaults to the escalation's target role, and `--note`
optionally links the workflow note containing the answer. The resolution marker
is addressed to the escalation's parsed asker and atomically records a pending
reply wake-outbox row, so an opt-in `reply` rule wakes the asker. A legacy typed
escalation with no identifiable asker still resolves, prints a warning, and
records no invented target. Resolved escalations are omitted from org dashboard
projections while the original journal entry remains intact.

`gitmoot org directive send --to <role> --workflow <label> (--stdin | -F
<file> | <text>) [--home <dir>]` records a typed, downward-only assignment from
the acting `GITMOOT_ORG_ROLE`. The sender must be an ancestor of the target;
peer, upward, and same-role directives fail closed. The note and its pending
`directive:<role>` wake obligation commit atomically, separately from reply
coalescing. A directive rule wakes the target with the directive note ID and
exact acknowledgment command. With no matching directive rule, the note and
pending outbox row remain durable and the drain stays config-inert.

`gitmoot org directive ack <id> [--by <role>] [--home <dir>]` is restricted to
the addressed target or one of its configured ancestors and records receipt,
not completion. `gitmoot org directive cancel <id> [--by <role>] [--home
<dir>]` is restricted to the sender. Both commands require an acting identity
from `--by` or `GITMOOT_ORG_ROLE`; missing identity fails closed. They append
typed markers to the directive's workflow journal.

Receipt is normally recorded by the **transport**, not by the seat. When Herdr
confirms that a directive prompt landed in the addressed role's pane, Gitmoot
appends an `[org:directive-delivered id=<id> to=<role>]` marker itself, and
that marker satisfies the receipt obligation everywhere an `[org:directive-ack ]`
marker does: the acknowledgment ladder stops and the completion phase starts
from the delivery time. The delivered marker is a separate verb from `ack` and
carries `to=` rather than `by=`, because an acknowledgment is the seat's own
assertion and a machine must not write one on its behalf. Nothing in the
delivered marker claims the seat read the directive.

`gitmoot org directive ack` therefore remains available but is no longer part
of the delivery path, and the first-delivery prompt no longer asks for it. The
receipt write is best-effort: if it fails, the wake still counts as delivered
and the directive simply takes another acknowledgment-phase nudge, which is the
behaviour that predates this change.

**A remaining acknowledgment-phase nudge now means "delivery is not proven".**
A stalled, failed or `delivery_unknown` wake records no receipt, so the ladder
keeps running for exactly the directives whose arrival nobody can demonstrate.

**The first-delivery prompt carries the directive itself**, not a pointer to
its row: `gitmoot directive <id> for <role>: <directive text> -- record
completion with: gitmoot org directive done <id> --by <role>`. The carried text
is the body with its `[org:directive to=… from=… wf=…]` marker header stripped,
since the prompt already states the addressee. A body over 4,000 characters is
cut at a **word boundary** and states exactly how much was omitted plus the
command that returns the rest, for example `[3126 of 7126 characters omitted;
read the whole directive with: gitmoot workflow show-note 42 --json]`. Nothing
stops mid-clause without saying so.

`gitmoot org directive done <id> [--by <role>] [--home <dir>]` records
COMPLETION and ends the obligation, including its TTL nudges. **Completion
authority is the target subtree**: the addressed role, or a role below it in the
chart — someone who plausibly did the work.

**Ancestors cannot complete, and that exclusion is the point.** `send` requires
the sender to be an ancestor of the target, so permitting ancestors would permit
the sender under another name: a role could issue a directive and then certify
its own work as done. Ancestors are not stranded — they hold `cancel`, and the
two verbs assert different things. **Completion says the work happened;
cancellation says it is no longer needed.**

So the three verbs carry three different disciplines: `ack` is the target or an
ancestor, `cancel` is the sender, and `done` is the target subtree.

A completed directive stops being outstanding even when no acknowledgment was
ever recorded, since completion is strictly stronger than receipt.

The directive TTL checker nudges each phase on its own finite ladder. The
acknowledgment phase counts with `directive_nudge_count` and the completion
phase with `directive_done_nudge_count`, because the first counter is cumulative
and never resets at acknowledgment, so one counter cannot bound both phases.
Each phase caps at `directive_max_nudges`, emits **one** terminal escalation
naming which obligation went unmet, and then stamps `directive_exhausted_at`.

Exhaustion is terminal and leaves two records: `directive_exhausted_at` on the
directive row for the evaluator's own reads, and an `[org:directive-exhausted ]`
**marker note** in the directive's workflow journal. The marker is what makes the
terminal state discoverable — ack, cancel and done are all marker notes, and the
journal is what operator-facing readers consume, so a column-only terminal state
would have been invisible to exactly the person who needs it.

The stamp is **completion-phase only**. An acknowledgment ladder that exhausts
does not terminate the directive: a late acknowledgment starts a fresh completion
ladder, and the acknowledgment phase's own terminal condition is its counter
reaching the cap. Both the evaluator and the atomic completion claim refuse an
exhausted row, so a racing sweep cannot walk the terminal state back.

The sweep window is sized from the count of currently open obligations rather
than a fixed oldest-N, so a backlog of long-lived directives can no longer starve
newer ones out of the window. TTL nags are delivered through the durable wake
outbox with coalescing, like blocked and escalation wakes, rather than one live
prompt per due directive per sweep.

A completion nudge is **not delivered to a seat that is working**. The nudge
says "acknowledged but incomplete; finish the assigned deliverable", and an
inbound pane message ends the turn in flight, so nagging a working seat
terminates the attempt to finish. The sweep reads the persisted live-pane
observation: a `working` state observed within the last five minutes defers the
nudge without spending a ladder step, and the nudge fires on the first sweep
after the seat stops or is superseded by the completion receipt arriving. Any
other state, including the `unknown` a closed pane reports, and any staler
observation, deliver the nudge exactly as before.

The deferral is bounded. Past `anchor + directive_done_ttl * (directive_max_nudges + 1)`,
the point at which the whole ladder could have run, Gitmoot escalates to the
sender's current parent instead of interrupting the seat, stamps
`directive_exhausted_at` and records the marker note. A seat that reports
`working` forever therefore cannot hold an unmet obligation in silence, and it
is still never interrupted. The acknowledgment phase is unaffected: a receipt
request is not a nag about unfinished work.

The daemon evaluates directive TTLs on the existing one-minute org supervision
lane. `[org].directive_ack_ttl` defaults to `10m`,
`[org].directive_done_ttl` defaults to `0s` (completion nudges off), and
`[org].directive_max_nudges` defaults to `3`. An unacknowledged directive is
re-woken at most once per ack-TTL interval. When its persisted nudge count
reaches the maximum, Gitmoot emits an escalation addressed to the sender's
current parent in the org chart. Acknowledgment stops ack nudges; an acknowledged
but unfinished directive is evaluated only when its completion TTL is enabled.
An internal per-directive completion-TTL override, when present, takes
precedence over the global value. Typed completion or cancellation receipts
close the evaluator obligation. With no enabled org event rules, the lane is
config-inert: it performs no directive scan and emits nothing.

`gitmoot org await review --repo <owner/repo> --pr <number> --head <sha> --ttl
<duration> [--role <role>] [--home <dir>]` registers a bounded durable interest
in a review verdict. `--role` defaults to `GITMOOT_ORG_ROLE`; the role must exist
in the organization chart, `--ttl` is mandatory, and the exact head SHA is part
of the canonical subject key. Registration inserts the interest and rechecks
already-committed review jobs in one transaction, so a verdict that lands while
registration is starting is not missed. A later terminal review-job commit
satisfies only the matching repository, PR, and head, then writes an addressed
`fact:<role>` wake obligation.

`gitmoot org await list [--role <role>] [--state waiting|satisfied|expired]
[--json] [--home <dir>]` shows live and terminal subscriptions. The existing
one-minute org supervision lane expires overdue waits, retains the row as a
queryable `expired` terminal state, and addresses the expiry to the waiter's
current parent (or the waiter itself for a root role). If the waiter role was
removed from the chart, expiry still becomes terminal and the wake retains the
removed role as its exact address, leaving delivery failure observable instead
of making the wait immortal. Fact wakes are delivery only: they require a `fact`
event rule but create no acknowledgment or completion ceremony.

`gitmoot org interrupts [--window 24h|7d|0] [--json] [--home <dir>]` reports
**how often each seat is interrupted** (#1983). Per seat, over the window:
wakes, wakes per day, median gap, share of gaps under five minutes, a breakdown
by source (`workflow_note`, `escalation`, `blocked`, `awaited_fact`), delivered
versus unproven, wakes **collapsed** by coalescing, pending wakes with **no
enabled route** and the oldest such timestamp, and completion nags recorded
against the seat's directives. `--window` takes a Go duration, `<n>d`, or
`0`/`all`; default `7d`.

A collapsed row counts as an interrupt that did NOT happen, never as a wake, so
the coalescing saving is readable here rather than by hand-written SQL. A
`pending` row whose kind and role have no enabled rule is an obligation waiting
on configuration rather than on a tick.

A pending row whose kind and role have no enabled rule additionally records a
`wake_unroutable` job event once per row, naming role, kind, source and whether
the route was removed (a retired seat) or never configured (a gap a route would
close), so `NO ROUTE` here has a durable, queryable counterpart.

Event-rule wakes are separately opt-in:

```sh
gitmoot org events rule add --on attention --match owner/repo --wake maintainer
gitmoot org events rule add --on review-verdict --match owner/repo --wake maintainer
gitmoot org events rule add --on blocked --repo tendwire --wake maintainer
gitmoot org events rule add --on pane_input_pending --wake maintainer
gitmoot org events rule add --on reply --wake maintainer
gitmoot org events rule add --on directive --wake maintainer
gitmoot org events rule add --on fact --wake maintainer
gitmoot org events rule add --on reply --wake operator --scope observer
gitmoot org events rule list
gitmoot org events rule set-scope --home /alternate/home <rule-id> observer
gitmoot org events rule rm --home /alternate/home <rule-id>
```

`--on` accepts `escalation`, `attention`, `guard`, `job-terminal`,
`review-verdict`, `blocked`, `recycle-overdue`, `pane_input_pending`, `reply`,
`directive`, or `fact`. A successful review terminal whose decision is
`approved` or `changes_requested` matches both `job-terminal` and
`review-verdict` and addresses both the requesting org role and the resolved
implementing role. When a successful ownership lookup finds no implement job
or branch lock attribution, the review's persisted lead identifies a
persistent implementing seat; lookup errors remain fail-closed. If neither
role can be resolved, addressed rules fail closed while observer rules remain
eligible. `pane_input_pending` matches the
`org.input_pending` event emitted when Herdr continuously reports
`input_pending: true` for a role's pane longer than
`[orchestrate].blocked_role_wake_after`; it re-nudges at most once per that
interval while the dialog remains pending. The pending signal takes precedence
over the pane's last `idle` or `working` activity status.
`reply`, `blocked`, `escalation`, and `fact` wakes use the durable wake outbox.
`reply` matches workflow notes addressed to the same role as `--wake`. Reply
rows commit atomically with their source note.
Blocked and escalation rows are persisted synchronously by the event sink after
the source transition; an insert failure is logged but cannot roll back the
emitting job. The daemon holds each pending group for `[org].wake_coalesce_hold`
(default `5m`) after its
oldest row, then delivers every due pending row for that event kind and role as
one wake, bounded at ten rows per wake. Reply prompts carry `N new items,
oldest id X` and a retrieval command for each collapsed row; blocked and
escalation events retain their redacted event detail. The row the wake names
records the delivery outcome and each row it collapses is recorded `superseded`
with `coalesced into wake outbox row <id>`. Different event kinds never share a
coalescing key, and a later tick flushes a quiet tail without another event.
Rules default to `--scope addressed`: an addressed rule is eligible only when
the event names a target role and that role matches `--wake`.
`--scope observer` exempts a rule from the addressee gate, so observers receive
both directed events and events without a target; address-less events never
broadcast to addressed rules.
Durable-outbox claim authorization is scope-blind: among enabled,
filter-matching rules for the event's own kind, wake-role equality with the
addressed target is the only routing condition. An observer-scoped rule is
therefore delivered when its wake role equals the target; when it differs and
no other target-role rule authorizes the batch, the batch remains pending.
`set-scope` changes an existing rule between `addressed` and `observer`.
Upgrades preserve the existing global view by promoting non-reply rules with an
empty match filter to `observer`; reply rules remain `addressed` because reply
already carries a target role. `gitmoot doctor` warns when an event kind with a
production target-role writer has no enabled observer rule, including when the
rule set is empty.
Filtered non-reply rules remain `addressed` after upgrade and must be promoted
manually with `set-scope` when observer delivery is intended.
Every outbox row retains a queryable `pending`, `attempted`, `delivered`,
`stalled`, `failed`, or `delivery_unknown` state, so never-attempted is not
confused with success and outstanding rows contribute to daemon tick health.
Repository comparison for a slash-bearing owner/repo filter is
case-insensitive and exact. Job IDs always use case-insensitive substring
matching, including slash-bearing delegation IDs. Without a slash, repositories
also use substring matching; omit either flag to match every event of that kind.
Pass only one of `--match` and `--repo`. `--wake` must name a declared
role whose config sets `pane = "<pane-id-or-label>"`. Gitmoot
first resolves the value as an exact pane label and otherwise uses it as a
literal pane id. The daemon calls `herdr agent prompt <pane> <text> --wait --timeout
8000` and treats delivered (`result.type = "agent_prompted"`, or a post-delivery
`error.code = "timeout"`) apart from stalled (`error.code =
"agent_prompt_stalled"`). Stalls increment the role's consecutive missed-wake
counter and delivery resets it; transport failures leave it unchanged. A stall
and an `agent_blocked` pane are **transient**: the claimed rows return to
`pending` with the cause recorded and are re-delivered as one coalesced wake,
bounded at three attempts. Any other cause, and an exhausted budget, end the
rows terminally and record a `wake_delivery_failed` job event on
`wake-outbox:<id>` naming the role, cause and attempts.
An aged row whose delivery the store can PROVE is recorded `delivered` with a
`wake_delivered` job event carrying `policy=resolved_by_destination_evidence`,
rather than `delivery_unknown`. The proof is a note in the directive's own
workflow acknowledging it (`[org:directive-ack id=<id> ...]`) or recording its
delivery (`[org:directive-delivered id=<id> ...]`), naming that row's directive;
a reply-class row has no equivalent marker and keeps the unknown outcome.
`attention`, `guard`, `job-terminal`, `review-verdict`, `recycle-overdue`, and
`pane_input_pending` wakes remain best-effort. With no rule rows this path is
off. Task episodes due in one evaluator pass produce one oldest-first digest
event; blocked roles retain one event per role.

## External-coordinator workflow groups

Attach a global workflow label to work started outside Gitmoot's own task
coordinator. Labels are lowercase slugs up to 64 characters. They may contain
one `/` to split a namespace from a campaign; each side uses lowercase letters,
digits, and single hyphens with no leading or trailing hyphen. The label is
accepted by `agent ask`, `agent run`, `agent review`,
`orchestrate`, and `job open`; delegation children and every coordinator
continuation inherit it.

### Require workflow labels

`require_workflow` defaults to `true`. In the default `auto` mode, fresh
unlabeled agent dispatches are filed as `adhoc/<agent>-<yyyy-mm-dd>` and emit a
`workflow_autolabeled` event; they are never rejected. Set `[workflow]
require_workflow = false` to opt a repository out. To reject unlabeled
dispatches, ensure the applicable global or repository policy explicitly sets
`require_workflow = true`, then set `require_workflow_mode = "strict"`; the
required fix is `--workflow <namespace>/<campaign>`. Mode-only legacy
configurations remain in `auto`. Both settings can be overridden per repository
in `[repos."owner/repo"]`. GitHub comment dispatches
always take the auto-label path in either mode so acknowledgement ordering stays
unchanged; engine PR reactions inherit their initiating dispatch's label instead.
`gitmoot doctor` always reports unlabeled-job drift as advisory diagnostics
(including session-open and task-recover rows that bypass enforcement), while
the overview shows that item only for repositories where the policy is enabled.
`gitmoot repo add --agents-md` writes the recommended AGENTS.md discipline
section.

With `require_workflow = false`, dispatch and enqueue remain byte-identical;
doctor drift diagnostics remain always-on advisory, and the overview item
remains policy-gated.

```sh
gitmoot orchestrate planner "Coordinate the dashboard wave." --repo owner/repo --workflow fable/dashboard-redesign
gitmoot job list --workflow fable/dashboard-redesign
gitmoot workflow list
gitmoot workflow show fable/dashboard-redesign --limit 100
gitmoot workflow describe fable/dashboard-redesign "Coordinate and ship the dashboard redesign."
gitmoot workflow note fable/dashboard-redesign "Implementation started." --author operator --status active
gitmoot workflow show-note 42
gitmoot workflow close fable/dashboard-redesign --reason "Shipped and verified."
```

`workflow list` reports per-state counts, note count, first/last activity, and
best-effort token totals. Its JSON summary also includes the acknowledgment
timestamps `last_failure_at`, `last_human_note_at`, and
`last_merged_receipt_at`. `workflow show` merges jobs and notes chronologically.
By default it keeps the newest 100 entries and displays that window oldest to
newest; pass `--limit 0` for the complete timeline or a larger `--limit N` for
a wider window. When rows are omitted, text mode prints the shown and total
counts plus that guidance to stderr, while JSON includes `"truncated": true`.
The read-only web dashboard shows labels as Galaxy hubs and provides a Workflows
index plus a mission-log detail at `/workflows/<label>`. Workflows are `active`
while queued/running, `recent` when no work is live but activity occurred within
30 minutes, `stalled` when failed/blocked with an unacknowledged failure (newer
than any non-daemon journal note and any merged-PR daemon receipt; other daemon
receipts never acknowledge) and quiet for 30 minutes to 24 hours, and `settled`
otherwise. A workflow whose status is `done` or `settled` leaves the active
bucket immediately regardless of note recency, unless it has queued/running
work. The optional
`--pane`, `--session`, and `--workdir` note flags persist the latest coordinator
handoff shown on that page. When those flags are omitted inside Herdr
(`HERDR_SOCKET_PATH` or `HERDR_ENV=1`), `workflow note` reads the current pane
label, full runtime session UUID, and working directory automatically. Explicit
flags always win; `--no-auto` skips detection for scripted callers. Detection
is fail-open and never prevents a note, and it does not infer `--author`.
Dashboard resume commands require a full UUID; legacy short session values stay
visible in the workflow index as context but are not rendered into a broken
command. Author defaults to the newest note author.
`workflow note --repo <owner/repo>` records the note's repo COLUMN and nothing
else - it opts the note into no other behaviour. Malformed input is refused with
exit 2 rather than stored as a column nothing matches. Prefer setting it on an
operating-mode or reconciliation note: the workload-mode gate reads those
through two bounded windows, one repo-scoped and one for the repo-less rows, so
a scoped note cannot be crowded out of the window by other repositories' notes.

Each workflow has a stable `description` and live `status`, both shown by
`workflow show`. Description is seeded automatically from a referenced local
issue title, else the first kickoff-note sentence, else the label campaign.
`workflow describe <label> "<text>"` overrides it. Legacy `workflow note
--summary` remains an alias for description and also mirrors the value into the
retained summary field for older clients. `workflow note --status "..."` is the
manual status control and accepts only `active`, `blocked`, `ready_to_merge`,
`done`, `settled`, or `parked` (plus an explicit empty value to unset it).
Put free-text detail in the note body. Existing legacy status strings remain
readable but cannot be written anew. Each metadata field is limited to 300
bytes.

`workflow close <label> [--reason "..."]` refuses workflows with queued/running
jobs, appends a typed `[workflow:close]` journal note, and sets status to `done`
atomically. Repeating close on a `done` or `settled` workflow is an idempotent
success without another close note. A later note without an explicit
`--status` records a preceding `[auto:workflow:reopened]` receipt and returns
the workflow to `active`; an explicit status remains authoritative.

The daemon conservatively auto-settles a workflow only when it references at
least one PR, every referenced PR is locally known as merged or closed, no job
is queued or running, its status is not `blocked`/`parked`/`done`/`settled`
(deliberate human-set states are never auto-settled), and the latest human note
or job update has been quiet for `[workflow].auto_settle_after` (default `24h`;
set `"0"` to disable). Daemon receipts do not extend the quiet period.
Auto-settle appends an `[auto:workflow:settled]` note, sets status to `settled`,
never deletes data, and any later note revives the workflow. Two edges are
reversible-by-note rather than auto-revived: a task paused at `awaiting_human`,
and a PR reopened after auto-settle — post a workflow note to revive it.

For a workflow-linked PR, the daemon adds a structured `[auto:pr:...]` note as
author `daemon` and advances status when the PR opens, checks turn green, and it
merges or closes without merging. The structured workflow/PR/transition key
deduplicates repeated polls; these system breadcrumbs remain distinct from
coordinator handoff notes and never overwrite description.
Labels may be reused; timestamps expose the reuse. `workflow note` stores body
and author verbatim (10 KiB and 128-byte limits) and rejects labels with no jobs.
`workflow show --json` returns those verbatim bytes; plain-text output strips
terminal escape sequences, maps control characters to spaces (except tabs), and
caps each rendered field to one bounded line.

## Tasks

Inspect and retire task state. Gitmoot no longer STARTS a task: `task run` went
with implementer dispatch (#2203), and `task recover` / `task resume-work` went
with it because both existed only to restart or re-enter a dispatched implement
job. Review dispatch still mints the `review-pr-<n>-<hash>` task identity, and a
`planned` task is the plan a SEAT picks up in its own checkout, recording the
work against the task id, which is how 568 of the last 570 implement rows were
already produced:

```sh
gitmoot task list --repo owner/repo
gitmoot task list --repo owner/repo --state implementing --json
gitmoot task list --repo owner/repo --state stranded --json
gitmoot task events task-001 --json
gitmoot job record --agent lead --repo owner/repo --type implement --task task-001 \
  --decision implemented --summary "What changed and why."
```

`task list` and `task events` are the whole surviving verb set;
`gitmoot task --help` prints exactly those two.

Dismissal is automatic; no command dismisses a task. The daemon's stale sweep
preserves the branch and worktree and releases the branch lock best-effort.

Past `[workflow].stale_task_ttl`, blocked tasks are disposed by forge evidence:
own merged PR (`merged`), later merged work on the same referenced issue
(`superseded`), closed subject (`superseded`), then the bounded `stranded`
fallback. The JSON list includes `disposal_tier`, `disposal_reason`,
`disposed_at`, and `disposal_escalation_role`. The pass never uses branch
ancestry, and an open `awaiting_human_merge` PR is not inferred complete.
Blocked-task alerts have their own finite ladder: after three interval-spaced
nudges Gitmoot emits one terminal escalation and stops nagging. The task remains
queryable until the separate evidence-disposal pass transitions it.

`task events <id>` prints the append-only task lifecycle trail. Automatic stale
dismissals use `task_dismissed_auto`; opt-in never-started-plan retirement uses
`task_dismissed_planned_ttl`; restoring a dismissed task by retrying one of its
jobs uses `task_recovered_job_retry`. A clean closed-unmerged PR records
`pr_closed_unmerged` while moving `pr_open`, `reviewing`, or
`changes_requested` to `blocked`. Once advancement/delegation handling has no
live successor, an implemented top-level job with no attached PR first checks
for an existing open PR on the task branch. A match restores the task to
`pr_open` and records `task_terminal_pushed_to_open_pr`; otherwise the task is
blocked with `task_blocked_terminal_no_pr`, whose reason names the recoverable
branch and recorded head SHA when available. Other terminal outcomes remain
`task_blocked_job_failed`.

A dismissed task is not resurrected implicitly: branch or worktree allocation,
review continuation, and task-state advancement all leave it dismissed.
Retrying one of its jobs explicitly restores the task first and records
`task_recovered_job_retry`. There is no longer a `task recover` escape hatch,
because the dead-implementer case it finalized — a dispatched implement job
that edited a task worktree and exited before committing — can no longer occur:
Gitmoot does not dispatch that job. A seat that abandons its own work owns its
own worktree and finishes it with ordinary git, then records the outcome with
`gitmoot job record --type implement`.

If an existing task worktree has fallen off the resolved base lineage, Gitmoot
re-cuts it only when it is clean. When it also has uncommitted changes, the
delegation leg that would have used it preserves the worktree, moves the task to
`blocked`, and records `stale_worktree_dirty_blocked`; manually salvage, commit,
stash, or clean the changes before retrying. This is a read-only delegation
concern now: an implement leg cannot be allocated at all (#2203).

Planned-task retirement is separate from the default-on stale implementation
sweep. Set `[workflow].planned_ttl = "720h"` only when the repository explicitly
wants old never-started plans dismissed. It is off by default; unset, empty,
`"0"`, or invalid values disable it because dismissal can lose human planning
context that nothing else restores. Live jobs, open PRs, remote branches, and
uncertain remote checks prevent dismissal.

## PR Comments

Use GitHub PR comments as the public audit trail:

```text
/gitmoot help
/gitmoot status
/gitmoot <agent> review [instructions]
/gitmoot ask <agent> [question]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
/gitmoot resume <job-id> retry|continue|abort|answer [instructions]
@<agent> ask|review [instructions]
```

`implement` is NOT a PR-comment command. It parses as an unknown action and is
refused (#2203); on a pull request an unrecognized action is logged without a
reply, so nothing will answer it. Record implementation work with `gitmoot job
record --type implement` instead.

`/gitmoot merge` runs the policy gate even when automatic merge is disabled. It
does not weaken exact-head review: if a reviewed head needs a branch update, the
gate blocks without changing it. Update the branch explicitly, then obtain a new
review for the new head.

`/gitmoot merge` always posts the outcome it observed. A refusal names the
cause and remedy, including an acting role whose `merge_rule` denies the
operation, a repository ruleset that requires GitHub's merge queue, no approval
for the current head, or an approval bound to an ancestor head. When
`merge_gate.auto_merge = false`, the reply also states that no earlier
`gitmoot/merge-gate` marker was expected: absence of that marker is not evidence
that the gate passed. The explicit command still runs the gate.

A bare `@<agent> <action> …` mention on a PR comment (or, with the daemon's
`--watch-issues` flag, an issue comment) is treated as the same command as the
`/gitmoot <agent> <action>` form (#389). `/gitmoot resume <jobID>
retry|continue|abort|answer` resumes a delegation tree paused by
`escalate_human` or an ask-gate `human_questions` pause — see
`references/RESULT_CONTRACT.md` for the pause/resume semantics (`answer` is
PR-comment-only).

Before parsing, Gitmoot removes triple-backtick and tilde fenced code blocks and
closed inline backtick spans (including single-backtick spans).

**Symptom: my command in a bulleted list did nothing.** Any command line
indented by four spaces or a tab is treated as code and will not run, including
list-item continuation under a bullet or numbered item. For example, this
command is ignored:

```text
- Please run:

    @helper ask check the build
```

Put the command at column zero to run it:

```text
@helper ask check the build
```

GitHub must then
report that the comment author currently has `write`, `maintain`, or `admin`
repository permission; unauthorized addressed commands are rejected without
parsing their command text. Ordinary prose and code examples produce no reply.
An authorized malformed command still produces a visible routing error. An
unclosed fenced code block treats the remainder of the comment as code, so any
later command is ignored without a reply.

On a **pull request**, a line that is addressed by shape but names an action
Gitmoot does not implement produces **no reply** — it is recorded in the daemon
log instead. Source code reaches this path routinely, because a decorator or
attribute line such as `@Published private(set) var state = .uninitialized`
matches the `@<agent> <action>` mention form outside a code fence. A
*recognized* action given a bad argument (for example `@helper retry` with no
job id) still replies, so genuine command feedback is preserved.

On an **issue**, only `ask` is acted on, so any other action — recognized or
not — is dropped before dispatch with no reply **and no log entry**. When an
issue comment appears to have been ignored, the daemon log will not explain it.

If delivery of a stored result fails during workflow advancement, Gitmoot
records the job as `blocked` and the attributed result comment reports
`Decision: blocked`, even when the agent originally returned `implemented`.
The agent's summary remains visible, and the delivery failure appears under
`Diagnostics`. A downstream precondition such as pending CI does not replace a
successfully delivered job's terminal result.

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo   # add --json for machine-readable rows
gitmoot job list --killed            # only deliveries a signal killed, which nothing requeues
gitmoot job show <job-id>            # add --json for the full job + operational detail
gitmoot job watch <job-id>
gitmoot job watch <job-id> --transcript [--log-path <path>] [--runtime codex|claude|kimi|omp|shell]
gitmoot job transcript <job-id> --export md|jsonl [--output <path>] [--log-path <path>] [--runtime codex|claude|kimi|omp|shell]
gitmoot job transcript --all [--state succeeded,failed] [--since 720h] --export jsonl [--output <path>]
gitmoot job events <job-id>
gitmoot job retry <job-id>
gitmoot job cleanup list [--state pending|retryable|removed|quarantined] [--json]
gitmoot job cleanup reopen <resource-id>
gitmoot job gates <job-id>                                         # list resumable gates; add --json
gitmoot job gates clear <job-id> --need "<text>"|--all             # satisfy gate(s); auto-resume on last
gitmoot job cancel <job-id>                                        # one queued|running|blocked job
gitmoot job cancel --state blocked [--older-than 7d] [--repo owner/repo] [--agent name] [--yes]
gitmoot job kill <root-job-id>
gitmoot job answer <job-id> "<question-id>: answer text" [--json]  # resume a job paused at awaiting_human
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

A delivery killed by a signal records a `delivery_signal_killed` event naming
the signal and which evidence identified it. Both renderings count: a
direct-child runtime reports `signal: terminated`, while a wrapped CLI such as
claude or omp reports `exit status 143` and the signal NAME is lost, so a
consumer matching only the first would miss most review deaths. A timeout is
excluded twice over, by its own deadline error and by its `job_timeout` event,
because a timeout burned its whole wall rather than being abandoned.

`--killed` lists exactly that population. **Nothing requeues it, deliberately.**
Work consumed before a restart is never silently re-run: a killed delivery may
already have pushed a branch, posted a pull-request comment, taken a lock or
spent tokens, so re-running it trades a lost job for a double-executed one.
Inspect these rows and retry explicitly with `job retry`.

Terminal background `ask` and `review` jobs that ran in a throwaway read-only
worktree preserve a bounded `git status --short` plus `git diff HEAD` snapshot
before Gitmoot removes that worktree. `job show` prints the captured diff and
`job list` adds a compact `DIFF:` badge. Their JSON forms expose
`read_only_worktree_diff`, `read_only_worktree_diff_truncated`, and
`read_only_worktree_diff_error`; the same durable fields are present under
`job show --json`'s `payload`. Capture is capped at 4 MiB, and an oversized
snapshot ends with an explicit omitted-byte marker instead of being silently
cut. Git subprocesses and the wait for each index-file copy share a 10-second
context: on expiry Gitmoot kills the subprocess or stops waiting for the copy.
The operating system still owns cancellation of an in-progress filesystem
syscall, so this is a bounded-wait guarantee rather than a promise that kernel
I/O itself is cancelled. Failures are recorded and never prevent worktree
removal.

### Where a review's wall time went (`phase_profile`)

Every **review** job with a retained transcript emits one `phase_profile` job
event per ATTEMPT, visible in `gitmoot job events <job-id>`. Other job types are
deliberately untouched: the profile is appended after a job's terminal events,
so emitting it everywhere would change the observable event sequence of jobs
this measurement has no business affecting.

It answers "where did the wall time go" without a second measurement pass,
because the transcript records what ran and not when: the streams carry no
event timestamps, and replay deliberately refuses to invent elapsed time from
parser speed. The timing is therefore recorded live or not at all.

The message is one JSON object.

**TWO IDENTITIES, and the difference between them is the finding.** Commands can
run CONCURRENTLY, so their durations do not partition anything:

    covered_ms + residual_ms == wall_ms          (exact, up to ms rounding)
    sum(bucket_ms)           == covered_ms + overlap_ms

- `covered_ms` — wall time during which at least one command was in flight (the
  UNION of command intervals).
- `residual_ms` — wall time with **no command in flight**. Non-negative by
  construction, because it is `wall - covered` and never `wall - sum`.
- `overlap_ms` — how much command time ran concurrently. Reported, not absorbed:
  an earlier version subtracted the SUM and clamped a negative result to zero,
  which silently swallowed 24ms of a measured 76ms run and made the documented
  partition false exactly when overlap occurred.
- `bucket_ms` / `bucket_count` — each command's OWN measured time, classified as
  `test` (`go test`), `build` (`go build`, `vet`, `generate`, `gofmt`), `vcs`
  (`git`, `gh`), `mixed`, `unknown`, or `other`. Under overlap these sum to more
  than `covered_ms`, and `overlap_ms` is the difference.
- `commands` / `unpaired` / `tool_events` — shell commands measured; tool
  results whose call id was never seen (they contribute no time, and are
  reported so a stream this cannot follow stays visible); and non-shell tool
  results such as `file_change`, which are tool activity but not commands.
- `in_flight` — commands still running when the transcript closed. Their
  measured-so-far time IS counted: a review killed mid-`go test` otherwise
  reported that time as residual and looked idle when it was busiest.
- `id_collisions` — tool calls that arrived on an id already open. The FIRST
  call is kept; a non-zero value means the stream reused ids and some command
  text was not recorded.
- `dropped_bytes` — bytes discarded from an over-long unterminated line. The
  parser's pending buffer is capped so a runtime emitting one enormous line
  cannot grow daemon memory without bound; the loss is recorded rather than
  silent, and the retained transcript still holds every byte.
- `attempt` — the job's lifecycle generation. `job retry` preserves prior
  events and re-delivers the same job id, so one job id can hold several
  profiles; this field is the attempt boundary.
- `coverage` — **read this before the buckets.** `decomposed` means the runtime
  emits per-tool events (codex, kimi). `opaque_runtime` means it does not:
  Claude reports a single final envelope and no tool events, so an opaque row
  has zero commands however long the job took. An opaque profile is a blind
  spot, NOT a measurement that the job spent all its time outside commands -
  exclude those rows from any decomposition rather than averaging them in.

**`unknown` is a REFUSAL, not a leftover.** The classifier is a cheap lexer, not
a shell parser, and its supported grammar is declared in
`internal/cli/review_phase_grammar.go`: quoting (single literal, double with
backslash escapes), escapes, the sequencing operators and redirect forms,
comments, and wrapper prefixes. Anything outside it — command/parameter/
arithmetic substitution, backquotes, process substitution, subshells, arithmetic
commands, heredocs and herestrings, `;;`, an unterminated quote or a trailing
line continuation — yields `unknown` rather than a confident bucket. Its time is
still counted in `covered_ms`, so a refusal narrows the claim without losing the
measurement. Treat `other` as "understood, matched no phase" and `unknown` as
"not classifiable by this lexer"; averaging them together reintroduces exactly
the confidently-wrong attribution the boundary exists to prevent.

**The grammar is an ACCEPTOR with per-character provenance.** A command is
classified only if every token positively matches the declared shapes:
assignments (whose NAME characters are unquoted and form an identifier),
redirections (whose OPERATOR characters are unquoted, with a validated operand),
a plain command word, and arguments carrying no UNQUOTED `{ } * ? [ ] ~ < > | &`.
Quoting is tracked per character, because a quote around one fragment does not
disable expansion in the rest: `go test ./internal/"cli"*` still globs and
returns `unknown`, while `go test "./internal/cli*"` is literal and returns
`test`. A redirection and its operand are validated as ONE unit, so
`go >probe* test` and `go > "" test` refuse, while `go 2>"out file" test` and
`go 2>&"1" test` classify normally. Backslashes follow bash: inside double
quotes a backslash is literal unless it precedes `$`, a backquote, `"` or `\`,
and an unquoted backslash-newline is a line continuation. `env` consumes its own
assignments, so `env PROBE=1 go test` is `test`.

Wrapper handling follows each wrapper's real semantics: only `env` consumes
variable assignments, so `env PROBE=1 go test ./...` is a test while
`bash PROBE=1 go test ./...` is not - bash, sh, zsh and nohup take that word as
their script or command operand, and really do exit without invoking Go.

Shell INTERPRETERS are not executable wrappers. `bash`, `sh` and `zsh` unwrap
only when a command-string option is present (`-c`, `-lc`, `-ce`, and other
short bundles containing `c`; long options never qualify). Without one, the
operand is a SCRIPT FILE - `bash go test ./...` runs a script named `go` and
never invokes the Go toolchain, so it is not reported as a test. Executable
wrappers (`nohup`, `env`, `timeout`, `sudo`, ...) do exec their operand and
keep unwrapping.

Interpreter OPTION STATE is honoured, not guessed from token shape: `--` ends
option parsing (so `bash -- -c cmd` runs a script named `-c`), value-taking
options consume their argument (`--rcfile FILE`, `-O NAME`, `-o NAME`), and an
option outside the declared set - an invalid bundle such as `-zc`, or an
ambiguous value letter in mid-cluster - is reported as `unknown` rather than
given a bucket, because the shell aborts or the parse is ambiguous. A `-c` is
honoured only when it belongs to an interpreter: `nohup -c cmd` execs a command
named `-c`.

The implemented boundary, stated exactly. After `-c`, exactly ONE operand is the
command string; following words are positional parameters, so
`bash -c "go" test ./...` is not a Go test. Options that parse or print instead
of executing - `-n`, `-D`, `--help`, `--version` - mean the command string never
runs. Option grammars are PER INTERPRETER: POSIX sh rejects `--noprofile`, `-O`
and `-o` here, boolean long options take no inline value (`--noprofile=x`
aborts), and zsh is UNMEASURED on this box, so only `-c` and `--` are declared
for it and every other zsh option is reported `unknown`. Anything outside a
declared grammar is `unknown`, never a bucket.

Option POLARITY and per-interpreter VALIDITY are modelled as data, not shared
logic. `-x` sets and `+x` clears, and the last setting wins: `bash -n +n -c cmd`
runs the command while `bash +n -n -c cmd` does not. Tables are measured against
the interpreters installed on the machine - bash accepts `-h`, the installed
dash does not and has NO long options at all, and `--pretty-print` runs the
command rather than suppressing it. zsh is unmeasured here, so only `-c` and
`--` are declared for it and even `+c` refuses, although `+c` does introduce a
command string in bash and dash.

Options are modelled PER OPTION on four axes, each measured against the
installed interpreters. EXECUTION EFFECT distinguishes clearable noexec (`-n`,
`+n`, `-o noexec`) from sticky terminal forms (`-D`, `+D`, `--help`,
`--version`, `--dump-strings`, `--dump-po-strings`) that print or dump and are
never restored by a later `+n`. ARGUMENT FORM: long options take a separate
value only, so `--rcfile=/dev/null` is refused while `--rcfile /dev/null` runs.
VALUE DOMAIN: `-o`/`-O` values are checked against the shell's own `set -o` and
`shopt` names, which differ per interpreter - `sh -o pipefail` is refused
because dash does not have it, and dash has no `shopt` at all. ORDERING: a named
long option after a short cluster is refused, matching the real shells, while
the bare `--` terminator is exempt. Anything outside a measured table is
`unknown`.

WRAPPERS have declared grammars too, one per wrapper: options are matched by
exact token with value domains measured against the installed binaries - a
timeout duration is any number the shell's own parser accepts (including `.5s`,
`1.s`, `+1s`, `1e3` and `inf`) with at most one lowercase s/m/h/d suffix and no
leading minus; a signal is an exact name - the full `kill -l` set including the
RTMIN/RTMAX family and the IOT/CLD/POLL aliases - or 0-64, with no whitespace
padding; `nice -n` is a signed integer; `ionice -c` is 0-3 OR a class name
(`none`, `realtime`, `best-effort`, `idle`) and `ionice -n` has no small upper
bound; `xargs -n` is at least 1 and `-P` allows 0, both with an optional `+`;
and stdbuf's THREE STREAMS DO NOT SHARE A DOMAIN - `-o` and `-e` take `L`, `-i`
does not, and all three take 0 or a size with a unit suffix - and the `--` terminator is accepted in option
position before the duration, so `timeout --definitely-invalid 1s ...`, `timeout -s -999 1s ...` and
`timeout 1x ...` are refused rather than stripped. The token after the duration
is the COMMAND even when it is dash-prefixed. THE CLAIM IS DELIBERATELY NARROW
in two further places. Where two installed implementations of a wrapper
disagree - GNU coreutils exits 127 on `timeout <dur> -- cmd` while another
`timeout` on PATH runs it - the result is `unknown`, because measured-twice-with-
different-answers admits no confident bucket. And where a shell option's effect
depends on runtime state rather than on the option, classification fails closed:
bash restricted mode reports `unknown` because what it blocks depends on the
command text - but only where it is actually ENABLED: `-r`, `--restricted` and
`-o restricted` refuse, and so does `+r` AFTER one of them, since bash exits 2
on that. A bare `+r` is valid, leaves the shell unrestricted and classifies
normally, and an errexit
command string (`-e`, `-o errexit`) reports `unknown` when it has more than one
segment, since which segment runs depends on an exit status no lexer can know.
A single-segment errexit command still classifies normally.



Classification reads EVERY segment of a command, not the leading token:
`go test ./... && go build ./...` is `mixed` rather than being billed wholly to
`test`, `cd repo && go test ./...` and `time go test ./...` are `test` (the
prefix is plumbing), `go test ./... | grep FAIL` is `test` (a pipeline consumer
is a filter, not a phase), and `git commit -m "go test is slow"` is `vcs`. A
tool payload may be a bare command or JSON (`{"command":"go test ./..."}`),
which kimi uses; both are read.

Two confounders to hold fixed when comparing profiles, both measured: jobs whose
id does not begin `local-review-` include `workflow-*` rows that run an order of
magnitude shorter and will halve an aggregate median, and concurrency-at-start
alone moves p50 by roughly 2.6x.

### Evidence-graded proof manifests

```sh
gitmoot proof <root-id>
gitmoot proof --json <root-id>
gitmoot proof --verify <service-run-id>
# As with other Go-flag commands, place --home before the positional root id:
gitmoot proof --home /tmp/isolated-home <root-id>
```

`gitmoot proof` is a **read-only**, structured-store projection of the complete
job tree under `<root-id>`. The default output is a graded tree of sessions,
delegation lineage and synthesis rules, reported tests, reviews, commits, and PR
receipts. Missing evidence renders as `-`; `tests_run` claims remain
**reported** with an explicit CI-verification gap. Read-only means the command
opens SQLite in `mode=ro`, runs no migrations, and never changes store data.
SQLite may still create or refresh `-wal`/`-shm` bookkeeping sidecars while it
reads a live WAL database; immutable mode is deliberately not used because it
can observe a torn live snapshot.

`--json` emits the canonical content-addressed manifest. Every node uses a
`sha256:<hex>` content id, parents refer to children by hash, and the root hash
is the proof id. Re-projecting unchanged store records is byte-identical. Core
grading is store-only: content hashes, DAG consistency, and `result_hash` column
consistency can be **verified**, while independent review jobs, job events, and
daemon PR receipts are **observed**. This is internal consistency, not
cryptographic tamper-proofing against someone who can edit the database; signing
is future work. The headline grade tally excludes `integrity.*` meta-claims and
reports those separately on the `integrity:` line. The command never parses
transcripts, contacts GitHub, or mutates store data.

For a succeeded service pipeline run, `--verify` performs an **offline,
store-only** outcome check: the run succeeded, all stages succeeded/skipped,
each jobful stage has its expected terminal structured result, no failed or
blocked job tail remains, result hashes match, and the projected manifest DAG
verifies. It records `stored_pipeline_outcome`; it never reruns commands, queries
CI/GitHub, or upgrades reported test claims. Remote CI upgrades remain future
work. See [RESULT_CONTRACT.md](RESULT_CONTRACT.md)
for the structured result and delegation inputs, and [SAFETY.md](SAFETY.md) for
the read-only and bounded-DAG safety contracts.

`gitmoot job show` reports the model selected when the job was enqueued: an
explicit per-job/delegation model first, then the agent model, then the effective
runtime's configured default model. Text output prints `model: -` when no model
was known; `--json` carries the same durable value on the job row. This is the
enqueue-time selection Gitmoot knew (P1), not a later runtime-reported effective
model; runtime-reported truth is reserved for P2.

When standard output is an interactive terminal (and `NO_COLOR` is unset),
the transcript renders styled: agent turns get blank-line spacing and keep
their line breaks plus lightweight heading/list/inline-code treatment. Tool
calls use type-specific icons; shell output previews its last five lines while
read/search output previews its first 10-15 lines, with exact omitted-line
counts. Tool and turn durations render dim, cancelled tools render yellow,
completed machinery and usage render dim, and failed tool results render red.
Piped or redirected output always uses the plain byte-stable format.

Every transcript opens with an orientation header — job action, agent,
runtime/model (per-job override first, then the agent default), workflow label,
and the redacted, length-capped prompt — so a pane or saved transcript is
self-describing.

`job watch --transcript` follows a runtime tee log from offset zero and renders
redacted, bounded human-readable runtime output until the job settles, then
drains the file to EOF. It is incompatible with `--json`. Without `--log-path`,
Gitmoot derives the job-mode path under `<home>/logs/jobs/`; if that file is not
available, it prints `transcript unavailable; showing job events` and uses the
normal event watcher. `--log-path` and `--runtime` are primarily internal wiring
flags, but remain usable for diagnosis. When `--runtime` is omitted, the job's
runtime override wins over the registered agent runtime.

Fidelity follows each runtime's actual output contract: Codex JSONL renders
live; Kimi stream-json is turn-buffered and kimi-code 0.19.2 reports no usage;
Claude emits only its final JSON envelope, so its transcript remains quiet until
completion; shell output passes through as redacted raw lines. omp's NDJSON is
retained verbatim and currently passes through **undecoded** — every row carries
the transcript kind `raw`, and omp's own `type` discriminator survives only
inside each row's raw JSON text, so an export offers no per-event kinds; a
translator that lifts tool calls and usage out of the stream is a named
follow-up. Usage is labeled
`latest reported usage` because resumed Codex counts can be session-cumulative.
Malformed or unknown lines degrade individually to redacted capped raw output
without stopping later lines.

`job transcript <job-id> --export md` remains the deterministic, ANSI-free
Markdown snapshot. `--export jsonl` emits schema-versioned, self-contained
trajectory rows for every normalized event. Bulk export requires the explicit
`--all` guard; `--state` and `--since` filter the created-time-then-id ordered
stream. Bulk mode skips pre-retention or GC-missing logs and reports counts on
stderr, while explicit single-job absence is an error. `--output <path>` uses a
mode-`0600` temporary file plus atomic rename. Oversized runtime lines become
marked truncated raw steps instead of aborting the export.

JSONL export redacts every text-bearing event field with Gitmoot's best-effort
credential masker and has no raw bypass. The source log remains unredacted and
mode `0600`; best-effort export masking is not a vault.

Verified Codex command/file-change events and Kimi function tool calls/results
render as typed compact lines; unrecognized shapes keep the generic/raw
fail-open path. Render-time redaction is a per-line best-effort defense in depth:
a secret split across physical lines may be only partially masked, and the raw
tee log plus the external `tail -F` fallback remain unredacted.

### Resumable gates (make `blocked` + `needs` actionable)

When a stage returns `blocked` with a `needs` list (e.g. `needs: ["Maps API key"]`),
gitmoot persists each need as a **gate** attached to the blocked job. `gitmoot job
gates <job-id>` lists them (open / satisfied). Clearing a gate marks the blocker
resolved:

```sh
gitmoot job gates clear <job-id> --need "Maps API key"   # satisfy one need
gitmoot job gates clear <job-id> --all                   # satisfy every open gate
```

When the **last** gate is cleared, the blocked stage **auto-re-runs** via the same
`RetryJob` machinery `gitmoot job retry` uses (re-queued, then dispatched by the
daemon; downstream stages follow the normal delegation DAG) — no polling, resume
happens on clear. Two cases are deliberately **never** auto-resumed even with all
gates cleared: a **session job** (externally driven, #657) and a stage whose tree is
**paused awaiting a human** (`escalate_human` / ask-gate, #305/#340/#445) — a
resource gate must not bypass the human's `gitmoot resume` decision; the command
reports `not resumed: …` with the reason. A blocked job with **no** `needs` records
no gates and is byte-identical to before this feature.

### Session jobs (record "here"-method work)

The "here" method — importing an agent's prompt into your calling session with
`gitmoot agent prompt <agent>` — does the real work in your session but creates
**no gitmoot job**, so the dashboard / `job list` / event stream never reflect it.
Session jobs record that work as a first-class tracked job **without gitmoot
spawning a runtime** — a clock-in / clock-out pair (plus a one-shot recorder):

```sh
# Clock in: create a RUNNING, externally-driven job (no dispatch); prints its id.
gitmoot job open --agent <name> --repo owner/repo --type ask|review|implement \
                 [--title "..."] [--task <id>] [--parent-job-id <id>] \
                 [--pr <n>] [--head-sha <sha>] \
                 [--workflow <label>] [--json]

# Clock out: apply the result and move the job to its terminal state.
gitmoot job close <id> --decision approved|changes_requested|blocked|implemented|failed|skipped \
                 [--severity P0|P1|P2|P3] [--summary "..."] \
                 [--pr <n>] [--head-sha <sha>] [--branch <name>] \
                 [--model <name>] [--input-tokens <n>] [--output-tokens <n>] [--json]

# One-shot post-hoc: create an already-terminal job (open + close in one).
gitmoot job record (--agent <name> | --acting-role <role>) --repo owner/repo --type ask|review|implement \
                 --decision <decision> [--severity P0|P1|P2|P3] \
                 [--title "..."] [--summary "..."] [--task <id>] \
                 [--parent-job-id <id>] [--pr <n>] [--head-sha <sha>] \
                 [--branch <name>] [--model <name>] \
                 [--input-tokens <n>] [--output-tokens <n>] [--json]
```

`--agent` and `--acting-role` are mutually exclusive and exactly one is required.
Use `--agent` when a registered agent performed the work. Use `--acting-role` when
it was done in session by an org role that is not a registered agent (#1718):
roles and agents are separate namespaces, so recording an agent for role work
would be a false statement about who acted. The role is validated against the
configured org registry, and only its existence is checked -- availability and
recycle enforcement are dispatch-time policies, while this command records work
that has already happened. Attribution is then the role, and the merge gate still
enforces independence against it: a reviewer may not approve work attributed to
the identically named role.

An `externally_driven` job is created directly in `running` (it never queues, so
the daemon never claims or Delivers it — no runtime subprocess, no runtime-session
or checkout lock) and the stuck-`running` reaper **skips** it, so a session may
hold it open for as long as the work takes. `close` reuses the exact result path an
engine-run job uses: `--decision` maps to the same terminal state
(`approved`/`changes_requested`/`implemented`/`skipped` -> succeeded, `blocked` -> blocked,
`failed` -> failed) and emits the same finished/failed/blocked event, so a recorded
job is indistinguishable from an engine-run one in the dashboard and events. A job
can be closed **once** (it must be a running session job); an orphaned open job
stays `running` (reaper-exempt) until you `job close --decision failed` or `job
cancel <id>` it. A session job is never engine-executed, so `job retry` **refuses**
it (retrying would re-queue it for a real runtime with an empty payload) — recover
one by opening a fresh session job instead. The agent and repo must exist.
`--parent-job-id` must name an existing job. Model and token values are
caller-reported evidence: Gitmoot records only values supplied explicitly to
`close` or `record`, and leaves the model empty and token counts zero when they
are omitted.

A review recorded with `changes_requested` must pass `--severity` with the
highest finding severity. Other decisions and non-review session jobs may omit
it.

For a seat's internal fan-out, the recording convention is to use the seat's
registered agent name, link each recorded unit to the seat job with
`--parent-job-id`, and give it a descriptive `--title`. This is a convention,
not an enforced identity or spawn relationship; the CLI validates only that the
agent and parent job exist.

For an in-session PR review, clock in before reading the diff and bind the
display row to its exact head and workflow journal:

```sh
gitmoot job open --agent <name> --repo owner/repo --type review \
  --pr <n> --head-sha <sha> --workflow <label>
gitmoot workflow note <label> "reviewed tests and error paths"
# Post the verdict, then:
gitmoot job close <id> --decision approved|changes_requested|blocked \
  --summary "..." [--severity P0|P1|P2|P3]
```

For a running externally-driven review, `job list` and `job show` derive
`review_status: in_progress|stalled` from the newest workflow note, falling back
to the job's creation time; the signal becomes stale after 20 minutes.
`head_sha` is also exposed in text and JSON. Every such session status is explicitly
`review_status_grade: reported` and
`review_status_authority: non_authoritative` (text lists use `REVIEW (reported;
non-authoritative): ...`). Workflow notes are caller assertions, not
system-observed reviewer attribution. These fields are display-only: they never
satisfy, block, or otherwise feed the merge gate, and this feature never writes
`tasks.state`.

When an externally-driven review closes, its payload retains
`review_status_grade: reported`; `job list --json` and `job show --json` continue
to expose that field after the running liveness status ends. The grade is never
`observed` or `verified` because both the reviewed head and verdict came from
caller-supplied flags. This durable reported record is not merge-admissible
evidence, and no merge gate consumes it.

For a running engine-dispatched review with an isolated worktree, `job list`
also samples the verified daemon's descendant process tree twice. A descendant
whose cwd is that worktree (or a directory below it) yields
`review_status: in_progress`; a conclusive absence in both samples yields
`review_status: stalled`. A runtime root remains visible while the agent is idle
between tool commands, so the check does not depend on a transient tool child.
These statuses carry `review_status_grade: observed` and
`review_status_authority: non_authoritative`. If the daemon identity or process
table cannot be verified, the status is omitted rather than guessed. The
daemon-valued `jobs.runner_pid`, job timestamps, and event age do not decide this
status. This is observation only: it does not cancel, retry, reclaim, or feed the
merge gate.

**Make in-chat / "here" work show on the dashboard.** The one-step default is
`gitmoot agent prompt <agent-or-template> --record`: it opens the session job *as
you import the prompt* and prints a header naming the job id (see the `agent
prompt` section above). It works for a registered agent (repo defaults to its
scope) and for a bare template id with an explicit `--repo` (the template id is
recorded as the identity, #673). Apply the prompt, do the work, then clock out with
`gitmoot job close <id> --decision …`. That is all it takes for otherwise-invisible
current-chat ("here") work to appear in `job list`, the dashboard, and the event
stream — no daemon, no runtime, no PR comment. Use plain `agent prompt` (no
`--record`) only when you just want to read a prompt without tracking it.

`gitmoot job kill <root-job-id>` is the operator kill switch for a runaway
delegation tree: it terminates the tree identified by its **root** job id
gracefully. In-flight jobs finish normally; the coordinator's next continuation
is routed through the graceful finalize path (synthesize what completed → stop)
and the daemon stops starting queued children of that root. See
`references/SAFETY.md` for how it relates to the other termination bounds.

For a `queued` or `blocked` job, `gitmoot job list` appends a `WHY:` column and
`gitmoot job show` prints a `why_stuck:` (and, when a lease applies, a
`next_retry_at:`) line explaining what the job is waiting on — e.g. `waiting on
runtime session lock runtime:codex:<ref> (held by job <id>)`, `blocked: awaiting
human`, `auth failing: …`, `throttled: …`, `retrying: …`, or a
`blocked-operational: <class>` deferral. The reason is derived from the most
authoritative existing signal (the latest reason-bearing `job events` entry plus
the owning resource lock's lease); a healthy job's output is unchanged. When a
deferral usually needs a human (a dirty or wrong-head checkout), the row/`job
show` also carries a `suggested_action` naming the concrete fix. `gitmoot doctor`
proactively validates `gh auth` (with an actionable remediation hint) and the
Claude runtime token so a bad credential is caught before a job stalls on it.

A `queued` job whose reason is already recorded no longer renders as a bare
`queued` row (#1887), and **which form you see depends on whether a
reason-bearing event exists**, because the event is the more authoritative
signal:

- When the classifier deferred the job it wrote a `blocker_deferred` event, and
  that event is rendered: `WHY: blocked-operational: <class>: attempt <n>/<max>`.
  This is the common case for a real runtime deferral.
- When the hold exists only in the payload — no reason-bearing event available —
  the payload is read directly and rendered `WHY: deferred (<class>)`, with the
  recorded earliest retry as `(next retry <RFC3339>)` and, when the deferral
  usually needs a human, `[action: …]`.

**A row whose `blocker_retry_at` is absent renders `deferred (<class>), retry
time unknown` and leaves `next_retry_at` empty; it never shows a zero time.**
Absence is the common case on at least one path, and a formatted empty timestamp
would read as a retry long overdue.

`gitmoot job show` prints the same reason as `why_stuck:` (and `next_retry_at:`,
`suggested_action:`) rather than the `WHY:` column form used by `job list`.

The sibling cause is surfaced on the same pass (#1553), but it is **never
inferred from a lock table**: a job withheld while another job works the repo is
rendered from the deferral the daemon actually recorded for it. The pre-flight
emits `branch <branch> is locked by <owner>`, which is classified as the
`checkout_contention` blocker class and persisted as both the payload class and a
`blocker_deferred` event naming the branch and the holder, so it renders through
the two forms above like any other deferral.

Neither `resource_locks` nor `branch_locks` is consulted, and that is a
deliberate reversal of two earlier attempts. Resource-lock keys are
`runtime:<rt>:<ref>` and `checkout-mutation:<absolute path>` and encode no
repository segment, so matching them against a repo name attributed unrelated
holders. Reading `branch_locks` instead was the wrong inference rather than the
wrong table: a branch lock records who owns a LANE, not who is withholding a
given job, and the lock is acquired with the agent as owner BEFORE that agent's
own job is enqueued — so an ordinary queued job was reported as withheld by its
own agent. A queued row with no recorded deferral is therefore left silent
rather than given a holder no lock row can prove.

`gitmoot job watch` surfaces a hold while it waits, as `HOLD: <reason>` with the
optional `(next retry <RFC3339>)` and `[action: …]` suffixes, re-checked on every
poll so a hold that begins after the watch attached is still shown. The line is
reprinted only when the hold **changes**. In the default event mode the watcher
already replays every job event, so `HOLD:` is emitted only when no
reason-bearing event exists — otherwise the deferral would be stated twice under
two labels. `job watch --transcript` has TWO paths and they differ, so the guarantee is
qualified: with a retained log it enters `transcript.Follow` and renders log
lines rather than job events, and the hold is printed there in every case
because no event can have spoken for it. With NO retained log it prints
`transcript unavailable; showing job events` and DELEGATES to event watch,
inheriting that mode's behavior exactly - including the reason-event
suppression above, so a deferral that already has a `blocker_deferred` event
appears as the event and not as `HOLD:`.

`gitmoot job watch --json` carries the last hold observed during the watch as
`held_reason`, `held_next_retry_at` and `held_suggested_action`. They are named
for that distinction deliberately: the JSON object is emitted once the job has
settled, so a field called `why_stuck` would assert a condition that is no longer
true. All three are omitted when no hold was observed.

For a terminal (`succeeded`, `failed`, `blocked`, or `cancelled`) job whose
recorded worktree still has a locally observable process, `job list` reports
`LIVE PROCESS: worktree still has an active process` and `job show` prints the
same detail as `process_active: worktree still has an active process`; their
JSON forms carry `process_active: true`. The badge is omitted when no live
process is observed, when local process liveness is unavailable, or while the
job is still non-terminal.

For an `implement` job, `job show` and `job list --json` expose Gitmoot's own
post-agent delivery verdict as `delivery_status`: `delivered` means the job's
result decision was `implemented` and either a completion marker is paired
with a persisted non-zero pull request, or the job has no advance events at
all and already carries a persisted non-zero pull request; `pending` means
delivery is in flight or scheduled for retry, and `blocked` means the latest
delivery attempt blocked. A completed advancement alone (e.g. an implemented
result that produced no PR, or a non-`implemented` decision) does NOT count as
delivered — the field is omitted in that case, never a false `delivered`. The
field is derived at read time
from the existing `advance_*` job events and pull-request payload; it is omitted
for non-implement jobs and legacy/unknown states. This field, not the agent's
free-text result summary, is authoritative about commit/push/PR delivery because
Gitmoot performs that step after the agent turn ends.

For a `running` job dispatched through a PID-aware runtime runner, the payload
records `runtime_pid` plus a process-start identity. `job list --json` and `job
show --json` derive `runtime_process_active`: `true` means that exact process is
alive, `false` means it is confirmably gone (including PID reuse), and an omitted
field means unknown because no PID/identity was recorded or process inspection
is unavailable. This is direct per-job ground truth and is separate from the
terminal worktree-scanning `process_active` badge above.

Operational blockers auto-retry (#532): a delivery failure classified as an
operational blocker — `runtime_auth`, `runtime_quota`, `network_outage`
(transient network/GitHub outage), or `checkout_contention` (a self-healing
branch-lock conflict, or a dirty/wrong-head checkout that carries a
`suggested_action`) — does **not** fail the job terminally. The daemon re-queues
it as **deferred** with a bounded retry budget and a hold until the earliest
retry time, shown as `blocked-operational: <class>: attempt n/3`. `gitmoot job
show --json` carries the `blocker_class`, attempt count, and `suggested_action`.
Over the `[events]` stream the deferral is now a **first-class** `job.deferred`
transition emitted **instead of** `job.failed` (no preceding `job.failed` for
that run). A `runtime_auth` deferral only re-dispatches once a live doctor-style
credential probe passes; the probe failing just extends the hold without spending
a retry. So a job that "failed then reappeared as queued" is the deferral
working, not a bug. Product failures (the agent answered with a `gitmoot_result`,
including `decision=failed`) are never auto-retried.

A **capability refusal is the one class that does not retry** (#1821). When a
delivery fails because the runtime could not be executed at all - its binary is
absent from the seat's `PATH`, or resolves to a published unavailable shim that
exits 126, or the sandbox could not resolve the target - the job terminates
**`blocked`** with class `runtime_unavailable`, a
`blocker_runtime_unavailable` event carrying the remedy, and **no** retry
instant or attempt count. It is the only operational class whose condition does
not clear on its own: the other four wait out a provider window, an outage or a
lock, while a missing executable waits for an operator. Measured before it was
built: 29 refusal deaths in this store, zero recorded `blocked`, and 25 of the
29 belonged to an agent dispatched into the same wall more than once - one of
them eleven times. `failed` invited that; `blocked` does not. Find them with
`gitmoot job list --state blocked`.

When a runtime session ends **without** producing a `gitmoot_result` envelope —
the CLI process crashed, exited non-zero, was signal-killed, or completed but
never emitted a valid envelope even after repair attempts — the job records
**failure diagnostics** (#806): a `phase` marker (`launched` = died before any
stdout, `streaming` = died mid-output, `result-parse` = every delivery completed
but no valid envelope was found, `recovery` = daemon recovery proved the durable
runner was gone), the process `exit_code` **or** terminating
`signal`, a **redacted** stderr tail (hard-capped at 4 KB; redaction runs over
the full text with the same token-redaction rules as job comments *before* the
tail is cut, so a secret can never leak partially), the **`delivery_error`** —
the engine's own terminal error for the delivery, i.e. the exact line the daemon
logs as `job <id> failed: <err>` (#1620) — and the runtime session id when one is
known. `gitmoot job show` prints a `failure_diagnostics:` block,
`job show --json` carries `payload.failure_diagnostics`, and `gitmoot report
bug` includes a "Failure diagnostics" section. Successful jobs never store one,
and a retried job clears the previous run's crash report. Terminal `failed`
events embed the same bounded, redacted stderr tail for durable triage.

`delivery_error` is what distinguishes faults the row otherwise collapses into
one identical `phase: streaming, exit_code: 1` — a model/CLI mismatch (`400
invalid_request_error: The '<model>' model requires a newer version of Codex.`)
and a compaction-endpoint `404`, say, need different remedies but used to look
the same on the job row because the runtime's real error existed only in
`journalctl --user -u gitmoot-daemon`. It is a **different source** from
`stderr_tail` (which is the runtime CLI's own stderr, often just an echo of the
skill system prompt) and never replaces it; both go through the identical
redaction and 4 KB bound, because a provider error routinely carries a URL, a
request id, or a token. A delivery that failed before the adapter reported any
session evidence records `delivery_error` with **no** `phase` line, and a
delivery that failed with an empty error records no field at all.

Jobs stuck in `running` are also backstopped, but transcript silence alone never
kills work. The recurring same-boot sweep moves `running` to `failed` only when
all three facts agree: `updated_at` is frozen past the staleness window, the
default-on job log is byte-identical across two sweeps and older than
`[daemon].quiet_kill_after` (default 45m, minimum 5m), and the recorded runtime
PID is dead. A live PID with the recorded `/proc` start-time identity is an
absolute veto however quiet the log; a recycled PID records an identity-mismatch
event and leaves the job running. Legacy rows without a runtime PID require
twice the quiet threshold and no held runtime lease.

On Linux, runtime commands start as the leader of a dedicated process group;
the OnPID write persists that PID, its `/proc` start time, and the PGID before
the runner waits. Context cancellation signals the whole group (TERM, then
KILL). After a same-boot liveness verdict wins the `running` → `failed`
transition, cleanup signals the recorded negative PGID only when the current
group leader still has the persisted start time. A mismatch sends no signal and
records `pgid recycle suspected, orphans leaked`. Daemon death does not itself
reap the process group. A descendant that deliberately calls `setsid` escapes
the recorded process group; reaping such escapees remains an explicit
out-of-scope residual.

The staleness age leg is tunable via the
`GITMOOT_STALE_RUNNING_AFTER` env var; the smallest honored value is 1m —
below-1m, malformed, or non-positive values are rejected (with a one-time
warning) in favor of the 30m default rather than clamped (#560). It is a crash
backstop, not a kill deadline.

### Migration note: daemon recovery now fails interrupted jobs

As of #1308, daemon-owned jobs recovered after a restart are **failed, never
silently requeued**. A changed Linux boot id, an expired runtime-session lease,
or the three-leg stale liveness predicate produces a `job_recovery_failed` event
that names the cause and recovery age since the row's last durable update and
carries the redacted last 4 KB of the retained job log when available. A startup
row with `job_kill_pending` but no
terminal event is likewise failed with the explicit reason `daemon died
mid-kill`. Inspect the evidence, then use `gitmoot job retry <job-id>` when a
rerun is appropriate. `session-*` jobs are unchanged because they execute
outside the daemon.

**Configuration audit:** this migration makes no new config value load-bearing.
`GITMOOT_STALE_RUNNING_AFTER` still controls the age leg and
`[daemon].quiet_kill_after` still controls the byte-silence leg; only the final
recovery action changed from implicit requeue to evidence-bearing failure. After
a reboot there is no age wait: the daemon detects the changed kernel boot id on
startup and each recovery sweep (Linux only; other platforms use lease/age
recovery).

`gitmoot job cancel <job-id>` is the single-job **abandon** verb. It dismisses a
`queued`, `running`, **or `blocked`** job (a blocked job is one paused awaiting a
human — an operator permission gate or an unrecoverable blocker — so dismissing it
is the same abandon intent as cancelling a queued/running one; #631). Cancel is a
single-row transition: it does **not** propagate to a delegation tree, touch task
state, or set the killed flag — abandoning a whole tree is `gitmoot job kill`.
Cancelling also releases any resource locks the cancelled job still owned —
including a stranded `runtime:<rt>:<session>` lock left behind when a foreground
`gitmoot agent ask` was killed — so the next ask on that agent does not wait out
the lock TTL before it can run. Dismissal is reversible: `gitmoot job retry`
accepts a cancelled job (its accepted source states are failed/blocked/cancelled),
so a mistakenly dismissed job can be resurrected.

`gitmoot job cancel --state blocked` is the **bulk** form for clearing a backlog
of blocked jobs. Only `blocked` is accepted for `--state` (queued/running jobs use
single-job cancel; terminal jobs use retry). Narrow the selection with
`--older-than` (a Go duration like `168h`, or a convenience `<N>d` days suffix like
`7d`; age is measured from when each job became blocked), `--repo owner/repo`
(matches the job's payload repo), and `--agent name`. The bulk form is a **dry-run
by default** — it prints the matching jobs (id, agent, repo, age) and exits without
cancelling anything; pass `--yes` to actually cancel the selection. Each selected
job is dismissed through the same per-job cancel path, so its locks are released
too. `<id>` and `--state` are mutually exclusive, and `--older-than`/`--repo`/
`--agent` require `--state`. `gitmoot doctor` warns when blocked jobs older than
30d have piled up and prints this exact command as the remediation.

To automate the sweep, set `[orchestrate].blocked_ttl` to a positive Go duration
(e.g. `blocked_ttl = "168h"`): the daemon's housekeeping tick then dismisses any
blocked job whose blocked-transition timestamp (updated_at, created_at fallback)
is older than the TTL, through the same `CancelJob` abandon path, recording a
distinct `blocked_ttl_expired` job event so a TTL auto-expiry is distinguishable
from a manual `job cancel`. It is **off by default** — an empty or `0s` value
disables it (a negative value is rejected). Unlike `[orchestrate].escalation_ttl`
(which auto-finalizes a whole paused delegation *tree* and is on by default, 24h),
`blocked_ttl` dismisses a *single* blocked job and is off by default.

Native task auto-merge is enabled by default only behind an exact-head approved
review and green SHA-scoped commit statuses/check-runs. A gate miss parks the
task as `awaiting_human_merge` and records an org escalation. It selects the
most-specific live role whose scope matches the repo and addresses that role's
nearest live chart ancestor. With no live scope match it addresses a live
`owner`; a root match addresses itself. With no live upward recipient, the gate
fails closed without journaling an addressed note. Archive filtering does not
predict delivery; `org validate` reports missing wake routes and pane bindings.
Set `[repos."owner/repo".merge_gate] auto_merge = false` as an explicit
kill-switch; that deliberate hold does not escalate. Pipeline `allow_auto_merge`
is independent, and an authorized `@gitmoot merge` remains an explicit override.

Merge-gate retries are automatic while the daemon is running. Retryable states,
such as a busy base-branch merge queue or an active job on the PR branch, are
retried on the next daemon poll tick. The default poll interval is `30s` unless
the daemon was started with a different `--poll`.

### Draining before a deploy

`gitmoot daemon drain` stops the daemon claiming new work so a busy fleet can
reach the idle window the deploy recipe requires (#1207):

```bash
gitmoot daemon drain                 # stop claiming, wait up to 15m for in-flight jobs
gitmoot daemon drain --timeout 30m   # wait longer
gitmoot daemon drain --clear         # resume claiming, AFTER the restart
```

**Queued work is not lost.** Draining pauses selection at the same choke point the
disk guard uses, so queued jobs stay queued and resume when the drain clears.

The command exits **0** only when no engine-dispatched job is in flight - that is
the point at which a restart is safe. If jobs remain past the deadline it exits
**1** and NAMES them rather than killing or parking them: a CLI cannot see the
state of work it did not start, and terminating it is the loss drain exists to
prevent. Drain stays active meanwhile, so the set can only shrink.

Session-recorded jobs (`session-*`) are excluded from the wait. They run outside
the daemon process, hold no lease, and may legitimately stay running for hours -
counting them would make the drain never finish.

**The drain sentinel is a file under the gitmoot home, not a database row**, so it
still reads when the store is contended - which is exactly when a deploy is
likely to be waiting.

**`--clear` is required after the restart.** The sentinel outlives the process;
a new daemon started while it exists will claim nothing.

`gitmoot job run <id>` is **intentionally not gated by drain**. It calls the
worker directly rather than going through the scheduler's queued-job selection,
so it is the one path that starts work on a drained daemon. That is deliberate:
it is a manual, explicit, single-job operator command, and an operator who types
it during a drain means it. A job it starts is still counted by an in-progress
`daemon drain` wait, because the job transitions to `running`; the only gap is
the ordinary race where it is invoked after a drain has already reported success
and exited.

## Findings Ledger

`gitmoot findings` reports the #1822 findings ledger. Two shapes:

```bash
# Per-repository summary: who is recording obligations and who is answering them.
gitmoot findings
gitmoot findings --json

# Individual rows for one pull request, with the UIDs a reviewer must cite.
gitmoot findings --repo owner/repo --pr 12
gitmoot findings --repo owner/repo --pr 12 --json
```

`--repo` and `--pr` are given **together or not at all**. The store keys findings
on `(repo, pull_request)` and a finding UID is unique only within one repository's
pull request, so an unpaired flag cannot identify a set. It is refused rather than
rendered: an earlier revision accepted `--repo` alone, silently listed the
`pull_request = 0` rows, and printed `no findings recorded` for a repository full
of them.

Rows are folded to the **latest observation per finding**, using the engine's own
rule (`latestObservation`), so the listing shows current state rather than the
append-only log. A listing that folded differently from the gate would disagree
with it about which obligations are open.

`STATE` is the last recorded observation and is not the same question as "does this
still block": an answered finding can still be mandatory at a later head if the
files its relevance keys name changed again. Only the merge gate decides that, and
the command says so in its own output.

A failed review can still record a source finding when it supplies executed
evidence or a static locator and rationale. Quoted-only output from a failed
review stays a job-level protocol/runtime failure; it does not become a code
finding or merge obligation.

The row listing exists because the **obligation brief is byte-bounded**. Once its
budget is exhausted the brief omits mandatory UIDs and tells the reviewer to
consult the ledger, and until this command could print rows there was no supported
way to do that: the summary reports counts, and a read-only review seat has no
database access. Cite a UID verbatim as `continues_uid` to continue a finding -
typing its label mints a new one instead.


### Asking what the gate still demands

`--at-head` answers a different question from the row listing, and the difference
is the reason it exists (#2097).

```bash
gitmoot findings --repo owner/repo --pr 12 --at-head <sha>
gitmoot findings --repo owner/repo --pr 12 --at-head <sha> --json
```

The rows above print `STATE`, the last recorded observation. `--at-head` prints
the **obligations the merge gate would still demand at that head**, which is not
derivable from any state column and differs in both directions:

- an **open** finding recorded at an earlier head remains an obligation when
  that head is the same as or an ancestor of the target head. A finding observed
  only on a proven-divergent branch line is not an obligation for the rewritten
  tree;
- an **answered** finding on the same commit line becomes mandatory again when
  the diff since the answer touches its relevance keys, so a state filter is
  blind to exactly that row.

It is not a reimplementation. It calls `LedgerObligationsAtHead` through
`LedgerResolvers.ScopeFor`, which the type documents as the only production path
to a `LedgerScope`, built by the same constructor the review brief and the merge
gate already share. The command therefore cannot answer differently from the
thing that blocks.

`--at-head` requires `--repo` and `--pr`: obligations are computed for one pull
request at one head, and the refusal names that combination rather than only
calling the input invalid.

**Degradations are printed, including beside an empty list.** `LedgerScope`
degrades rather than failing. Without an ancestry resolver, observations cannot
be limited to the target commit line. Without a changed-file resolver, answered
findings stay advisory and simply do not appear, which under-reports. The engine
records those as task events; this command has no task, so it prints them as
`degraded: ...` lines. An empty obligation list and an instrument that could not
look otherwise read identically, and the empty one reads as good news.


### Finding merged pull requests that still carry obligations

```bash
gitmoot findings --merged-unresolved
gitmoot findings --merged-unresolved --repo owner/repo
gitmoot findings --merged-unresolved --json
```

Lists pull requests that **merged** while still carrying unresolved findings at
their branch head. The report is the **union of two sets**, and they are not the
same question:

1. the obligations `LedgerObligationsAtHead` would demand, the gate's own predicate;
2. findings whose **latest observation at that exact head is still open**, which
   the gate's `dischargedAtHead` step deliberately removes.

The second set exists because the gate asks "what must a **new** review at this
head still observe", and a row already recorded there has been observed. That is
correct for the gate and wrong for this report, which asks what was unresolved
**when the pull request merged** - the case where a P1 sits at the exact head
that merged. Those rows are labelled `still open at the merged head` so the two
sets stay distinguishable. **The report and the gate can therefore disagree, by
design, and only in that direction:** the report **contains every gate
obligation and may add exact-head-open findings**. When no finding's latest
observation sits at the merged head the second set is empty and the two
coincide.

**It keys on the branch head, never the merge commit.** Every merge in this
repository is a squash, so the merge commit is a commit no reviewer ever
observed: measured across 28 merged pull requests carrying findings, the ledger
holds 19 observations at branch heads and **zero** at merge commits, and the two
SHAs are never equal. Keying on the merge commit fails silently in both
directions - filter the observations to it and the report is empty forever;
pass it as the head and nothing is ever discharged, so every answered row
reappears.

**It reports what it scanned.** `scanned 7 repositories, 40 pull requests with
findings, 26 merged` - because an empty list from a scan of seven and an empty
list from a scan of zero read identically otherwise, and the second is an
instrument failure wearing a success message.

A pull request the forge cannot answer for is reported as a `degraded:` line
rather than skipped: an outage must not render every merged pull request as
resolved. `WAIVED` reflects the repository's **current** `findings_consumption` setting,
read at report time. It is **not** evidence about the merge: the setting may have
changed since, and no durable merge-time record of it exists, so reading it as
"the gate let this through by declaration" reverses history whenever the config
moved after the merge.

`--merged-unresolved` accepts `--repo` alone to narrow the scan and takes
neither `--pr` nor `--at-head`, and the refusal names that combination.

## Result Checks

After a daemon-run job's `gitmoot_result` is parsed, Gitmoot runs a set of
**deterministic, LLM-free binary checks** over the parsed result (#526) — a
contract-hygiene audit that catches results that are technically valid but vague
or missing evidence. Each check is a yes/no question with an explanation, e.g.:

- **implement** — a result whose decision is `implemented` must list its
  `changes_made` and its `tests_run`. The engine resolves the effective checkout
  selected by the worker, persists `payload.result_observation` from its diff,
  and fails `implement-changes-observed` if a claim names a path absent from the
  diff, a claim names no file path, or the diff contains a path no claim
  mentions. A worktree-less delegation child instead persists the typed source
  `excluded_worktree_less_delegation_child`; exclusion is observable and is not
  graded as diff evidence.
- **review** — a `changes_requested` review must carry `findings` (evidence). A
  terminal review verdict (`approved` / `changes_requested`) must additionally
  ACCOUNT FOR ITSELF (`review-verdict-has-evidence`): cite `findings`, name a
  substantive `tests_run` / `changes_made` entry, or give a summary long enough
  to state why there was nothing to run. An entry counts as substantive when it
  is a phrase (a command with its outcome) or a single token long enough to name
  a real path or target — so `tests_run: ["."]` is not evidence, while an honest
  docs-only approval that explains what it read passes on its rationale.

  The check measures ACCOUNTING, not truth: it cannot detect fabricated evidence,
  and no deterministic check can. A **coordinator fan-out** — a review result
  that declares `delegations` — is exempt, because it announces a panel that has
  not reported yet and legitimately has nothing to record. See
  [Fan-outs are not verdicts](#fan-outs-are-not-verdicts).
- **ask** — the answer (`summary`/`artifact_body`) must be non-empty and
  actionable.
- **blocked** (any action) — a `blocked` result must list actionable `needs`.
- **coordinator finalize** — a finalize continuation must produce a substantive
  reconciliation summary.

The mode is set in `config.toml` and is **warn by default**:

```toml
[workflow]
result_checks = "warn"   # off | warn | block (default: warn)
```

- `warn` (default) — failing checks are recorded as a `result_checks_failed`
  job event (visible in `gitmoot job events <id>` and `gitmoot job show <id>`)
  and attached to the job detail (`job show --json` `payload.result_checks`, and
  the web dashboard), but the job still finishes on its own decision.
- `block` — a failing check additionally fails the job through the same terminal
  path a malformed result takes (opt-in, for strict workflows).
- `off` — the audit emits no check, event, or failure record. The worktree
  observation is still persisted when available; recording evidence is
  independent of whether a gate refuses it.

A result that passes every applicable check records nothing, so the audit is
quiet on healthy jobs. Failed checks are also stored durably so a later pass can
consume them as structured feedback; nothing consumes them today.

## Pipelines

A pipeline (#681) runs a **declared DAG of shell and managed-agent stages** — a
fixed, repeatable multi-step flow — on demand, on an interval schedule, or after
another pipeline succeeds. Each
stage is an ordinary queued job: shell commands use the shell runtime, while agent
stages use their registered runtime. The normal worker tick claims and runs it,
and a scan-based advancer folds each stage's `gitmoot_result` **decision**
and enqueues the stages whose `needs` have all succeeded. Pipelines reuse the job
queue, the result contract, and the heartbeat scheduling idiom (durable `next_due`,
overlap guard, missed-ticks-coalesce). They are **off by default** (no pipelines ⇒
the daemon's pipeline scan returns before touching state).

Define a pipeline in a YAML file and register it:

```yaml
name: nightly-sync          # required, name-safe token (letters, digits, - _)
repo: owner/repo            # optional to register; REQUIRED to run
env_file: /root/.config/nightly-sync/env # optional 0600 secret file
env:                         # optional inline NON-secret defaults
  OUTPUT_DIR: /srv/nightly-sync
schedule:                   # optional interval schedule (no cron in v1)
  interval: 24h             #   positive Go duration (required with a schedule block)
  jitter: 15m               #   optional random [0, jitter] added to next_due
trigger:                    # optional pipeline-success chain
  kind: pipeline
  pipeline: upstream-name
stages:                     # the DAG, keyed by unique id and wired by needs
  - id: source
    cmd: "curl -sf https://example.com/data > data.json"
    env_keys: [SOURCE_API_TOKEN]
  - id: score
    cmd: "python score.py data.json"
    isolate: true          # optional shell-only detached read-only worktree
    needs: [source]         # runs only after every listed stage SUCCEEDS
  - id: triage              # an AGENT stage instead of a shell cmd (exactly one of cmd|agent|gate)
    agent: reply-triager    #   #757 read-only leaf
    action: ask             #   ask (default) | review | implement (+ write: true, #768)
    prompt: "Triage the scored data; block if a human is needed."
    needs: [score]          #   upstream results are prepended to the prompt
  # other agent-stage kinds:
  #   implement (#768): action: implement + write: true → declarable, but NO allocator (#2203/#2213)
  #   bound review (#813): action: review + source: <impl stage> -> reviews that PR/head, report-only
  #   orchestrate (#758): orchestrate: true → sub-tree coordinator (fans out owned children, folds synthesis)
  #   gate (#768): gate: pr_merged + source: <impl stage> (no agent) → jobless; human merge is default
  #   auto-merge gate: add merge: auto plus top-level allow_auto_merge: true; requires source-bound review
  - id: deploy
    cmd: "rclone copy out/ r2:bucket"
    needs: [triage]
    timeout: 30m            # optional per-stage job timeout
    retry: 2                # optional; re-attempt a FAILED stage up to N times
```

To start this pipeline once for every newly-succeeded run of another pipeline,
use the alternative pipeline trigger shape (do not combine it with `schedule`):

```yaml
trigger:
  kind: pipeline
  pipeline: upstream-name
```

```sh
gitmoot pipeline add nightly-sync.yaml --enable   # validate + store; omit --enable to add disabled
gitmoot pipeline export nightly-sync --output ./nightly-sync.bundle
gitmoot pipeline import ./nightly-sync.bundle --repo new-owner/new-repo
gitmoot pipeline remote set owner/pipeline-catalog
gitmoot pipeline remote show
gitmoot pipeline publish nightly-sync [--remote owner/pipeline-catalog] [--create]
gitmoot pipeline pull --list [--remote owner/pipeline-catalog]
gitmoot pipeline pull nightly-sync [--remote owner/pipeline-catalog] --repo new-owner/new-repo [--agent-map exported=local]
gitmoot pipeline list [--json]
gitmoot pipeline show nightly-sync [--json]        # registry view for a name
gitmoot pipeline run nightly-sync [--payload key=value ...] [--payload-json '<obj>']
gitmoot pipeline watch <run-id> [--timeout 10m] [--poll 5s] [--json]
gitmoot pipeline show <run-id> [--json]            # run funnel for a "prun-…" id
gitmoot pipeline expose --schema schema.json <name> # issue one bearer token (shown once)
gitmoot pipeline serve                              # loopback API on 127.0.0.1:8792
gitmoot pipeline resume <run-id> [--from <stage>]
gitmoot pipeline cancel <run-id>
gitmoot pipeline enable|disable nightly-sync
gitmoot pipeline remove nightly-sync
```

### Expose a shell pipeline as a service

Service exposure is explicit and v1 accepts only shell-only, template-free
pipelines. Define a bounded flat schema, expose the pipeline, then run the
separate authenticated listener:

```json
{"version":1,"fields":{"count":{"type":"integer","required":true,"minimum":1,"maximum":5}}}
```

```sh
gitmoot pipeline expose --schema schema.json nightly-sync
gitmoot pipeline serve # --addr defaults to 127.0.0.1:8792
```

The exposure command stores only the SHA-256 token digest and prints the
base64url bearer token once. `--disable` blocks new POSTs but does **not** revoke
read/poll access to already accepted runs; use `--rotate-token` to revoke the old
bearer credential. The API accepts
`POST /v1/pipelines/<name>/runs` and exposes authenticated status/bundle reads at
`/v1/pipelines/runs/<id>`. Inputs are schema-validated and delivered only as
reserved `GITMOOT_INPUT_*` environment variables, never prompt text. Admission
is atomic (rate bucket, global cap, per-pipeline overlap, run/stages/receipt),
uses unpredictable 128-bit run ids, and every service shell stage runs in a
fail-closed detached worktree. Service exposure rejects any stage that declares
`env_keys`, network access, or extra read/write authority.

Successful service shell stages may deliver files beneath `out/`. Gitmoot
collects them before disposing the detached worktree, namespaces them as
`artifacts/<stage-id>/...`, and caps the run at 64 MiB total; exceeding the cap
fails finalization instead of truncating output. On the first authenticated GET
after success, Gitmoot freezes an archive with those files, the accepted #941
bundle, `proof.json`, and `verification.json`. Per-file size and SHA-256 metadata
is committed by artifact nodes in the offline proof.

The public, read-only `/receipts/<id>` page lists artifact names, sizes, and
digests, but its sanitized `/receipts/<id>/bundle` omits artifact bytes. Only the
authenticated `/v1/pipelines/runs/<id>/bundle` delivers the files. Neither
surface discloses bearer tokens, inputs, prompts, logs, or raw result text. Both
bundles intentionally include the frozen pipeline spec with its full shell
command bodies and referenced environment-variable names. Never inline a secret
literal in `cmd`; an `env_keys`-bearing pipeline cannot be exposed at all. Public
capability receipt URLs remain public after token rotation. Use `--allow-remote`
only behind owner-controlled TLS/firewall policy.

### Export and import a pipeline bundle

`pipeline export <name> --output <dir>` creates a portable directory, never a
single YAML blob:

```text
nightly-sync.bundle/
├── bundle.yaml          # version, requirements, warnings, agents, spec hash
└── spec.yaml            # stored bytes with only repo replaced by __GITMOOT_REPO__
```

A bundle carries each agent's template id as a REFERENCE only. #2204 removed
template distribution, so no prompt body travels in a bundle and there is no
`templates/` directory.

The exporter carries YAML comments and ordering through unchanged and reports
host-specific `/root`, `/home`, `/Users`, and `/tmp` paths found in command
stages. It never exports environment values or local runtime state.

Import on another machine with its real repository and any local agent mapping:

```sh
gitmoot pipeline import ./nightly-sync.bundle \
  --repo new-owner/new-repo \
  --name nightly-sync-copy \
  --agent-map reply-triager=local-triager
```

Every import prints a requirements report first: available/missing runtimes,
present/missing upstream pipelines, write-authority flags that need consent, and
absolute-path warnings.
Without `--agent-map`, the declared agents are registered against template ids
that must already exist in the importing home. Existing agents or pipeline names
with different content are refused unless `--force`; identical content is a
no-op. A missing runtime for an unmapped agent is a hard failure.

Imported pipelines land **disabled**, even when replacing an enabled row. Review
the report and stored spec, then run `pipeline enable <name>` or pass `--enable`
to the import to explicitly re-consent any `allow_scheduled_writes`,
`allow_triggered_writes`, or `allow_auto_merge` authority. The target repo and an
optional new name are injected without re-marshaling the rest of the YAML. The
stored `spec_hash` therefore hashes the imported bytes and correctly differs from
the source bundle whenever repo/name/agent mappings changed. Missing upstream
pipelines are allowed and leave the imported pipeline dormant until they exist.

### Share a pipeline via GitHub

Configure one GitHub repository as the default catalog, publish from the source
home, then list and pull from another home:

```sh
# Source home. --create creates owner/pipeline-catalog as a PRIVATE repo.
gitmoot pipeline remote set owner/pipeline-catalog
gitmoot pipeline publish nightly-sync --create

# Target home.
gitmoot pipeline remote set owner/pipeline-catalog
gitmoot pipeline pull --list
gitmoot pipeline pull nightly-sync \
  --repo new-owner/new-repo \
  --agent-map reply-triager=local-triager
```

The optional `[pipeline_remote]` config section has the same `repo`, `ref`, and
`path` shape as the other GitHub-backed remotes; `ref` defaults to `main` and `path` to
`pipelines`. An explicit `--remote owner/repo` wins over the configured repo.
`remote set` also accepts `--ref` and `--path`.

Each published entry is a reviewable directory at
`pipelines/<name>/bundle.yaml` and `spec.yaml`. Publishing
compares the exported bytes with HEAD: unchanged files cause no commit, changed
files alone are upserted, and files removed from the current bundle are deleted
from that managed pipeline directory. `--create` creates a **private**
repository; without it the remote must already exist.

`pipeline pull --list` prints each available name, description, and a one-line
requirements summary. Pull downloads the selected directory at HEAD and hands
it to the same `pipeline import` path: the requirements report, `--agent-map`,
`--name`, collision/`--force` gates, and `--enable` behavior are unchanged.
Nothing beyond the spec is installed, and the
pipeline lands disabled unless `--enable` explicitly re-consents its authority.

### Reading pipeline status

The registry view is self-describing even before the first run. `mode` is
`after: <upstream>`, `scheduled <interval>`, or `manual`, and stage lines carry
their kind plus bounded command/prompt previews:

```text
enabled: true
mode: after: nightly-sync
interval: -
...
stages:
  fetch   [SHELL]      cmd: ./fetch-message.sh  needs=-
  answer  [AGENT ask]  reply-planner (codex/gpt-5.6-sol)  timeout=10m  needs=fetch
```

An absent agent row renders as `(unregistered)` without failing `show`.
`pipeline list` appends a seventh description column, truncated to about 60
characters, and uses `after: <upstream>` in the interval column for a trigger
pipeline. A removed or missing upstream is shown as
`after: <upstream> (upstream missing)`. JSON includes the full pipeline
description plus `mode`, and stage `kind`, `agent_runtime`, `prompt_preview`, and
`cmd_preview`; the existing full fields remain unchanged.


An enabled `trigger.kind: pipeline` pipeline fires once for each upstream run
that reaches `succeeded`; failed and cancelled runs never fire it. The durable
upstream-run cursor makes daemon re-ticks and restarts idempotent. Adding or
enabling the downstream re-arms the cursor at the latest upstream run, so old
history and successes from a disabled period never backfill. If the downstream
already has an active run, the scan leaves the cursor unchanged and fires after
that run settles. Missing upstreams warn at add time and remain dormant; removing
an upstream is allowed. Add-time validation rejects self-reference and trigger
cycles such as `B -> A -> B`. Pipeline triggers use local database state.

For example, replace a clock-staggered downstream schedule (such as 24h30m
after a 24h upstream) with an ordered success chain:

```yaml
name: nightly-report
repo: owner/repo
trigger:
  kind: pipeline
  pipeline: nightly-sync
stages: [...]
```

`pipeline add` validates the whole spec at add time (unknown keys, duplicate/self/
cyclic `needs`, self/cyclic pipeline triggers, a schedule+pipeline-trigger hybrid,
a stage that is not exactly one of `cmd`/`agent`/`gate`, an agent stage
missing a `prompt` / invalid `action` / `implement` without `write: true` / a mutating
stage on a scheduled pipeline without `allow_scheduled_writes` / a mutating stage on
a triggered pipeline without `allow_triggered_writes` / a gate or review's
bad `source` / `source` on another stage kind / `isolate: true` on a non-shell
stage, invalid durations, a `success_decisions` outside
`approved`/`implemented`/`changes_requested`/`skipped`) so a mistake is a clear error, not a
stuck run. It stores the raw YAML **verbatim** plus a content hash; each run
snapshots that hash and executes its snapshot, so editing the file later never
mutates an in-flight run. `pipeline add` also auto-creates one hidden shell runner
agent (`pipeline-<name>-runner`) that owns the **shell** stage jobs — filtered out of
`agent list` and disposed by `pipeline remove`. An **agent stage** (#757) instead
runs a named managed agent on its own runtime as a read-only leaf (`ask`/`review`);
its `needs` stages' result summaries are prepended to the prompt, and a repo-bound
agent stage runs in its own detached read-only worktree so same-repo agent stages
parallelize without touching the live checkout.

A shell stage may set `isolate: true` to opt into a disposable detached read-only
worktree at the managed checkout's committed tip. The default remains the shared
checkout, including its uncommitted/gitignored data and any intentional writes.
Opt-in allocation is fail-open: failure emits `readonly_worktree_skipped` and runs
on the shared checkout. On success the command receives
`GITMOOT_CHECKOUT=<live-checkout>` for cwd-independent access to data omitted from
the clean worktree. This removes checkout-lock serialization, and each isolated stage
also takes a job-scoped shell runtime-session key (`runtime:shell:job:<hash(job)>`)
instead of the command-hash key, so same-repo stages run concurrently even when they
share the identical command (#1034). Service shell stages retain their unconditional,
fail-closed isolation; agent and gate stages reject this shell-only field.

Shell and agent stages can opt into scoped key access with `env_keys`. Source
files must be absolute, operator-owned regular files
with mode exactly `0600`, outside the Gitmoot home and every managed checkout;
inline `env` is for non-secret defaults. A shell stage with no list gets
nothing. Its resolution is own
`env_file`, then a shared `injected` or configured `proxied` key granted with
`gitmoot key grant`, then inline default. Registered but ungranted names do not
match exact or glob selectors. Structural errors always fail add; unresolved
names warn only while the pipeline remains disabled, then hard-fail
add-with-enable, enable, manual run, and scheduled/triggered preflight.

Agent stages resolve only configured `proxied` registry keys granted to their
registered seat. The seat grant and the stage selector are both required;
injected agent grants, pipeline files/defaults/grants, and gate-stage selectors
are refused. Ordinary agent jobs receive no keys, and delegation children do
not inherit the parent stage's key access.

Sources are revalidated and reread at delivery, so rotation applies without a
daemon restart. Each payload audits `PipelineName` and names-only
`PipelineKeyAccess` rows `{stage,name,source,mode}`; `pipeline show --json`
exposes the same projection. A shared grant is rechecked immediately before
delivery: revocation fails closed and never switches to another source. Gitmoot
internal `GITMOOT_*` entries remain final. Injected mode exposes the value to
that shell process. Proxied mode puts a per-job placeholder in `<KEY>` and a
loopback endpoint in `GITMOOT_PROXY_<KEY>_URL`; every request rereads the value,
rechecks the pipeline or agent-seat grant, and is constrained to the configured
upstream/base path. For agent stages the real value never enters the process,
but the authorized agent can exercise it against that pinned upstream.
The lease is revoked when delivery ends.

Proxied mode hides key bytes; it does **not** prevent an authorized child from
exercising the credential on the pinned upstream. Curated upstreams and base
paths are part of the model. Configure only trusted upstreams. Pipeline key
delivery stays separate from the Claude model gateway. For authenticated remote
leases, response filtering covers accidental exact-byte reflection in body
bytes and HTTP field names or values. Malicious upstreams and transformed
application payloads, including application-layer compression without a
`Content-Encoding` header, are outside that boundary.

For a non-empty run payload, every agent stage (including roots) receives a
dynamically fenced, 6000-byte-bounded `UNTRUSTED external data` block before
upstream context; each rendered value is capped at 1500 bytes. Shell stages receive
exact `GITMOOT_TRIGGER_<UPPERCASE_KEY>` exec environment entries, never shell-source
interpolation. The full canonical payload is retained in the SQLite run row and
shown as redacted/truncated key provenance by text `pipeline show <run-id>` and
as `payload_json` in JSON; job env/prompt projections follow normal job-data
retention.

Every pipeline shell stage also receives `GITMOOT_PIPELINE_NAME`,
`GITMOOT_PIPELINE_RUN_ID`, and `GITMOOT_PIPELINE_STAGE_ID`. A dependent shell
stage receives `GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE`, naming a delivery-scoped
`0600` JSON tempfile with v1 `schema_version`, `complete`, and a `stages` map of
settled state/summary records. Each summary's marshaled JSON string is capped at
16 KiB with rune-safe truncation, and the final marshaled file at 64 KiB;
`complete:false` or `summary_truncated:true`
signals partial data. The content is persisted and re-created at a fresh path for
retries; the file is removed after delivery. Treat summaries as untrusted data,
never shell source, and keep credentials out of them. A strict consumer can start
with `jq -e '.schema_version == 1 and .complete == true' "$GITMOOT_PIPELINE_UPSTREAM_CONTEXT_FILE"`.
A successfully isolated non-service shell stage additionally receives
`GITMOOT_CHECKOUT`, a best-effort path to the live managed checkout for reading
gitignored `repos/**` or uncommitted data. Prefer the stage cwd for committed files.
Treat this path as read-only, and do not run the isolated reader beside a default
stage that mutates the checkout: those live reads can observe a torn tree or
`index.lock` contention. The input, trigger, metadata, keycard, and upstream-context
variables are unchanged by the detached cwd.

`action: produce` (#814/#825) is a sandboxed pipeline leaf for writing operator-owned
data, never repo/branch/task/PR state. Codex uses its native sandbox; Claude and
modern Kimi require a successful `gitmoot sandbox probe` and are re-execed under
strict Landlock. Non-Linux/unsupported hosts keep the Codex-only refusal. It requires
`write: true`, one or more absolute
cleaned `writes:` paths, a `produce`-capable writable agent, and optionally
`network: true`, `check: <cmd>`, and `check_retries: N`. `pipeline add` resolves
symlinks and rejects targets overlapping `/`, the Gitmoot home, or a managed checkout.
The worker repeats that same canonicalization immediately before delivery to close
symlink-retargeting races. Declared paths are additive `--add-dir` grants. For
Claude/Kimi, Landlock limits writes to those existing directories plus the workdir,
temp roots, standard device nodes, and runtime-owned state: `$HOME/.claude` plus
`$XDG_CACHE_HOME/claude-cli-nodejs` for Claude, and `$HOME/.kimi-code` for Kimi.
Gitmoot sets `CLAUDE_CONFIG_DIR=$HOME/.claude` so Claude's mutable config stays inside
that state grant. Apart from runtime state/cache and device nodes, declared data paths,
the disposable workdir, and temp roots are the only writable locations. Codex behavior
is unchanged. Landlock does not govern network access.
A Codex danger-full-access agent receives no add-dir/network arguments because it is
already unrestricted. Checks re-ask the same session with
redacted/capped output; stage retries must reconcile partial data idempotently.
Gitmoot cleans only the disposable cwd, never the declared data directories.

An `action: review` stage may set `source: <implement-stage>` (also listed in its
`needs`) to bind to that implement job's structured PR stamp. The review payload
inherits the PR number, head SHA, branch, task, and lead agent; its detached worktree
is pinned to the exact PR head. The verdict still posts as a PR comment and folds by
`success_decisions`, but pipeline reviews are report-only: they dispatch no native
fix job and never run the native merge gate. Declaring this review also sets
`SkipNativeReviewFanout` on the source implement request, avoiding duplicate native
reviewer jobs. If the source permanently produces no PR (no-op or `skipped`), the
review folds blocked immediately with `source stage produced no PR; nothing to
review` and no unbound review is dispatched.

The implement stage this binding reads is the one `implement` surface #2203 left
declarable. Its stage KIND still validates, because `write: true` and
`allow_scheduled_writes` are #768's mutating-safety contract and `action:
produce` is mutating too — that half is live and was not touched. What went is
the pipeline-stage writable-worktree ALLOCATION plus the enqueue, so a declared
implement stage has nothing to run in and no pipeline run will produce the PR
stamp this review binds to. Lifetime pipeline stage jobs: ask 5,229, produce 92,
implement zero — no pipeline ever created one. Whether `pipeline add` should
refuse the kind outright is the open decision in
[#2213](https://github.com/gitmoot/gitmoot/issues/2213); until it lands, treat
the `source: <implement-stage>` binding and the `pr_merged` gate below as
mechanisms whose implement source cannot currently run.

The `pr_merged` gate remains a human-merge waiter by default. Opt-in
`merge: auto` is gate-only and also requires top-level `allow_auto_merge: true`
plus at least one review stage bound to the same implement source. The advancer,
not the report-only review job, performs one squash attempt only after every bound
review folded `approved`, the live PR head still equals the reviewed payload
`HeadSHA`, GitHub reports mergeable, and checks pass. Pending checks keep waiting;
head drift, conflicts/unmergeability, or a merge API error fold the gate blocked.
Merge errors are not retried. Scheduled pipelines need both `allow_auto_merge` and
the existing `allow_scheduled_writes` key. Omitting `merge` preserves human merge.
Pending checks wait; skipped/neutral check-runs pass; failures block; and zero
external statuses/checks always block regardless of `require_external_ci`. The
source job atomically records `pipeline_auto_merge_claim` before the write,
`pipeline_auto_merge_confirmed` after GitHub confirms it. A scan that loses the
claim never parks the run; it ages its wait from the claim row's own
`created_at` and records `pipeline_auto_merge_claim_orphaned` past 15m, or
immediately with `cause=claim_timestamp_unreadable` when that value will not
parse, and keeps waiting. A stage `timeout` parks either case terminally rather
than recovering it - nothing releases an orphaned claim, so a new run is the
remedy. A claim released in the window between a losing scan's failed claim and
its read is the ordinary hold cycle: the scan retries SILENTLY, so nothing is
reported while the gate is still waiting. It is not discarded, though - the
release reason travels with the wait, and if the stage `timeout` has elapsed it
appears in the TERMINAL PARKED summary, so the human who finds a parked run reads
why it waited. A workload-mode
reconciliation hold records `pipeline_auto_merge_held` with its cause, releases
the claim so the merge is re-attempted when the row lands, and parks with that
cause at the gate `timeout` or 24h after the hold began when no timeout is set;
the episode is keyed on the head and the decision, not on the cause text.

### Satisfying the workload-mode gate

A pull request whose diff changes the `**Current mode:` marker in `AGENTS.md` is
HELD until an exact-head reconciliation row exists. The gate is keyed on the
repository OWNER, so every `gitmoot/*` repository is held, human-requested merges
included.

Two note streams decide it, both matched by an exact body PREFIX and compared
case-insensitively on repository:

- DECISION: `[operating-mode repo=<owner/repo> mode=<THROUGHPUT|STEADY|DRAIN>]`
- RECONCILIATION: `[workload-mode-reconciliation repo=<owner/repo> pr=<number> head=<40-character reviewed head> mode=<mode> decision_note=<note id|none>]`

Every field is `key=value`, whitespace separated, inside the leading bracket. A
field with an empty key or value makes the whole body unparseable, so the
`repo=` field inside the body is what identifies the repository:

```
gitmoot workflow note <label> "[operating-mode repo=owner/repo mode=STEADY]"
```

`<label>` must be a workflow that already has jobs - `workflow note` refuses an
unknown label to guard against a typo - so file the row under the lane the PR is
already being coordinated in rather than inventing a label.

The note's repo COLUMN is a separate, optional thing. `gitmoot workflow note
--repo <owner/repo>` sets it and nothing else - it no longer opts the note into
anything, because the memory surface it used to accompany is gone (#2202). A
note whose column is empty still counts when its body names this repository,
which is the ordinary case, so setting the column is optional; getting `repo=`
right in the BODY is not. Prefer setting it anyway for an operating-mode or
reconciliation note: the gate reads those through two bounded windows, one
repo-scoped and one for the repo-less rows, and a scoped note cannot be crowded
out of the window by other repositories' notes.

PRECEDENCE. The NEWEST decision wins, and a reconciliation row must be newer than
it. `decision_note=none` means the PR itself is the decision, and the row's own
`created_at` is `decided_at`; otherwise `decision_note` names the newest
operating-mode note the exact head implements. A row that cites a note the PR
contradicts cannot ratify it.

WHEN A DECISION CANNOT BE READ. An unreadable newest decision - a malformed field
list, a `mode` that is not a workload mode, or a note recorded for this
repository whose body omits or contradicts `repo=` - SUPERSEDES like any other
decision rather than reading as "no decision". It is a recency boundary, not a
veto: a fresh exact-head row that agrees with the PR's own marker still merges,
so correcting the note is optional. The gate refuses outright only when nothing
readable remains - the note is unreadable AND the PR's marker patch is missing or
ambiguous - and the hold then names both exits: append a readable operating-mode
note, and file an exact-head row citing it.

CLEARING A HOLD. The hold is retryable: the pipeline gate releases its
at-most-once merge claim, records `pipeline_auto_merge_held` with the cause, and
merges on a later scan once the row lands. It is bounded on both the ordinary
evaluate path and the final merge boundary - the stage parks at the gate
`timeout`, or 24h after the episode began when no `timeout` is set. An episode is
keyed on the head and the decision, so a new head or a new decision (including a
new unreadable one) starts a fresh budget rather than inheriting the old one.

A stage signals its outcome by printing a `gitmoot_result` blob to stdout; the
advancer folds by the **decision**, never the job's exit state (`changes_requested`
is a succeeded job but folds as a stage **failure** by default — a stage folds on the
decision, not the job state):

```sh
printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"synced"}}'
printf '%s' '{"gitmoot_result":{"decision":"blocked","summary":"secret missing","needs":["R2 token"]}}'
```

- a decision in the stage's `success_decisions` (default `approved`/`implemented`/`skipped`) ->
  **succeeded**, dependents enqueue;
- `blocked` → the stage blocks, its `needs` persist at the stage **and** run level,
  the run **parks blocked** (downstream never enqueues, zero compute while parked);
- `failed` / any other decision / a cancelled job / no `gitmoot_result` → the stage
  **fails** (retried if budget remains), else the run **parks failed**.

`skipped` means the stage itself had no work and advances by default with a
`[skipped: no work]` summary marker. An explicit `success_decisions` list is
strict: omitting `skipped` makes it fail. A `pr_merged` gate whose source skipped
parks blocked because no PR can exist for that run.

`pipeline run` prints only the run id (script-stable: `RUN=$(gitmoot pipeline run
nightly-sync)`); repeat `--payload key=value` or use one `--payload-json` string
object to enter the existing trigger-input seam. The forms are mutually exclusive
and share the bridge's key/count/size validation. A manual run ignores `enabled` but still needs a `repo` and refuses
to start while a run is already active. `pipeline show <run-id>` renders the **text
funnel** (`source OK -> score BLOCKED (needs: R2 token) -> deploy SKIPPED`) under a
run header; a **failed** run also prints the exact `gitmoot report bug --job
<stage-job>` command (gitmoot never auto-files it).

`pipeline watch <run-id>` is the blocking completion primitive. It prints each
stage state transition once, returns `0` for succeeded, `1` for terminal
failed/blocked/cancelled, and `2` with `still running` when `--timeout` expires.
Use `--json` for the same final summary shape as `pipeline show --json`.

While a stage is queued or running, the run view adds an honest
`STATE; enqueued <elapsed> ago` detail (the stage timestamp is enqueue time, not
claim time). Long-running pipeline jobs publish a latest-only `progress` event
after one minute and about every 30 seconds thereafter. A running stage shows the
event's real age and last sanitized activity line; the age continues increasing if
the daemon dies. Orchestrate stages whose current coordinator has no fresh event
show `(sub-tree running; no per-stage progress)`. JSON stage objects add
`started_at`, `finished_at`, and an optional structured `progress` object.

`pipeline show <run-id>` also reports best-effort total tokens; JSON carries run
`tokens` plus per-stage `input_tokens` and `output_tokens` where captured. A zero
can mean the runtime/session shape did not report usage.

`pipeline resume` re-runs a **parked** (blocked/failed) run from its halted stage
(or `--from <stage>`) plus its transitive dependents — bumping their attempt — while
**never re-running a succeeded stage**; it refuses a non-parked run and a run whose
spec drifted. `ResumePipelineRun` is the #682 approval-gate seam. `pipeline cancel`
abandons a run through the shared `job cancel` path.

A pipeline stage is a **leaf**: a stage result carrying `delegations[]` does not
spawn children — the advancer ignores them and the engine strips them for a pipeline
stage job. Use an orchestra for dynamic fan-out, a pipeline for a fixed shell DAG.
See `docs/pipelines.md` for the full reference and `WORKFLOWS.md → Pipelines` for the
end-to-end story.

## Routing Telemetry (Advisory)

Gitmoot records lightweight **execution-grounded routing telemetry** (#530): one
additive row per job at its terminal transition, capturing which combination
actually ran and how it turned out — `repo`, `action` (ask/review/implement/
continuation/…), `phase`, `runtime`, `model`, `agent`, resolved `template_id` +
commit, terminal `job_state` (succeeded/failed/blocked), result `decision` +
approval flag, a coarse tests-run count, `duration_ms`, and `input`/`output`
tokens (best-effort; a runtime that reports no usage contributes 0). Capture is
**always on, additive, and fail-safe**: it writes only to the new
`routing_telemetry` table and a telemetry error can never fail a job, so wire
output is unchanged.

**v1 is advisory only.** Nothing reads this back to change routing — no automatic
model/runtime override happens anywhere. It is a local feedback loop you inspect,
not a global benchmark. (This capture slice subsumes the phase-aware capability
telemetry proposed in #522.)

Inspect observed performance (read-only), grouped by `(action, runtime, model,
template)`:

```sh
gitmoot router summary [--repo owner/repo] [--action ask|review|implement] [--since 30d] [--json]
```

It reports per-group count, success rate, approval rate, median duration, and
summed tokens, always labeled **"local observed performance, not a benchmark"**.
`--since` accepts a Go duration or an `<N>d` days suffix.

Optionally feed a **bounded** (≤12-line) observed-performance table into a
**coordinator's** prompt so it can weigh which runtime/model/template has done
well on the repo. It is **off by default**; with it off, coordinator prompt
assembly is byte-identical and no telemetry query runs during a job:

```toml
[router]
context_enabled = true   # inject the advisory table into top-level coordinator prompts (default false)
```

The injected block carries the same "not a benchmark" disclaimer and is only added
to top-level (coordinator) jobs — a delegation child inherits its coordinator's
routing decision. Routing stays advisory: the block never forces a route.

## Bridge (localhost HTTP for external automation)

```bash
gitmoot bridge serve [--addr 127.0.0.1:8791]   # localhost-only unless --allow-remote (dangerous)
gitmoot bridge token [--rotate]                 # prints the token FILE PATH, never the token
```

The bridge exposes a small authenticated HTTP surface over the same internal
seams the CLI uses (no new authority): POST /v1/pipelines/{name}/run,
GET /v1/runs/{id}, GET /v1/jobs/{id},
POST /v1/agents/{name}/ask. Every request needs
`Authorization: Bearer $(cat ~/.gitmoot/bridge.token)`. Requests are
rate-limited (30/min) and generally body-capped at 1 MiB. Pipeline run accepts no
body, `{}`, or `{"payload":{"key":"value"}}` for any enabled repo-bound pipeline.
Its raw body cap is 64 KiB: at most 32 entries, 1–64 byte lowercase identifier
keys, 32 KiB UTF-8 values without U+0000, and 48 KiB decoded keys+values. Invalid
payloads return 400, oversize 413, missing pipelines 404, and overlapping runs 409.
Containers reach the host bridge at `http://host.docker.internal:8791`, or at
the Docker bridge IP on Linux.
