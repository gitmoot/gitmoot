# Troubleshooting

Start with the local doctor from the repository checkout:

```sh
gitmoot doctor --repo .
gitmoot status --repo owner/repo
gitmoot daemon status
```

Most Gitmoot failures come from one of four places: the installed binary, GitHub
CLI auth, runtime/plugin discovery, or a local daemon/job/lock state.

## Install Script Failed

Symptom: `curl -fsSL https://gitmoot.io/install.sh | sh` exits before
installing `gitmoot`.

Likely cause: network failure, unsupported platform, missing shell tools, or a
release artifact that is not available for the current OS/architecture.

Check:

```sh
uname -s
uname -m
curl -fsSL https://gitmoot.io/install.sh -o /tmp/gitmoot-install.sh
sh -n /tmp/gitmoot-install.sh
```

Fix: retry the installer or use the direct binary fallback from the install
page. Verify the artifact checksum before running it.

## Binary Not On PATH

Symptom: `gitmoot: command not found` after install.

Likely cause: the install directory is not on `PATH`, or the shell has not been
restarted after `pipx ensurepath` or installer profile changes.

Check:

```sh
command -v gitmoot
echo "$PATH"
ls -l ~/.local/bin/gitmoot
```

Fix: add the install directory to `PATH`, restart the shell, or move the binary
to a directory already on `PATH`.

## Checksum Mismatch

Symptom: the local SHA256 does not match the release checksum.

Likely cause: partial download, wrong artifact, stale checksum, or a tampered
download.

Check:

```sh
sha256sum <artifact>
shasum -a 256 <artifact>
```

Fix: delete the file, download the artifact again from GitHub Releases, and
compare against the checksum for that exact release and platform.

## GitHub CLI Auth Fails

Symptom: PR comments, issue comments, review publication, status checks, or
merge actions fail.

Likely cause: `gh` is not installed, is authenticated as the wrong account, or
does not have access to the repo.

Check:

```sh
gh auth status
gh repo view owner/repo --json nameWithOwner
gh pr list --repo owner/repo --state open
```

Fix: authenticate `gh` for the account that can read and write the repository,
then retry the Gitmoot operation.

## Send A Gitmoot Bug Report

Symptom: a Gitmoot job failed, blocked, or was cancelled, and you want to send a
useful report without exposing raw runtime output.

Likely cause: the failing job has local context that matters for debugging:
repo, agent, runtime, action, task, selected error, result summary, and recent
events.

Check:

```sh
gitmoot job show <job-id>
gitmoot report bug --job <job-id> --preview
```

Fix: preview first. The report is redacted, omits raw runtime output by default,
adds the `gitmoot-dashboard-report` and `bug` labels, and includes a
fingerprint marker so open duplicates can be reused.

Create the GitHub issue only when you intend to file it:

```sh
gitmoot report bug --job <job-id> --create --yes
```

The command prints either `created issue: ...` or `existing issue: ...`; use that
URL when sharing status. In the interactive dashboard, select a failed, blocked,
or cancelled job and press `B report bug` to open the same preview, then `g` to
create or reuse the issue. If creation fails, the preview stays open and shows
the error inline.

## Plugin Doctor Fails

Symptom: Codex or Claude Code does not discover Gitmoot, or
`gitmoot plugin doctor` reports missing package or runtime state.

Likely cause: plugin package was not generated, runtime CLI is missing, runtime
uses a different home directory, or the package cache is stale.

Check:

```sh
gitmoot plugin doctor
gitmoot plugin doctor codex
gitmoot plugin doctor claude
gitmoot plugin path codex
gitmoot plugin path claude
```

Fix:

```sh
gitmoot plugin install codex --force
gitmoot plugin install claude --force
gitmoot plugin doctor
```

The plugin is discovery and guidance. It does not replace `gitmoot`, GitHub
CLI auth, or runtime/model credentials.

## Runtime Session Not Found

Symptom: `gitmoot agent doctor <name>` cannot validate a Codex, Claude, or Kimi
session, or a job resumes the wrong session.

Likely cause: a `last` reference changed, the runtime home changed, or the
session id is stale. For a Kimi agent, the runtime reference must be a Kimi
session id (`session_<uuid>`) or empty.

Check:

```sh
gitmoot agent list
gitmoot agent show <agent>
gitmoot agent doctor <agent>
codex exec resume --help
claude --help
kimi --help
```

Fix: prefer explicit session UUIDs or thread names over `last`, then
re-subscribe the agent with the correct session reference.

## Daemon Not Running

Symptom: queued jobs do not move, PR comments are not consumed, or dashboard
shows the daemon as down.

Likely cause: daemon was never started, exited, or is running with the wrong
repo/home.

Check:

```sh
gitmoot daemon status
gitmoot daemon logs
gitmoot status --repo owner/repo
```

Fix:

```sh
gitmoot daemon start --repo owner/repo --poll 30s --workers 1
```

Use `gitmoot daemon run` only when you intentionally want a foreground process.

## Job Stuck Or Failed

Symptom: a job is queued, blocked, failed, or no longer changing state.

Likely cause: runtime delivery failed, worker is read-only, GitHub auth failed,
or another lock is active.

Check:

```sh
gitmoot job show <job-id>
gitmoot job events <job-id>
gitmoot agent show <agent>
gitmoot lock list --repo owner/repo
```

Fix: resolve the underlying runtime/auth/lock issue, then retry when safe:

```sh
gitmoot job retry <job-id>
```

Cancel only when the job should not continue:

```sh
gitmoot job cancel <job-id>
```

## Stale Lock

Symptom: implementation or merge work is blocked by a lock whose owner is gone.

Likely cause: a worker died or the daemon stopped before cleanup.

Check:

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

Fix: let a running daemon reclaim stale resource locks automatically. Release a
branch lock only after confirming the owner is no longer working:

```sh
gitmoot lock release owner/repo <branch> --owner <agent>
```

## Dashboard Blank Or Noninteractive

Symptom: `gitmoot dashboard` does not open the TUI, prints plain output, or
looks blank under a script/agent.

Likely cause: stdin/stdout is not a TTY, `TERM=dumb`, or TUI was disabled.

Check:

```sh
gitmoot dashboard --plain
gitmoot dashboard --json
gitmoot dashboard --watch
echo "$TERM"
```

Fix: run from a real terminal for the interactive TUI, or use `--plain` /
`--json` in agents, CI, pipes, and redirected output.

## SkillOpt Optimizer Missing

Symptom: `gitmoot skillopt train continue` reaches optimizer handoff and
reports `blocked_config` or missing `gitmoot-skillopt`.

Likely cause: the separate Python optimizer is not installed or is not on
`PATH`.

Check:

```sh
gitmoot-skillopt --version
gitmoot-skillopt optimize --help
gitmoot skillopt train status --session <session-id> --verbose
```

Fix:

```sh
python3 -m pip install --user pipx
python3 -m pipx ensurepath
pipx install https://github.com/jerryfane/gitmoot-skillopt/releases/download/v0.3.0/gitmoot_skillopt-0.3.0-py3-none-any.whl
gitmoot-skillopt --version
gitmoot-skillopt optimize --help
```

For a virtualenv install, pass `--skillopt-bin /path/to/gitmoot-skillopt`.

## SkillOpt Dependency Or Credential Failure

Symptom: optimizer preflight starts but fails before producing a candidate.

Likely cause: missing Python dependency, backend/model credentials, evaluator
configuration, or writable output directory.

Check:

```sh
gitmoot skillopt train status --session <session-id> --verbose
gitmoot-skillopt optimize --help
```

Fix: install the missing dependency, configure the required backend credential
through user-owned environment/config, and restart any daemon or runtime that
must inherit the environment. Do not commit secrets.

## Train Session Recoverable

Symptom: verbose status reports `status_phase: recovery_available`.

Likely cause: optimizer wrote completed artifacts but the wrapper failed before
Gitmoot imported the result.

Check:

```sh
gitmoot skillopt train status --session <session-id> --verbose
```

Fix:

```sh
gitmoot skillopt train recover --session <session-id> --out-root <optimizer-output-root>
```

Recovery validates artifacts and imports either a completed candidate or a
completed no-candidate result through the normal gate.

## Live Docs Or LLM Context Stale

Symptom: `gitmoot.io/docs` or `/llms.txt` does not show current source docs.

Likely cause: docs were changed but not rebuilt/deployed, or stale deployed
files were not deleted.

Check:

```sh
cd website
npm run build
curl -fsS https://gitmoot.io/docs/reference/cli | rg 'gitmoot dashboard'
curl -fsS https://gitmoot.io/llms.txt | rg 'SkillOpt|Dashboard|Release Notes'
```

Fix: deploy the current static build with delete semantics:

```sh
cd /root/gitmoot/website
npm run build
rsync -a --delete build/ /var/www/gitmoot-docs/
```
