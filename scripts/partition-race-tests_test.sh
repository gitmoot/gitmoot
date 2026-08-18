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

echo "partition-race-tests test: PASS"
