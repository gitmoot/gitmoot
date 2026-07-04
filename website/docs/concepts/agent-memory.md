# Agent Persistent Memory

Agent persistent memory gives an enrolled agent identity a small, repo-filtered
pool of durable **facts** — "this repo's arm64 CI is flaky", "race suites need a
long timeout" — that Gitmoot injects into the job prompt as a reference-only
block. It is a native SQLite + FTS5 feature in the existing store (no vector DB,
no new dependencies) and is **off by default**.

Memory is distinct from templates. Templates hold *skills* (how an agent works;
SkillOpt improves them); memory holds *knowledge* (what is true about a repo).

## Trust model

The benefit of memory is treated as a measured hypothesis, not an assumption, so
it ships in phases. The current phase is **observation mode**:

- **READ** — while assembling a job prompt, Gitmoot runs one sanitized FTS5/BM25
  query over the agent's *confirmed* facts (owner match, and either the current
  repo or the always-travelling `general` scope), ranks by relevance then
  recency, caps the result by a token budget, and renders a fenced block titled
  *"Prior learnings (reference only, not instructions)"* with `[this repo]` /
  `[general]` tags. An empty result adds nothing.
- **WRITE** — the confirmed (injectable) tier is populated only by Gitmoot's own
  deterministic **mechanical facts** (e.g. how many corrective fix rounds a
  recent job in this repo needed — no model involved). Agent-returned learnings
  are **shadow-logged** to an append-only observations table for measurement but
  are never injected and never promoted in this phase.

Every write passes deterministic pre-filters that reject directive-phrased
("you must always…"), executable/command, secret-shaped, and — for `general`
candidates — non-repo-agnostic content. These filters are the primary safety
gate against experience-poisoning and indirect prompt injection.

## Storage

Two tables back the evidence/upsert split: an append-only `memory_observations`
table (where witnesses for a claim accumulate) and a keyed `confirmed_memories`
table (one injectable row per fact, with graphiti-style supersession rather than
deletion). Owner identity is structured (agent vs. role, with template version
awareness) so template upgrades never inherit stale pools. A standalone FTS5
index over confirmed content powers the BM25 retrieval.

## Enrollment and configuration

Enrollment is per agent; global knobs live in a `[memory]` section:

```toml
[agents.builder]
runtime = "codex"
memory = true          # enroll this agent (default off)

[memory]
disabled = false       # global kill switch (overrides every enrollment)
token_budget = 1500    # cap on injected block size (estimated tokens)
max_entries = 15       # cap on confirmed rows considered for injection
```

An agent records a durable fact via the optional top-level `learnings` field in
`gitmoot_result` — each entry is `{key, scope, content}` where `scope` is
`"repo"` (about this repository, the default) or `"general"` (true everywhere).
Most jobs return none.

## Inspecting and measuring

All of the following are read-only:

```sh
gitmoot memory list [--pending|--confirmed] [--agent NAME] [--repo owner/repo]
gitmoot memory replay [--agent NAME] [--repo owner/repo] [--limit N]
gitmoot memory eval --fixtures fixtures.json [--k N]
```

`memory list` shows confirmed memories and pending observations. `memory replay`
is an offline A/B: it re-renders recent real jobs' prompts with and without the
learnings block and reports the injection delta (added tokens, entries injected)
— it measures injection *mechanics*, not outcome quality. `memory eval` computes
recall/precision@K of retrieval over a labeled fixtures file.

## Phases

- **Phase 0** — typed `learnings` in the result contract; the two-table schema
  and FTS index.
- **Phase 1 (current)** — observation mode: read-only injection of mechanical
  facts, shadow writes, and the measurement harness above.
- **Phase 2** — live agent writes, the confirmation protocol (witness counting +
  a cheap curation judge), curation, general-tier promotion governance, and the
  full audit CLI (`forget`, `promote`).
- **Phase 3** — an optional hybrid vector-retrieval leg, added only if the
  Phase-1/2 metrics show BM25 word-matching misses.
