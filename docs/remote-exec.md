# Execution Backend Selection

Gitmoot names where a job's runtime subprocess executes through the
**execution backend** seam (`internal/execbackend`, issue #1535 contract,
#1536). The seam is deliberately distinct from Landlock **local confinement**
(`internal/sandbox`, the `sandbox` CLI, agent path grants), which restricts
what a locally-running subprocess may touch and is unaffected by backend
selection.

```toml
[remote_exec]
backend = "local"
```

`local` is the default and the only implemented backend. It is a
byte-for-byte passthrough: the runner composes in exactly the historical
order (credential gateway when configured, the Landlock produce wrapper for
claude/kimi produce jobs with path grants, `subprocess.GroupRunner{}`
innermost), and a config file with no `[remote_exec]` section behaves
identically. Gitmoot reads this section when it dispatches a job; it is not
cached and needs no SIGHUP wiring.

A job payload's `exec_backend` field overrides the config value for that one
job.

Any value outside the allowed set — currently only `local` — **fails the job
loudly at dispatch**: the failure names the offending value and the allowed
set (for example `unknown execution backend "e2b" (allowed: local)`). There
is no silent fallback. Remote backends (see the parallel-isolated-execution
epic, #1529) will extend the allowed set in later phases.
