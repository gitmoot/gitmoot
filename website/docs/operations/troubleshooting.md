# Troubleshooting

Start with:

```sh
gitmoot doctor --repo .
gitmoot status --repo owner/repo
gitmoot agent list
gitmoot job list --repo owner/repo
```

## GitHub CLI

```sh
gh auth status
gh repo view owner/repo --json nameWithOwner
gh pr list --repo owner/repo --state open
```

## Runtime Sessions

Use explicit session IDs when possible. `last` is convenient for demos but can
point at the wrong session if another runtime session starts later.

```sh
gitmoot agent doctor <name>
codex exec resume --help
claude --help
```

## Presets

```sh
gitmoot preset list
gitmoot preset show <id>
gitmoot preset diff <id>
gitmoot preset update <id>
```

## Branch Locks

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

For the longer troubleshooting reference, see
[`docs/troubleshooting.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/troubleshooting.md).

