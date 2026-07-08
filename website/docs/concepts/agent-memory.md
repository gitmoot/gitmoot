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
  deterministic **mechanical facts** (no model involved). A fact is written only
  when a terminal job carries a genuine, bounded signal — never one fact per job:
  a **fix-round fact** when a job needed corrective verify/retry rounds, and a
  **terminal-outcome fact** when an *ordinary* job (an `agent ask`/`run`/`review`
  job with no verify/retry loop) ends on the **notable** decision
  `changes_requested` — a normal, repeatable review conclusion. A routine
  first-try success writes nothing, and the *anomalous* one-off terminals
  (`failed`, `blocked`) are deliberately **not** auto-promoted: with no recurrence
  threshold yet, a single flaky failure must not become a durable, injected repo
  fact. Facts are keyed by low-cardinality **closed** categories — the outcome is
  a validated decision value and the action is collapsed to a small fixed
  allowlist (any free-form delegation action buckets to a generic token), never
  free-form content — so repeated jobs UPSERT the same row rather than growing the
  pool. Agent-returned learnings are
  **shadow-logged** to an append-only observations table for measurement but are
  never injected and never promoted in this phase.

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

## Vault view (a derived, disposable Obsidian view)

```sh
gitmoot memory vault export [--out DIR] [--agent NAME] [--force] [--json]
gitmoot memory vault import <DIR> [--dry-run|--yes] [--json]
```

`memory vault export` renders confirmed memory as an Obsidian-compatible vault:
one Markdown note per confirmed memory (sorted-key YAML frontmatter, the content
verbatim, and a `## Links` section of FTS co-occurrence `[[wikilinks]]`), a
per-owner index note, and a `manifest.json` staleness anchor.

The vault is a **view, not a replica**: SQLite stays the *only* source of truth,
so the export never becomes a second store to keep in sync. It is regenerated
from scratch on every run, is safe to delete, and is fully **deterministic** —
the same store produces byte-identical files (there is deliberately no
`exported_at`, and filenames are stable `NNNNNNNNN-<slug>.md` derived from the
memory id). That determinism is what lets `vault import` (below) diff hand-edits
against a fresh export. The export is read-only (zero writes to any table) and
atomic (it writes a temp directory and renames it over `--out`, which defaults to
a `vault/` directory under the home's evals area). Since the export **replaces
`--out` wholesale**, it refuses to overwrite a non-empty directory that is not
itself a prior gitmoot vault (one carrying a `manifest.json`), so pointing it at
an existing Obsidian vault such as `--out ~/my-vault` can never silently delete
your own notes; pass `--force` to override.

`memory vault import <DIR>` closes the loop as the **human curation gate**: export a
vault, edit/delete/add notes in any editor, then `import` **diffs the folder against
a fresh export** and applies only on confirmation. The diff is the audit trail. It
regenerates a fresh export first and **aborts as stale** if the store moved since the
vault was written (the manifest `snapshot_hash` mismatches), so a stale edit can
never clobber newer facts. Then:

- an **edited** note rewrites its source memory's content — an optimistic
  **compare-and-set on `updated_at`** targets the exact row (never key-based, so it
  can't clobber a different fact) and resyncs the FTS index;
- a **deleted** note **retires** its memory: additive `retired_at`/`retired_reason`
  columns plus FTS removal stop it being injected or exported, while the row is kept
  for audit (retirement is distinct from `superseded_by` replacement and never
  hard-deletes);
- a **new** `.md` file (no `memory_id`) stages a **pending observation**
  (`provenance=vault-import:<file>`, trust `normal` — it is owner-authored) behind
  the usual confirmation gate; it is never auto-confirmed.

Frontmatter identity edits (key/scope/owner) are out of scope — detected, warned,
and skipped (only the content edit lands). `--dry-run` is the **default**: it prints
the diff and writes nothing. `--yes` applies edits, retirements, and new
observations in **one transaction** (all-or-nothing). If any note fails to parse
(e.g. broken YAML frontmatter) `--yes` **refuses to apply** — a malformed note could
otherwise be misread as a deletion and silently retire a live memory. A vault
produced by `export --agent NAME` stays importable even when other owners have
memories, because import rebuilds the fresh export with the manifest's recorded scope.

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
