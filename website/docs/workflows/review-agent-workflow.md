# Review Agent Workflow

Gitmoot includes a strict review template named
`thermo-nuclear-code-quality-review`.

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --model gpt-5-codex \
  --start-daemon
```

`--runtime` accepts `codex`, `claude`, `kimi` (Kimi Code CLI), or `kimi-cli`
(the opt-in legacy Kimi CLI adapter). The optional `--model <name>` flag sets
the agent's default runtime model; an omitted `--model` preserves the runtime's
own default.

Ask it from a PR comment:

```text
/gitmoot thermo-review review
```

The thermo template is review-only. Route implementation work to a separate agent
with `implement` capability and normal branch-lock protection.

## Risk-Tiered Adaptive Review

Gitmoot can scale review depth to a change's blast radius through the
off-by-default `[review]` config section (not in the generated default config):

```toml
[review]
risk_tiers_enabled = true
# Changed-path globs that mark a PR high-risk (** matches any path depth):
high_risk_paths = ["**/auth/**", "**/security/**", "**/payment/**", "**/migration/**", "go.mod"]
risk_label_high = "risk:high"        # a PR label that forces the high tier
risk_label_routine = "risk:routine"  # a PR label that forces the routine tier
```

When enabled, every opened PR is classified — **explicit PR label > changed-path
glob match > default routine**. A `risk:high` / `risk:routine` label wins over the
path heuristics, and a high label wins a label tie (safety-biased).

- **routine** PRs keep the existing single-reviewer fan-out, unchanged.
- **high** PRs fan out a delegation batch of **refutation-framed lens reviewers**
  (correctness, security, and — with three or more configured reviewers —
  regression). Each lens is prompted to actively *disprove* the change along one
  axis and return structured findings `{lens, refuted, severity, confidence,
  evidence}` in `gitmoot_result.findings`.

The lens outcomes are synthesized by the existing delegation `synthesis_rule =
quorum` engine (not a bespoke synthesizer): **any critical-severity refutation —
reported as a `blocked` lens decision — fails the quorum and blocks the merge**;
unanimous approval satisfies it. The resolved tier is recorded as a
`risk_tier_resolved` job event so an escalation is explainable in the report and
dashboard.

With the `[review]` section absent or `risk_tiers_enabled` off, PR review is
byte-for-byte the single-reviewer path. The competition tier (two independent
implementations plus a judge) is a planned follow-up.

