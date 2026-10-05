# Execution Backend Selection

Gitmoot names where a job's runtime subprocess executes through the
**execution backend** seam (`internal/execbackend`, issue #1535 contract,
#1536, #1537). The seam is deliberately distinct from Landlock **local confinement**
(`internal/sandbox`, the `sandbox` CLI, agent path grants), which restricts
what a locally-running subprocess may touch and is unaffected by backend
selection.

```toml
[remote_exec]
backend = "local"
# Optional privilege drop; both fields are required together.
local_uid = 1000
local_gid = 1000
# Use a traversable root when the Gitmoot home is below /root.
local_root = "/var/tmp/gitmoot-local"
# Remote E2B templates. OMP needs a dedicated template with at least 2 GiB RAM.
# e2b_api_key_file = "/run/secrets/e2b-api-key"
# e2b_template = "gitmoot-shell"
# e2b_omp_template = "gitmoot-omp"
# For opt-in broker access from a remote shell, configure both:
# credential_gateway_listen = "0.0.0.0:8443"
# credential_gateway_url = "https://broker.example.com:8443"
```

The default remote provider is cloud E2B, using its existing dollar caps.
Two sandboxd providers can be declared beside it: `sandboxd` (Linux ARM64 VMs
on a Mac) in `[remote_exec.sandboxd]`, and `sandboxd-linux` (Linux AMD64
Firecracker VMs on the Gitmoot host) in `[remote_exec."sandboxd-linux"]`. Each
is used only by a job that opts in with `--exec-provider sandboxd` /
`--exec-provider sandboxd-linux` or by a repository whose `checks_provider`
names it; nothing else routes to them.

## Versioned OMP review template

The credential-free OMP review image is defined in
`templates/e2b/omp-review/Dockerfile`. It pins both the E2B base-image digest
and the Go archive checksum. Root is used only for image construction; the
final image user and its runtime directories are UID/GID 1000 (`user`).
Gitmoot uploads the OMP binary and job-scoped gateway identity after
provisioning.

Build a new immutable version from the template directory:

```sh
cd templates/e2b/omp-review
npm ci --ignore-scripts
E2B_TEMPLATE_NAME=gitmoot-omp-go126-v3 \
E2B_CPU_COUNT=4 E2B_MEMORY_MB=4096 npm run build
```

`E2B_API_KEY` and a new `E2B_TEMPLATE_NAME` are required.
`E2B_CPU_COUNT` defaults to 4 and `E2B_MEMORY_MB` defaults to 4096. The command
prints the template ID and build ID; record both with the selected resources in
the workflow evidence before changing `e2b_omp_template`. Never put the API
key, GitHub credentials, repository data, runtime sessions, or job state in
this directory.

Every Dockerfile, base digest, Go version, or checksum change gets a new
versioned template name. Verify a fresh sandbox by reading the pinned tool
versions without installation, uploading an exact-head source archive, running
build, vet, and focused tests, and checking that no credential paths or secret
environment variables exist.

Toolchain review images for Swift, Rust + Zig, Node + pnpm and Flutter
repositories (`gitmoot-omp-review-{swift,rust,node,flutter}-v1`) sit beside it
under `templates/e2b/` and share its base and runtime layout.
`templates/e2b/README.md` lists each template's pins and resources and covers
building, bumping and verifying them; `npm run build -- ../<template-dir>`
builds one, and `templates/e2b/omp-review/verify.mjs` runs a repository's
checks in a fresh sandbox.

Activation changes only the operator config:

```toml
[remote_exec]
e2b_omp_template = "gitmoot-omp-go126-v2"
```

Gitmoot reloads this value while provisioning each job, so the next remote OMP
dispatch uses the new template without a daemon restart. Rollback restores the
previously recorded template name (`gitmoot-omp-full-review` for the initial
build); the next dispatch uses it. Do not delete the rollback template until
the replacement canary is complete. Listener-coordinate changes remain
process-bound and still require a restart.

`local` is the default. `remote` provisions E2B for engine-driven shell and OMP
review jobs -- implement remains on the allowlist but #2203 removed every way to
dispatch one, so `review` is the type that actually reaches the backend.
Remote OMP uploads the host OMP executable into the instance, requires
`e2b_omp_template` with at least 2 GiB RAM, and refuses before provider
allocation when that template or the credential gateway is missing.
Unsupported job types and other model runtimes also refuse before allocation.

### Opt-in sandboxd ARM64 provider

The sandboxd endpoint on the Mac speaks the E2B control and envd protocols;
Gitmoot does not run a second backend. The home keeps cloud E2B as its default
remote provider and declares sandboxd beside it. A review runs on sandboxd only
when it is requested with `--exec-provider sandboxd`; the choice is stored in
the job payload (`exec_provider`), survives retries and model fallbacks, and
drives that job's admission, reservation, provisioning, envd routing, keepalive
and teardown. The disk guard and `remote_routing_enabled` policy only ever
route to E2B; a repository's `checks_provider` is the one policy that selects
the provider (see Per-repository checks routing).
A request for a provider that is unknown, or for `sandboxd` on a home without
`[remote_exec.sandboxd]`, is refused before anything is enqueued.

The provider was named `mac` before #2328, and the alias is now removed:
`[remote_exec.mac]` is a config load error naming `[remote_exec.sandboxd]`,
and `--exec-provider mac`, `checks_provider = "mac"` and a queued job that
stored `mac` are refused as an unknown provider, naming `sandboxd`. Existing
ledger attempts were migrated to `sandboxd`. Keep `max_concurrent` set while
sandboxd may predate `GET /sandboxd/capacity`.

The `sandboxd` host requires Apple `container` 1.4.1 and its Linux kernel
(`container system start --enable-kernel-install`). Build the credential-free
`sandboxd/images/linux-arm64/Dockerfile` with `container build --platform
linux/arm64 --tag sandboxd/review-go126:<version> <image-directory>` and
allowlist that exact image in `sandboxd --image`. Create an owned host-only
network with `container network create --internal --label
gitmoot.sandboxd.network=apple-v1 sandboxd-internal` and pass
`sandboxd --network sandboxd-internal`; provisioning refuses a missing or NAT
network. Each guest uses a read-only root, a private 10 GiB volume at
`/home/user`, and bounded `/tmp` mounts. Keep the SQLite ledger and 0600
control-key file outside disposable paths; serve sandboxd's loopback listener
only through an authenticated private HTTPS gateway. Neither building the
image nor starting Apple's runtime activates Gitmoot's sandboxd provider.

Apple's `hostOnly` network blocks external egress but still reaches Mac
services listening on all interfaces, so sandboxd v0.1.5 installs
deny-by-default guest-subnet-to-host firewall rules with only the fixed
model-gateway relay (`192.168.128.1:43181`) allowed. The control and envd API
is served privately through Tailscale Serve HTTPS.

The metrics endpoint samples Apple VM CPU and memory usage; it does not
invent disk consumption or cloud charges for the fixed-size volume.

For ARM64 OMP reviews, set `omp_linux_arm64_file` to a checksum-verified
Linux AArch64 build of the same OMP release the host runs (verify the release
`SHA256SUMS` entry before use). Gitmoot refuses a non-ELF or wrong-architecture
file; it does not compare versions.

```toml
[remote_exec]
# ... the existing cloud E2B keys stay unchanged and remain the default ...
credential_gateway_listen = "0.0.0.0:8443"
credential_gateway_url = "https://203.0.113.7:8443"

[remote_exec.sandboxd]
api_key_file = "/run/secrets/sandboxd-key"
template = "review-arm64"
omp_template = "review-arm64"
base_url = "https://mac.example.ts.net:8443"
envd_base_url = "https://mac.example.ts.net:8443"
omp_linux_arm64_file = "/opt/gitmoot/omp-linux-arm64"
credential_gateway_url = "https://192.168.128.1:43181"
# max_concurrent = 4  # optional ceiling; required only for a sandboxd without GET /sandboxd/capacity
```

`base_url` and `envd_base_url` must be HTTPS origins, or plain HTTP on a
loopback address (a gateway on the daemon's own host); `envd_base_url` sends
`E2b-Sandbox-Id`, `E2b-Sandbox-Port: 49983`, and `X-Access-Token` to that one
host on both upload and process streaming.

sandboxd's concurrency comes from its `GET /sandboxd/capacity` report
(authenticated with `X-API-Key`), read immediately before each reservation and
cached process-wide for 10 seconds; errors are not cached, and a create refused
with 409 drops the cached report. The provider-wide cap is the reported
cluster `totalSlots`, lowered to `max_concurrent` when that is set (absent or
0 adds no ceiling; negative is a load error), and attempts of one template
also count against that template's reported `totalSlots`. sandboxd attempts
reserve zero dollars and count only against these caps, while E2B attempts
count only against the E2B dollar and concurrency caps, so neither provider can
take the other's slot. A shrinking report never cancels a running attempt.

| Capacity report | Result |
|---|---|
| 200, template has slots | cap as above |
| 200 with 0 total slots, or no online worker serves the template | `capacity` refusal; the review waits |
| transport error, timeout, 5xx, 429, malformed JSON | `capacity` refusal; the review waits |
| 404 (sandboxd predates the endpoint), `max_concurrent` > 0 | cap is `max_concurrent` (the old behaviour) |
| 404 without `max_concurrent` | unconfigured; fails fast naming both fixes: upgrade sandboxd or set `max_concurrent` |
| 401/403 | unconfigured; fails fast |

Every sandboxd review that meets a full cap or a `capacity` refusal, or whose
create sandboxd refuses with 409 (a race between the report and sandboxd's
scheduler), waits in the queue as described under Per-repository checks
routing, bounded by the job timeout. A cloud E2B 409 is handled as before.

One credential gateway listener serves every provider. Its certificate names
every advertised host (`credential_gateway_url` and each sandboxd section's
`credential_gateway_url`, as DNS or IP SANs), and each job's lease is bound to
its own provider's origin. sandboxd guests reach it through sandboxd's
relay at `192.168.128.1:43181`, which forwards to the Mac's `127.0.0.1:43184`; a
supervised `ssh -R 127.0.0.1:43184:127.0.0.1:8443` tunnel then carries it to
the daemon's existing listener port (the port of `credential_gateway_listen`).
No general-purpose proxy or secret is exposed to the VM.

Reconciliation lists each provider's inventory separately and settles or
orphans only that provider's attempts, so a sandboxd attempt is never settled
because E2B or the other sandboxd gateway does not list it, nor the reverse. sandboxd's list is complete for
its worker (a failed inventory is a 503, never a short list) and sandboxd kills
each sandbox at its persisted deadline, but Gitmoot still treats the sandboxd
list as partial: it reads the list and its ledger in separate steps, so an
attempt reserved in between would look absent. A `destroying` sandboxd attempt
settles by the same provider-TTL grace rule as E2B.

sandboxd OMP uploads only the configured absolute-path executable after verifying
an executable Linux ELF header and interpreter for the provider's guest
architecture, before provider allocation. No cross-architecture fallback or
emulation is attempted. Supply a real Linux build of OMP for that architecture
and a matching guest image and credential gateway. The
private endpoint's control, upload, streaming, and token behavior must be
verified before a canary; configuring these values alone does not assert
compatibility.

### Opt-in sandboxd-linux AMD64 provider

`sandboxd-linux` is a second, independent sandboxd gateway: sandboxd running
its Firecracker driver on the Gitmoot host itself (see sandboxd's
`docs/firecracker.md`). It is not enrolled behind the Mac gateway. It is
declared in its own section with the same keys as `[remote_exec.sandboxd]`;
`[remote_exec.sandboxd-linux]` and the quoted `[remote_exec."sandboxd-linux"]`
are the same section. Everything is per provider: endpoint, API key,
templates, guest gateway origin, OMP upload, `max_concurrent`, the capacity
report and its cache, and the ledger rows (`provider = 'sandboxd-linux'`), so
filling one gateway never consumes or frees the other's slots. Select it with
`--exec-provider sandboxd-linux` or `checks_provider = "sandboxd-linux"`.

The guest architecture is declared by which OMP key is set:
`omp_linux_arm64_file` uploads to ARM64 guests and `omp_linux_amd64_file` to
AMD64 guests. Setting both is a load error. The key name carries the
architecture, so a separate `guest_arch` key cannot disagree with it, and
`[remote_exec.sandboxd]` stays byte-for-byte valid. A missing file, a
non-executable file, or an ELF for the other architecture is refused by
`gitmoot doctor` (the `remote exec config` check) and when the provider is
requested, not mid-review.

Firecracker guests can reach exactly one host port, at slirp4netns's host
alias `10.0.2.2` (sandboxd `-fc-host-port 8443`); slirp forwards it to the
host's `127.0.0.1:8443`. So the provider's `credential_gateway_url` is
`https://10.0.2.2:<credential_gateway_listen port>`, and the listener must
accept loopback connections (`0.0.0.0:8443` does). The gateway certificate
gets the IP SAN `10.0.2.2` because the address is advertised; guests dial the
IP and send no SNI.

```toml
[remote_exec]
# ... cloud E2B keys and [remote_exec.sandboxd] unchanged ...
credential_gateway_listen = "0.0.0.0:8443"
credential_gateway_url = "https://203.0.113.7:8443"

[remote_exec."sandboxd-linux"]
api_key_file = "/run/secrets/sandboxd-linux-key"
template = "review-amd64"
omp_template = "review-amd64"   # at least 2 GiB RAM
base_url = "http://127.0.0.1:43190"
envd_base_url = "http://127.0.0.1:43190"
omp_linux_amd64_file = "/opt/gitmoot/omp-linux-amd64"
credential_gateway_url = "https://10.0.2.2:8443"
max_concurrent = 2
```

Automatic review routing is opt-in and job-scoped. With no policy, even a
process-wide remote backend setting does not reroute reviews. For example:

```toml
[review]
remote_routing_enabled = true
remote_purposes = ["security"] # code, security, ui, or architecture
remote_final_reviews = false  # opt in to every ready, green exact head

[repos."owner/repo".review]
remote_routing_enabled = false # repo override; keep this repo local
```

For background reviews, a configured purpose or high-risk path/`risk:high`
label selects E2B only when the PR is open, not a draft, the requested head is
current, and at least one current-head CI check exists with no pending or failed
checks. Auth/security, credentials, sandbox/execbackend, deployment/release,
lifecycle paths and their matching CLI files are protected. `risk:routine`
overrides any automatic remote route, including `remote_final_reviews`.
If no purpose, label, final-review rule, or known high-risk path qualifies,
the review stays local. With
`remote_final_reviews = true`, any ready exact-head review with green CI is
eligible. Drafts, stale heads, missing/red CI, unsupported runtimes, and
foreground reviews stay local. An explicit per-job `--exec-backend` always
wins. Each dispatch persists a `review_backend_route_selected` event with its
reason and the chosen backend. The remote worker rechecks current head and CI
immediately before reserving capacity; a later red result refuses the cloud job.

### Per-repository checks routing

A repository whose review checks need a toolchain the local review seat lacks
(Swift, Rust, Node, Flutter) can send every review to a remote provider and
image (#2316):

```toml
[repos."owner/repo".review]
checks_backend = "remote"          # the only value
checks_provider = "e2b"            # or "sandboxd" / "sandboxd-linux"; default "e2b"
checks_template = "gitmoot-swift"  # optional; replaces the provider's template
```

- Every review of that repository runs remotely on `checks_provider` with
  `checks_template`, whichever producer enqueued it: policy-routed,
  `agent review`, `review request`, native PR fan-out, comment-triggered,
  heartbeat and pipeline review stages. The dispatch verbs record the route
  when they enqueue; the worker applies it to any other review before it
  resolves the job's backend, with the same `review_backend_route_selected`
  event. The provider and template are stored in the job payload
  (`exec_provider`, `exec_template`) so retries and model fallbacks keep them.
  The template replaces the provider's `e2b_template`/`e2b_omp_template` for
  that job only.
- The `remote_routing_enabled` gates (CI-green, risk labels, purposes, stale
  head at admission) are skipped: they control spend, and this routing is for
  capability. Repositories without `checks_backend` are unchanged.
- A review without `--exec-provider` (or `exec_provider`) adopts
  `checks_provider`. Refused, with the repository and the setting named, at
  dispatch or when the worker picks the job up: an explicit
  `--exec-backend local`, an `--exec-provider` other than `checks_provider`, a
  foreground review, and a reviewer whose runtime cannot run remotely (only
  `shell` and `omp` can). There is no per-job override; to review the
  repository locally, remove `checks_backend`.
- `checks_provider` is validated when the config loads: an unknown provider, or
  one with no `[remote_exec]`/`[remote_exec.sandboxd]` API key file or no template
  at all, makes every review of that repository refuse with that reason. Other
  repositories keep loading. The `checks_*` keys are repository-scoped; in the
  global `[review]` section they are an error.
- Without `checks_template`, the template depends on the reviewer's runtime:
  `omp` provisions the provider's `e2b_omp_template`, `shell` its
  `e2b_template`. A provider that sets only one of them serves only that
  runtime; a review on the other is refused naming the missing key.
- When the provider's cost or concurrency cap is full, the review returns to
  the queue instead of failing: a `remote_review_cap_waiting` job event names
  the provider and the reason, the next retry time and the wait so far, and
  the scheduler retries on its normal tick once `blocker_retry_at` passes
  (every 30 seconds). When a slot frees, the review provisions and
  `remote_review_cap_admitted` records how long it waited. The wait is bounded
  by the job timeout (one hour when none is resolved); past it the job fails
  with the cap refusal as its reason. An unconfigured cap fails immediately,
  because it never frees. Every sandboxd review, routed here or not, waits the
  same way on a full cap, a `capacity` refusal or a sandboxd create 409; a
  cloud E2B 409 is unchanged.
- A remote review that declares `executed` evidence while its `tests_run`
  reports a missing toolchain (`command not found`, exit 127, "toolchain
  unavailable", "not installed") or names nothing that ran is recorded as
  `static_only`, with a `remote_checks_evidence_clamped` job event.

Actual cost: `[remote_exec].cost_per_hour_usd` is the provider's price for one
sandbox-hour (for a 2 vCPU / 4 GiB E2B template, about `0.166`). When set, each
destroyed attempt records `execbackend_attempts.cost_actual_usd` as its
lifetime, from reservation to teardown, times that rate. The E2B API reports no
per-sandbox cost, so without the key `cost_actual_usd` stays NULL. The sandboxd
provider has no dollar cost and records none. Attempts settled by
reconciliation, whose teardown time is unknown, also stay NULL.

Before reserving cost or calling E2B, remote review admission re-reads the pull
request head and refuses stale jobs, duplicate repo/PR/head/purpose subjects,
unsupported runtime/backend pairs, and unapproved repeat attempts. One accepted
active or terminal review owns its exact-head subject. A cancelled review
releases that ownership. The first cloud attempt is the default; a later
lifecycle generation needs either `gitmoot job retry` or a provider create
conflict classified as authoritative proof that nothing was allocated.

For explicitly selected remote reviews, current-head CI is optional and off by
default. Enable it globally or override it per repository:

```toml
[review]
remote_require_ci_green = true

[repos."owner/repo".review]
remote_require_ci_green = false
```

When enabled, admission requires at least one reported check and every check must
be passing, skipped, or neutral. Policy-routed remote reviews always require
this, regardless of `remote_require_ci_green`.
Refusals record durable `remote_review_admission_avoided_<reason>` job events,
where `reason` is `stale`, `duplicate`, `unsupported`, `red_ci`, or `retry`.

For an engine-driven daemon job, Gitmoot provisions one job-scoped instance, syncs the selected host
checkout into a distinct detached Git worktree, streams runtime commands there,
collects changes, and destroys the instance after the job. The same instance
survives Mailbox repair deliveries. Only an implement job imports changes, and none can be dispatched; a review
returns its FINDINGS in the result envelope and needs no change set. An
implement job's changes would return through
the bounded transactional `BuildChangeSet` / `ImportChangeSet` transport before
result observation; host Git commands and the finalizer still run against the
host worktree, and backend-created commits are refused.

On the remote backend the instance workspace is a Git repository whose `HEAD`
is the exact host commit, not a copy re-committed under a new SHA. Gitmoot
fetches that commit and its history back to the review diff base into a
throwaway host repository and uploads only its object store and shallow
boundary, plus a patch of any uncommitted host changes. No host `.git`
directory, config, remote, credential helper or GitHub token is uploaded, and
the instance has no `origin`. For a pull-request review the diff base is the
merge base with the refreshed `origin/<base>` (or the prior reviewed head for a
bounded re-review); the instance gets it as the local branch
`gitmoot-review-base`, which the checked-out `gitmoot-head` branch tracks, so
`git status`, `git log @{upstream}..` and `git diff @{upstream}` show the
review scope. A remote review also receives the same rendered, repo-scoped
prior-verdict list a host read-only seat gets, including each verdict's
individual findings (`finding_details`) as recorded, at
`/home/user/.gitmoot/runtime/evidence/prior-verdicts.json`, named by
`GITMOOT_PRIOR_VERDICTS`. If the list cannot be rendered the review still runs
and the job records a `remote_review_evidence_unavailable` event.

`local_uid` and `local_gid` opt agent commands into a kernel-enforced non-root
identity. Gitmoot never guesses a host account: both numeric values must be set
together and both must be non-zero. After sync, Gitmoot hands the independent
workspace to that identity; collection still runs in the daemon, temporarily
reclaiming the clone for Git's ownership check, and import creates host files as
the daemon user. Credential-application errors fail the command and never retry
as the daemon user.

The configured identity must be able to traverse every operator-managed parent
of the local backend root. The default root is below the Gitmoot home, so a root
daemon whose home is under `/root` should also set absolute `local_root` to a
dedicated directory beneath a suitable operator-managed parent such as
`/var/tmp`. A filesystem root is rejected. Gitmoot keeps its backend root and
per-instance roots owned by the daemon, assigns their group to `local_gid`, and
uses mode `0710` so the configured command group can traverse them without an
execute bit for the Unix `other` class. This is a group boundary: every process
carrying `local_gid` can traverse, so use a dedicated group whose only member is
the configured execution account when unrelated local users must be excluded.
Gitmoot validates that the numeric UID/GID are paired and non-zero, but cannot
portably prove group exclusivity across host account databases, NSS, or
containers. With no uid/gid configured, local execution retains the daemon
identity for backward compatibility.

The runtime executable and any child-readable file-backed state must also be
reachable by the configured identity. In particular, a root daemon whose
`claude` or `codex` resolves below `/root` must install the runtime at a
traversable location and put that location first in the daemon's `PATH`; do not
make `/root` traversable just to satisfy this requirement. An inaccessible
runtime fails command startup rather than falling back to root.

For daemon jobs that own an execution-backend lifecycle, runtime contract
preflight evaluates UID-dependent requirements against configured `local_uid`.
This lets a root daemon dispatch Claude with `danger-full-access` to a non-root
local backend without weakening Claude's root refusal. Host-only paths such as
`gitmoot job run` do not provision that lifecycle and still evaluate the host
process identity. When `local_uid` is absent, daemon jobs do the same.

The local worktree's `.git` file points at an absolute gitdir in the source
repository. That pointer resolves on the same filesystem, so `local` needs no
history hydration; the remote backend ships exact history as described above. Cancel
kills active command groups and destroys the instance. On daemon restart, the
remote reaper positively observes provider inventory and matches the complete
sandbox identity (job, attempt, generation, fencing token, boot ID, and sandbox
ID) against the durable local ledger before deleting an old-boot or dead-owner
sandbox. A foreign account sandbox with no matching ledger row is never deleted.
The periodic daemon pass checks active remote ledger rows even when `local` is
the configured default; a restart does not require a new remote dispatch to
reclaim an old-boot sandbox. With no active remote rows, the local-first pass
does not query E2B inventory. A provider-only allocation without a matching
ledger row is not deleted by Gitmoot; it remains subject to the provider TTL.
An incomplete E2B inventory cannot prove a missing sandbox was destroyed; only
provider-confirmed deletion releases its cost reservation. The one exception is
an attempt left in `destroying` (its teardown DELETE was inconclusive): once a
successful inventory pass no longer lists its sandbox AND the attempt is past
its `ttl_expires_at` plus a 15-minute grace, E2B's own timeout has killed the
sandbox, so the attempt settles to `destroyed` with an
`execbackend_destroy_confirmed` job event and frees its cap slot. A per-ID 404
never settles it (E2B returns that for an inaccessible live sandbox too), and a
failed inventory keeps the slot. A provider with a complete inventory settles
on absence alone. A partially-created non-empty directory that Git never registered remains the
known orphaned-but-present cleanup limitation tracked in #1572.

## Proving a Parallel Local Wave

Run the proof with a dedicated `--home`; never point it at the live Gitmoot
home. Keep the source checkout under a path the configured identity can
traverse, set `backend = "local"`, and give every Claude leg a distinct
`fresh:<suffix>` session:

```sh
gitmoot agent subscribe gate-a --runtime claude --session fresh:gate-a \
  --role reviewer --repo OWNER/REPO --policy danger-full-access \
  --capability ask --home "$PROOF_HOME"
```

Keep the daemon running in its own shell:

```sh
gitmoot daemon run --repo OWNER/REPO --parallel 4 --poll 1s \
  --home "$PROOF_HOME"
```

From the dispatch shell, submit every leg together. Gitmoot no longer
dispatches implementation (#2203), so the legs are background `ask` jobs; a
`danger-full-access` ask still writes, which is all this gate needs — it proves
the execution backend's identity and isolation, not the implement finalizer:

```sh
gitmoot agent ask gate-a "record uid, gid, pwd, start, end, and visible markers" \
  --repo OWNER/REPO --background --home "$PROOF_HOME"
```

Dispatch the other legs together, not after the first settles. Every leg's
`gitmoot_result` should include its UID, GID, workspace, start and end
timestamps, and visible marker set in `summary`; its marker in `changes_made`;
and the read-back and visibility checks in `tests_run`.

The proof passes only when all of these are independently observed:

- every runtime delivery returns a parsed `gitmoot_result`;
- the reported UID/GID equal the configured non-root identity;
- the intervals overlap with measured peak concurrency equal to the leg count;
- every workspace path is distinct, and each leg sees only its own marker;
- the isolated ledger contains zero remote execution attempts, and no E2B
  credential or `remote` selector is present.

One criterion is weaker than it was when this gate dispatched implement legs.
A background taskless `ask` is auto-isolated into a detached committed-tip
worktree only when same-repo readers are **contended**, and that isolation is
deliberately fail-open — allocation failure records
`readonly_worktree_skipped` and falls back to the registered checkout. Four
simultaneous legs do contend, so in practice the paths are distinct, but read
the reported workspace from each `gitmoot_result` and treat distinctness as
OBSERVED, never assumed. When the gate needs guaranteed writable per-job
isolation instead, run the legs as pipeline `action: produce` stages with
declared `writes:` roots: produce is mutating, live, and sandboxed per job.

Gitmoot reads `[remote_exec]` when it dispatches a job. Once the process starts
a remote credential listener for a home, those listener coordinates are
immutable: a later dispatch with changed coordinates fails loudly until the
daemon restarts instead of silently reusing a stale endpoint. Foreground
dispatch refuses `remote` because it has no daemon-owned lifecycle, ledger, or
reaper.

When `[credentials].model_gateway = true`, a remote shell or OMP job receives the
non-secret route in `GITMOOT_CREDENTIAL_GATEWAY_URL` and a path to an owner-only
curl configuration in `GITMOOT_CREDENTIAL_GATEWAY_CURL_CONFIG` for the
sandbox-reachable credential gateway. The second listener requires a per-job
mTLS certificate and an opaque
capability bound to the sandbox id, runtime, job lease expiry,
and the exact upstream allowlist. Provider keys remain host-side and are loaded
only after those checks pass. The route is revoked before sandbox teardown.
Before release, the host refuses an initial residual `Content-Encoding` or an
unexpected HTTP/2 response. It drops response field names containing the key,
redacts key bytes from remaining field values, and incrementally filters body
bytes with bounded carry-over so matches split across transport chunks are
removed without delaying streamed responses until EOF. Standard reversible
URL/base encodings remain best-effort defense in depth. The contract covers an
accidental exact-byte reflection by the trusted, operator-selected upstream;
malicious upstreams and transformed application payloads, including
application-layer compression without `Content-Encoding`, are out of scope.
Standalone Claude, Codex, and Kimi runtimes remain unsupported on `remote`
until their clients can target this mTLS path. OMP uses an instance-local HTTP
forwarder for Anthropic and a job-scoped `models.yml` that routes the
`openai-codex`, `xai-oauth` and `devin` providers (for example
`openai-codex/gpt-6-sol`, `xai-oauth/grok-4.7` and `devin/swe-2`) through OMP's
native gateway transport. The sandbox receives only an mTLS identity, capability,
loopback route, and inert placeholder. Provider credentials must exist in the
host OMP broker, which also performs any OAuth refresh; Gitmoot never supplies
a raw provider key as a fallback. Any other provider has no credential in the
sandbox, so a remote review's model fallback skips such pool entries (event
`review_model_remote_skipped`) and leaves them to local reviews.

For these models, `[credentials].model_gateway_key` must name a proxied
host key whose upstream is the host OMP auth-gateway, backed by broker
credentials for Codex, xAI and Devin. Allow that upstream host in
`model_gateway_allow_hosts`; a loopback upstream also requires
`model_gateway_allow_loopback_upstream = true`. The default
`api.anthropic.com` upstream cannot serve OMP's `/v1/pi/stream` route.
The uploaded host OMP binary must support `pi-native` (verified with 18.2.11).

A job payload's `exec_backend` field overrides the config value for that one
job. When either selector is explicitly present its value must be non-blank;
an absent selector defaults to `local`, while `backend = ""` or an explicit
`"exec_backend":""` fails loudly.

Any value outside the allowed set (`local`, `remote`) **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local, remote)`).
There is no silent fallback.
