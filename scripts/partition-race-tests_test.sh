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

cat >"$WORK/stale.tsv" <<'EOF'
example/package	TestAlpha	10
example/package	TestBeta	9
example/package	TestGamma	8
example/package	TestDelta	7
example/package	TestRemoved	6
EOF
set +e
"$PARTITION" \
  --tests "$WORK/tests.list" \
  --timings "$WORK/stale.tsv" \
  --package example/package \
  --shards 2 \
  --out-dir "$WORK/stale" >"$WORK/stale.stdout" 2>"$WORK/stale.stderr"
status=$?
set -e
[[ "$status" -eq 2 ]] || fail "stale timings returned $status, want 2"
grep -q 'current tests missing from timings' "$WORK/stale.stderr" || fail "stale timings did not report missing tests"
grep -q 'timing rows absent from current tests' "$WORK/stale.stderr" || fail "stale timings did not report removed tests"

echo "partition-race-tests test: PASS"
