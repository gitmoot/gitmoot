# Runtime Ambient Credential Hygiene

Gitmoot can curate the environment of runtime-agent subprocesses. The feature is
off by default and applies to Codex, Claude Code, Kimi Code, legacy `kimi-cli`,
and shell runtime adapters in foreground and daemon-worker delivery paths.

```toml
[credentials]
env_curation = true
env_passthrough = ["GOCACHE", "NPM_*"]
github = "deny"
```

`env_curation = false` preserves the historical behavior: runtime commands
inherit the daemon or foreground shell environment unchanged. Gitmoot reads this
section when it constructs an adapter for a job; it is not cached and needs no
SIGHUP wiring.

## Curated environment

When curation is enabled, Gitmoot forwards only these exact base names:

- `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`
- `TMPDIR`, `TMP`, `TEMP`, `TZ`
- `LANG`, `LANGUAGE`, `TERM`, `COLORTERM`, `NO_COLOR`, plus every `LC_*` name
- `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`
- `GOTOOLCHAIN`
- `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`,
  `GIT_COMMITTER_EMAIL`
- `GITMOOT_HOME`

Runtime-specific additions are:

- Codex: `CODEX_HOME`
- Claude Code: `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, and `CLAUDE_CONFIG_DIR`. This is a transitional P1
  exception; P1 does not relocate Claude state or replace its runtime auth.
- Kimi Code, legacy `kimi-cli`, and shell: no additions. Shell stage variables,
  chat-relay variables, pipeline metadata, and the upstream-context file variable
  are job-owned injections and are appended after the curated base.

`SSH_AUTH_SOCK`, `GH_*`, `GITHUB_*`, proxy variables, and toolchain cache
variables such as `GOCACHE` are not in the base list. Add non-secret operational
variables with `env_passthrough`. Each entry is an exact name or a single
trailing-`*` prefix glob, such as `GOCACHE` or `NPM_*`. Names containing `=` or
NUL and non-trailing `*` forms are rejected.

## GitHub denial and opt-out

With curation on, `github = "deny"` is the default. Ambient `GH_*` and
`GITHUB_*` values, including `GH_TOKEN`, `GITHUB_TOKEN`,
`GH_ENTERPRISE_TOKEN`, and `GITHUB_ENTERPRISE_TOKEN`, are omitted even if an
`env_passthrough` glob would otherwise match them. Gitmoot injects
`GH_PROMPT_DISABLED=1` and points `GH_CONFIG_DIR` at a fresh, empty, delivery
scratch directory with mode `0700`. The directory is removed when the runtime
subprocess completes on success, failure, timeout, or cancellation.

Set `github = "inherit"` as an explicit opt-out. Gitmoot then forwards ambient
`GH_*` and `GITHUB_*` variables and does not inject a GitHub config directory or
disable prompts.

## Limits

This is ambient credential hygiene and denial, not egress confinement and not a
network proxy. It creates no placeholder tokens and changes no proxy settings.
Runtime sandboxes can still read credential files that are visible on disk;
the current Linux Landlock policy includes read-only `/`. SSH keys, SSH agents,
Git credential helpers, and direct network access are also untouched. Network
proxy enforcement is P2, and narrower Landlock read rules are P3.
