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

`local` is the default. `remote` provisions E2B for engine-driven shell
implement jobs; unsupported job types and model runtimes refuse before
provider allocation. For an
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

For daemon jobs that own an execution-backend lifecycle, runtime contract
preflight evaluates UID-dependent requirements against configured `local_uid`.
This lets a root daemon dispatch Claude with `danger-full-access` to a non-root
local backend without weakening Claude's root refusal. Host-only paths such as
`gitmoot job run` do not provision that lifecycle and still evaluate the host
process identity. When `local_uid` is absent, daemon jobs do the same.

The local worktree's `.git` file points at an absolute gitdir in the source
repository. That pointer resolves on the same filesystem, so `local` needs no
bundle/base-ref hydration; hydration remains a remote-provider concern. Cancel
kills active command groups and destroys the instance. A restarted daemon reaps
instances whose recorded owner process is gone. A partially-created non-empty
directory that Git never registered remains the known orphaned-but-present
cleanup limitation tracked in #1572.

## Proving a Parallel Local Wave

Run the proof with a dedicated `--home`; never point it at the live Gitmoot
home. Keep the source checkout under a path the configured identity can
traverse, set `backend = "local"`, and give every Claude leg a distinct
`fresh:<suffix>` session:

```sh
gitmoot agent subscribe gate-a --runtime claude --session fresh:gate-a \
  --role implementer --repo OWNER/REPO --policy danger-full-access \
  --capability implement --home "$PROOF_HOME"
```

Keep the daemon running in its own shell:

```sh
gitmoot daemon run --repo OWNER/REPO --parallel 4 --poll 1s \
  --home "$PROOF_HOME"
```

From the dispatch shell, submit every leg together:

```sh
gitmoot agent implement gate-a "record uid, gid, pwd, start, end, and visible markers" \
  --repo OWNER/REPO --base HEAD --background --skip-native-review-fanout \
  --home "$PROOF_HOME"
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
Before release, the host refuses an initial residual `Content-Encoding` or an
unexpected HTTP/2 response. It drops response field names containing the key,
redacts key bytes from remaining field values, and incrementally filters body
bytes with bounded carry-over so matches split across transport chunks are
removed without delaying streamed responses until EOF. Standard reversible
URL/base encodings remain best-effort defense in depth. The contract covers an
accidental exact-byte reflection by the trusted, operator-selected upstream;
malicious upstreams and transformed application payloads, including
application-layer compression without `Content-Encoding`, are out of scope.
Claude, Codex, Kimi, and omp remain unsupported on `remote` until their clients
can target this mTLS path; Gitmoot never supplies a raw key as a fallback.

A job payload's `exec_backend` field overrides the config value for that one
job. When either selector is explicitly present its value must be non-blank;
an absent selector defaults to `local`, while `backend = ""` or an explicit
`"exec_backend":""` fails loudly.

Any value outside the allowed set (`local`, `remote`) **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local, remote)`).
There is no silent fallback.
