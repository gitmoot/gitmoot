# Gitmoot Result Contract

Every agent job must return a `gitmoot_result` JSON object. Keep it concise,
truthful, and tied to work that actually happened.

```json
{
  "gitmoot_result": {
    "decision": "approved|changes_requested|blocked|implemented|failed",
    "summary": "Brief outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": []
  }
}
```

## Delegations

Use `delegations` to request follow-up work by named Gitmoot agents. Each
delegation describes a child job:

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "summary": "Plan ready for review.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": [],
    "delegations": [
      {
        "id": "review-plan",
        "agent": "thermo-review",
        "action": "review",
        "prompt": "Review the implementation plan for correctness."
      }
    ]
  }
}
```

Delegation fields:

- `id` (required): stable identifier for this delegation within the result.
- `agent` (required): name of the Gitmoot agent to run.
- `action` (required): job action, e.g. `ask`, `review`, or `implement`.
- `prompt` (required): instructions for the delegated job.
- `worktree`, `artifacts`, `deps`, `timeout`, `retry`, `failure_policy`,
  `fingerprint`, `synthesis_rule`: optional controls for advanced dispatchers.

## Decisions

- `approved`: review found no blocking issues.
- `changes_requested`: review found issues that should be fixed before merge.
- `blocked`: work cannot continue without human input or an external state change.
- `implemented`: the requested implementation work was completed.
- `failed`: the attempted action errored or could not complete.

## Reporting Rules

- Do not claim tests were run unless they were actually run.
- Do not claim files were changed unless they were actually changed.
- Use `needs` for missing credentials, unclear scope, unavailable tools, failing
  external services, or required human decisions.
- Use `delegations` when another named Gitmoot agent should be invoked.
- Redact secrets from summaries, findings, raw command output, and examples.
