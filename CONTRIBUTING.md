# Contributing To Gitmoot

Gitmoot is a local-first coordinator for AI agents. Keep changes scoped,
auditable, and easy to verify from the local checkout.

## Setup

Install the local prerequisites:

```sh
git --version
gh --version
gh auth status
go version
node --version
npm --version
```

Install Gitmoot from source or the latest beta, then verify:

```sh
gitmoot version
gitmoot plugin doctor
```

For SkillOpt optimizer work, also verify the separate Python package:

```sh
gitmoot-skillopt --version
gitmoot-skillopt optimize --help
```

## Development Checks

Run focused checks for the area you changed. Common checks are:

```sh
go test ./...
git diff --check
```

For docs:

```sh
cd website
npm install
npm run build
```

The docs build also regenerates `website/static/llms-full.txt` through
`npm run build:llms`.

For SkillOpt train-mode changes:

```sh
scripts/skillopt-train-smoke.sh
```

## Continuous Integration

A GitHub Actions workflow named `CI` (`.github/workflows/ci.yml`) enforces the
build/vet/test gate on every push to `main` and every pull request. The
`build / vet / test` job runs on `ubuntu-latest`:

```sh
go build ./...
go vet ./...
go test ./...
go test -race -timeout 20m ./internal/workflow/ ./internal/db/ ./internal/daemon/
```

A second parallel job, `race (internal/cli)`, runs the `internal/cli` race suite
on its own with a 35m timeout — it has outgrown the shared 20m bound (#733), so
splitting it keeps a slow runner from failing green code while total wall-time
stays roughly the max of the two jobs rather than their sum.

The race detector covers the workflow engine and the concurrency-sensitive
core packages. CI does not run the live
multi-runtime (codex/claude/kimi) E2E — that needs runtime auth and stays a
manual step.

## Docs And LLM Context

Public docs live in `website/docs`. Root docs under `docs/` and skill
references under `skills/gitmoot/references/` are canonical source material for
agent-facing details.

When updating docs:

- keep `website/docs` in sync with current root docs and skill references
- update `website/static/llms.txt` when a new important public docs page exists
- update `website/scripts/build-llms-full.mjs` when agents need fuller context
- run `cd website && npm run build`
- quote Mermaid node labels that contain paths or punctuation, for example
  `P["/var/www/gitmoot-docs"]`

## Style

- Prefer existing repo patterns over new abstractions.
- Keep user-facing command examples runnable.
- Put full flag matrices in reference docs, not every workflow page.
- Do not commit caches, local state, logs, generated reports, secrets,
  downloaded release artifacts, or runtime transcripts.
- Do not paste tokens into issues, PRs, docs, or logs.

## Pull Requests

Open focused PRs with:

- what changed
- why it changed
- checks run
- skipped checks or residual risk

For Gitmoot-managed task work, include the raw final
`codex exec review --uncommitted` output in the PR body when that workflow is
used.
