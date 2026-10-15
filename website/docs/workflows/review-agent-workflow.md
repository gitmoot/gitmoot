# Review Agent Workflow

Gitmoot includes a strict review template named
`thermo-nuclear-code-quality-review`. Its `agent_templates` row must already be
installed; confirm with `gitmoot agent template list`.

```sh
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --model gpt-5-codex \
  --start-daemon
```

`--runtime` accepts `codex`, `claude`, `kimi` (Kimi Code CLI), or `omp`.
The optional `--model <name>` flag sets
the agent's default runtime model; an omitted `--model` preserves the runtime's
own default.

Ask it from a PR comment:

```text
/gitmoot thermo-review review
```

The thermo template is review-only. Route implementation work to a separate agent
with `implement` capability and normal branch-lock protection.

For a local review, name that implementer explicitly:

```sh
gitmoot agent review thermo-review --repo owner/repo --pr 12 --lead lead "Review this PR."
```

Add `--exec-backend remote` to either `agent review` or `review request` to run
that one review remotely. Omit it for local execution. The selector is stored
on the job and never changes where another review runs; remote review supports
only `shell` and `omp`, and unsupported pairs are refused before enqueue.

Gitmoot validates the lead's agents-database row before it creates the review
job: the lead must exist, have access to the repository, hold `implement`, and
use a write-granting policy. If the review returns `changes_requested`, its fix
job targets the lead while the reviewer stays read-only. Omitting `--lead`
checks the reviewer as the fallback implementer and therefore refuses a strict
review-only agent before spending a review session.

## Review Policy

Review severity controls whether a reported finding restarts the fix loop. The
default preserves the existing block-all behavior:

```toml
[review]
blocking_severity = "P3"

[repos."themartianapp/keephair".review]
blocking_severity = "P1"
```

The threshold is inclusive. `P1` blocks `P0` and `P1`; `P2` and `P3` findings
are still posted and the raw `changes_requested` result remains stored, but the
round resolves as approved-with-notes and Gitmoot does not dispatch a fix. The
global default is `P3`. Configured values must be `P0`, `P1`, `P2`, or `P3`.

A blocking verdict must also say WHERE. If a `changes_requested` result blocks at
the threshold, carries findings, and no finding at or above the threshold names a
location — `evidence_locator`, `locator`, `file`, `location`, cited `evidence`, or
a path like `internal/db/store.go:88` inside its prose — the round resolves as
approved-with-notes instead of blocking. The findings are still stored and posted;
only the block is withdrawn, because a merge cannot be stopped on a defect nobody
can open. The `review_approved_with_notes` event names this reason, distinct from
the below-threshold fold.

This is deliberately conservative and keeps blocking whenever the gate cannot see
the whole picture: no findings at all, findings that are empty objects, a finding
that does not decode, or a severity Gitmoot cannot rank.
An invalid `blocking_severity` value falls back to `P3` while other valid review
fields remain active. Any other review-policy parse or read error rejects the
entire applied review policy, restoring `P3` with native fanout and risk tiers
off.

The threshold applies to native review rounds only. A pipeline review stage is
report-only — the pipeline advancer owns folding its verdict — so its raw
`changes_requested` keeps blocking the merge gate at every threshold and never
counts toward required-reviewer approval. The resolved outcome is recorded on
the review job as a `review_approved_with_notes` event, which is what
`gitmoot proof` reads for its approval claim and what the `review_verdict`
awaited fact reports when it wakes a waiting role.

### Risk-Tiered Adaptive Review

Set `risk_tiers_enabled = true` in `[review]` to scale review depth to a
change's blast radius:

```toml
[review]
risk_tiers_enabled = true
# Changed-path globs that mark a PR high-risk (** matches any path depth):
high_risk_paths = ["**/auth/**", "**/security/**", "**/payment/**", "**/migration/**", "go.mod"]
risk_label_high = "risk:high"        # a PR label that forces the high tier
risk_label_routine = "risk:routine"  # a PR label that forces the routine tier
```

When enabled, every opened PR is classified: **explicit PR label > changed-path
glob match > default routine**. A `risk:high` / `risk:routine` label wins over
the path heuristics, and a high label wins a label tie.

- **routine** PRs keep the existing single-reviewer fan-out.
- **high** PRs fan out a delegation batch of **refutation-framed lens reviewers**
  (correctness, security, and, with three or more configured reviewers,
  regression). Each lens is prompted to actively *disprove* the change along one
  axis and return structured findings `{lens, refuted, severity, confidence,
  evidence}` in `gitmoot_result.findings`.

The lens outcomes are synthesized by the existing delegation `synthesis_rule =
quorum` engine: **any blocking refutation fails the quorum and blocks the merge**;
the configured quorum of effective approvals satisfies it. The resolved tier is
recorded as a `risk_tier_resolved` job event so an escalation is explainable in
the report and dashboard.

With `risk_tiers_enabled` off, PR review uses the single-reviewer path. The
competition tier (two independent implementations plus a judge) is a planned
follow-up.
