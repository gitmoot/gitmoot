# Gitmoot Result Contract

Every agent job must return a `gitmoot_result` JSON object. Keep it concise,
truthful, and tied to work that actually happened.

```json
{
  "gitmoot_result": {
    "decision": "approved|changes_requested|blocked|implemented|failed|skipped",
    "summary": "Brief outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": [],
    "needs": []
  }
}
```

## Review evidence: executed or static-only

Review results carry a top-level `"evidence"` field with one of two values:

- `"executed"` — you ran the commands you cite and read their real output.
- `"static_only"` — you read code, diffs or prior verdicts and could not run
  anything (no usable toolchain, no network, a sandbox refusal).

Declare it on every review. Say `"executed"` only for commands you actually
ran. **Never infer test success, or failure, from an inability to execute** —
"the tests presumably still pass" is not evidence, and neither is a red result
that came from a broken instrument rather than the code.

A static-only review is a legitimate verdict: an honest one that names what it
inspected and says plainly that it could not execute is more useful than a
verdict that pretends. A MISLABELLED one is not, because a merge decision
cannot tell the difference afterwards.

Omitting the field records `static_only`. Silence never becomes an execution
claim, so a producer that does not know about this field cannot be read as
having run anything. An unrecognised value is rejected outright.

A no-execution review (#2364) MUST return `"static_only"` with an empty
`tests_run`. Its seat cannot run anything; a result that
claims otherwise is refused and the job fails, whatever `result_checks` mode is
set.

**The engine now records WHETHER YOU DECLARED IT**, alongside the value, in an
engine-owned `evidence_declared` flag. Do not send that flag: it is assigned
during normalization and a supplied value is overwritten, because provenance
about a value must not come from the value's author.

It exists because the safe default is lossy. Measured over 3,431 succeeded
review jobs on one host, 3,192 of them (93%, 14.3 G tokens) omitted `evidence`
entirely, 198 declared `executed` and 41 declared `static_only`. So an honest
"I could run nothing" and "I have never heard of this field" stored the same
value. **The practical consequence for anyone reading these rows: a policy that
refused `static_only` would today refuse 93% of all reviews**, almost none of
which made the claim. Declaring the field is what makes your verdict
distinguishable from silence, so declare it.

**No check fails a static-only review.** The distinction a merge decision needs
is carried by the stored field, not by refusing the verdict. An earlier version
of this feature failed every static-only review, which under
`result_checks = block` does not warn but KILLS the job - so an honest reviewer
that could not execute would have been discarded instead of read.

Two result checks consume this field:

- `review-verdict-has-evidence` - does the verdict account for itself at all?
- `review-executed-claim-is-substantiated` - if it claims `executed`, does it
  name something it actually ran? Declaring `static_only` always passes; a bare
  "everything looks fine" beside `executed` does not.

## Why checks could not run: `checks_blocked`

A review that could not run some or all of the project's checks may add a
top-level `"checks_blocked"` field naming the main reason:

- `"toolchain"` - the language toolchain or test runner is missing or not
  runnable (no `pytest`, `cargo`, `node`).
- `"dependencies"` - the toolchain ran, but the project's dependencies were
  missing (an empty module cache, packages not installed).
- `"sandbox"` - the sandbox refused a path or device the check needed.
- `"network"` - the check needed network access.
- `"service"` - the check needed a database, a browser or a credentialed API.
- `"other"`.

It is optional and carries no policy: `evidence` still says whether anything
ran, and a review can declare `executed` for the checks it ran alongside a
`checks_blocked` reason for the ones it could not. Say what you could not run in
`summary` or `tests_run` as well.

A router review (#2520) that cannot run a check because its environment lacks
the platform toolchain (for example Xcode on a Linux image) may treat that check
as satisfied by a green CI run at the exact reviewed head that it verifies
itself: the run's head SHA equals the reviewed head, the job succeeded, and the
job builds or tests the changed paths. It records the check as its own
`tests_run` entry of the form `CI-verified: <check> -> success; run <run id> job
<job id>` and nothing else in that entry, puts why the check cannot run here in
`checks_blocked` and `summary`, lists the checks it ran itself as separate
entries, and does not block or request changes for that reason alone. For the
remote evidence ceiling a `CI-verified:` entry never counts as a check the
review ran, so a review whose only `tests_run` entries are CI-verified is
`static_only` whatever `changes_made` says; and like every entry it clamps the
result if it reports a missing toolchain. It still runs every check its
environment can run, and a changed path no green exact-head CI job exercised is
not CI-verified. An unrecognised value is rejected outright,
like `evidence`, so a misspelling is an error rather than an uncountable row.
The field exists so the reasons reviews end static-only can be counted from
stored results instead of searched for in summaries (#2314). The rollout
ordering below applies to it too.

## Rollout ordering for the evidence field

Upgrade PARSERS before EMITTERS. A daemon older than this change rejects
unknown result fields outright, so a result carrying `evidence` produced against
a newer prompt fails on an older daemon. Absence is always accepted and recorded
as `static_only`, so the reverse direction - a newer daemon reading an older
producer's result - is safe.

## Review severity

Review results may add a top-level `"severity": "P0"` field. A
`changes_requested` review must set it to the highest-severity finding, using
one of `P0`, `P1`, `P2`, or `P3` (`P0` is most severe). Approved reviews may
include it when reporting non-blocking findings; approved reviews with no
findings and non-review jobs omit it.

This field is the engine-readable round severity. A `severity` key nested inside
an individual `findings[]` object remains useful for display, but does not
replace the top-level field.

Repository policy may set an inclusive blocking threshold. With
`blocking_severity = "P1"`, `P0` and `P1` restart the fix loop; `P2` and `P3`
resolve as approved-with-notes. Gitmoot still stores and posts the original
`changes_requested` result and its findings. The default threshold is `P3`, so
all valid severities block unless configured otherwise.

Session review jobs use the same rule. Pass `--severity P0|P1|P2|P3` to
`job close` or `job record` when the decision is `changes_requested`.

## Display-only in-session review state

An inline PR review can be recorded with `job open --type review --pr <n>
--head-sha <sha> --workflow <label>`, progress notes, and `job close`. The
resulting `review_status` is always graded `reported` and labeled
`non_authoritative`: note authorship is a caller assertion, not system-observed
reviewer identity. It is an operator visibility hint only and never enters the
merge gate or changes `tasks.state`.

## Findings

`findings` is a free-form array — each entry may be a plain string or a JSON
object; there is no rigid schema. When you emit **object** findings, the posted
PR/issue comment renders them as readable markdown (a bold heading plus an
indented `key: value` sub-list) instead of an inline JSON blob, so a few
conventional keys make the result easier for humans to audit:

- **Heading**: the renderer uses the first present of `title`, `approach`,
  `name`, `summary`, or `finding` as the bold heading.
- **Qualifier**: the first present of `recommendation`, `severity`, or `status`
  is shown in parentheses next to the heading (e.g. `(PRIMARY)`, `(high)`).
- **Links**: a `source_url`, `url`, or `source` value is rendered as a clickable
  markdown link.
- **Nested values**: arrays become a nested bullet sub-list; nested objects
  render as `key: value` lines (depth-bounded).

None of these keys is mandatory. String findings still render exactly as the
plain text you provide, and any finding the renderer cannot map (for example a
top-level array) falls back to a pretty-printed fenced ```json``` block.

## String lists: `changes_made`, `tests_run`, `needs`

Emit **strings**. That is the contract, and it is what the renderers and the
result digest are built for.

An **object** element is also accepted, because agents send them and refusing
one used to fail the entire result. A review that reported

```json
"tests_run": [{"name": "diff scope", "command": "git show --stat", "result": "pass"}]
```

was rejected with `cannot unmarshal object into Go struct field
AgentResult.tests_run of type string` and spent a repair attempt, which made a
job that did the work and reported it richly indistinguishable from one that
returned nothing. An object element is now stored as compact JSON with its keys
in canonical order at the TOP level, and every field is carried through verbatim — including
shell metacharacters, so a recorded `command` can be copy-pasted back. The
contract names none of the keys, so picking one would be a guess.

**Duplicate keys are rejected** at the top level, not merged. `[{"name":"a","name":"b"}]` fails
with the duplicate named, because the alternative is keeping one silently and
losing the other, which would contradict the guarantee above.

A `null` element is accepted and becomes an **empty entry**, which is what it
did before object elements were supported. It is not an error: failing the whole
result over one null would spend a repair attempt on an envelope that already
parsed.

**Both properties stop at the top level, and the digest consumes nested values.**
A NESTED object keeps whatever key order the agent sent, and a duplicate key inside a nested object is
NEITHER rejected NOR resolved: BOTH copies are kept verbatim in the stored
entry, because the value is copied as raw bytes and re-marshalling only
compacts it. Measured, not inferred.
Only the outermost object of a list element is canonicalised and duplicate-checked.
So two agents reporting the same nested entry can produce different bytes, and
the result digest reads those bytes.

**The evidence gate also changed, not only the decoder.** A list entry that
decodes cleanly but carries no information — `{}`, or an object whose values
are all empty such as `{"name":"","command":""}` — is NOT evidence. A bare
array element like `[]` or `["",""]` is NOT in this category: the decoder
rejects any element that is not a string or an object, so those fail the whole
result rather than reaching the gate. Empty containers ARE reachable nested
inside an object value, and are judged there.
same test the `needs` gate uses, so a content-free entry no longer satisfies
`implement-changes-listed` or `implement-tests-listed`. An entry with at least
one populated value, at any depth, still counts.

**Which lists accept an object at all.** Only the narrative lists an agent
writes for a human: `changes_made`, `tests_run` and `needs`. The engine's
structural lists stay STRICT and reject an object element — `capabilities`,
`artifacts` and `choices` are identifiers the engine consumes (a capability
name, an artifact path, a prompt option), so an
object there is not a richer report, it is an unusable value that would corrupt
routing. That asymmetry is deliberate.

Nothing else is accepted. A number, a boolean, a nested array, or a bare string
where the array belongs still fails the result — and the error names the field,
so a repair attempt knows which list to fix. Prefer a string and put the detail
in it.

## Removed fields fail; they are not ignored

The accepted-field roster is derived from the contract struct, so a
`gitmoot_result` carrying a field that no longer exists fails validation with
`unsupported gitmoot_result field "<name>"`. The agent then gets a repair turn
naming the field and carrying its complete previous response (up to 96 KiB, so
the whole prompt fits in one command-line argument; a longer response is visibly
cut and the agent is told to return one complete result that keeps every item,
shortening wording if needed), and must return the same result without it. If no repair attempt
validates, the job fails. This is deliberate: the roster is what catches a typo
or an invented field before it becomes a silent no-op.

`learnings` was removed in #2202 with the persistent-memory feature. If you are
an agent reading a cached prompt, do not send it - the prompt no longer asks for
it, and sending it fails the job rather than being dropped. `next_agents` was
removed earlier on the same terms.

`delegations` and `human_questions` were removed in #2588 with Orchestra (the
delegation fan-out, coordinator continuations and the ask-gate). A result that
carries either is refused by name — the error says to do the work in this job
or report it in `summary`/`needs` — rather than being silently dropped.

## Decisions

- `approved`: review found no blocking issues.
- `changes_requested`: review found issues that should be fixed before merge.
- `blocked`: work cannot continue without human input or an external state change.
  A blocked job is *settled* (`job watch` stops tailing it) but not *final*: it
  can be resumed via retry, so
  subsystems that stamp an end time or tear down live resources treat it as still
  live. See `internal/workflow` `IsSettledJobState` vs `IsFinalJobState` (#632).
- `implemented`: the requested implementation work was completed.
- `failed`: the attempted action errored or could not complete.
- `skipped`: the task itself had no work to do. Do not use `skipped` in a PR
  review to mean "nothing to flag"; reviewers use `approved`. It maps to a
  succeeded job state.

## Reporting Rules

- Do not claim tests were run unless they were actually run.
- Do not claim files were changed unless they were actually changed. Name
  repo-relative file paths in each `changes_made` entry.
- Use `needs` for missing credentials, unclear scope, unavailable tools, failing
  external services, or required human decisions.
- Redact secrets from summaries, findings, raw command output, and examples.
