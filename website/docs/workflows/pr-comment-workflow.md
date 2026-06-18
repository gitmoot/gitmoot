# PR Comment Workflow

PR comments are the public coordination surface for Gitmoot.

```text
/gitmoot help
/gitmoot status
/gitmoot ask planner Write a task-by-task plan for this PR.
/gitmoot thermo-review review
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
```

The daemon polls GitHub, checks that comments are from users allowed to route
work, queues jobs, invokes the selected agent runtime, and posts attributed
results back to the PR. The selected agent's runtime can be `codex`, `claude`,
or `kimi` (Kimi Code CLI).

Expected result comments include the agent identity, runtime, the template when
one is attached, and the job id:

```md
> Agent: `planner`
> Runtime: `codex`
> Template: `planner`
> Job: `local-ask-...`
```

The `Template` line is present only when the job ran with a template. The body
then continues with the `Decision` and `Summary`, plus any `Findings`,
`Changes Made`, `Tests Run`, `Needs`, and `Delegations` the agent reported.

Use `gitmoot job list --repo owner/repo` and
`gitmoot events --repo owner/repo` to inspect routing state.

