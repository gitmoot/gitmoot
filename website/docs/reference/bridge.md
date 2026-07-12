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
  which is dangerous: the bridge speaks plain HTTP, so a remote bind sends the
  bearer token in cleartext. Use it only behind your own network controls
  (firewall, private interface, or a TLS-terminating reverse proxy).
- Single bearer token, generated on first serve into `bridge.token` (0600) in
  the Gitmoot home. `gitmoot bridge token` prints the file path (never the
  token); `--rotate` regenerates it.
- The bridge adds no new authority: every endpoint calls the same internal
  functions the CLI commands use, so all existing gates apply.
- 30 requests/minute, a 1 MiB general request-body cap, one audit log line per
  call. Pipeline-run requests have a stricter 64 KiB cap.

## Endpoints

| Method | Path | Effect |
| --- | --- | --- |
| POST | `/v1/pipelines/{name}/run` | start a pipeline run (409 when one is active) |
| GET | `/v1/runs/{id}` | pipeline run state and stages |
| POST | `/v1/memory/recall` | ranked confirmed-memory lookup (`{query, repo?, agent?, shared?, limit?}`) |
| GET | `/v1/jobs/{id}` | job state and result payload |
| POST | `/v1/agents/{name}/ask` | enqueue a background ask (`{message, repo, model?, runtime?}`) |

## Start a pipeline with input

`POST /v1/pipelines/{name}/run` accepts no body, `{}`, or:

```json
{
  "payload": {
    "subject": "Build failed",
    "sender": "alerts@example.com"
  }
}
```

`payload` is strictly a JSON object of string values. Unknown top-level fields,
non-object payloads, and non-string values are rejected. Payload transport is
available to every enabled, repo-bound pipeline; the pipeline does not need a
`trigger:` block.

Limits reject the request rather than truncating it:

- raw request body: 64 KiB;
- entries: at most 32;
- key: 1–64 bytes matching `^[a-z][a-z0-9_]*$`;
- value: at most 32 KiB of valid UTF-8 and no U+0000;
- decoded keys plus values: at most 48 KiB.

The successful response remains `200 {"run_id":"prun-…"}`. Errors are:

| Status | Meaning |
| --- | --- |
| `400` | Invalid schema or payload rule; disabled pipeline; pipeline has no repo. |
| `404` | Pipeline not found. |
| `409` | The pipeline already has an active run. Requests are not queued or deduplicated. |
| `413` | The 64 KiB pipeline-run body cap was exceeded (`{"error":"request body too large"}`). |

The validated map is stored canonically in the pipeline run's SQLite row. Shell
stage environment entries and agent-stage prompt projections are retained with
normal job data; treat external content as stored Gitmoot data.

## Reaching the bridge from a container

Use `http://host.docker.internal:8791` (Docker Desktop) or the docker bridge
IP such as `http://172.17.0.1:8791` on Linux, passing the token from
`bridge.token`. This is the seam the gitmoot Activepieces piece uses - see
[Connect Gmail To A Pipeline](../workflows/gmail-pipeline-workflow.md).
