# Security Policy

Gitmoot is beta software. Please report security issues privately before
opening public issues or pull requests.

## Supported Versions

Security fixes target the latest published beta and the current `main` branch.
Older beta builds are best-effort only unless a release note says otherwise.

## Reporting A Vulnerability

Send a private report to the maintainer through GitHub security advisories when
available, or contact the repository owner directly through a private channel.

Include:

- affected version or commit
- operating system and install method
- concise reproduction steps
- impact and expected access level
- any relevant logs with secrets removed

Do not include tokens, private keys, GitHub access tokens, Telegram bot tokens,
runtime session transcripts, repository secrets, or proprietary code in a
public issue, PR body, log excerpt, or generated artifact.

## Handling

The maintainer will triage the report, request clarification if needed, and
coordinate a fix or mitigation. Response times are not guaranteed during beta,
but reports that affect credential handling, local command execution, GitHub
write permissions, plugin packaging, or agent job routing should be treated as
high priority.

## Security-Relevant Areas

Pay special attention to changes that touch:

- installer and release artifact verification
- plugin package generation for Codex or Claude Code
- GitHub CLI token usage and PR/issue writes
- daemon job routing, branch locks, and merge gates
- runtime adapter command execution
- local config, SQLite state, logs, and generated files
