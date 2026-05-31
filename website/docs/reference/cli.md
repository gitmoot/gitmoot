# CLI Reference

This page is the compact reference. The canonical full command reference lives
in [`skills/gitmoot/references/CLI.md`](https://github.com/jerryfane/gitmoot/blob/main/skills/gitmoot/references/CLI.md).

## Install And Update

```sh
gitmoot version
gitmoot update --check
gitmoot plugin doctor
```

## Repos And Daemon

```sh
gitmoot init
gitmoot repo add owner/repo --path . --poll 30s
gitmoot status --repo owner/repo
gitmoot daemon start --repo owner/repo --poll 30s --workers 1
gitmoot daemon status
```

## Agents

```sh
gitmoot agent start <name> --runtime codex --repo owner/repo --template <template>
gitmoot agent subscribe <name> --runtime codex --session <id> --repo owner/repo
gitmoot agent prompt <agent-or-template>
gitmoot agent ask <name> --repo owner/repo "question"
gitmoot agent ask <name> --repo owner/repo --background "queued task"
gitmoot agent type list
gitmoot agent gc
gitmoot agent list
gitmoot agent doctor <name>
```

## Agent Templates

```sh
gitmoot agent template list
gitmoot agent template show <id>
gitmoot agent template update <id>
gitmoot agent template draft <id>
gitmoot agent template validate .gitmoot/templates/<id>.md
gitmoot agent template add <id> --file agents/<id>.md
gitmoot agent template diff <id>
```

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job watch <job-id>
gitmoot job retry <job-id>
gitmoot lock list --repo owner/repo
```

## SkillOpt Exchange

```sh
gitmoot skillopt export --run <run-id> [--output training.json]
gitmoot skillopt import --file candidate.json
gitmoot skillopt candidate list [--template id]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> [--reason text]
gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>
gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id> [--reviewer name]
gitmoot skillopt feedback github publish --run <run-id> [--repo owner/repo] [--pr <number>]
gitmoot skillopt feedback github sync --run <run-id> [--repo owner/repo] (--issue <number>|--pr <number>)
```
