# Gitmoot Review Result

A review ends when the reviewer prints one `gitmoot_result` JSON object.
Gitmoot finds it in the reviewer's output, validates it, and stores it as the
verdict (`payload.result` in `gitmoot run show <run-id>`). Keep it concise,
truthful, and tied to work that actually happened.

`decision` and `summary` are required. The parser accepts any `"decision"` of
approved|changes_requested|blocked|implemented|failed|skipped; a review returns
one of the four listed under Decision below.

```json
{
  "gitmoot_result": {
    "decision": "approved",
    "severity": "P2",
    "summary": "One sentence on the outcome.",
    "findings": [],
    "changes_made": [],
    "tests_run": ["go test ./internal/... -> ok"],
    "needs": [],
    "evidence": "executed",
    "checks_blocked": "network"
  }
}
```

## Decision

- `approved`: no blocking issues at this head.
- `changes_requested`: issues to fix before merge. Requires `severity`.
- `blocked`: the review cannot finish without a human or an outside change.
  The run ends `blocked`; its `needs` say what would unblock it.
- `failed`: the review errored or could not complete. The run ends `failed`.

`implemented` is not a review decision, and `skipped` means the run had no work
to do; never use `skipped` for "nothing to flag" (that is `approved`). Any
decision other than `blocked` or `failed` ends the run `succeeded`.

## Severity

`severity` is the highest-severity finding: `P0` (most severe) to `P3`. It is
required with `changes_requested`, allowed on an approval that reports
non-blocking findings, and omitted on an approval with no findings. A
`severity` key inside a finding is for display; it does not replace the
top-level field.

A repository's `[review] blocking_severity` (default `P3`) is the least severe
`changes_requested` verdict that counts as blocking. With
`blocking_severity = "P1"`, a `P2` or `P3` `changes_requested` resolves as
approved-with-notes; the original verdict and findings are still stored.
Separately, the merge gate holds on any open P0, P1 or P2 finding at the head.
P3 findings never hold a merge; after merge they go to the design-notes issue.

When a seat records its own review with `gitmoot run record --type review`, a
`changes_requested` decision needs `--severity P0|P1|P2|P3` too.

## Evidence

`evidence` is `executed` or `static_only`. Declare it on every review.

- `executed`: you ran the commands you cite and read their real output.
- `static_only`: you read code, diffs or prior verdicts and could not run
  anything (no toolchain, no network, a sandbox refusal).

Never infer that tests pass, or fail, from being unable to run them. A
static-only review that says plainly what it read is a legitimate verdict; a
mislabelled one is not, because a merge decision cannot tell the difference
afterwards. Omitting the field records `static_only`; an unknown value is
rejected. A review requested with `--no-execution` must return `static_only`
with an empty `tests_run`, or the run fails.

Two result checks read this field: `review-verdict-has-evidence` (does the
verdict account for itself at all?) and `review-executed-claim-is-substantiated`
(does an `executed` claim name something that ran?). A verdict passes the
first with any finding, a `tests_run` or `changes_made` entry that names a
command, path or target, or a `summary` of at least 40 characters saying what
was examined; `"summary": "ok"` with nothing else fails it. A failed check is
recorded in `payload.result_checks` and as a `result_checks_failed` run event.

## checks_blocked

When some or all of the project's checks could not run, `checks_blocked` names
the main reason: `toolchain`, `dependencies`, `sandbox`, `network`, `service`
(a database, browser or credentialed API), or `other`. It is optional and sets
no policy; say what could not run in `summary` or `tests_run` as well. An
unknown value is rejected.

A check the environment cannot run (its platform toolchain is missing, for
example Xcode on Linux) may be satisfied by a green CI run at the exact
reviewed head that the reviewer verifies itself: the CI run's head SHA equals
the reviewed head, its GitHub Actions job succeeded, and it builds or tests the
changed paths.
Record it as its own `tests_run` entry,
`CI-verified: <check> -> success; run <run id> job <job id>`, and name the
reason in `checks_blocked`. A `CI-verified:` entry is not a check the review
ran: a review whose only `tests_run` entries are CI-verified is `static_only`.

## Findings

`findings` is a free-form array: each entry is a string or a JSON object. An
object renders as readable markdown in the posted comment:

- heading: the first of `title`, `approach`, `name`, `summary`, `finding`;
- qualifier in parentheses: the first of `recommendation`, `severity`, `status`;
- a `source_url`, `url` or `source` becomes a link;
- nested arrays become sub-lists, nested objects `key: value` lines.

No key is mandatory for the comment. Cite the file and line, the failure, and
the fix.

### The findings ledger

Each object finding of a completed review is also recorded in the findings
ledger (`gitmoot findings`) at the reviewed head, under a UID such as
`owner/repo#42-f1`. To be recorded it needs a `severity` (`P0` to `P3`) and a
concern: a `title` or `detail` (or a `rationale` together with a `file`); a
finding without them is refused and reported against the run. A reported
finding is `open`.

A later review closes or continues a finding with these finding keys:

- `continues_uid`: the UID verbatim, as `gitmoot findings` prints it. Without
  it the finding is new, and the old one stays open; retyping its title is not
  a reference.
- `state`: `answered` (checked at this head), `withdrawn` (requires
  `withdraw_reason`), or `superseded`; omitted means still `open`.

An `answered` finding counts only with evidence: `"evidence": "executed"` and
the checks listed in `tests_run`, or, for a static-only review, a `file` plus
`rationale`. Otherwise the gate keeps it open. Example, at the fix head:

```json
{"gitmoot_result":{"decision":"approved","summary":"check.sh now reads b.txt; ran it at this head and it passed.","findings":[{"continues_uid":"acme/widget#1-f1","state":"answered","severity":"P2","file":"b.txt","title":"b.txt untested","detail":"check.sh now checks b.txt"}],"changes_made":[],"tests_run":["sh check.sh -> exit 0"],"needs":[],"evidence":"executed"}}
```

After it, `gitmoot findings --repo acme/widget --pr 1` shows the finding
`answered`, and `--at-head <fix-sha>` prints `no obligations`.

## changes_made, tests_run, needs

Emit strings. An object element is accepted and stored as compact JSON, a
`null` becomes an empty entry, and anything else (a number, a nested array)
fails the result naming the field. An entry with no content (`{}` or all-empty
values) does not count as evidence. A review usually leaves `changes_made`
empty.

## Unknown fields fail

The top-level `gitmoot_result` fields a review uses are the ones in the
example at the top; gitmoot checks top-level fields against its result
schema. Keys inside a finding object, such as `continues_uid` and `state`, are
free-form and not checked here. A top-level field gitmoot does not know,
including any an older prompt asked for, fails validation with
`unsupported gitmoot_result field "<name>"`. The reviewer gets a repair turn
to return the same result without it; if no repair validates, the run fails.

## Attribution

Gitmoot records who asked for a review in `dispatched_by`, resolved from the
dispatcher the request names, then the acting org role (`--role` or
`GITMOOT_ORG_ROLE`), then the dispatch channel (`local`, `github`). Neither the
run's own reviewer nor `lead_agent` is an attribution fallback: the reviewer
only reviewed, and a CLI review dispatch records no `lead_agent` at all. Who
wrote the change comes only from the implement run a seat records
with `gitmoot run record`.

## Reporting rules

- Do not claim tests ran unless they ran.
- Use `needs` for missing credentials, unclear scope, unavailable tools, failing
  outside services, or decisions only a human can make.
- Redact secrets from the summary, findings and quoted output.
