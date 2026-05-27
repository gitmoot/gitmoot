# Review Agent Workflow

Gitmoot includes a strict review template named
`thermo-nuclear-code-quality-review`.

```sh
gitmoot agent template update thermo-nuclear-code-quality-review
gitmoot agent start thermo-review \
  --runtime codex \
  --repo owner/repo \
  --template thermo-nuclear-code-quality-review \
  --start-daemon
```

Ask it from a PR comment:

```text
/gitmoot thermo-review review
```

The thermo template is review-only. Route implementation work to a separate agent
with `implement` capability and normal branch-lock protection.

