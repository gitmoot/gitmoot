# Activepieces bootstrap

`gitmoot activepieces setup` creates a local Activepieces installation and wires
it to the authenticated Gitmoot bridge. The resulting workflow builder can run
Gitmoot pipelines and enqueue agent jobs without giving Activepieces direct
access to the Gitmoot database or daemon internals.

## Quick start

Docker with the Compose plugin must be available. Then run:

```sh
gitmoot activepieces setup --yes
```

The command performs the complete local bootstrap:

1. Creates or reuses `~/.gitmoot/bridge.token`.
2. Starts `gitmoot bridge serve` in the background if the selected bridge
   address is not already listening.
3. Writes an Activepieces Compose stack under
   `~/.gitmoot/activepieces/` and starts Activepieces 0.82.0, Postgres 16, and
   Redis 7. Redis queue mode is required for reliable container-to-host calls.
4. Waits for Activepieces to become healthy.
5. Creates the local admin account, or signs into an existing account.
6. Installs `@gitmoot/piece-gitmoot` from public npm.
7. Creates the project-scoped `gitmoot-bridge` custom-auth connection.
8. Imports the starter flows unless `--no-templates` is set.

Open `http://localhost:8080` when setup finishes. When setup generates an admin
password, it prints it once and saves it in
`~/.gitmoot/activepieces/ADMIN_CREDENTIALS.txt` with mode `0600`.

The generated `.env` is also mode `0600`. Re-running setup preserves
`AP_ENCRYPTION_KEY`, `AP_JWT_SECRET`, and the Postgres password because changing
those values would make existing encrypted connections unreadable. Only the
port and frontend URL are updated.

## Bridge architecture

The Gitmoot bridge is intentionally local. On Linux, setup binds it to the
Docker bridge gateway at `172.17.0.1:8791` and enables the bridge's explicit
non-loopback guard. On macOS and Windows with Docker Desktop, it binds to
`127.0.0.1:8791`. In both cases, the Activepieces container calls:

```text
http://host.docker.internal:8791
```

The Compose stack maps `host.docker.internal` to the host gateway. A cloud
Activepieces deployment cannot reach this local bridge. `--url` is therefore
for an existing local Activepieces instance, or an instance with a separately
secured network path back to this machine.

The Linux bridge address is reachable from local containers, so keep the host
firewall in place and do not expose port 8791 publicly. Override the bind and
container-facing URL together when the Docker network uses a different gateway:

```sh
gitmoot activepieces setup \
  --bridge-addr 192.168.65.1:8791 \
  --bridge-url http://host.docker.internal:8791
```

Use `--no-bridge-spawn` when another supervisor already runs the bridge. Setup
then requires the configured address to be listening and never launches a
duplicate process.

## Setup flags

```text
--home path                Gitmoot home parent directory
--url URL                  existing local Activepieces URL; skip Docker
--port 8080                local Activepieces port
--piece-version version    npm version; latest is resolved when omitted
--email address            admin email (admin@gitmoot.local)
--password value           admin password; generated when omitted
--bridge-addr host:port    bridge bind selected for the current OS
--bridge-url URL            URL used inside Activepieces
--no-bridge-spawn          require an already-running bridge
--recreate-connection      replace the gitmoot-bridge connection
--compose-project name     Docker Compose project name
--no-templates             skip starter flow import
--yes                      accept setup prompts
```

If npm version lookup is temporarily unavailable, setup omits the version and
lets Activepieces resolve the latest registry release. Pass `--piece-version`
for a fully pinned bootstrap.

## Starter templates

List the embedded templates without contacting Activepieces:

```sh
gitmoot activepieces templates list
```

Import all templates into the default local instance:

```sh
gitmoot activepieces templates import
```

Import selected IDs, with flags before the IDs:

```sh
gitmoot activepieces templates import \
  --url http://localhost:8080 \
  webhook-run-pipeline gmail-imap-ask-agent
```

The importer lists existing flows first and skips a template when its display
name already exists.

### `webhook-run-pipeline`

This flow receives an unauthenticated Activepieces webhook and calls the
Gitmoot piece's `run_pipeline` action. Edit the `<your-pipeline>` placeholder
before publishing the flow. Add webhook authentication or an upstream access
control when the endpoint is not confined to a trusted network.

### `gmail-imap-ask-agent`

This flow uses the official IMAP `new_email` trigger, calls `ask_agent`, and
sends an acknowledgement through the official SMTP `send-email` action. Fill
in the IMAP and SMTP connections, `<agent>`, `<owner/repo>`, and sender address.
The repo value is required; the bridge rejects an empty repo.

`ask_agent` is asynchronous and returns only a `job_id`. The supplied SMTP
reply therefore acknowledges that the job was queued. Returning the agent's
full answer requires a future polling or callback flow.

For Gmail mailbox credentials and app authorization, continue with
[Gmail setup](gmail.md). The OAuth-native Gmail template is intentionally
deferred until that connection flow can be shipped without a broken placeholder.

## Stop or remove the local stack

Stop the containers while preserving Postgres and Redis data:

```sh
gitmoot activepieces down
```

Also remove the named data volumes:

```sh
gitmoot activepieces down --volumes
```

Removing volumes deletes local Activepieces accounts, flows, connections, and
execution state. The files under `~/.gitmoot/activepieces/` remain available for
a later setup.
