#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PARTITION="$ROOT_DIR/scripts/partition-race-tests.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/gitmoot-race-partition-test.XXXXXX")"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

fail() {
  echo "partition-race-tests test: $*" >&2
  exit 1
}

assert_coverage() {
  local out_dir="$1"
  local actual="$WORK/actual"
  local unique="$WORK/unique"
  cat "$out_dir"/shard-*.tests | sort >"$actual"
  uniq "$actual" >"$unique"
  diff -u "$WORK/expected.sorted" "$actual"
  diff -u "$actual" "$unique"
}

cat >"$WORK/tests.list" <<'EOF'
TestAlpha
TestBeta
TestGamma
TestDelta
TestEpsilon
ok  	example/package	0.001s
EOF
grep '^Test' "$WORK/tests.list" | sort >"$WORK/expected.sorted"

# Deliberately shuffled; equal durations prove test-name tie-breaking.
cat >"$WORK/timings.tsv" <<'EOF'
example/package	TestGamma	4
example/package	TestEpsilon	1
example/package	TestBeta	10
example/package	TestDelta	4
example/package	TestAlpha	10
EOF

"$PARTITION" \
  --tests "$WORK/tests.list" \
  --timings "$WORK/timings.tsv" \
  --package example/package \
  --shards 2 \
  --out-dir "$WORK/lpt-one"
assert_coverage "$WORK/lpt-one"
[[ "$(cat "$WORK/lpt-one/mode")" == "lpt" ]] || fail "timed partition did not select LPT"
diff -u <(printf 'TestAlpha\nTestDelta\nTestEpsilon\n') "$WORK/lpt-one/shard-0.tests"
diff -u <(printf 'TestBeta\nTestGamma\n') "$WORK/lpt-one/shard-1.tests"

"$PARTITION" \
  --tests "$WORK/tests.list" \
  --timings "$WORK/timings.tsv" \
  --package example/package \
  --shards 2 \
  --out-dir "$WORK/lpt-two" >/dev/null
diff -ru "$WORK/lpt-one" "$WORK/lpt-two"

"$PARTITION" \
  --tests "$WORK/tests.list" \
  --shards 2 \
  --out-dir "$WORK/fallback" >/dev/null
assert_coverage "$WORK/fallback"
[[ "$(cat "$WORK/fallback/mode")" == "alternation" ]] || fail "missing timings did not select alternation"
diff -u <(printf 'TestBeta\nTestDelta\n') "$WORK/fallback/shard-0.tests"
diff -u <(printf 'TestAlpha\nTestGamma\nTestEpsilon\n') "$WORK/fallback/shard-1.tests"

# A PARTIALLY stale artifact is the normal case, not an error: nearly every PR
# adds, renames, or deletes a test. Demanding an exact match meant LPT balancing
# effectively never ran (#930). The timings must be reconciled against the current
# list instead — new tests weighted at the median, vanished rows dropped — and the
# partition must still balance and still cover every test exactly once.
cat >"$WORK/stale.tsv" <<'EOF'
example/package	TestAlpha	10
example/package	TestBeta	9
example/package	TestGamma	8
example/package	TestDelta	7
example/package	TestRemoved	6
EOF
"$PARTITION" \
  --tests "$WORK/tests.list" \
  --timings "$WORK/stale.tsv" \
  --package example/package \
  --shards 2 \
  --out-dir "$WORK/stale" >"$WORK/stale.stdout" 2>"$WORK/stale.stderr"
assert_coverage "$WORK/stale"
[[ "$(cat "$WORK/stale/mode")" == "lpt" ]] || fail "a partially stale artifact fell back to $(cat "$WORK/stale/mode"); LPT must still run"
grep -q 'reconciled timings' "$WORK/stale.stderr" || fail "reconciliation was not reported"
grep -q '1 new test(s)' "$WORK/stale.stderr" || fail "the new test was not counted"
grep -q '1 stale row(s) dropped' "$WORK/stale.stderr" || fail "the vanished test's row was not dropped"
# TestEpsilon is new here: it must be scheduled at the median of the known times
# (8), not as a free test — a zero-weight test would pile onto the loaded shard.
grep -qx 'TestEpsilon' "$WORK/stale/shard-0.tests" "$WORK/stale/shard-1.tests" ||
  fail "the new test was not assigned to any shard"

# A duplicate row makes a test's weight ambiguous: that is a CORRUPT artifact, not
# a stale one, and must stay fatal rather than silently pick a weight.
cat >"$WORK/dupe.tsv" <<'EOF'
example/package	TestAlpha	10
example/package	TestAlpha	3
example/package	TestBeta	9
EOF
set +e
"$PARTITION" \
  --tests "$WORK/tests.list" \
  --timings "$WORK/dupe.tsv" \
  --package example/package \
  --shards 2 \
  --out-dir "$WORK/dupe" >"$WORK/dupe.stdout" 2>"$WORK/dupe.stderr"
status=$?
set -e
[[ "$status" -eq 2 ]] || fail "duplicate timing rows returned $status, want 2"
grep -q 'duplicate rows' "$WORK/dupe.stderr" || fail "duplicate rows were not reported"

echo "partition-race-tests test: PASS"
