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
# For opt-in broker access from a remote shell, configure both:
# credential_gateway_listen = "0.0.0.0:8443"
# credential_gateway_url = "https://broker.example.com:8443"
```

`local` is the default and the only currently provisioned backend. `remote` is
parseable, but its provider is not configured in this slice; unsupported routes
refuse explicitly rather than falling back to host execution. For an
engine-driven daemon job, Gitmoot provisions one job-scoped instance, syncs the selected host
checkout into a distinct detached Git worktree, streams runtime commands there,
collects changes, and destroys the instance after the job. The same instance
survives Mailbox repair deliveries. An implement job's changes return through
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

The local worktree's `.git` file points at an absolute gitdir in the source
repository. That pointer resolves on the same filesystem, so `local` needs no
bundle/base-ref hydration; hydration remains a remote-provider concern. Cancel
kills active command groups and destroys the instance. A restarted daemon reaps
instances whose recorded owner process is gone. A partially-created non-empty
directory that Git never registered remains the known orphaned-but-present
cleanup limitation tracked in #1572.

Gitmoot reads `[remote_exec]` when it dispatches a job. Once the process starts
a remote credential listener for a home, those listener coordinates are
immutable: a later dispatch with changed coordinates fails loudly until the
daemon restarts instead of silently reusing a stale endpoint. Foreground
dispatch refuses `remote` because it has no daemon-owned lifecycle, ledger, or
reaper.

When `[credentials].model_gateway = true`, a remote shell job receives the
non-secret route in `GITMOOT_CREDENTIAL_GATEWAY_URL` and a path to an owner-only
curl configuration in `GITMOOT_CREDENTIAL_GATEWAY_CURL_CONFIG` for the
sandbox-reachable credential gateway. The second listener requires a per-job
mTLS certificate and an opaque
capability bound to the sandbox id, the `shell` runtime, the job lease expiry,
and the exact upstream allowlist. Provider keys remain host-side and are loaded
only after those checks pass. The route is revoked before sandbox teardown.
Provider response headers and streamed bodies are decoded before filtering the
raw key, ASCII-case variants, and standard reversible URL/base encodings across
chunk boundaries. The host stages each filtered response through EOF before
release, then evaluates initial headers and finalized HTTP/1.1 trailers
together. Upstream requests use HTTP/1.1 because Go discards forbidden HTTP/2
encoding trailers before exposing them; any unexpected HTTP/2 response fails
closed.
Claude, Codex, Kimi, and omp remain unsupported on `remote` until their clients
can target this mTLS path; Gitmoot never supplies a raw key as a fallback.

A job payload's `exec_backend` field overrides the config value for that one
job. When either selector is explicitly present its value must be non-blank;
an absent selector defaults to `local`, while `backend = ""` or an explicit
`"exec_backend":""` fails loudly.

Any value outside the allowed set (`local`, `remote`) **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local, remote)`).
There is no silent fallback. Provider construction for `remote` lands in later
phases of the parallel-isolated-execution epic (#1529).
