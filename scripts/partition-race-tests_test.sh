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

"$PARTITION" \
  --tests "$WORK/tests.list" \
  --shards 2 \
  --out-dir "$WORK/alternation" >/dev/null
assert_coverage "$WORK/alternation"
[[ "$(cat "$WORK/alternation/mode")" == "alternation" ]] || fail "partition did not select alternation"
diff -u <(printf 'TestBeta\nTestDelta\n') "$WORK/alternation/shard-0.tests"
diff -u <(printf 'TestAlpha\nTestGamma\nTestEpsilon\n') "$WORK/alternation/shard-1.tests"

# A benchmark in the list must be EXCLUDED from the run set, not refused and not
# sharded. `-test.run` cannot select a benchmark at all, so refusing here (the
# behavior before #1824) forbade the repo from holding any benchmark in a
# race-lane package, while sharding one would emit a pattern that matches
# nothing. Coverage of the real tests must be unaffected.
cat >"$WORK/bench.list" <<'EOF'
TestAlpha
TestBeta
BenchmarkSomething
ok  	example/package	0.001s
EOF
printf 'TestAlpha\nTestBeta\n' >"$WORK/expected.sorted"
"$PARTITION" \
  --tests "$WORK/bench.list" \
  --shards 2 \
  --out-dir "$WORK/bench" >/dev/null 2>"$WORK/bench.stderr" \
  || fail "partition refused a list containing a benchmark"
assert_coverage "$WORK/bench"
if grep -q 'BenchmarkSomething' "$WORK"/bench/shard-*.tests "$WORK"/bench/shard-*.regex; then
  fail "benchmark leaked into the run set"
fi
grep -q 'BenchmarkSomething' "$WORK/bench.stderr" \
  || fail "benchmark exclusion was silent - it must be reported"

echo "partition-race-tests test: PASS"
