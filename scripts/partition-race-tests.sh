#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C

usage() {
  cat <<'EOF'
Usage: partition-race-tests.sh --tests FILE --shards N --out-dir DIR

Partitions the current top-level Go test list into N shards using the
deterministic alternating split (line number modulo shard count).

Any coverage assertion failure uses status 1 and must remain a hard failure.
EOF
}

fail() {
  echo "partition-race-tests: $*" >&2
  exit 1
}

tests_file=""
shards=""
out_dir=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tests)
      [[ $# -ge 2 ]] || fail "--tests requires a value"
      tests_file="$2"
      shift 2
      ;;
    --shards)
      [[ $# -ge 2 ]] || fail "--shards requires a value"
      shards="$2"
      shift 2
      ;;
    --out-dir)
      [[ $# -ge 2 ]] || fail "--out-dir requires a value"
      out_dir="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "$tests_file" ]] || fail "--tests is required"
[[ -f "$tests_file" ]] || fail "test list does not exist: $tests_file"
[[ "$shards" =~ ^[1-9][0-9]*$ ]] || fail "--shards must be a positive integer"
[[ -n "$out_dir" ]] || fail "--out-dir is required"

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/gitmoot-race-partition.XXXXXX")"
cleanup() { rm -rf "$work_dir"; }
trap cleanup EXIT

current_tests="$work_dir/current-tests"
unexpected="$work_dir/unexpected"
benchmarks="$work_dir/benchmarks"
: >"$current_tests"
: >"$unexpected"
: >"$benchmarks"

# A compiled test binary's -test.list output is just names. go test -list also
# appends an ok/? package status line, which is accepted for local dry-runs.
awk -v tests="$current_tests" -v unexpected="$unexpected" -v benchmarks="$benchmarks" '
  /^(Test|Fuzz|Example)[^[:space:]]*$/ { print > tests; next }
  /^Benchmark[^[:space:]]*$/ { print > benchmarks; next }
  /^[[:space:]]*$/ { next }
  /^(ok|\?)[[:space:]]/ { next }
  { print > unexpected }
' "$tests_file"

[[ -s "$current_tests" ]] || fail "no runnable tests found in $tests_file"
if [[ -s "$unexpected" ]]; then
  echo "partition-race-tests: unexpected go test -list output:" >&2
  sed 's/^/  /' "$unexpected" >&2
  exit 1
fi
if [[ -s "$benchmarks" ]]; then
  echo "partition-race-tests: benchmarks are not selected by -test.run:" >&2
  sed 's/^/  /' "$benchmarks" >&2
  exit 1
fi

sorted_tests="$work_dir/current-tests.sorted"
sort "$current_tests" >"$sorted_tests"
duplicates="$work_dir/current-tests.duplicates"
uniq -d "$sorted_tests" >"$duplicates"
if [[ -s "$duplicates" ]]; then
  echo "partition-race-tests: duplicate current tests:" >&2
  sed 's/^/  /' "$duplicates" >&2
  exit 1
fi

mkdir -p "$out_dir"
rm -f "$out_dir"/shard-*.tests "$out_dir"/shard-*.regex \
  "$out_dir/manifest.tsv" "$out_dir/mode"
for ((shard = 0; shard < shards; shard++)); do
  : >"$out_dir/shard-$shard.tests"
done

# Alternation: awk NR starts at one, so the first test goes to shard 1 (or
# shard 0 when there is only one shard).
awk -v shards="$shards" -v out="$out_dir" '
  { print > (out "/shard-" (NR % shards) ".tests") }
' "$current_tests"
mode="alternation"

# Coverage is checked from the current package list: every test must be
# assigned to exactly one shard, with no extras. A coverage mismatch is
# status 1 and must remain a hard failure.
assigned="$work_dir/assigned"
for ((shard = 0; shard < shards; shard++)); do
  cat "$out_dir/shard-$shard.tests" >>"$assigned"
done
assigned_sorted="$work_dir/assigned.sorted"
sort "$assigned" >"$assigned_sorted"
assigned_duplicates="$work_dir/assigned.duplicates"
uniq -d "$assigned_sorted" >"$assigned_duplicates"
missing="$work_dir/assigned.missing"
extra="$work_dir/assigned.extra"
comm -23 "$sorted_tests" "$assigned_sorted" >"$missing"
comm -13 "$sorted_tests" "$assigned_sorted" >"$extra"

if [[ -s "$assigned_duplicates" || -s "$missing" || -s "$extra" ]]; then
  echo "partition-race-tests: COVERAGE ASSERTION FAILED" >&2
  [[ ! -s "$assigned_duplicates" ]] || { echo "  tests assigned more than once:" >&2; sed 's/^/    /' "$assigned_duplicates" >&2; }
  [[ ! -s "$missing" ]] || { echo "  tests not assigned:" >&2; sed 's/^/    /' "$missing" >&2; }
  [[ ! -s "$extra" ]] || { echo "  assignments absent from current list:" >&2; sed 's/^/    /' "$extra" >&2; }
  exit 1
fi

total="$(wc -l <"$current_tests" | tr -d ' ')"
: >"$out_dir/manifest.tsv"
for ((shard = 0; shard < shards; shard++)); do
  tests="$out_dir/shard-$shard.tests"
  regex="$out_dir/shard-$shard.regex"
  count="$(wc -l <"$tests" | tr -d ' ')"
  if [[ "$count" -eq 0 ]]; then
    echo 'a^' >"$regex"
  else
    awk 'BEGIN { printf "^(" } { if (NR > 1) printf "|"; printf "%s", $0 } END { print ")$" }' "$tests" >"$regex"
  fi
  awk -v shard="$shard" '{ print shard "\t" $0 }' "$tests" >>"$out_dir/manifest.tsv"
  echo "partition-race-tests: shard $shard: $count of $total tests"
done
echo "$mode" >"$out_dir/mode"
echo "partition-race-tests: coverage assertion passed: all $total current tests assigned exactly once ($mode)"
