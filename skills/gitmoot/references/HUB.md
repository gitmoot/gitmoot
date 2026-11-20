# The hub API (#2710)

One gitmoot runs the org: one database, one daemon. The hub is how gitmoot on
another machine reaches it. Off by default.

## Who can call it

A request is served only when all of these hold:

1. It arrives on the `[hub]` listen address, which must be a Tailscale address.
   The daemon refuses any other address, including `0.0.0.0`, so the hub can
   never listen on a public interface by mistake.
2. The peer address is a Tailscale address (`100.64.0.0/10` or
   `fd7a:115c:a1e0::/48`).
3. TLS 1.3 with a client certificate signed by the hub's own certificate
   authority. A certificate from any other authority fails the handshake.
4. The certificate's SHA-256 fingerprint belongs to a machine added with
   `gitmoot hub machine add` that is not revoked and not expired. Revocation is
   checked on every request, so it applies to connections already open.
5. The request acts as one of that machine's roles, and the role is still in
   the org config. A machine with one role uses it; a machine with several
   names one in the `X-Gitmoot-Role` header.

A host firewall in front of the port that admits only fleet machines is
recommended, but the mutual TLS check above is the real gate.

## Configure

```toml
[hub]
listen = "100.106.218.88:7711"          # this machine's Tailscale address
advertise = ["dev2306.tailnet.ts.net"]   # optional extra names in the server certificate
```

The daemon reads `[hub]` at start; a change needs a restart, not a SIGHUP. A hub
that cannot start (a bad address, a port in use) is logged as `hub: not
started: ...` and the daemon carries on without it.

The authority is created on first use under `<home>/hub/ca/` (`ca.pem`, and
`ca.key` with mode 600). The daemon refuses a key other users can read. Back up
that directory: losing it invalidates every machine's certificate.

## Machines

```sh
gitmoot hub machine add mac-studio --role mac-studio --out ./mac-studio-certs   # --days 365 by default, at most 1095
gitmoot hub machine ls [--all] [--json]
gitmoot hub machine revoke mac-studio
```

`add` checks the roles exist, issues a certificate, and writes `ca.pem`,
`NAME.crt` and `NAME.key` (mode 600) into `--out`, which must be new or empty.
The private key exists only in that directory: copy the three files to the
machine, then delete them here. One unrevoked machine per name; to replace a
certificate, revoke the machine and add it again.

## Endpoints

All responses are JSON; errors are `{"error": "..."}`. Message objects have the
same fields as the JSON output of the matching `message` command.

| Method and path | Same as |
|---|---|
| `GET /v1/whoami` | the machine, the acting role and the machine's roles |
| `GET /v1/messages?before=ID&limit=N` | `message inbox` (limit 1-100, default 20) |
| `GET /v1/messages/{id}` | `message show ID` |
| `GET /v1/messages/{id}?thread=1&before=ID&limit=N` | `message show ID --thread` |
| `POST /v1/messages` `{"to", "body", "campaign"?}` | `message send TO BODY [--campaign ID]` |
| `POST /v1/messages/{id}/reply` `{"body"}` | `message reply ID BODY` |
| `GET /v1/org/brief` | `org brief --role <caller's role>` (presence noted as `org brief (hub: MACHINE)`) |
| `GET /v1/tasks/{ref}?repo=OWNER/REPO` | `pr show REF [--repo]` |
| `POST /v1/reviews/level` `{"repo", "pr"}` | `review level`; returns `{"decision", "warnings"}` |
| `POST /v1/reviews` `{"repo", "pr", "head_sha"?, "message", "purpose"?, "reviewer"?, "model"?, "post_merge"?, "no_execution"?}` | `review request --org-role <caller's role>`; returns `{"review", "warnings"}` |
| `POST /v1/notifications/claim` `{"runtime", "event"}` | `message pending --claim --hook EVENT --runtime claude\|codex` for the caller's role; returns `{"hook_output"}` (null when nothing is pending), the same output a local turn hook prints |

Each endpoint calls the same code as its CLI command, so a remote request is
checked exactly like a local one. Every write is recorded in `hub_writes` with
the machine and role that made it.

The task and review endpoints refuse a repository outside the caller role's
org `scope`. A review request's `head_sha`, when given, must be the full
40 characters; omitted, it is the pull request's current head, as locally. The
verdict goes to the caller's role. Host paths and execution overrides
(`--evidence-manifest`, `--related-pr`, `--session`, `--runtime`,
`--exec-backend`, `--exec-provider`, `--supersede`,
`--allow-prompt-head-mismatch`) are not accepted remotely; an unknown field is
refused, not ignored. Org changes, merges, deploys and config are not served:
they stay on the hub machine.

```sh
curl --cacert ca.pem --cert mac-studio.crt --key mac-studio.key \
  https://100.106.218.88:7711/v1/whoami
```

## The client side

On the other machine, a gitmoot home with a `[client]` section is a client of
the hub. It has no org database of its own.

```toml
[client]
hub = "https://100.106.218.88:7711"
certs = "hub-certs"        # ca.pem, NAME.crt, NAME.key; relative to the gitmoot home
name = "mac-studio"        # NAME the certificate was issued for
role = "mac-studio"        # optional: the role to act as when the machine holds several
agent = "mac-studio"       # optional: the local omp agent `client deliver` notifies
```

On a client home:

- `message send|reply|inbox|show`, `org brief`, `pr show`, `review level` and
  `review request` call the hub and print what they print locally. The acting
  role is `--role`, then `GITMOOT_ORG_ROLE`, then `[client] role`; with none,
  the hub uses the machine's only role.
- `review request` refuses the flags the hub does not take, naming them.
- `message pending --claim --hook ...` (the Claude Code and Codex turn hook)
  claims the seat's mail through the hub, so a remote seat gets the same
  turn-hook delivery as a local one.
- Every other command that needs the org database refuses with "this gitmoot
  home is a hub client". It never answers from an empty local store.

An omp seat gets its mail through its machine's omp inbox add-on, which only a
local process can reach. Run the delivery loop on that machine:

```sh
gitmoot client deliver            # every 10s; --once for a single pass
```

Each pass claims the role's pending mail on the hub, hands it to the add-on of
`[client] agent`, and reports the outcome:

| Add-on outcome | Result on the hub |
|---|---|
| Admitted | Delivered, receipt `omp-client:NAME` |
| Refused (nothing reached omp) | Back to pending, with the add-on's reason |
| Unknown | Left attempted for the hub's aged-attempt check; never resent |

The pane is never typed into. On the hub, these are the endpoints
`POST /v1/notifications/omp/claim` and
`POST /v1/notifications/omp/finish {"id", "attempt", "outcome": "delivered"|"deferred", "reason"}`.
A claim is all or nothing for its batch and returns an `attempt` per row;
finish changes the row only while that attempt is still current and the row
is the caller role's, so a late or replayed outcome cannot reopen a newer
attempt.
