---
sidebar_position: 9
---

# Bridge

`gitmoot bridge serve` runs a small authenticated HTTP server over the local
Gitmoot store, so external automation that cannot shell out (for example a
workflow tool running in a container) can read job state and enqueue agent
asks.

## Security model

- Localhost-only by default. A non-loopback `--addr` requires `--allow-remote`,
  which is dangerous: the bridge speaks plain HTTP, so a remote bind sends the
  bearer token in cleartext. Use it only behind your own network controls
  (firewall, private interface, or a TLS-terminating reverse proxy).
- Single bearer token, generated on first serve into `bridge.token` (0600) in
  the Gitmoot home. `gitmoot bridge token` prints the file path (never the
  token); `--rotate` regenerates it.
- The bridge adds no new authority: every endpoint calls the same internal
  functions the CLI commands use, so all existing gates apply.
- 30 requests/minute, a 1 MiB request-body cap, one audit log line per call.

## Endpoints

| Method | Path | Effect |
| --- | --- | --- |
| GET | `/v1/jobs/{id}` | job state and result payload |
| POST | `/v1/agents/{name}/ask` | enqueue a background ask (`{message, repo, model?, runtime?}`) |

## Reaching the bridge from a container

Use `http://host.docker.internal:8791` (Docker Desktop) or the Docker bridge
IP, such as `http://172.17.0.1:8791` on Linux, passing the token from
`bridge.token`.
