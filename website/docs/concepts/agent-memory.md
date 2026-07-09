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

- **READ:** while assembling a job prompt, Gitmoot runs one sanitized FTS5/BM25
  query over the agent's private *confirmed* facts plus the reserved shared pool
  (and either the current repo or the always-travelling `general` scope). BM25
  ranks first; if scores tie, the agent's private facts outrank shared facts,
  then recency breaks ties. A small floor guard keeps the strongest private match
  in the injected slice when private matches exist but shared rows would fill the
  limit. Gitmoot caps the result by a token budget and renders a fenced block
  titled *"Prior learnings (reference only, not instructions)"* with
  `[this repo]` / `[general]` tags. An empty result adds nothing.
- **WRITE:** the confirmed (injectable) tier is populated only by Gitmoot's own
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
awareness) so template upgrades never inherit stale pools. The reserved shared
pool uses `owner_kind = "shared"` and `owner_ref = "shared"`. When a fact moves
there, `author_ref` preserves who wrote it; an empty `author_ref` means the author
is the same as `owner_ref`. A standalone FTS5 index over confirmed content powers
the BM25 retrieval.

## Enrollment and configuration

Enrollment is per agent; global knobs live in a `[memory]` section:

```toml
[agents.builder]
runtime = "codex"
memory = true          # enroll this agent (default off)

[memory]
disabled = false            # global kill switch (overrides every enrollment)
token_budget = 1500         # cap on injected block size (estimated tokens)
max_entries = 15            # cap on confirmed rows considered for injection
distill_at_terminal = false # stage deterministic failure signal at terminal (P4.1)
distill_max_per_job = 3     # hard cap on distilled observations per job
distill_all_jobs = false    # true → distill every job, not only enrolled agents
```

Every `[memory]` key is read **per tick**, so flipping `distill_at_terminal`
(or any knob) takes effect on the next job with **no daemon restart**.

### Distill-at-terminal

`distill_at_terminal` (off by default) enables a deterministic producer that,
on an *anomalous* terminal (`failed`/`blocked`/`changes_requested`), mines the
job's own result for two closed-category signals — **failing tests** (test names
from explicit `--- FAIL:` markers in the job output, *not* mere presence in
`tests_run`, which only records that a test was **run**) and **named errors**
(stable tokens from the summary and the tail of the raw output, normalized by
stripping hashes, paths, addresses, line numbers, and timestamps). Unlike the mechanical facts above, distilled rows are written as
**pending observations** at trust `low` with provenance `distill:<job-id>` — they
are **never** confirmed memory, so the human `memory confirm` gate stays the only
promotion path.

Distill is bounded on every axis: each candidate passes the same PreFilter, a
content-hash dedup blocks a repeat from staging twice, and `distill_max_per_job`
caps writes per job. A **recurrence gate** stops a one-off failure from ever
becoming a pending memory — the first sighting of a normalized key records only a
low-trust *witness* (`distill-seen:<job-id>`), and the observation stages only
when the same key recurs across a later job. A witness is internal recurrence
bookkeeping: it is **never** shown in `memory list` and can **never** be promoted
by `memory confirm`, so a one-off failure is invisible until it recurs. By default distill follows
enrollment; `distill_all_jobs = true` harvests failure signal box-wide while the
read path and confirmed producers stay enrolled-only.

An agent records a durable fact via the optional top-level `learnings` field in
`gitmoot_result` — each entry is `{key, scope, content}` where `scope` is
`"repo"` (about this repository, the default) or `"general"` (true everywhere).
Most jobs return none.

## Inspecting and measuring

All of the following are read-only:

```sh
gitmoot memory list [--pending|--confirmed] [--agent NAME] [--repo owner/repo]
gitmoot memory recall "<query>" [--repo owner/repo] [--agent NAME|--shared] [--limit N]
gitmoot memory replay [--agent NAME] [--repo owner/repo] [--limit N]
gitmoot memory eval --fixtures fixtures.json [--k N]
```

`memory list` shows confirmed memories and pending observations. `memory recall`
is an on-demand relevance search over confirmed memory. It uses the same
FTS5/BM25 retrieval as prompt injection. By default it searches all agent pools
plus the shared pool; `--agent NAME` searches that agent's private pool plus
shared, and `--shared` searches only shared facts. Without `--repo`, recall
searches every repo and general-scope facts. `--repo owner/repo` narrows
repo-scoped facts to that repo while still including general-scope facts. JSON
output includes `author_ref` when a shared fact preserves a different author.
`memory replay` is an offline A/B: it re-renders recent real jobs' prompts with and without the
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
verbatim, and a `## Links` section of FTS co-occurrence plus persisted
`[[wikilinks]]`), a per-owner index note, and a `manifest.json` staleness anchor.
Shared notes include an `author:` frontmatter line when `author_ref` is set, so a
fact moved into shared still points graph tooling at the real author. `--agent NAME`
narrows the export to that agent's private facts plus shared facts authored by
that agent.

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

## Markdown ingest and the human confirm gate

The vault export is the bridge's *outlet*; `memory ingest` is its *mouth*. It
reads arbitrary Markdown (session notes, runbooks, incident writeups) and stages
it as **pending observations** behind the existing confirmation gate — it never
writes injectable memory directly.

```sh
gitmoot memory ingest <path|dir> --agent NAME [--shared] [--repo owner/repo] [--tier repo|general] [--dry-run] [--json]
gitmoot memory observations [--agent NAME] [--provenance-prefix P] [--json]
gitmoot memory confirm <obs-id>... | --provenance-prefix P [--agent NAME] [--to-shared] [--yes] [--json]
gitmoot memory promote --to-shared <id>... [--json]
gitmoot memory links backfill [--dry-run] [--json]
gitmoot memory links list <id> [--json]
```

`memory ingest` walks `*.md`, strips a leading YAML frontmatter block when
present, and chunks a file only when its body exceeds ~512 estimated tokens
(smaller files stay one observation). Over budget it splits on `## ` headings,
and any section still over budget is sub-split on paragraph/line boundaries so no
single chunk exceeds the token budget (an oversized memory would otherwise be
force-injected wholesale). Every chunk passes the same deterministic
**PreFilter** that gates agent learnings (rejecting directive-phrased,
secret-shaped, executable, or — for `--tier general` — non-repo-agnostic
content), reported as per-reason rejection counts. A chunk whose exact content
already exists **in the same visibility domain** (same scope and repo) is
**deduped**, so re-ingesting a source is a no-op — but the same note ingested
under a second repo still stages, because repo-scoped memory injects only for its
own repo. Survivors land in `memory_observations` with
`provenance = ingest:<relpath>` and `trust_mark = low`. `--dry-run` reports the
plan without writing. `--shared` stages observations in the shared pool and
records `--agent NAME` as the authoring identity.

`memory observations` lists pending observations, flagging which have already
been confirmed. `memory confirm` is the **human-gated promotion**: it copies
selected observations (by id, or every one matching a `--provenance-prefix`) into
confirmed memory, carrying provenance through. Without `--yes` it prints the plan
and writes nothing; with `--yes` it promotes idempotently. It is **CLI-explicit
only**: no daemon path, no auto-confirm. `--to-shared` confirms selected
observations into the shared pool while preserving the observation author.
`memory promote --to-shared <id>...` moves active confirmed facts into shared,
refuses retired or superseded rows, preserves existing links, and stamps
`author_ref` from the previous owner when needed.

When a fact is confirmed, Gitmoot also records up to three deterministic outgoing
links from that confirmed row to active related confirmed memories. These links
live in the `memory_links` side table with BM25-derived scores. They do not rewrite
the memory's content. Link candidates use the same private-plus-shared visibility
as prompt injection, so private facts can link to shared facts and shared facts
can link back through their author pool. `memory links backfill` runs the same
pass over all active confirmed memories in id order; `--dry-run` reports what
would be created, and repeat runs create nothing new. `memory links list <id>`
inspects a fact's persisted outgoing links. Vault export merges persisted links
with content-derived links in each note's `## Links` section and removes
duplicates by target.

:::warning Ingested Markdown is untrusted
Ingested Markdown is an **indirect-prompt-injection vector**. Ingest stamps
`trust_mark = low` on every observation, and observations are inert (never
injected) until a human runs `memory confirm`. That confirm step **is** the trust
boundary. Trust-aware injection — having the read path weigh `trust_mark` — is
future work; nothing reads `trust_mark` for a decision yet.
:::

## Grooming stale memory

`memory groom` mechanizes the periodic pass that retires stale, low-signal
confirmed memories, as an explicit **propose → review → apply** round-trip that
never mutates memory without an owner `--yes`:

```sh
gitmoot memory groom --propose [--out PLAN.json] [--json]
gitmoot memory groom --yes --plan PLAN.json [--json]
```

`--propose` reads every **active** confirmed memory (retired rows excluded),
computes the current vault `snapshot_hash` (the same anchor `vault export`/`import`
use), runs deterministic detectors, and writes a reviewable plan artifact
(`{schema_version, snapshot_hash, proposed_retirements, rewrite_flags, stats}`). It
touches nothing in the store. The detectors flag:

- **status/changelog/ToC snapshots** — notes dominated by `STATUS:` markers,
  `SHIPPED`/`merged & deployed` phrases, ISO-date-led lines, or Markdown link-list
  index entries (short notes under 3 lines also need a strong `STATUS:`/`… & deployed`
  marker, so a lone date-led or `SHIPPED`-mentioning keeper is not retired);
- **bare to-do lists** — content whose every non-blank line is a checkbox item;
- **exact duplicates** — identical content **within the same owner/repo/scope**; the
  lowest id is kept and the rest proposed. Copies across owners/repos/scopes are kept
  (each is the only one its scope can see);
- **over-long "bricks"** (> ~1200 chars) are **flagged for rewrite, not retired** —
  P4.2 only lists them for the owner (LLM rewriting is the follow-up P4.3).

`--yes --plan` recomputes the `snapshot_hash` and **aborts as stale** if it differs
from the plan's (a vault edit between propose and apply invalidates it), then
retires exactly the planned ids in one transaction (reason `groom:<detector>`,
clearing each from the FTS index). It is retire-only and idempotent — an
already-retired or missing id is skipped gracefully.

A ready-to-register nightly proposal pipeline (propose + notify-on-nonempty, apply
held behind the owner) lives in
[`docs/examples/memory-groom-nightly`](https://github.com/jerryfane/gitmoot/tree/main/docs/examples/memory-groom-nightly).

## Emergent clusters

`memory clusters` groups confirmed facts into **emergent communities** detected over
the fact-similarity graph — the **same** bm25 + id-tiebreak signal the vault
`[[links]]` use. They replace the dashboard's old fixed key-prefix "category" hubs:
clusters are discovered from what facts actually say, not how their keys are namespaced.

```sh
gitmoot memory clusters [--json]
gitmoot memory clusters recompute --propose [--out PLAN.json] [--json]
gitmoot memory clusters recompute --apply [--plan PLAN.json] [--json]
gitmoot memory cluster rename <cluster-id> <label>
```

- **Deterministic by construction.** The community detection is **id-ordered label
  propagation** with **lowest-label tie-breaks** over a fixed graph (sorted-id visit
  order, id-valued initial labels, summed neighbor influence). No map order,
  randomness, or wall clock enters, so the **same store yields byte-identical
  clusters, labels, medoids, and cluster ids** — matching the vault byte-identity rule.
- **Labels** are up to three distinctive terms (term frequency inside the cluster
  weighted against corpus document frequency), joined with `-` and anchored to the
  cluster **medoid** (the member with the highest total intra-cluster similarity) for
  stability. Facts with no neighbors fall into the reserved cluster **0 `unclustered`**.
- **`recompute`** is a human-gated **propose → review → apply** round-trip like
  `memory groom`: `--propose` writes a reviewable plan of the moves/merges plus a
  **staleness anchor** over every active fact's `(id, updated_at)`; `--apply --plan`
  re-checks the anchor, **aborts as stale** if the store moved, then rewrites the
  whole clustering in one transaction. On **first run** (no clusters yet) a bare
  `recompute --apply` is allowed since there is nothing to protect.
- **Incremental attach:** confirming a new fact best-effort joins it to the cluster
  of its nearest neighbor without a full recompute; nothing is re-shelved silently.
- `memory cluster rename` sets an **owner label override** that wins over the computed
  label and survives recomputes (anchored to the medoid).

The dashboard Knowledge view renders this as a **repo → cluster → fact** hierarchy.

## Phases

- **Phase 0** — typed `learnings` in the result contract; the two-table schema
  and FTS index.
- **Phase 1 (current)** — observation mode: read-only injection of mechanical
  facts, shadow writes, and the measurement harness above.
- **Phase 2** — live agent writes, the confirmation protocol (witness counting +
  a cheap curation judge), curation, general-tier promotion governance, and the
  remaining audit CLI.
- **Phase 3** — an optional hybrid vector-retrieval leg, added only if the
  Phase-1/2 metrics show BM25 word-matching misses.
