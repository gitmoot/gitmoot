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
bundle/base-ref hydration; hydration remains a remote-provider concern. Cancel
kills active command groups and destroys the instance. On daemon restart, the
remote reaper positively observes provider inventory and matches the complete
sandbox identity (job, attempt, generation, fencing token, boot ID, and sandbox
ID) against the durable local ledger before deleting an old-boot or dead-owner
sandbox. A foreign account sandbox with no matching ledger row is never deleted.
An incomplete E2B inventory cannot prove a missing sandbox was destroyed; only
provider-confirmed deletion releases its cost reservation. A partially-created
non-empty directory that Git never registered remains the known
orphaned-but-present cleanup limitation tracked in #1572.

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
Claude, Codex, and Kimi remain unsupported on `remote` until their clients can
target this mTLS path. OMP targets it through an instance-local HTTP forwarder;
the sandbox receives only a job-scoped mTLS identity, capability, and
placeholder. Gitmoot never supplies a raw provider key as a fallback.

A job payload's `exec_backend` field overrides the config value for that one
job. When either selector is explicitly present its value must be non-blank;
an absent selector defaults to `local`, while `backend = ""` or an explicit
`"exec_backend":""` fails loudly.

Any value outside the allowed set (`local`, `remote`) **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local, remote)`).
There is no silent fallback.
