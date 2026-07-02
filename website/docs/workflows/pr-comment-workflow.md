# PR Comment Workflow

PR comments are the public coordination surface for Gitmoot.

```text
/gitmoot help
/gitmoot status
/gitmoot ask planner Write a task-by-task plan for this PR.
/gitmoot planner review [instructions]
/gitmoot planner implement [instructions]
/gitmoot retry <job-id>
/gitmoot cancel <job-id>
/gitmoot merge
/gitmoot resume <job-id> retry|continue|abort|answer [instructions]
```

A bare `@<agent>` mention works as the same command (#389):

```text
@planner ask What is blocking this PR?
@thermo-review review
```

Mentions are also routed on **issue** comments when the daemon runs with
`--watch-issues` (on issues only the `ask` action is acted on).

The daemon polls GitHub, checks that comments are from users allowed to route
work, queues jobs, invokes the selected agent runtime, and posts attributed
results back to the PR. The selected agent's runtime can be `codex`, `claude`,
`kimi` (Kimi Code CLI), or `kimi-cli` (the opt-in legacy Kimi CLI adapter).

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

## Resuming paused orchestrations

When a delegation tree pauses for a human — an `escalate_human` failure pause,
or an ask-gate `human_questions` pause — the daemon @-tags the escalation
handle in a comment with the resume instructions, and a comment on the tree's
**open** PR resumes it. Resume commands are PR-comment-only: the issue watcher
routes only the `ask` action, so `/gitmoot resume` posted on an issue is
silently ignored — always resume on the tree's open PR:

```text
/gitmoot resume <coordinator-job-id> retry [instructions]   # re-run the failing leg
/gitmoot resume <coordinator-job-id> continue               # proceed the continuation
/gitmoot resume <coordinator-job-id> abort                  # graceful finalize
/gitmoot resume <coordinator-job-id> answer q1: <value>     # answer an ask-gate question
```

See the [result contract](../reference/result-contract.md) for the
`escalate_human` / ask-gate pause semantics, TTLs, and how a paused tree is
surfaced under the dashboard's Attention page.

