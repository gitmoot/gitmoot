# Bootstrap Activepieces for Gitmoot workflows

Use the Activepieces bootstrap when you want a visual workflow builder to run
Gitmoot pipelines or enqueue agent jobs through the authenticated local bridge.

## Start the local stack

Install Docker with the Compose plugin, then run:

```sh
gitmoot activepieces setup --yes
```

The command starts Activepieces 0.82.0 with Postgres and Redis, starts the
Gitmoot bridge when needed, creates or signs into the local admin account,
installs the public `@gitmoot/piece-gitmoot` package, creates a
`gitmoot-bridge` connection, and imports two starter flows.

Open `http://localhost:8080` after setup. A generated password is shown once
and stored in `~/.gitmoot/activepieces/ADMIN_CREDENTIALS.txt` with mode `0600`.
Re-running setup preserves the encryption key and database credentials.

For a declarative email-triggered pipeline, the next step is headless:

```sh
gitmoot activepieces connect gmail
gitmoot pipeline add triage-email.yaml --enable
```

`connect gmail` creates and live-validates `gmail-imap`; `--with-smtp` adds an
optional `gmail-smtp` connection for manual send flows. Generated receive flows
use IMAP only. See [Connect Gmail to a pipeline](./gmail-pipeline-workflow.md).

Use another port or an existing local Activepieces instance when needed:

```sh
gitmoot activepieces setup --port 8090 --yes
gitmoot activepieces setup --url http://localhost:8090 --password '<password>' --yes
```

`--url` skips Docker. It does not make a cloud Activepieces instance compatible
with the local bridge.

## Keep the bridge local

The [Gitmoot bridge](/docs/reference/bridge) is a local authority boundary. The
Activepieces container reaches it at `http://host.docker.internal:8791`.
On Linux, setup binds the bridge to the Docker gateway at `172.17.0.1:8791`.
Docker Desktop can reach the loopback bind used on macOS and Windows.

A cloud Activepieces service cannot reach this bridge unless you separately
build and secure a network path to the host. Do not publish bridge port 8791 to
the internet. Use `--bridge-addr` and `--bridge-url` together for a nonstandard
local Docker network, or `--no-bridge-spawn` when a supervisor already runs the
bridge.

## Declarative pipeline triggers and starter flows

A pipeline `trigger: {kind: email}` is materialized into an owned Activepieces
flow on `pipeline add --enable`. Use `gitmoot pipeline bind-trigger <name>` for
an explicit or repaired sync; it recreates an owned flow deleted in
Activepieces. This is the preferred receive-only Gmail path;
starter flows remain examples and escape hatches.

List the embedded flows without making a network request:

```sh
gitmoot activepieces templates list
```

Import all flows, or choose IDs:

```sh
gitmoot activepieces templates import
gitmoot activepieces templates import webhook-run-pipeline
gitmoot activepieces templates import gmail-imap-ask-agent
```

The importer skips an existing flow with the same display name.

`webhook-run-pipeline` accepts a webhook and runs the pipeline named in its
`<your-pipeline>` placeholder. Edit the placeholder and protect the webhook
before publishing it. The target pipeline must be enabled because the bridge
rejects disabled pipelines. See the [pipelines workflow](/docs/workflows/pipelines-workflow)
for pipeline setup.

`gmail-imap-ask-agent` listens for mail with the official IMAP piece, enqueues
`ask_agent`, and sends an acknowledgement through SMTP. Configure the IMAP and
SMTP connections, the agent, the sender, and the required `<owner/repo>` value.
Continue with the Gmail setup guide in `docs/gmail.md` for mailbox authorization.

The bridge returns only the queued `job_id` from `ask_agent`. The starter flow
therefore acknowledges the queued job and does not send the agent's eventual
answer. A polling or callback reply is future work.

## Stop the stack

Preserve all Activepieces data while stopping the containers:

```sh
gitmoot activepieces down
```

Delete the Postgres and Redis volumes too:

```sh
gitmoot activepieces down --volumes
```

The second command permanently removes local accounts, flows, connections, and
execution state stored in those volumes.
