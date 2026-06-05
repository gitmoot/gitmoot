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

## SkillOpt Review Operations

```sh
gh auth status --hostname github.com
gh repo view owner/reviews --json nameWithOwner
gitmoot skillopt train status --session <session-id> --verbose
gitmoot repo list
```

GitHub review operations use `gh`; authenticate it for the expected review repo
before publishing, syncing, candidate review publication, or review watching.
Preview publication can push Pages files before a later review issue preflight
fails, so run the `gh` checks before starting review publication.

Confirm `review.expected_repo` in train status. Preview review runs must publish
and sync against the preview/review repo, not the target product repo.

Preview links can be labeled `pending deployment`, `failed deployment`, or
`stale deployment`. Pending means Pages had not finished within the bounded
wait, failed includes the Pages error when available, and stale means the latest
build still points at another commit. Existing review links keep their recorded
label; `train continue` skips options that already have a preview URL and does
not refresh old deployment status.

Candidate review decisions are explicit: promote, reject with a reason, wait,
or reject and `--start-next` to keep improving. Required Vue/Vite options retry
once for actionable preview-bundle validation errors; repeated failures stop
with item, option, validation class, and retry count.

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
