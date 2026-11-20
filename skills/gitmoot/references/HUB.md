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
`NAME.pem` and `NAME.key` (mode 600) into `--out`, which must be new or empty.
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

Each endpoint calls the same code as its CLI command, so a remote send is
checked exactly like a local one. Every write is recorded in `hub_writes` with
the machine and role that made it. Org changes, merges, deploys and config are
not served, and will not be: they stay on the hub machine.

```sh
curl --cacert ca.pem --cert mac-studio.pem --key mac-studio.key \
  https://100.106.218.88:7711/v1/whoami
```
