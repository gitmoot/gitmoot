# PR Comment Workflow

PR comments are the public coordination surface for Gitmoot.

```text
/gitmoot help
/gitmoot status
/gitmoot ask planner Write a task-by-task plan for this PR.
/gitmoot thermo-review review
/gitmoot retry <job-id>
/gitmoot merge
```

The daemon polls GitHub, checks that comments are from users allowed to route
work, queues jobs, invokes the selected agent runtime, and posts attributed
results back to the PR.

Expected result comments include the agent identity:

```md
> Agent: `planner`
> Runtime: `codex`
> Job: `local-ask-...`
```

Use `gitmoot job list --repo owner/repo` and
`gitmoot events --repo owner/repo` to inspect routing state.

