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
```

`local` is the default and the only implemented backend. For an engine-driven
daemon job, Gitmoot provisions one job-scoped instance, syncs the selected host
checkout into a distinct detached Git worktree, streams runtime commands there,
collects changes, and destroys the instance after the job. The same instance
survives Mailbox repair deliveries. An implement job's changes return through
the bounded transactional `BuildChangeSet` / `ImportChangeSet` transport before
result observation; host Git commands and the finalizer still run against the
host worktree, and backend-created commits are refused.

The local worktree's `.git` file points at an absolute gitdir in the source
repository. That pointer resolves on the same filesystem, so `local` needs no
bundle/base-ref hydration; hydration remains a remote-provider concern. Cancel
kills active command groups and destroys the instance. A restarted daemon reaps
instances whose recorded owner process is gone. A partially-created non-empty
directory that Git never registered remains the known orphaned-but-present
cleanup limitation tracked in #1572.

Gitmoot reads `[remote_exec]` when it dispatches a job; it is not cached and
needs no SIGHUP wiring. Foreground dispatch retains the host runner path because
the lifecycle is acquired at the daemon job-worker boundary.

A job payload's `exec_backend` field overrides the config value for that one
job. When either selector is explicitly present its value must be non-blank;
an absent selector defaults to `local`, while `backend = ""` or an explicit
`"exec_backend":""` fails loudly.

Any value outside the allowed set — currently only `local` — **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local)`). There
is no silent fallback. Remote backends (see the parallel-isolated-execution
epic, #1529) will extend the allowed set in later phases.
