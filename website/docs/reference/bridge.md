---
sidebar_position: 9
---

# Bridge

`gitmoot bridge serve` runs a small authenticated HTTP server over the local
Gitmoot store, so external automation that cannot shell out (for example a
workflow tool running in a container) can start pipelines, recall memory,
and enqueue agent asks.

## Security model

- Localhost-only by default. A non-loopback `--addr` requires `--allow-remote`,
  which is dangerous and should be paired with your own network controls.
- Single bearer token, generated on first serve into `bridge.token` (0600) in
  the Gitmoot home. `gitmoot bridge token` prints the file path (never the
  token); `--rotate` regenerates it.
- The bridge adds no new authority: every endpoint calls the same internal
  functions the CLI commands use, so all existing gates apply.
- 30 requests/minute, 1MB request bodies, one audit log line per call.

## Endpoints

| Method | Path | Effect |
| --- | --- | --- |
| POST | `/v1/pipelines/{name}/run` | start a pipeline run (409 when one is active) |
| GET | `/v1/runs/{id}` | pipeline run state and stages |
| POST | `/v1/memory/recall` | ranked confirmed-memory lookup (`{query, repo?, agent?, shared?, limit?}`) |
| GET | `/v1/jobs/{id}` | job state and result payload |
| POST | `/v1/agents/{name}/ask` | enqueue a background ask (`{message, repo, model?, runtime?}`) |

## Reaching the bridge from a container

Use `http://host.docker.internal:8791` (Docker Desktop) or the docker bridge
IP such as `http://172.17.0.1:8791` on Linux, passing the token from
`bridge.token`. This is the seam the planned Activepieces integration uses.
