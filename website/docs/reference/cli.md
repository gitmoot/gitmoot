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
