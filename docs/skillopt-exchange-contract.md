# Gitmoot-SkillOpt Exchange Contract

Gitmoot keeps the SkillOpt optimizer outside the main binary. The boundary is a
pair of JSON package formats handled by `gitmoot skillopt export` and
`gitmoot skillopt import`.

For the guided product workflow, use `gitmoot skillopt train` instead of
assembling every low-level command manually. Train mode creates sessions and
iterations, manages review items, generates options, publishes review packets,
syncs feedback, exports the package, runs the external optimizer, imports a
pending candidate, publishes candidate review context, and starts follow-up
iterations only after an explicit decision. The low-level commands documented
here are still useful for advanced debugging, custom research runs, and
recovering individual train steps.

See [SkillOpt Train Workflow](skillopt-train-workflow.md) for the end-to-end
train-mode sequence.

## Training Package

Export a local eval run:

```sh
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

The exported package has:

- `kind: gitmoot-skillopt-training-package`
- `contract_version: 1`
- `training_mode`: the Gitmoot training/review workflow mode, such as
  `explore`, `refine`, `distill`, or `validate`
- `template`: the logical template id, current or pinned version id, content
  hash, metadata, source, and exact template content
- `eval_run`: run id, target repo, state, mode, exploration level, option
  count, metadata, and template version
- `items`: review/eval items with artifact references for source, baseline,
  candidate, preview, diff, and ranked option artifacts
- `artifacts`: local artifact manifests with content hashes, media type, size,
  and driver
- `feedback_events`: canonical human feedback events when available
- `ranked_feedback_events`: canonical N-way ranked feedback events when
  available, including the ordered ranking, optional winner, trait notes, and
  reasoning
- `pairwise_preferences`: derived pairwise preferences expanded from ranked
  feedback, for example `C > A > D > B` becomes six ordered preferences
- `evaluator_config`: evaluator and run metadata used by the external
  optimizer. Top-level workflow mode is exported as `training_mode`, not as
  `evaluator_config.mode`; `evaluator_config.mode` is reserved for evaluator
  implementation ids or drivers. The judge config additively carries
  per-`task_kind` judge prompts under `judge.config.judge_prompt_templates`
  (with a `judge_prompt_version`), so a judge prompt tuned by gitmoot-skillopt's
  judge-prompt optimization can be selected per task kind without a
  `contract_version` bump.

Artifact package entries reference local SHA256 blobs stored under Gitmoot home.
The export does not copy blobs into the repository by default.

A/B validation runs keep the existing `feedback_events` shape. Ranked
exploration data is additive: optimizers that only consume A/B validation can
ignore `items[].options`, `ranked_feedback_events`, and
`pairwise_preferences`.

## Ranked Exploration Export

Ranked runs use `mode` to tell the optimizer how broad the update should be:

- `explore`: learn broad directions from four to six diverse options.
- `refine`: combine winning traits into a smaller set of stronger candidates.
- `distill`: update the template body from accumulated feedback.
- `validate`: compare the current template against a candidate on fresh review
  items.

The `exploration_level` field describes how much variation the optimizer should
try in the next candidate set:

- `high`: prioritize wider exploration and visibly different outputs.
- `medium`: combine promising traits while still testing alternatives.
- `low`: make narrow refinements and prepare for validation.

Each ranked item exports `options` with the blind option label, artifact id,
role, and optional metadata such as preview URLs. Ranked feedback exports:

- `ranking`: canonical option labels ordered best to worst
- `winner`: optional first-place option
- `useful_traits`: JSON object keyed by canonical option label
- `rejected_traits`: JSON object keyed by canonical option label
- `required_improvements`: JSON array of requested improvements not tied to one
  option
- `reasoning`: reviewer notes

Derived `pairwise_preferences` are provided so an optimizer can use simple
preference comparisons without reparsing every ranking. Each pairwise row
includes `ranked_event_id`, which matches the corresponding
`ranked_feedback_events[].id`. Trait notes remain attached to the ranked
feedback event so a future optimizer can combine useful traits across multiple
winning options rather than only copying the top option.

## Normalized \{score, feedback\} projection

The training package already carries a scalar quality signal and rich textual
feedback, but they are spread across several optional fields. Gitmoot exposes a
pure, read-side helper, `skillopt.ProjectSignal`, that fuses those existing
fields into one uniform `NormalizedSignal{score, has_score, feedback}` view so a
consumer reads one signal instead of N optional fields.

This projection is **read-side only**: it is a return type, not a wire field. It
adds no field to any package, does not change the JSON the optimizer subprocess
reads, and leaves `contract_version` at `1`. The training and candidate packages
are byte-identical with or without it.

**Scalar (`score` in `[0,1]`, with `has_score`).** Precedence is
`soft > mean(dimension_scores) > hard`:

- If `hard` is present and equal to `0` (the gate failed), `score = 0` and
  `has_score = true` — a hard-fail is an authoritative, informative `0`, not
  "missing".
- Otherwise the quality component is `soft` if present, else the arithmetic mean
  of `dimension_scores` when that map is non-empty, else `hard` when `hard > 0`.
  A positive `hard` is a gate, not a weight, so it does not scale the component.
- If a quality component exists, `score` is that value clamped to `[0,1]` and
  `has_score = true`.
- If no usable field exists (`hard`, `soft`, and `dimension_scores` all absent),
  `has_score = false`. The projection reports genuine absence rather than
  fabricating a neutral `0.5`, so consumers can omit rather than invent a value.

**Textual (`feedback`).** Non-empty parts are concatenated in a fixed section
order — `optimizer_hint`, then `required_improvements`, then `useful_traits`,
then `rejected_traits`, then `reasoning` — each under a stable header. Trait
fields are best-effort decoded into their known shapes (`map[string][]string`
keyed by option label, or `[]string`); map keys are rendered in sorted order for
deterministic, byte-stable output, and any section that fails to decode is
skipped rather than dumped raw. The assembled string is bounded to an 8 KiB byte
cap; when it would exceed the cap it is truncated on a UTF-8 boundary with a
trailing `… (truncated)` marker. When no textual signal is present, `feedback`
is empty.

## Automatic trace-harvested feedback (Mode A, off by default)

Gitmoot can derive training feedback from the **verifiable outcomes** an
implement job already reaches — a PR that merged with passing external CI vs. one
that was blocked at the merge gate, a review that requested changes, or a later
revert — and write it as synthetic feedback into the **existing** feedback tables
without any human ranking. This is **Mode A** of the trace-harvester (issue
#465). It is **off by default** and **strictly additive**: `contract_version`
stays `1`, no new field is added to any package struct, and **promotion stays
100% manual** — the harvester writes only `eval_runs`, `eval_review_items`, and
`feedback_events`; a human still promotes a candidate with
`gitmoot skillopt candidate promote`.

**Enabling.** Add a `[skillopt]` section to the gitmoot config:

```toml
[skillopt]
auto_trace_enabled = true   # default false
```

With the key unset or `false`, no harvester is constructed and behavior — and
every human-run training package — is **byte-identical**. The admission knob
mirrors the off-by-default `[events]` stream.

**What gets written.** On a verifiable implement-job outcome the harvester:

- Resolves the job's template version from its `template_id` +
  `template_resolved_commit` attribution.
- Upserts one dedicated **auto-trace `eval_run` per template version**, with id
  `auto-trace:<template_version_id>`, `mode = validate`, and
  `metadata_json` carrying `feedback_source = automatic_trace`.
- Upserts a per-PR `eval_review_item` (item id `<repo>#<pr>`).
- Upserts one `FeedbackEvent` (not a ranked event — a single implement diff has
  no paired second artifact) tagged `source = auto-trace`,
  `reviewer = gitmoot-auto`, with the verifiable-outcome `score`/`feedback`
  assembled by `skillopt.ProjectSignal` (above). A **positive** outcome is
  `choice = a` (the current promoted template as the implicit baseline champion);
  a **negative** is `choice = b`. The textual `reasoning` is the projected
  `NormalizedSignal.feedback`.

**Outcome → score.**

- **Merged with real CI** (a passing non-`gitmoot/` external status/check at the
  merged head) → strong positive (`soft = 1.0`, `choice = a`).
- **Merged through an empty gate** (the synthetic `gitmoot/ci` context, or no
  external CI at head) → **near-neutral** (`soft ≈ 0.5`, `choice = a`), never a
  strong positive. Rewarding an empty gate would optimize toward "merges that
  pass no real CI"; the no-CI guard reads the combined status at the merged head
  SHA and demotes it. (On the merge side, an empty gate is itself deferred by a
  grace window and workflow-awareness before it ever stamps `gitmoot/ci` — see
  [`SAFETY.md`](../skills/gitmoot/references/SAFETY.md); #596 — so this
  near-neutral band applies only to genuinely CI-less heads.)
- **Blocked at the merge gate** → authoritative gate-fail (`hard = 0`,
  `choice = b`).
- **Review changes_requested** → graded negative (`choice = b`) whose score
  decreases with the fix-round count.
- **Reverted** → corrective negative. A later revert of a previously-merged PR
  **re-upserts the same** `UNIQUE(run_id, item_id, reviewer, source, source_url)`
  row, flipping the earlier positive `choice = a` to `choice = b` **in place**
  (the row count is unchanged). **Wired (#467):** the daemon PR-watcher detects a
  GitHub Revert-button PR — whose body is `Reverts owner/repo#NN` / `Reverts #NN`,
  same-repo only — being **merged**, parses the **original** PR number, and (via
  `Engine.HandlePullRequestReverted` → `implementJobForTask` → the harvester)
  fires the `reverted` outcome against the original PR's auto-trace row, attributed
  to the original implement job's template version. It is gated off-by-default on
  `auto_trace_enabled` **and** the optional opt-out `revert_detection_enabled`
  (unset = on whenever the harvester is on; set `false` to keep the harvester
  running but turn the corrective revert overwrites off). It is best-effort and
  fail-safe: a malformed/cross-repo body, an unresolvable original implement job,
  or a harvest error never blocks or fails the poll. Re-polling the persistent
  revert PR is idempotent (the in-place upsert re-writes the same `choice = b`
  row). **Not detected** (best-effort v1): a revert opened without the
  `Reverts #NN` body line, or a `git revert` pushed straight to the default branch
  with no PR. With `auto_trace_enabled` off (the default) **no PR body is parsed**
  and behavior is byte-identical.

**Scope.** Only implement-family jobs that carry a template attribution are
harvested; coordinator continuation jobs (which produce no diff of their own) and
non-implement jobs are skipped. Only genuine outcome transitions are harvested —
operational job events (`runtime_lock_wait`, `repair_retry`,
`comment_post_failed`, `advance_retry`, …) never produce feedback. Harvesting is
**best-effort**: a harvest error never blocks or fails a job; it is swallowed and
recorded as an `auto_trace_harvest_failed` job event.

**Export side.** When `ExportTrainingPackage` runs over an auto-trace run, the
run-level `feedback_context.feedback_source` is `automatic_trace` (rather than the
human default `imported_human_review`), and the run lives in its own
`auto-trace:<version>` namespace, so a consumer can filter or down-weight
automatic feedback independently of sparse human gold. A human run never carries
the `feedback_source` metadata key, so its export is byte-identical.

### Cross-family review soft signal (off by default)

Setting **both** `[skillopt].auto_trace_enabled = true` **and**
`[skillopt].cross_family_review_enabled = true` (both default `false`) adds a
read-only **cross-family review leg** on every merge. A reviewer of a *different
runtime family* than the implementer (codex→claude, claude→codex, kimi→claude —
preferring a registered review-capable agent of another family scoped to the
repo, else an ephemeral different-family read-only leg in the `verifier.md` style)
scores subjective quality **and** scope-fidelity as ONE rubric:

- **coverage / containment / fidelity** — intended scope (the implement job's
  instructions + task title + goal title) vs. the delivered work (the PR diff,
  read-only at the head SHA, plus the agent's self-reported `changes_made` as a
  secondary cross-check). Coverage = did all of the ask; containment = no creep
  beyond it; fidelity = the ask, not something adjacent.
- **architecture / readability / abstraction** — subjective code quality.

Each dimension is in `[0,1]`. The rubric maps to `dimension_scores`, is fused by
the **mean-of-dimensions** path, and is written as a **second** `FeedbackEvent` in
the SAME `auto-trace:<version>` run under a distinct item id (`review#<repo>#<pr>`)
and the fixed `gitmoot-review` reviewer sentinel, so it **never overwrites** the
verifiable floor — and so a re-review by a *different* reviewer family overwrites
the row in place rather than accumulating a stale duplicate. The mapped mean
**drives the a/b choice** (a below-`0.5` mean registers as a non-baseline `b` vote,
not a baseline win) and an empty rubric writes **no row**. The review eval item
carries `judge_derived = true`, the `reviewer_runtime` family, the projected
`rubric_score`, and (on the same-family fallback) `self_family = true`, while the
run keeps `feedback_source = automatic_trace` — additive, **no new contract field,
`contract_version` stays `1`**.

The signal is **soft, judge-tagged, and weighted low** — weight tiers:
**human gold > verifiable floor > cross-family judge > same-family judge.** It
runs **off the blocking merge path** and is best-effort: a review-leg failure
records a `cross_family_review_failed` job event and never blocks or fails a job.
The rubric text is **never** injected into the implementer's prompt (anti-gaming).

When no different-family reviewer is available/authed, gitmoot falls back to a
**same-family** reviewer — never silently: it logs and records a
`cross_family_review_samefamily_fallback` job event, tags the review item
`self_family = true` (with `reviewer_runtime` carrying the family), and weights it
below a cross-family review. Only when no review-capable runtime is authed at all
is the review skipped (no review row).

The judge is uncalibrated in this slice — judge-tagged and weighted low, with a
measure-the-judge calibration hook for #344/#345 as a follow-on. Promotion stays
manual (the harvester writes only `eval_runs` / `eval_review_items` /
`feedback_events`); the review signal feeds the optimizer like any other and is
subject to the configurable `[skillopt].auto_promote` policy (#463) when that
lands — weighted-low + judge-tagged, **not** barred from promotion.

### Deterministic checker objective signal (off by default)

Setting **both** `[skillopt].auto_trace_enabled = true` **and**
`[skillopt].deterministic_checkers_enabled = true` (both default `false`) adds an
**OBJECTIVE, non-LLM** checker leg on every merge — the tool-measured complement to
the subjective cross-family review. Where the review asks an LLM to *estimate*
quality, the checker runs plain external **tools** that *measure* it exactly:

- **`diff_size`** — pure-Go, **always available** (no tool, no checkout): parses the
  PR file patches, counts changed `+/-` hunk lines + changed files, and normalizes
  against a soft cap so a tight diff scores ~`1.0` and a larger-than-budget one
  scores lower. It is the one dimension that is never a no-op on a tool-less host.
- **`duplication`** — code-clone count via `dupl` (else `jscpd`).
- **`lint`** — issue count via `golangci-lint`.
- **`complexity`** — over-threshold function count via `gocyclo`.

Each tool dimension is normalized to `[0,1]` (fewer issues → higher). The optional
`[skillopt].deterministic_checkers` comma list selects which run (default:
`diff_size` only); widen it to opt heavy tools in. The dimensions map to
`dimension_scores`, are fused by the **mean-of-dimensions** path, and are written as
a **third** `FeedbackEvent` in the SAME `auto-trace:<version>` run under a distinct
item id (`checker#<repo>#<pr>`) and the fixed `gitmoot-checker` reviewer sentinel —
so it **coexists with** (never overwrites) both the verifiable floor (`gitmoot-auto`,
`repo#pr`) and the cross-family review (`gitmoot-review`, `review#<repo>#<pr>`) under
the run's UNIQUE `(run_id,item_id,reviewer,source,source_url)` key. A re-evaluation
re-upserts the SAME checker row in place (stable row count). The checker eval item
carries `objective = true`, the projected `checker_score`, and the per-dimension
`dimension_scores`, while the run keeps `feedback_source = automatic_trace` —
additive, **no new contract field, `contract_version` stays `1`**.

These dimensions are **objective and un-gameable**, so the export/optimizer weights
them distinctly from the subjective judge row via the `gitmoot-checker` sentinel +
the `objective` item tag. The leg runs **off the blocking merge path** (detached,
timeout-bounded) and is **fail-safe throughout**: a missing tool binary
(`LookPath` miss), no PR-head checkout, a tool error, or a timeout **SKIPS that one
dimension** (no row for it) and never errors the harvest, blocks the merge, or
stalls `AdvanceJob`. An all-skipped run yields an empty rubric ⇒ **no checker row**.
A dispatch failure records a `deterministic_checkers_failed` job event and is
swallowed. Promotion stays manual (the leg writes only `eval_runs` /
`eval_review_items` / `feedback_events`).

> Note: the merge-gate path holds the daemon's own checkout, not a working tree at
> the PR head, so the tool-backed dimensions are effectively produced only when such
> a checkout happens to be available; `diff_size` always runs. Plumbing a PR-head
> worktree through to the tool runners is a documented follow-on.

### Promotion policy + notifications (off by default)

When a candidate becomes **pending** (after `skillopt import` or `train
continue`), gitmoot can (a) **notify** over the [event stream](events.md) and
(b) optionally **auto-promote** it — both **off by default** and **additive**
(#471). Importing **never** promotes: the import write only ever creates the
pending version; the notify + auto-promote is a separate, config-gated step that
runs *after* import returns and, on a guardrails pass, calls the same
`gitmoot skillopt candidate promote` machinery.

**Notifications (always-on when `[events]` is configured).** Every newly-pending
candidate emits a single **`candidate.awaiting_promotion`** event carrying the
version id (`job_id`), template id (`root_id`), and a redacted score/samples/CI
reason — independent of the auto-promote policy, so a human is notified even in
the manual default. The dashboard **Attention** page also lists every pending
candidate (read-only), so candidates are visible locally even with `[events]`
off. A successful auto-promotion additionally emits **`candidate.auto_promoted`**
so the change can be reviewed or rolled back (`gitmoot skillopt candidate` /
template revert) even in full-auto.

**Auto-promote (gated on guardrails, fail-safe).** With `auto_promote = true`, a
candidate is auto-promoted **only when every configured guardrail holds**; **any**
uncertainty fails safe to *notify, don't promote*:

```toml
[skillopt]
auto_promote = false                      # default false = manual, byte-identical
auto_promote_min_samples = 0              # UNSET = hard "do not promote" (NOT 0)
auto_promote_min_score = 0.0              # UNSET, or a candidate with no score = do not promote
auto_promote_require_external_ci = false  # require >= 1 real-external-CI feedback event in the eval_run
auto_promote_require_measured_judge = false  # PARSED but DEFERRED (#344): true => fail safe to no-promote
auto_promote_canary = false               # #484: true + a valid sample => promote to a CANARY (sampled) instead of direct; false = direct promote
auto_promote_canary_sample = 0.1          # #484: canary sampled-traffic fraction in (0,1]; UNSET disables the canary (notify-only fail-safe when canary on)
auto_promote_min_confidence = 0.95        # #473 Mode B: UNSET = ignore; SET = require bandit P(>) >= floor (+ enough samples)
bandit_min_samples = 30                   # #473/#482 Mode B low-traffic floor (manual `skillopt ab` always allowed)
live_ab_sample_rate = 0.0                 # #482 Mode B live A/B: 0.0 (default) = never intercept; fraction of foreground asks to A/B
mode_b_judge_enabled = false              # #483: opt the A/B into a cross-family LLM judge row (same as `skillopt ab --judge`)
mode_b_jury_size = 1                      # #349: >= 2 = a cross-family judge JURY (distinct families, majority vote); 0/1 = single judge, byte-identical
mode_b_jury_veto_dimensions =             # #349: comma list of rubric dimensions subject to minority-veto (e.g. safety,correctness); empty (default) disables the veto
mode_b_jury_veto_floor = 0.0              # #349: [0,1] floor for the veto dimensions; 0.0 (default) makes the veto inert
mode_b_jury_disagreement_tau = 0.0        # #349: per-dimension std threshold that flags jury DISAGREEMENT; 0.0 disables the std check
```

The guardrails read the candidate's **harvester auto-trace run**
(`auto-trace:<template_version_id>`) — the run the OutcomeHarvester writes the
verifiable `{score, feedback}` signal into — **not** the human/markdown review
run. A feedback read error (or an unresolvable run) is treated as *feedback
unavailable* → notify-only (we never promote on evidence we could not read), and
**zero feedback samples is always a hard do-not-promote** regardless of the
configured `auto_promote_min_samples` (so a sparse `skillopt import` with no
auto-trace evidence fails safe to notify-only).

- `auto_promote_min_samples` — the count of `feedback_events` in the candidate's
  auto-trace run must meet this floor. **Unset is a hard do-not-promote, never
  `0`**, so flipping `auto_promote` on without a sample floor never promotes. Even
  an explicit `auto_promote_min_samples = 0` cannot promote a zero-evidence
  candidate — there is an absolute floor of at least one real sample.
- `auto_promote_min_score` — the candidate's `summary.score` must be `>=` this.
  **Unset, or a candidate with no score, is a hard do-not-promote.**
- `auto_promote_require_external_ci` — at least one auto-trace feedback event must
  record a merge that passed **genuine external CI** (not the no-CI near-neutral
  band). The predicate keys off the harvester's own provenance (the `auto-trace`
  source + `gitmoot-auto` reviewer) AND its real-CI marker phrase, so it reads
  only the harvester's verifiable rows and a sibling cross-family review row's
  free-text findings can never spoof it. It therefore applies only to **Mode A
  (auto-trace) runs**; a candidate with no harvested external-CI evidence fails
  safe to notify-only.
- `auto_promote_require_measured_judge` is **parsed but deferred**: there is no
  judge↔human calibration source yet (gated on #344), so setting it `true` **forces
  notify-only** today. It is documented so a user can write the knob
  forward-compatibly.
- `auto_promote_canary` + `auto_promote_canary_sample` (#484) turn a guardrails-pass
  promotion into a **canary** instead of a direct promote. When `auto_promote_canary`
  is on **and** `auto_promote_canary_sample` is a fraction in `(0,1]`, the candidate
  is promoted to the `canary` state behind the live champion (which stays `current`,
  so non-sampled resolutions are byte-identical) and that fraction of new job
  resolutions route to the canary version. A bounded daemon **regression window**
  then reuses the candidate's auto-trace outcomes (no new evaluator) to compare the
  canary against the prior champion: parity-or-better **graduates** it to `current`
  (`candidate.auto_promoted`); a material regression **auto-rolls-back** (champion
  stays current, canary rejected, `candidate.rolled_back`). It is **fail-safe** —
  too few canary outcomes, no champion baseline, or unread feedback all **hold**
  (keep sampling), never rolling back on unread evidence and never graduating without
  confirming non-regression. `auto_promote_canary` on with the sample **unset/invalid
  forces notify-only** (a misconfigured canary never promotes). Off (the default) is
  byte-identical to the direct promote.
- `auto_promote_min_confidence` (#473 Mode B) is **additive and optional**. **Unset
  ignores it entirely** — the auto-promote decision is byte-identical to the
  guardrails above. When **set**, auto-promote additionally requires a non-nil
  bandit confidence `P(challenger > champion) >=` this floor, backed by at least
  `auto_promote_min_samples` bandit pulls (a Beta posterior on a couple of pulls
  can read 0.9 by luck — the sample floor refuses thin evidence). A nil, low, or
  thin confidence **fails safe to notify-only**. The confidence is supplied by the
  manual `skillopt ab` A/B (below); promotion is still the existing
  `PromoteAgentTemplateVersion` path, never a bypass.

## Champion-challenger A/B preferences (Mode B, off by default)

For **ask / research** agents there is no verifiable terminal outcome (no PR / CI
/ merge), so Mode A's single-artifact harvester and the score/CI guardrails above
cannot supply a promotion confidence. **Mode B** (issue #473, scoped from RFC
#463) closes the loop via **pairwise preference**: A/B a query across two template
variants and feed the human's pick to the optimizer as a contract
`RankedFeedbackEvent`.

```sh
gitmoot skillopt ab <agent> "<prompt>" [--challenger <versionId>] [--pick a|b] [--seed N] [--judge] [--judge-only] [--home path]
```

- **Champion** = the agent template's **current promoted** version; **challenger**
  = a **pending** candidate version (the sole pending one, or `--challenger`). With
  **no pending challenger** the command is a clean no-op (`nothing to A/B …`) — no
  rows are written.
- It delivers **both** variants through the runtime adapter (serialized, so the two
  one-shot asks never overlap and runtime-session locks release cleanly), presents
  the two answers **label-shuffled** as Option A / Option B, and records the human
  pick. The shuffle mapping is recorded so the stored event maps the pick back to
  the **correct** champion/challenger label.
- Each pick writes a 2-option `eval_run` (`OptionsCount = 2`,
  `metadata.feedback_source = preference_ab`, run id `skillopt-ab:<versionId>`),
  **two `eval_review_options`** (`champion` / `challenger`) whose answer text is
  stored as `eval_artifacts` via the existing blob path, and **one
  `RankedFeedbackEvent` per pick** (`ranking = [winner, loser]`, `reviewer =
  human`, `source = skillopt-ab`) that passes `validateRankedFeedbackEventOptions`.
  Repeated A/Bs of the **same** challenger each persist as a **distinct** preference
  row: the `ranked_feedback_events` conflict key is
  `(run_id, item_id, reviewer, source, source_url)`, and every pick carries a unique
  per-pick `source_url`, so picks accumulate as evidence instead of overwriting one
  row. The distinct `source` tag and run-id prefix keep Mode B rows trivially
  separable from Mode A (`auto-trace`) and human gold. **No contract struct changes
  — `ContractVersion` stays `1`.**

**The bandit.** Each `(template_id, version_id)` variant is one **Beta-Bernoulli
arm** (uniform `Beta(1,1)` prior; a win bumps `alpha`, a loss bumps `beta`),
persisted in the dedicated `skillopt_bandit_arms` table as the sufficient
statistic. A pick increments the winner (`+win`) and loser (`+loss`), then the
confidence `P(challenger > champion)` is recomputed by Monte Carlo (deterministic
given an injected seed) and printed as `NN% likely better over K samples`. That
scalar is exactly what `auto_promote_min_confidence` consumes: at the candidate
notify seam the candidate's challenger arm + the champion arm produce the
confidence carried into `candidate.awaiting_promotion`'s detail and into the
auto-promote guardrails.

**Tiering.** Below `bandit_min_samples` (default 30) the bandit still records
preferences and updates the posterior but live-traffic interception never fires
and the confidence is never trusted to auto-promote (the promotion-side floor
reuses `auto_promote_min_samples`). The **manual** `skillopt ab` CLI is always
allowed regardless of the floor.

### Live-traffic A/B interception (off by default, #482)

The same champion-vs-challenger pick can fire **automatically** on a **sampled
fraction** of real foreground `gitmoot agent ask` traffic, so a template
self-improves the more you use it — no operator has to remember to run
`skillopt ab`. It is purely an **additional caller** of the #473 surface above: it
adds **no new bandit math and no new contract field** (`ContractVersion` stays
`1`), only a sampling decision, the `bandit_min_samples` traffic floor, and a
serialized double-`Deliver` at the ask seam.

- **Off by default.** `live_ab_sample_rate` is `0.0` (and unset with no
  `[skillopt]` section), which makes the foreground ask path **byte-identical** —
  no extra `Deliver`, no A/B row, no bandit update; the hot path is untouched when
  off.
- When `live_ab_sample_rate > 0`, a foreground ask on a **managed** agent whose
  **champion bandit arm** has accrued `>= bandit_min_samples` pulls is intercepted
  with probability `live_ab_sample_rate`. Low-traffic / bespoke agents (e.g. the
  `researcher`) below the floor are **never** auto-A/B'd.
- An intercepted ask runs the **champion** (the canonical answer you receive, via
  the normal job path) and then a single pending **challenger** strictly
  **serialized under the one runtime-session lock already held** by the foreground
  ask — never a second lock, so it can never self-deadlock on `session is busy`. It
  presents both answers and routes the human pick through the **exact same**
  `recordSkillOptABPick` path as the manual A/B (same `source = skillopt-ab`
  `RankedFeedbackEvent`, same Beta-Bernoulli bandit update).
- It is **fail-safe**: a challenger `Deliver` error, no single pending challenger,
  or no pick captured degrades to the normal single champion answer and logs a
  `live_ab_skipped` job event — the primary ask is never blocked or degraded. Each
  intercepted ask runs the runtime **twice** (the sampled cost, bounded by the rate
  + floor).
- **Promotion stays MANUAL.** Live A/B only writes feedback + updates the
  posterior; it never auto-promotes or rolls back a version. The bandit confidence
  still flows into `auto_promote_min_confidence` as a **suggestion**, gated by an
  explicit human `promote`.

### Cross-family LLM-judge auto-pairwise (off by default, #483)

Every A/B pick needs a human in the loop, so the comparison signal is cheap to
want but expensive to get. With `--judge` (or `[skillopt].mode_b_judge_enabled =
true`, both **off by default**), in addition to the human pick a **cross-family
LLM judge** — a runtime family **different** from the agent under test — also
picks the better of the two answers automatically:

```sh
gitmoot skillopt ab <agent> "<prompt>" --judge      # human pick AND a judge row
gitmoot skillopt ab <agent> "<prompt>" --judge-only # only the judge row (skip the human prompt)
```

- **Off by default:** with neither the flag nor the config knob set, no
  cross-family judge is selected, no judge delivery happens, and no judge row is
  written — **byte-identical** to the #473 human-only path.
- **Cross-family only.** The judge family is chosen by the **same**
  `PickCrossFamilyReviewer` selector Mode A uses; a `SelfFamily` result (no
  different family available) is **skipped with a clear message**, never a silent
  same-family self-judgment of one's own template family.
- The judge is shown the **same shuffled Option A / Option B** the human sees (it
  never learns champion vs challenger) and returns a small `{"pick":"a"|"b"}`,
  parsed leniently; **unparseable / empty / tie output drops the judge result**
  (no row, no error) — fail-safe. Its pick maps back through the **same** shuffle
  mapping, so the recorded judge winner is the correct champion/challenger role.
- The judge writes its **own** `RankedFeedbackEvent` with `reviewer =
  skillopt-ab-judge` and `source = skillopt-ab-judge`, so it **coexists** with the
  human `source = skillopt-ab` row on the same run/item (the conflict key differs
  by reviewer+source) and is **weighted below human** by the source tag.
- The judge is **evidence-only**: it **never** increments the promotion
  Beta-Bernoulli bandit and **never** changes `P(challenger > champion)`. Promotion
  stays manual + human-driven. **No contract struct changes — `ContractVersion`
  stays `1`** (the only new differentiator is the source/reviewer tag value).
- **Trust is gated on measure-the-judge (#344):** the judge row is judge-tagged
  and weighted low here and is **not** calibrated against human gold in this slice
  — judge-tagged now, calibrated later.

The jury variant of the judge (`mode_b_jury_size >= 2`, #349) convenes up to N
judges from **distinct** model families on the same blind A/B and aggregates
their verdicts (majority vote + a disagreement flag governed by
`mode_b_jury_disagreement_tau`); `mode_b_jury_veto_dimensions` +
`mode_b_jury_veto_floor` add an optional fail-closed minority veto on
safety/correctness rubric dimensions. Like the single judge it is evidence-only
and never promotes.

A reviewer's blinded paired-review packet (the fork's `pairwise-review.json` +
secret map + picks) is imported with `gitmoot skillopt pairwise import
<packet-dir> [--packet] [--secret-map] [--picks] [--reviewer] [--json]`, which
de-blinds it and stores the pairwise-preference feedback events.

The canary path shipped as `[skillopt].auto_promote_canary` +
`auto_promote_canary_sample` (#484, documented above); the unattended auto A/B
scheduling loop remains **deferred** to follow-ons.

## Ranked Exploration Workflow

Use ranked exploration when the template is still ambiguous and humans need to
compare meaningfully different directions. Use A/B validation when the question
is whether a specific candidate should replace the current template.

1. `explore`: generate four to six diverse options for each review item. Ask
   reviewers to rank every option and name useful and rejected traits. Keep
   `exploration_level` set to `high` while the best direction is still unclear.
2. `refine`: use two to three candidates that combine the strongest traits
   discovered during exploration. Keep asking for rankings and trait notes, but
   focus the alternatives around the same product/workflow goal.
3. `distill`: convert accumulated ranked feedback into a candidate template
   update. This phase should not require broad new directions.
4. `validate`: compare the current template against the candidate on fresh
   review items. Use the A/B path by default for final promotion decisions.

`gitmoot skillopt review status --run <run-id>` reports the current mode,
feedback count, pairwise preference count, ranking stability, and recommended
next mode. Recommendations are advisory only. Gitmoot does not change the run
mode, import a candidate, or promote a template automatically.

Phase recommendations intentionally wait until every review item has imported
feedback. This prevents one heavily reviewed item from advancing a whole run
while other items are untouched. Blind Markdown and GitHub packets hide
outcome-bearing recommendation details after feedback exists so later reviewers
do not see the current winner before responding.

Do not run heavy SkillOpt optimization after every tiny feedback update unless
the user explicitly wants that. A practical cadence is:

- collect enough rankings to make the status recommendation stable;
- export the training package;
- run the external optimizer;
- import the candidate;
- review or validate the candidate with fresh items before promotion.

## Candidate Package

Import a candidate produced by an external optimizer:

```sh
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
```

The imported package must have:

- `kind: gitmoot-skillopt-candidate-package`
- `contract_version: 1`
- `template_id`: an installed Gitmoot agent template id
- `base_version_id`: optional pinned version used by the optimizer
- `candidate.content`: full agent-template Markdown with YAML frontmatter
- `candidate.metadata`: metadata that exactly matches the candidate
  frontmatter
- `eval_report`: optional optimizer report
- `summary`: optional diff, score, and preference summary
- `artifacts`: optional candidate artifact manifest entries with `id`, relative
  `path`, SHA256 `hash`, `media_type`, `driver`, and optional `size_bytes`

Importing never promotes a candidate. Gitmoot stores it as a pending template
version so later review and promotion commands can decide whether it becomes
current.
If `eval_report` or `summary.metadata` marks `promotable: false` with
`no_candidate_reason`, or if the candidate content hash matches the base
version, Gitmoot rejects the import as `optimizer produced no candidate` instead
of creating a fake pending version.
If `artifacts` is present, `--artifact-dir` is required. Gitmoot rejects
absolute paths, path traversal, missing files, hash mismatches, duplicate
artifact ids, and invalid `summary.diff_artifact_id` references before creating
the pending candidate version. Verified blobs are stored in Gitmoot's
content-addressed artifact store and registered in SQLite.

Review pending candidates:

```sh
gitmoot skillopt candidate list --template planner
gitmoot skillopt candidate show planner@v2
```

`candidate show` includes the candidate state, source, content hash, base
version, optimizer score, preference summary, eval report JSON, and a content
diff against the base/current template version. It does not expose hidden A/B
assignment mappings while blind reviews are active.

Promote or reject after human review:

```sh
gitmoot skillopt candidate promote planner@v2
gitmoot skillopt candidate reject planner@v3 --reason "Too broad for the current workflow"
```

Promotion updates the template's current version. Rejection records an audit
reason and prevents the rejected candidate from being selected by `@latest`.

## Human Feedback Trial Happy Path

Create an eval review run and add saved baseline/candidate outputs:

```sh
gitmoot skillopt review create \
  --template planner \
  --repo owner/repo \
  --run run-2026-05-31

gitmoot skillopt review item add \
  --run run-2026-05-31 \
  --item item-001 \
  --title "README planning task" \
  --baseline baseline.md \
  --candidate candidate.md \
  --metadata-json '{"path":"README.md"}'

gitmoot skillopt review status --run run-2026-05-31
```

Then export a blind local packet, collect human feedback, and import it:

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31

# Human opens index.md, reviews items/*.md, sets reviewer, and edits feedback.yml.

gitmoot skillopt feedback markdown import \
  --packet .gitmoot/evals/run-2026-05-31
```

When every review item has imported feedback, export the training package for
the external optimizer:

```sh
gitmoot skillopt review status --run run-2026-05-31
gitmoot skillopt export --run run-2026-05-31 --output training.json
```

Use `--dry-run` first to validate the exchange contract without model calls:

```sh
gitmoot-skillopt optimize \
  --training-package training.json \
  --artifact-root ~/.gitmoot/evals/blobs \
  --out-root .gitmoot/skillopt/run-2026-05-31 \
  --candidate-output candidate.json \
  --dry-run
```

For real model-backed optimization, verify the installed optimizer contract and
environment before running it:

```sh
gitmoot-skillopt --help
gitmoot-skillopt optimize --help
for name in OPENAI_API_KEY ANTHROPIC_API_KEY GITMOOT_SKILLOPT_BACKEND; do
  if [ -n "${!name:-}" ]; then
    printf '%s=set\n' "$name"
  else
    printf '%s=missing\n' "$name"
  fi
done
```

Use the backend, model, and budget flags shown by your installed
`gitmoot-skillopt optimize --help`. Do not assume flag names without checking
the local optimizer version.

Import and review the candidate. Importing never promotes automatically:

```sh
gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]
gitmoot skillopt candidate show <version-id>
gitmoot skillopt candidate promote <version-id>
gitmoot skillopt candidate reject <version-id> --reason "Needs narrower instructions"
```

## Markdown Feedback Packet

Generate a local blind A/B review packet:

```sh
gitmoot skillopt feedback markdown export \
  --run run-2026-05-31 \
  --output .gitmoot/evals/run-2026-05-31
```

The packet contains:

- `index.md`: review instructions and item links
- `items/*.md`: one file per item with Option A and Option B
- `feedback.yml`: the editable response file
- `.assignments.json`: hidden A/B assignment metadata used by Gitmoot on import

Humans fill `feedback.yml` with choices:

```yaml
run_id: run-2026-05-31
reviewer: alice
items:
  - item_id: item-001
    choice: b
    reasoning: More concrete and easier to execute.
  - item_id: item-002
    choice: tie
```

Allowed choices are exactly `a`, `b`, `tie`, `neither`, and `skip`. Reasoning is
optional.

Import the completed feedback:

```sh
gitmoot skillopt feedback markdown import \
  --packet .gitmoot/evals/run-2026-05-31
```

Gitmoot validates the full response before writing any events. On import, it
uses `.assignments.json` to de-blind `a` and `b` so stored canonical feedback
events use `choice: a` for the baseline artifact and `choice: b` for the
candidate artifact. Each event includes `run_id`, `item_id`, `choice`, optional
`reasoning`, `reviewer`, `source`, optional `source_url`, and `created_at`.

Generate a local ranked exploration packet by creating a ranked run and adding
repeated option artifacts:

```sh
gitmoot skillopt review create \
  --template landing-page-designer \
  --repo owner/gitmoot-web \
  --run landing-page-explore-001 \
  --mode explore \
  --exploration-level high \
  --options 4

gitmoot skillopt review item add \
  --run landing-page-explore-001 \
  --item hero-001 \
  --title "Gitmoot landing page hero" \
  --option a=previews/hero-a.md \
  --option b=previews/hero-b.md \
  --option c=previews/hero-c.md \
  --option d=previews/hero-d.md \
  --metadata-json '{"task":"landing-page","preview_url":"https://owner.github.io/gitmoot-previews/hero-001/"}'

gitmoot skillopt feedback markdown export \
  --run landing-page-explore-001 \
  --output .gitmoot/evals/landing-page-explore-001
```

Humans fill ranked feedback with ordered options and trait notes:

```yaml
run_id: landing-page-explore-001
reviewer: alice
items:
  - item_id: hero-001
    ranking:
      - C > A > D > B
    useful_traits:
      C:
        - explains what Gitmoot does before the fold
      A:
        - strongest mascot placement
    rejected_traits:
      B:
        - too generic for a developer tool
    required_improvements:
      - better mobile layout
      - stronger product visuals
    reasoning: C is clearest overall, but A has the better visual identity.
```

A non-visual text task uses the same structure:

```sh
gitmoot skillopt review create \
  --template x-post-writer \
  --repo owner/content-workflows \
  --run x-post-style-explore-001 \
  --mode explore \
  --options 5

gitmoot skillopt review item add \
  --run x-post-style-explore-001 \
  --item thread-hook-001 \
  --title "Launch-thread opening post" \
  --option a=posts/hook-a.txt \
  --option b=posts/hook-b.txt \
  --option c=posts/hook-c.txt \
  --option d=posts/hook-d.txt \
  --option e=posts/hook-e.txt
```

Rank every option from best to worst and use trait notes for style signals such
as pacing, specificity, voice, sentence length, and phrases to avoid.

## GitHub Feedback Collector

Publish a collaborative blind A/B review packet to a new GitHub issue:

```sh
gitmoot skillopt feedback github publish \
  --run run-2026-05-31 \
  --repo owner/reviews
```

To publish the packet as a comment on an existing PR instead:

```sh
gitmoot skillopt feedback github publish \
  --run run-2026-05-31 \
  --repo owner/repo \
  --pr 123
```

If `--repo` is omitted, Gitmoot resolves the target in this order: eval run
target repo, template source repo, configured `[feedback].repo`, then an error
asking for `--repo`.

Reviewers can reply with the issue body's YAML block or with short-form lines:

```text
run_id: run-2026-05-31
item-001: b - More concrete and easier to execute.
item-002: tie
```

Sync comments back into canonical feedback events:

```sh
gitmoot skillopt feedback github sync \
  --run run-2026-05-31 \
  --repo owner/reviews \
  --issue 42
```

For PR comment mode, use `--pr 123` instead of `--issue 42`. Sync ignores
unrelated comments and de-duplicates repeated imports by comment URL.

Ranked GitHub review uses the same run and item setup as the Markdown ranked
workflow, then publishes to a review issue or PR:

```sh
gitmoot skillopt feedback github publish \
  --run landing-page-explore-001 \
  --repo owner/gitmoot-previews

gitmoot skillopt feedback github sync \
  --run landing-page-explore-001 \
  --repo owner/gitmoot-previews \
  --issue 42
```

Reviewers can reply with the YAML ranked block or a short ranking form:

```text
run_id: landing-page-explore-001
hero-001 ranking: C > A > D > B
best traits:
- C: clearest product explanation
- A: best mascot placement
reject:
- B: too generic
```

Complete local review path:

1. `gitmoot skillopt review create --template <id> --repo owner/repo --run <run-id>`
2. `gitmoot skillopt review item add --run <run-id> --item <item-id> --baseline baseline.md --candidate candidate.md`
3. `gitmoot skillopt feedback markdown export --run <run-id> --output .gitmoot/evals/<run-id>`
4. Human opens `index.md`, reviews `items/*.md`, sets `reviewer`, and fills `feedback.yml`.
5. `gitmoot skillopt feedback markdown import --packet .gitmoot/evals/<run-id>`
6. `gitmoot skillopt export --run <run-id> --output training.json`
7. `gitmoot-skillopt optimize --training-package training.json --artifact-root ~/.gitmoot/evals/blobs --out-root .gitmoot/skillopt/<run-id> --candidate-output candidate.json --dry-run`
8. `gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]`
9. `gitmoot skillopt candidate show <version-id>`
10. `gitmoot skillopt candidate promote <version-id>` or `gitmoot skillopt candidate reject <version-id>`

Complete train-mode path:

1. `gitmoot skillopt train start --template <id> --repo owner/repo --workspace-repo owner/workspace --request <text> --items-file items.yml --yes` (`--workspace-repo` is required; without it the session stays at `request_confirmed` and cannot reach option generation)
2. `gitmoot skillopt train status --session <session-id> --json --verbose` or `--watch` to inspect `status_phase`, item progress, active locks with owner and heartbeat, review issue, candidate, recovery availability, no-candidate reason, and next action. `status_phase` is the stable automation field; it can pass through normal train states such as `items_ready` and can also report generation/optimizer/blocker phases such as `generating_options`, `generating_options_heartbeat_stale`, `preflight_running`, `optimizer_running`, `optimizer_heartbeat_stale`, `optimizer_completed_candidate`, `optimizer_completed_no_candidate`, `recovery_available`, `blocked_config`, `blocked_stale_lock`, or `failed_unrecoverable`. While option generation runs, `status_phase` reports `generating_options` (the dashboard shows the same live phase), so polling does not look frozen at `items_ready`.
3. `gitmoot skillopt train continue --session <session-id>` to generate options and publish the review packet.
4. Human feedback is imported from raw or fenced YAML comments; `train continue` auto-syncs GitHub comments when the review is published and feedback is missing.
5. Evaluator profiles run cheap artifact checks first, optional render adapters second, and LLM judges last. Structured failures flow into optimizer input with reasons, hints, evidence, failed checks, and stage status.
6. `gitmoot skillopt train continue --session <session-id> --backend codex` to export the package, print the resolved backend/preflight report, run `gitmoot-skillopt`, and import the pending candidate, or record `optimizer_completed_no_candidate` if the optimizer produced no promotable content.
7. If the optimizer wrapper fails after writing completed artifacts and status reports `recovery_available`, run `gitmoot skillopt train recover --session <session-id> --out-root <optimizer-output-root>` to import a completed candidate or record the completed no-candidate result through the same gate.
8. `gitmoot skillopt train continue --session <session-id>` to publish candidate review context with separate selection score, evaluator/test scores, gate status, no-op status, and promotability.
9. `gitmoot skillopt train continue --session <session-id> --promote <version>` or `--reject <version> --reason <text>`.
10. `gitmoot skillopt train continue --session <session-id> --start-next` only after the prior iteration is resolved.

Complete GitHub review path:

1. `gitmoot skillopt import --file candidate.json [--artifact-dir artifacts]`
2. `gitmoot skillopt feedback github publish --run <run-id> --repo owner/reviews`
3. Humans reply in GitHub comments using the run-scoped YAML or short-form block.
4. `gitmoot skillopt feedback github sync --run <run-id> --repo owner/reviews --issue <number>`
5. `gitmoot skillopt candidate show <version-id>`
6. `gitmoot skillopt candidate promote <version-id>` or `gitmoot skillopt candidate reject <version-id>`

## Future Live Pairwise Evaluation

The MVP exchange contract compares candidates against saved baseline outputs.
This keeps local review deterministic and avoids rerunning every baseline for
each candidate import.

Future live pairwise mode is tracked in
[GitHub issue #77](https://github.com/jerryfane/gitmoot/issues/77). That mode
would run the current promoted template and the pending candidate live for every
validation item before collecting blind A/B feedback. The tradeoff is more
faithful comparisons and better protection against stale baseline outputs, at
the cost of higher latency, token spend, and runtime/session complexity.
