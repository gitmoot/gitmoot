# Runtime Ambient Credential Hygiene

Gitmoot can curate the environment of runtime-agent subprocesses. The feature is
off by default and applies to Codex, Claude Code, Kimi Code, legacy `kimi-cli`,
and shell runtime adapters in foreground and daemon-worker delivery paths.

```toml
[credentials]
env_curation = true
env_passthrough = ["GOCACHE", "NPM_*"]
github = "deny"
model_gateway = false
model_gateway_allow_hosts = ["api.anthropic.com"]
# keychain_path = "/absolute/operator/path/keychain.env"
```

`env_curation = false` preserves the historical behavior: runtime commands
inherit the daemon or foreground shell environment, except for the Claude auth
overlay described below. Gitmoot reads this section when it constructs an
adapter for a job; it is not cached and needs no SIGHUP wiring.

## Claude runtime auth

`~/.gitmoot/runtime-auth.env` is the authoritative Claude auth source and is
always mode `0600`. Create or rotate it with `gitmoot auth set claude`; the
token is read from stdin and never appears in argv. Rotation is visible to the
next foreground or daemon delivery without a restart. `gitmoot auth status`
reports sources, masked fingerprints, mtime, and permissions without making a
runtime call. `gitmoot auth probe claude` performs a paid fresh-session live
check. `gitmoot auth unset claude` atomically writes an explicit empty file.

Managed names are `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, and
`ANTHROPIC_AUTH_TOKEN`. When the file contains any one of them, Gitmoot injects
all three and blanks absent names so an ambient API key cannot override a
file-selected OAuth token. An explicit-empty file injects nothing and leaves
Claude's ambient/credential-store fallback intact. If the authoritative file is
missing, Gitmoot imports legacy `daemon-runtime.env` once, or otherwise seeds
from ambient managed variables once. Existing files are never overwritten.
For systemd deployments, keep `daemon.env` for operational values such as
`PATH`; do not duplicate Claude secrets there after bootstrap.

## Key registry and grants

The shared key source is an operator-owned env file. Its default path
is `<base-home>/.config/gitmoot/keychain.env`; `gitmoot key path` prints the
resolved path and validation status. `[credentials] keychain_path` can select a
different absolute path. The file must be regular, owned by the current uid,
mode exactly `0600`, and outside the Gitmoot home and managed checkouts. Gitmoot
revalidates and rereads it at every registration, preflight, and delivery, so an
atomic rewrite rotates values without a daemon restart.

The `key` commands manage names, modes, and grants only. They never accept or
print values:

```sh
gitmoot key path
gitmoot key add SOURCE_API_TOKEN --mode injected
gitmoot key grant SOURCE_API_TOKEN --pipeline nightly-sync
gitmoot key add PARTNER_API_TOKEN --mode proxied
gitmoot key configure PARTNER_API_TOKEN --upstream https://api.partner.example/v1 --auth bearer
gitmoot key grant PARTNER_API_TOKEN --agent researcher
gitmoot key list --json
gitmoot key show SOURCE_API_TOKEN
gitmoot key revoke PARTNER_API_TOKEN --agent researcher
gitmoot key revoke SOURCE_API_TOKEN --pipeline nightly-sync
gitmoot key rm SOURCE_API_TOKEN       # refuses while grants remain
gitmoot key rm SOURCE_API_TOKEN --force
```

`rm` removes registry metadata and grants, not the operator's file entry.
`proxied` keys must be configured before they can be granted. `--auth bearer`
places the real value in the upstream `Authorization` header; `--auth
header:X-Api-Key` uses an approved custom header. The upstream must be an
absolute HTTPS origin and its normalized base path is pinned. Pipeline shell
stages may request pipeline-granted `injected` or configured `proxied` names
with `env_keys`. An agent stage can request only a configured `proxied` key,
and both the named agent seat grant and that stage's explicit `env_keys`
selector are required. Injected agent grants are always refused; gate stages
remain ineligible. Shell resolution is Gitmoot's reserved `GITMOOT_*`
namespace, then the pipeline's own `env_file`, then a granted shared key, then
inline non-secret `env`. Agent stages never resolve from the pipeline file,
inline defaults, or pipeline grants. Registered but ungranted names are not
visible to exact or glob selectors.

For a proxied key, the selected shell or agent process receives a per-job
`gitmoot-kc-...` placeholder in the requested variable and a
`GITMOOT_PROXY_<KEY>_URL` loopback URL. The real value never enters an agent
process. Gitmoot
rechecks the grant and configuration and rereads the keychain on every request,
so rotation applies to the next request and revocation fails closed with `401`.
The lease is revoked when the stage delivery ends.

Proxied mode hides key bytes; it does **not** prevent an authorized child from
exercising the credential on the pinned upstream. Curated upstreams and base
paths are part of the model. A malicious or compromised upstream could still
reflect a credential, so configure only trusted services.

## Claude model gateway

`model_gateway = true` opts Claude deliveries into a daemon-owned loopback
model gateway. The gateway listens only on an OS-assigned `127.0.0.1` port. For
each job, Gitmoot snapshots the selected real credential into daemon memory,
mints a random `gitmoot-kc-...` placeholder, and gives the Claude child only
that placeholder plus `ANTHROPIC_BASE_URL` pointing at the loopback listener.
The gateway authenticates the placeholder, replaces it with the real upstream
credential, streams the response, and revokes the placeholder when delivery
ends. Unknown and revoked placeholders receive `401`.

Because Claude Code prefers a cached `~/.claude/.credentials.json` over the
`CLAUDE_CODE_OAUTH_TOKEN` env var, the child is also pointed at a per-home
`CLAUDE_CONFIG_DIR` under the Gitmoot home that mirrors the operator's real
Claude config (settings, skills, commands, `CLAUDE.md`) but omits the cached
credential. Without this the child would authenticate from the cached
credential and ignore the placeholder — the gateway would then `401` the
delivery (#936). The mirror never contains a credential; the operator's real
config is only read, never modified.

Pipeline key delivery is independent of the Claude gateway. `injected` keys
give only a selected shell process the real value. `proxied` keys use generic
per-job loopback leases for selected shell or agent stages; their values are
resolved for each request and they do not alter Claude's model-gateway
placeholder flow. Ordinary non-pipeline agent jobs and delegated children
receive no pipeline-stage keys.

The feature is off by default and currently covers Claude only. A populated
`runtime-auth.env` is required while it is enabled; Gitmoot fails the delivery
instead of falling back to ambient auth, Claude's credential store, or direct
upstream egress. `model_gateway_allow_hosts` is an exact hostname allowlist and
defaults to `api.anthropic.com`. The upstream is fixed by Gitmoot, not selected
by the child. Credentials are read once when the adapter is built, never once
per proxied request, so rotation applies to the next adapter/job.

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
- Claude Code: `CLAUDE_CONFIG_DIR`; the managed auth overlay is appended from
  `runtime-auth.env` after the curated base.
- Kimi Code, legacy `kimi-cli`, and shell: no additions. Shell stage variables,
  chat-relay variables, pipeline metadata, and the upstream-context file variable
  are job-owned injections and are appended after the curated base.

`SSH_AUTH_SOCK`, `GH_*`, `GITHUB_*`, proxy variables, and toolchain cache
variables such as `GOCACHE` are not in the base list. Add non-secret operational
variables with `env_passthrough`. Each entry is an exact name or a single
trailing-`*` prefix glob, such as `GOCACHE` or `NPM_*`. Names containing `=` or
NUL and non-trailing `*` forms are rejected. A bare `*` entry is legal and
passes every ambient variable through (except denied GitHub credentials) --
that keeps GitHub denial while giving up the rest of the curation boundary,
so treat it as a deliberate escape hatch, not a default.

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

The model gateway is credential custody, policy, and attribution, not a hard
egress boundary. These two limits are deliberate:

1. env-var routing is cooperative, not a hard egress boundary — a malicious
   agent can unset `ANTHROPIC_BASE_URL`, or point `CLAUDE_CONFIG_DIR` back at
   the operator's real `~/.claude` to read the cached credential directly. This
   buys credential custody/policy/attribution against a prompt-injected or
   misbehaving agent, not enforcement against one that actively evades.
2. The strong "agents never hold real credentials" claim also requires
   Landlock read-rules for `runtime-auth.env` and `~/.claude/.credentials.json`
   (same-UID read is currently possible) — that is P3.

Codex/Kimi custody, MITM CA support, corporate proxy/`NO_PROXY` interoperability,
Landlock read restrictions, and hard egress enforcement remain P3. SSH keys,
SSH agents, Git credential helpers, and direct network access are untouched.
