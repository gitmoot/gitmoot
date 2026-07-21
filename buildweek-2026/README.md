# OpenAI Build Week 2026 — submission assets (temporary)

This folder documents the **Gitmoot Pipelines** submission for OpenAI Build Week 2026
(July 13–21). It is temporary and will be removed after judging. The submission is one
feature of this repo: agent graphs saved as yaml files that you can rerun, inspect, share,
and expose as a typed service API with verifiable receipts. Feature docs:
<https://gitmoot.io/docs/workflows/pipelines-workflow>. Demo pipeline repo:
<https://github.com/gitmoot/appkit-demo>. Live runs:
<https://gitmoot.themartian.app/pipelines/appkit-pro>.

## Install, platforms, and how to test it (dev tool)

**Install** (one static binary, no runtime dependencies):

    curl -fsSL https://gitmoot.io/install.sh | sh

or download a binary from the [releases page](https://github.com/gitmoot/gitmoot/releases)
(latest: v0.9.1.3). Building from source needs Go 1.26+: `go build ./cmd/gitmoot`.

**Supported platforms**: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.

**Test the pipeline feature in five minutes, no API keys needed** (shell stages need no LLM;
verified verbatim on linux/amd64 with a fresh home):

    mkdir /tmp/hello && cd /tmp/hello && git init
    git remote add origin https://github.com/you/hello.git   # identity only; nothing is pushed
    gitmoot repo add you/hello --path .
    cat > hello.yaml <<'YAML'
    name: hello-graph
    repo: you/hello
    stages:
      - id: a
        cmd: sh -c 'printf '"'"'{"gitmoot_result":{"decision":"implemented","summary":"a"}}'"'"''
      - id: b
        cmd: sh -c 'printf '"'"'{"gitmoot_result":{"decision":"implemented","summary":"b"}}'"'"''
      - id: join
        needs: [a, b]
        cmd: sh -c 'printf '"'"'{"gitmoot_result":{"decision":"implemented","summary":"join"}}'"'"''
    YAML
    gitmoot pipeline add hello.yaml
    gitmoot daemon start --poll 2s     # the daemon executes stages
    gitmoot pipeline run hello-graph
    gitmoot pipeline list              # a and b fork, join runs last, then: succeeded
    gitmoot daemon stop

To test the service side (`expose` / `serve` / receipts) and the full demo pipeline with
Codex agent stages, follow the README of
[gitmoot/appkit-demo](https://github.com/gitmoot/appkit-demo). Full feature docs:
<https://gitmoot.io/docs/workflows/pipelines-workflow>.

## Codex sessions used for Gitmoot Pipelines (main ones first)

The pipeline feature was built by Codex sessions dispatched and coordinated through gitmoot
itself; sessions 2 to 6 are gitmoot-managed Codex agents named after the issue they
implemented. The original pipeline primitive (#681) predates Build Week; everything below is
work done inside the July 13 to 21 window.

| # | Codex session ID | Date | Tokens | Role |
|---|---|---|---|---|
| 1 | `019f6e86-fdf2-78f1-8f6a-7c5e0f948340` | Jul 17 | 103,939,195 | **Main implementation session.** Codex (gpt-5.6-sol) built the Pipelines-as-a-Service layer: `pipeline expose` / `serve`, the typed input firewall, and public receipts (PR #1012, ~+4,480 lines). Gitmoot minted proof receipts for these jobs; one is committed verbatim in this folder (`codex-proof-receipt.txt`, proof `sha256:78b3ab08...`). |
| 2 | `019f6115-90d7-7b13-8cfc-d3dc57926dc6` | Jul 14 | 24,859,209 | **Feature: chained pipeline triggers** (#922, `trigger: pipeline`), the mechanism behind the Telegram delivery chain in the demo. Gitmoot-managed Codex agent `impl922`. |
| 3 | `019f6380-a7b1-7c82-bb64-1f8ff84fb30a` | Jul 15 | 3,582,523 | **Feature: pipeline descriptions** (#940). Gitmoot-managed Codex agent `impl940`. |
| 4 | `019f638d-ee99-7cc3-8fd8-64c7591ce759` | Jul 15 | 30,078,678 | **Feature: pipeline export/import bundles** (#935). Gitmoot-managed Codex agent `impl935`. |
| 5 | `019f63bd-097e-7422-85bf-a409d9e1e266` | Jul 15 | 13,755,176 | **Feature: pipeline publish/pull via GitHub** (#941), the sharing story on the Devpost page. Gitmoot-managed Codex agent `impl941`. |
| 6 | `019f704a-c8c6-7331-a990-e3465329df1f` | Jul 17 | 110,793,903 | **The demo pipeline repo itself** ([gitmoot/appkit-demo](https://github.com/gitmoot/appkit-demo)): Gitmoot-managed Codex implementer `g2impld3` building the launch-kit pipeline stages. |
| 7 | `019f8191-f721-79f1-9558-3c5f960da540` | Jul 20 | 2,387,485 | **Demo, graph side.** The single-prompt Codex session that ran the `appkit-pro` pipeline end to end (run `prun-appkit-pro-18c41ee476461c88`). Source of the graph column in the numbers table: 1 prompt, 4,231,476 tokens, 21:24 to Telegram delivery. |
| 8 | `019f7b98-2f12-7e90-94ce-5248969c289e` | Jul 19–20 | 18,814,978 | **Demo, loop side (baseline).** The plain-conversation Codex session doing the same mission without a pipeline. Source of the loop column: 6 prompts, 18,814,978 tokens, 54:41 active across two sittings. |
| 9 | `019f766e-3f8d-7b61-b0d9-74fadf3ce15f` | Jul 18 | 3,270,630 | Design brainstorm seat and reference marketing-loop footage. |
| 10 | `019f8180-c48e-71a0-8b2e-30ad98db83ad` | Jul 20 | 1,765,724 | Outtake: first graph-side attempt without the repo playbook. Codex misread normal scheduler gaps as a stall and improvised; it taught us the AGENTS.md patience rule described on the Devpost page. |

Beyond these interactive sessions, every `appkit-pro` run contains two **Codex (gpt-5.6-sol)
agent stages** executed by gitmoot itself: `derive` (reads the target repo, derives the app
identity with citations) and `content` (writes the store copy, running in parallel with the
screenshot branch). Gitmoot records per-job token usage for each (job IDs
`prun-appkit-pro-<run>-derive-a0` / `-content-a0`). The wider engine work during Build Week
was likewise implemented by Codex sessions coordinated through gitmoot; the branch and PR
history of this repo is the audit trail.

Token counts are each session's cumulative Codex usage (input + output, cache included).
The two largest sessions are the service layer (#1) and the demo pipeline build (#6);
the `/feedback` session for the Devpost form is #1, the one that built the submitted
core feature in this repo.

## Before the week (lineage, honestly excluded from the window claim)

The bare pipeline primitive (#681, a shell-only DAG runner) merged on July 6, before Build
Week. Codex's role there was architecture, not implementation: two research-panel seats
(`019f357a-a249-71f0-821c-0e8ab6638430`, 2,894,222 tokens and
`019f3597-a8eb-7652-b432-c182cc832ae1`, 5,509,044 tokens, July 6) whose settled design the
coordinator then implemented (PR #694 records the cross-runtime panel). Everything in the
table above is July 13–21 work.

## Files

- `codex-proof-receipt.txt` — a gitmoot proof receipt for the main implementation session,
  verbatim (`gitmoot proof` reproduces and verifies the manifest offline).
