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
gitmoot job events <job-id>
codex exec resume --help
claude --help
```

If a job reports `runtime_lock_wait` or `runtime session ... is busy`, another
job is already using that Codex or Claude session. Wait for it to finish, use a
different runtime session, or configure a managed agent type with
`max_background` greater than one.

## Agent Templates

```sh
gitmoot agent template list
gitmoot agent template show <id>
gitmoot agent template diff <id>
gitmoot agent template update <id>
```

## Branch Locks

```sh
gitmoot lock list --repo owner/repo
gitmoot lock show owner/repo <branch>
```

For the longer troubleshooting reference, see
[`docs/troubleshooting.md`](https://github.com/jerryfane/gitmoot/blob/main/docs/troubleshooting.md).
