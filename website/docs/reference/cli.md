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
gitmoot daemon start --repo owner/repo --poll 30s
gitmoot daemon status
```

## Agents

```sh
gitmoot agent start <name> --runtime codex --repo owner/repo --preset <preset>
gitmoot agent subscribe <name> --runtime codex --session <id> --repo owner/repo
gitmoot agent ask <name> --repo owner/repo "question"
gitmoot agent ask <name> --repo owner/repo --background "queued task"
gitmoot agent list
gitmoot agent doctor <name>
```

## Presets

```sh
gitmoot preset list
gitmoot preset show <id>
gitmoot preset update <id>
gitmoot preset add <id> --file agents/<id>.md
gitmoot preset diff <id>
```

## Jobs And Locks

```sh
gitmoot job list --repo owner/repo
gitmoot job show <job-id>
gitmoot job retry <job-id>
gitmoot lock list --repo owner/repo
```
