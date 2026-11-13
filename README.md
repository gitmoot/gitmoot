# gitmoot

[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-black.svg?style=flat-square)](./LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/gitmoot/gitmoot/ci.yml?branch=main&style=flat-square&label=CI)](https://github.com/gitmoot/gitmoot/actions/workflows/ci.yml)

Gitmoot runs a fleet of coding agents like a team. It gives each agent session
a **seat** (role, parent, repository scope, merge authority), carries durable
**messages and directives** between seats, and sends pull requests to a
**different agent** for an independent review bound to the exact head commit,
holding the merge until the verdict lands. It runs on your machine: one static
binary, one SQLite file, a daemon that polls GitHub as your `gh` user, no hosted
control plane.

## Requirements

- **omp is required.** Review jobs run on omp; the `shell` runtime exists for
  no-LLM tests.
- `gh` authenticated against the repositories you register.

## Install

```sh
curl -fsSL https://gitmoot.io/install.sh | sh
gitmoot setup --repo owner/repo --path . --start-daemon
gitmoot doctor
```

## Reference

The packaged Agent Skill is the only reference and ships with the code:
**[skills/gitmoot/SKILL.md](skills/gitmoot/SKILL.md)** (CLI reference:
[skills/gitmoot/references/CLI.md](skills/gitmoot/references/CLI.md)).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md).

```sh
go build ./... && go vet ./... && go test ./...
```

## License

Apache License 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).
