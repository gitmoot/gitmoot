#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C

usage() {
  cat <<'EOF'
Usage: partition-race-tests.sh --tests FILE --shards N --out-dir DIR [--timings FILE --package NAME]

Partitions the current top-level Go test list into N shards. With timings, it
uses deterministic longest-processing-time-first balancing. Without timings,
it uses the Stage 1 alternating split (line number modulo shard count).

Exit status 2 means the optional timings are malformed or do not exactly match
the current test list. Callers may retry without --timings. Any coverage
assertion failure uses status 1 and must remain a hard failure.
EOF
}

fail() {
  echo "partition-race-tests: $*" >&2
  exit 1
}

timings_invalid() {
  echo "partition-race-tests: unusable timings: $*" >&2
  exit 2
}

tests_file=""
timings_file=""
package=""
shards=""
out_dir=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tests)
      [[ $# -ge 2 ]] || fail "--tests requires a value"
      tests_file="$2"
      shift 2
      ;;
    --timings)
      [[ $# -ge 2 ]] || fail "--timings requires a value"
      timings_file="$2"
      shift 2
      ;;
    --package)
      [[ $# -ge 2 ]] || fail "--package requires a value"
      package="$2"
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
if [[ -n "$timings_file" && -z "$package" ]]; then
  fail "--package is required with --timings"
fi

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

mode="alternation"
if [[ -n "$timings_file" ]]; then
  [[ -f "$timings_file" ]] || timings_invalid "file does not exist: $timings_file"

  package_timings="$work_dir/package-timings"
  if ! awk -F '\t' -v package="$package" '
    NF != 3 { exit 1 }
    $1 == "" || $2 == "" || $3 !~ /^[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$/ { exit 1 }
    $1 == package { print $2 "\t" $3 }
  ' "$timings_file" >"$package_timings"; then
    timings_invalid "expected tab-separated package, test, elapsed rows"
  fi
  [[ -s "$package_timings" ]] || timings_invalid "no rows for package $package"

  timing_names="$work_dir/timing-names.sorted"
  cut -f1 "$package_timings" | sort >"$timing_names"
  timing_duplicates="$work_dir/timing-names.duplicates"
  uniq -d "$timing_names" >"$timing_duplicates"
  missing="$work_dir/timing-names.missing"
  unknown="$work_dir/timing-names.unknown"
  comm -23 "$sorted_tests" "$timing_names" >"$missing"
  comm -13 "$sorted_tests" "$timing_names" >"$unknown"

  if [[ -s "$timing_duplicates" || -s "$missing" || -s "$unknown" ]]; then
    echo "partition-race-tests: unusable timings: test set does not match current package $package" >&2
    if [[ -s "$timing_duplicates" ]]; then
      echo "  duplicate timing rows:" >&2
      sed 's/^/    /' "$timing_duplicates" >&2
    fi
    if [[ -s "$missing" ]]; then
      echo "  current tests missing from timings:" >&2
      sed 's/^/    /' "$missing" >&2
    fi
    if [[ -s "$unknown" ]]; then
      echo "  timing rows absent from current tests:" >&2
      sed 's/^/    /' "$unknown" >&2
    fi
    exit 2
  fi

  # LPT: longest tests first, test name as the stable duration tiebreak, then
  # assign to the lowest-load shard (lowest shard number breaks load ties).
  sorted_timings="$work_dir/package-timings.sorted"
  sort -t $'\t' -k2,2gr -k1,1 "$package_timings" >"$sorted_timings"
  awk -F '\t' -v shards="$shards" -v out="$out_dir" '
    BEGIN {
      for (i = 0; i < shards; i++) load[i] = 0
    }
    {
      selected = 0
      for (i = 1; i < shards; i++) {
        if (load[i] < load[selected]) selected = i
      }
      print $1 >> (out "/shard-" selected ".tests")
      load[selected] += $2
    }
  ' "$sorted_timings"
  mode="lpt"
else
  # Preserve Stage 1 exactly: awk NR starts at one, so the first test goes to
  # shard 1 (or shard 0 when there is only one shard).
  awk -v shards="$shards" -v out="$out_dir" '
    { print > (out "/shard-" (NR % shards) ".tests") }
  ' "$current_tests"
fi

# Coverage is checked from the current package list, never from the timings.
# A coverage mismatch is status 1 so CI cannot turn it into a timing fallback.
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
