# Connect Gmail To A Pipeline (Activepieces)

## How it fits together

The flow is:

```text
Gmail -> Activepieces in Docker -> gitmoot piece action -> gitmoot bridge serve -> pipeline or agent run
```

Activepieces holds **all Gmail, IMAP, SMTP, OAuth, or service-account
credentials**. The gitmoot piece holds only the bridge URL and bridge token.
Activepieces listens for mail and forwards selected fields. The pipeline or
agent does the analysis, routing, and other intelligent work inside gitmoot.

`gitmoot bridge serve` exposes bearer-token HTTP on localhost. Activepieces
therefore has to run on the **same box** as the bridge. A container reaches the
host at `http://host.docker.internal:8791` or, on Linux, a Docker bridge address
such as `http://172.17.0.1:8791`. Linux Compose usually also needs:

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
```

Activepieces Cloud cannot reach a bridge on your local machine. See the
[bridge reference](../website/docs/reference/bridge.md) for its security model.

## Why there is no plain "Sign in with Google" button

A "Sign in with Google" button is backed by a pre-registered OAuth application.
Google checks that the callback URL exactly matches an authorized redirect URI
on that application. A bare self-hosted Activepieces instance at your own domain
does not automatically have an OAuth application registered for that domain.

A hosted or preconfigured provider can supply such an application. Otherwise,
choose one of these paths: use an app password, register your own OAuth
application, or use a Google Workspace service account.

## Choose a path

| Path | Needs | Browser consent | Best for |
| --- | --- | --- | --- |
| **IMAP + SMTP app password (default)** | Google account with 2-Step Verification and app passwords allowed | No OAuth consent | The quickest self-hosted and headless setup |
| **Your own OAuth app** | Google Cloud project, Gmail API, OAuth client, reachable Activepieces URL | Once, then again if a Testing token expires | Teams that require OAuth or cannot use app passwords |
| **Workspace service account** | Google Workspace super admin, service account, domain-wide delegation | No | Fully headless organization-managed mailboxes |

## Default: IMAP + SMTP with an app password

This path needs no Google Cloud project and no OAuth browser-consent flow.

1. Turn on 2-Step Verification for the Gmail or Workspace account.
2. Open [Google App Passwords](https://myaccount.google.com/apppasswords) and
   generate a 16-character app password for Activepieces.
3. In Activepieces, create the IMAP trigger connection:
   - Host: `imap.gmail.com`
   - Port: `993`
   - Security: SSL
   - Username: the full Gmail or Workspace address
   - Password: the app password, not the normal account password
4. Create the SMTP action connection:
   - Host: `smtp.gmail.com`
   - Port: `465`
   - Security: SSL
   - Username: the full Gmail or Workspace address
   - Password: the same app password

Port `587` with STARTTLS also works for SMTP. Google's
[IMAP and SMTP reference](https://developers.google.com/workspace/gmail/imap/imap-smtp)
lists the hosts, ports, and transport requirements.

On a headless server, tunnel the Activepieces UI to your workstation and do the
setup there:

```sh
ssh -L 8080:127.0.0.1:8080 user@your-host
```

Then open `http://localhost:8080` locally. This is only UI access. It does not
add an OAuth consent step to the app-password path.

Google removed legacy basic-password access for Workspace IMAP and SMTP in
2025. App passwords are a separate feature and remain an explicit exception,
unless a Workspace administrator or account security policy disables them. See
Google's [Workspace transition notice](https://support.google.com/a/answer/14114704),
[app-password requirements](https://support.google.com/mail/answer/185833), and
[Gmail client setup guidance](https://support.google.com/mail/answer/7126229).

## OAuth: bring your own Google app

Use this path when policy requires OAuth or app passwords are unavailable.

1. Create or select a Google Cloud project.
2. Enable the Gmail API.
3. Configure the OAuth consent screen as **External** and add your Gmail address
   as a test user.
4. Create an OAuth client with application type **Web application**.
5. Add `https://<your-ap>/redirect` as an authorized redirect URI. Activepieces
   derives this callback from `AP_FRONTEND_URL`. Copy the exact redirect URL
   shown in the Activepieces connection dialog instead of reconstructing it by
   hand.
6. Paste the client ID and client secret into the Activepieces Gmail connection.
7. Click **Connect** and complete the Google consent flow once. An "unverified
   app" interstitial is expected for your own test application.

For a headless box, use `ssh -L` to open the Activepieces UI from a workstation
with a browser. The redirect still has to match the externally configured
`AP_FRONTEND_URL`; the tunnel is only how you operate the UI. Activepieces
[documents `AP_FRONTEND_URL`](https://www.activepieces.com/docs/install/reference/environment-variables)
as the public base used to build redirect and webhook URLs.

**Testing-mode expiry:** an External OAuth application whose publishing status
is **Testing** receives refresh tokens that expire after 7 days when Gmail
scopes are requested. Publish the application or reconnect and consent again
each week. See Google's
[refresh-token expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration).

## Google Workspace: service account (fully headless)

This path is only for Google Workspace. It has no per-user browser-consent step.

1. Enable the Gmail API in a Google Cloud project, create a service account,
   and enable domain-wide delegation.
2. Record its numeric client ID and keep its private key in a secret store.
3. In the Google Admin console, go to **Security > Access and data control > API
   controls > Manage Domain Wide Delegation**.
4. Add the service account client ID and grant the Gmail scopes used by the
   Activepieces Gmail connection:
   - `https://www.googleapis.com/auth/gmail.send`
   - `https://www.googleapis.com/auth/gmail.readonly`
   - `https://www.googleapis.com/auth/gmail.compose`
5. In Activepieces, choose **Service Account (Advanced)** for the Gmail
   connection. Paste the service-account JSON key and set **User Email** to the
   Workspace mailbox to impersonate.

Domain-wide delegation lets the service account act as a specific Workspace
user within the scopes the administrator granted. It does not apply to personal
`@gmail.com` accounts. Follow Google's
[service-account delegation guide](https://developers.google.com/identity/protocols/oauth2/service-account#delegatingauthority)
and [Workspace admin procedure](https://support.google.com/a/answer/162106).

## Wire the flow

First follow [Set Up Activepieces](./activepieces.md) to place Activepieces next
to the bridge. Install the published `@gitmoot/piece-gitmoot` package using the
[gitmoot-pieces README](https://github.com/jerryfane/gitmoot-pieces#building-and-publishing).
The target pipeline must already be registered with gitmoot. See
[Pipelines](./pipelines.md).

In the Activepieces flow:

1. Add a Gmail **New Email** trigger, or an IMAP new-message trigger for the
   app-password path.
2. Add the gitmoot **Run Pipeline** action, `run_pipeline`, and set
   `pipeline_name` to the registered pipeline name.
3. Create the gitmoot custom-auth connection with:
   - `bridge_url`: `http://host.docker.internal:8791`, or the same-host bridge
     address that works from the container
   - `bridge_token`: the bearer token created by `gitmoot bridge serve`
4. Map only the email fields the pipeline needs into the action input.

To ask a managed agent directly instead, use `ask_agent` with `agent`, `message`,
and `repo`. The bridge requires `repo`; use the full `owner/repo` value.

If the flow prepares a response, default to **Create Draft**. Never add an
automatic send step unless the operator explicitly opts in. Where the selected
mail connector cannot create drafts, stop after producing the proposed response
instead of adding an SMTP send action.

## Troubleshooting

### The app-password option is missing

Turn on 2-Step Verification first. The option can also be unavailable because of
a Workspace administrator policy, security-key-only 2-Step Verification, or
Advanced Protection. Ask the Workspace administrator whether app passwords are
allowed.

### Google shows an unverified-app screen

That is expected for your own External test application. Confirm that you created
the application and that the signed-in account is listed as a test user before
continuing.

### OAuth works for a week, then stops

The OAuth consent screen is probably External with publishing status **Testing**.
Its Gmail refresh token expires after 7 days. Publish the application or reconnect
and consent again.

### The Activepieces container cannot reach the bridge

Activepieces must run on the same box as gitmoot. Use
`http://host.docker.internal:8791` or the Linux bridge address such as
`http://172.17.0.1:8791`. For Linux Compose, add:

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
```

Activepieces Cloud cannot reach the localhost bridge.
