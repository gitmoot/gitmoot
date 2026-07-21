# OpenAI Build Week 2026 — submission assets (temporary)

This folder documents the **Gitmoot Pipelines** submission for OpenAI Build Week 2026
(July 13–21). It is temporary and will be removed after judging. The submission is one
feature of this repo: agent graphs saved as yaml files that you can rerun, inspect, share,
and expose as a typed service API with verifiable receipts. Feature docs:
<https://gitmoot.io/docs/workflows/pipelines-workflow>. Demo pipeline repo:
<https://github.com/gitmoot/appkit-demo>. Live runs:
<https://gitmoot.themartian.app/pipelines/appkit-pro>.

## Codex sessions used for Gitmoot Pipelines (main ones first)

| # | Codex session ID | Date | Role |
|---|---|---|---|
| 1 | `019f6e86-fdf2-78f1-8f6a-7c5e0f948340` | Jul 17 | **Main implementation session.** Codex (gpt-5.6-sol) built the Pipelines-as-a-Service layer: `pipeline expose` / `serve`, the typed input firewall, and public receipts (PR #1012, ~+4,480 lines). Gitmoot minted proof receipts for these jobs; one is committed verbatim in this folder (`codex-proof-receipt.txt`, proof `sha256:78b3ab08...`). |
| 2 | `019f8191-f721-79f1-9558-3c5f960da540` | Jul 20 | **Demo, graph side.** The single-prompt Codex session that ran the `appkit-pro` pipeline end to end (run `prun-appkit-pro-18c41ee476461c88`). Source of the graph column in the numbers table: 1 prompt, 4,231,476 tokens, 21:24 to Telegram delivery. |
| 3 | `019f7b98-2f12-7e90-94ce-5248969c289e` | Jul 19–20 | **Demo, loop side (baseline).** The plain-conversation Codex session doing the same mission without a pipeline. Source of the loop column: 6 prompts, 18,814,978 tokens, 54:41 active across two sittings. |
| 4 | `019f766e-3f8d-7b61-b0d9-74fadf3ce15f` | Jul 18 | Design brainstorm seat and reference marketing-loop footage. |
| 5 | `019f8180-c48e-71a0-8b2e-30ad98db83ad` | Jul 20 | Outtake: first graph-side attempt without the repo playbook. Codex misread normal scheduler gaps as a stall and improvised; it taught us the AGENTS.md patience rule described on the Devpost page. |

Beyond these interactive sessions, every `appkit-pro` run contains two **Codex (gpt-5.6-sol)
agent stages** executed by gitmoot itself: `derive` (reads the target repo, derives the app
identity with citations) and `content` (writes the store copy, running in parallel with the
screenshot branch). Gitmoot records per-job token usage for each (job IDs
`prun-appkit-pro-<run>-derive-a0` / `-content-a0`). The wider engine work during Build Week
was likewise implemented by Codex sessions coordinated through gitmoot; the branch and PR
history of this repo is the audit trail.

## Files

- `codex-proof-receipt.txt` — a gitmoot proof receipt for the main implementation session,
  verbatim (`gitmoot proof` reproduces and verifies the manifest offline).
- `framed-shot-en.png` — a real framed App Store shot produced by the pipeline.
- `landing-real.png` — the landing page embedding the real captured screens.
- `og.png` — social art from the generated launch kit.
