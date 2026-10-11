#!/usr/bin/env bash
# Run from any directory. Use a detached baseline worktree with identical
# scale_benchmark_test.go and commit_components_benchmark_test.go copied from the candidate.
# This script never changes either tree, creates worktrees, or changes branches.
set -euo pipefail
if [[ $# -ne 3 ]]; then
  echo "usage: $0 BASELINE_TREE CANDIDATE_TREE OUTPUT_DIRECTORY" >&2
  exit 2
fi
base=$(cd "$1" && pwd)
candidate=$(cd "$2" && pwd)
mkdir -p "$3"
output=$(cd "$3" && pwd)
export GOMAXPROCS=12 TIMEIDX_SCALE=1
for file in scale_benchmark_test.go commit_components_benchmark_test.go; do
  cmp "$base/index/timeidx/$file" "$candidate/index/timeidx/$file"
done
go version > "$output/environment.txt"
git -C "$base" rev-parse HEAD >> "$output/environment.txt"
git -C "$candidate" rev-parse HEAD >> "$output/environment.txt"
git -C "$candidate" diff --stat >> "$output/environment.txt"
echo "GOMAXPROCS=$GOMAXPROCS; same-host serial A/B; benchtime=200ms/10x" >> "$output/environment.txt"
: > "$output/baseline.txt"
: > "$output/candidate.txt"
measure() {
  local tree=$1 file=$2
  (cd "$tree" && go test ./index/timeidx -run '^$' \
    -bench '^BenchmarkModernization$/(Batch64|Search|Reopen)' \
    -benchmem -benchtime=200ms -count=1) >> "$file"
  (cd "$tree" && go test ./index/timeidx -run '^$' \
    -bench '^BenchmarkScaleDurableBatches$/N=10000000$/Random=false$/Batch=1$' \
    -benchmem -benchtime=10x -count=1) >> "$file"
}
for ((n=0; n<10; n++)); do
  if (( n % 2 == 0 )); then
    measure "$base" "$output/baseline.txt"
    measure "$candidate" "$output/candidate.txt"
  else
    measure "$candidate" "$output/candidate.txt"
    measure "$base" "$output/baseline.txt"
  fi
done
benchstat "$output/baseline.txt" "$output/candidate.txt" > "$output/benchstat.txt"
